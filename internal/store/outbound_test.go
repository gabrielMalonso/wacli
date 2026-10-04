package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func outboundFixture(t *testing.T, db *DB, n int, kind DraftKind, account string) (DraftRevision, OutboundReservation) {
	t.Helper()
	p := DraftPayloadData{Account: DraftIdentity{PN: account, LID: "90001@lid"}, Recipient: DraftRecipient{JID: "15550000002@s.whatsapp.net", PN: "15550000002@s.whatsapp.net", LID: "90002@lid"}, Kind: kind}
	switch kind {
	case DraftTextKind:
		p.Text = &DraftText{Text: "frozen text"}
	case DraftContactKind:
		contact, err := NewDraftContact("fixture", "15550000003")
		if err != nil {
			t.Fatal(err)
		}
		p.Contact = &contact
	case DraftDocumentKind:
		p.Document = &DraftDocument{Filename: "fixture.txt", MIME: "text/plain", Size: 7, SHA256: strings.Repeat("a", 64)}
	}
	id, rid := fmt.Sprintf("%032x", n), fmt.Sprintf("%032x", n+10000)
	payload, err := NewDraftPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	review := DraftReviewSnapshot{RequestedRaw: p.Recipient.JID}
	at := time.Unix(1700000000, 0)
	if kind == DraftDocumentKind {
		review.SnapshotPath, _ = DraftSnapshotRelativePath(rid)
		review.VerifiedAtCreate = at
	}
	revision, err := NewDraftRevision(id, rid, at, payload, review)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDraft(context.Background(), revision, ""); err != nil {
		t.Fatal(err)
	}
	return revision, OutboundReservation{Version: 1, ID: fmt.Sprintf("%032x", n+20000), DraftID: id, RevisionID: rid, Hash: payload.Hash(), Key: fmt.Sprintf("key-%d", n), MessageID: fmt.Sprintf("3EB0FIXTURE%d", n), Account: p.Account, CreatedAt: at.Add(time.Second)}
}
func assertOutboundCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *OutboundError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s: %v", code, err)
	}
}
func outboundStep(t *testing.T, db *DB, o OutboundOperation, phase OutboundPhase, result OutboundResult) OutboundOperation {
	t.Helper()
	next, err := db.Outbound().Checkpoint(t.Context(), OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: result, At: o.UpdatedAt.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func fixtureFact(o OutboundOperation, fact OutboundFact, at time.Time) OutboundObservation {
	f := OutboundObservation{Fact: fact, Source: OutboundLiveReceipt, ChatJID: o.Recipient.JID, ActorJID: o.Recipient.JID, ObservedAt: at}
	if fact == OutboundAck {
		f.Source = OutboundSendResponse
		f.ActorJID = o.Account.PN
	}
	if fact == OutboundOwnEcho {
		f.Source = OutboundLiveEcho
		f.ActorJID = o.Account.PN
	}
	return f
}

func TestOutboundReservationBindingDiscardOldRevisionAndIdentity(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	rev, r := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	newRev := draftFixtureRevision(t, rev.DraftID(), strings.Repeat("f", 32), "new head", rev.CreatedAt().Add(time.Second))
	// Keep the same frozen own identity and target while replacing the head.
	data := newRev.Payload().Data()
	data.Account = rev.Payload().Data().Account
	data.Recipient = rev.Payload().Data().Recipient
	p, err := NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	newRev, err = NewDraftRevision(newRev.DraftID(), newRev.ID(), newRev.CreatedAt(), p, newRev.Review())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDraft(ctx, newRev, rev.ID()); err != nil {
		t.Fatal(err)
	}
	o, err := db.Outbound().Reserve(ctx, r)
	if err != nil || o.RevisionID != rev.ID() || o.Hash != rev.Payload().Hash() || o.MessageID != r.MessageID {
		t.Fatal(o, err)
	}
	if _, err = db.DiscardDraft(ctx, rev.DraftID(), newRev.ID()); err != nil {
		t.Fatal(err)
	}
	duplicate := r
	duplicate.ID = strings.Repeat("e", 32)
	duplicate.MessageID = "IGNORED_CANDIDATE"
	duplicate.Account = DraftIdentity{}
	old, err := db.Outbound().Reserve(ctx, duplicate)
	if err != nil || old != o {
		t.Fatal("duplicate checked discard/current identity or changed IDs", old, err)
	}
	for _, change := range []func(*OutboundReservation){func(r *OutboundReservation) { r.Hash = strings.Repeat("0", 64) }, func(r *OutboundReservation) { r.RevisionID = newRev.ID(); r.Hash = newRev.Payload().Hash() }} {
		conflict := duplicate
		change(&conflict)
		_, err = db.Outbound().Reserve(ctx, conflict)
		assertOutboundCode(t, err, "idempotency_conflict")
	}
	newKey := duplicate
	newKey.Key = "other-key"
	_, err = db.Outbound().Reserve(ctx, newKey)
	assertOutboundCode(t, err, "draft_conflict")
	_, r2 := outboundFixture(t, db, 2, DraftTextKind, "15550000001@s.whatsapp.net")
	bad := r2
	bad.Account.PN = "15550000009@s.whatsapp.net"
	_, err = db.Outbound().Reserve(ctx, bad)
	assertOutboundCode(t, err, "identity_unavailable")
	bad = r2
	bad.Hash = strings.Repeat("0", 64)
	_, err = db.Outbound().Reserve(ctx, bad)
	assertOutboundCode(t, err, "hash_conflict")
}

func TestOutboundUniqueCASConcurrentAndDiscardRace(t *testing.T) {
	db := openTestDB(t)
	_, r := outboundFixture(t, db, 3, DraftTextKind, "15550000001@s.whatsapp.net")
	var wg sync.WaitGroup
	results := make(chan OutboundOperation, 8)
	failures := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := r
			candidate.ID = fmt.Sprintf("%032x", 30000+i)
			candidate.MessageID = fmt.Sprintf("FIXTURE%d", i)
			o, err := db.Outbound().Reserve(t.Context(), candidate)
			if err != nil {
				failures <- err
			} else {
				results <- o
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	id := ""
	var o OutboundOperation
	for result := range results {
		if id != "" && id != result.ID {
			t.Fatal("two operations")
		}
		id = result.ID
		o = result
	}
	count := 0
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM outbound_operations").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	ch := OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: OutboundPreparing, Result: OutboundPending, At: o.UpdatedAt.Add(time.Second)}
	wg.Add(2)
	successes := make(chan bool, 2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, err := db.Outbound().Checkpoint(t.Context(), ch)
			if err != nil {
				var e *OutboundError
				if !errors.As(err, &e) || e.Code != "checkpoint_conflict" {
					failures := fmt.Errorf("unexpected CAS error: %w", err)
					t.Error(failures)
				}
			}
			successes <- err == nil
		}()
	}
	wg.Wait()
	close(successes)
	wins := 0
	for ok := range successes {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("CAS winners", wins)
	}
	for i := 10; i < 16; i++ {
		rev, candidate := outboundFixture(t, db, i, DraftTextKind, "15550000001@s.whatsapp.net")
		start := make(chan struct{})
		reserve := make(chan error, 1)
		discard := make(chan error, 1)
		go func() { <-start; _, err := db.Outbound().Reserve(t.Context(), candidate); reserve <- err }()
		go func() { <-start; _, err := db.DiscardDraft(t.Context(), rev.DraftID(), rev.ID()); discard <- err }()
		close(start)
		reserveErr, discardErr := <-reserve, <-discard
		if discardErr != nil {
			t.Fatal(discardErr)
		}
		if reserveErr != nil {
			assertOutboundCode(t, reserveErr, "draft_conflict")
		}
		entry, err := db.Outbound().Read(t.Context(), candidate.ID, "", "", 20, "")
		if reserveErr == nil && (err != nil || entry.Operation.ID != candidate.ID) {
			t.Fatal("lost winning reserve", err)
		}
		if reserveErr != nil {
			assertOutboundCode(t, err, "not_found")
		}
	}
}

func TestOutboundCheckpointCrashReopenAndSQLInvariants(t *testing.T) {
	for _, kind := range []DraftKind{DraftTextKind, DraftDocumentKind, DraftContactKind} {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wacli.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			_, r := outboundFixture(t, db, 1, kind, "15550000001@s.whatsapp.net")
			o, err := db.Outbound().Reserve(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			phases := []OutboundPhase{OutboundReserved, OutboundPreparing}
			if kind == DraftDocumentKind {
				phases = append(phases, OutboundUploadPossible, OutboundUploadReturned)
			}
			phases = append(phases, OutboundDispatchPossible, OutboundFinalized)
			for i, phase := range phases {
				if i > 0 {
					result := OutboundPending
					if phase == OutboundFinalized {
						result = OutboundUncertain
					}
					o = outboundStep(t, db, o, phase, result)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
				ro, err := OpenReadOnly(path)
				if err != nil {
					t.Fatal(err)
				}
				entry, err := ro.Outbound().Read(t.Context(), o.ID, "", "", 20, "")
				if err != nil || entry.Operation.Phase != phase || entry.Operation.Generation != o.Generation {
					t.Fatal(entry, err)
				}
				ro.Close()
				if entry.Evidence.Accepted != "unknown" || entry.Operation.EvidenceStatus(entry.Evidence) == "accepted" {
					t.Fatal("invented ack")
				}
				db, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer db.Close()
			for _, query := range []string{"DELETE FROM outbound_operations", "UPDATE outbound_operations SET message_id='different'", "UPDATE outbound_operations SET phase='reserved',generation=generation+1", "UPDATE outbound_operations SET updated_at=updated_at+1"} {
				if _, err = db.sql.Exec(query); err == nil {
					t.Fatal("SQL allowed", query)
				}
			}
			_, err = db.Outbound().Checkpoint(t.Context(), OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: OutboundPreparing, Result: OutboundPending, At: o.UpdatedAt.Add(time.Second)})
			assertOutboundCode(t, err, "checkpoint_conflict")
		})
	}
}

func TestOutboundFactsMonotonicScopeDedupeAndPagination(t *testing.T) {
	db := openTestDB(t)
	_, r := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	o, err := db.Outbound().Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	o = outboundStep(t, db, o, OutboundPreparing, OutboundPending)
	o = outboundStep(t, db, o, OutboundDispatchPossible, OutboundPending)
	readAt := o.UpdatedAt.Add(time.Second)
	read := fixtureFact(o, OutboundRead, readAt)
	read.EventAt = &readAt
	for range 2 {
		o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, read)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []OutboundObservation{fixtureFact(o, OutboundRead, o.UpdatedAt.Add(time.Second)), fixtureFact(o, OutboundAck, o.UpdatedAt.Add(time.Second))} {
		bad.ActorJID = "15550000099@s.whatsapp.net"
		_, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, bad)
		assertOutboundCode(t, err, "invalid_arguments")
	}
	_, err = db.Outbound().Observe(t.Context(), o.ID, DraftIdentity{PN: "15550000099@s.whatsapp.net"}, o.MessageID, read)
	assertOutboundCode(t, err, "observation_scope_conflict")
	_, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, "OTHER", read)
	assertOutboundCode(t, err, "observation_scope_conflict")
	o = outboundStep(t, db, o, OutboundFinalized, OutboundUncertain)
	for _, fact := range []OutboundFact{OutboundDelivered, OutboundServerError, OutboundOwnEcho} {
		f := fixtureFact(o, fact, o.UpdatedAt.Add(time.Second))
		if fact == OutboundServerError {
			f.ErrorCode = "server_error"
		}
		o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, f)
		if err != nil {
			t.Fatal(err)
		}
	}
	entry, err := db.Outbound().Read(t.Context(), o.ID, "", "", 1, "")
	if err != nil || entry.Operation.Result != OutboundUncertain || entry.Operation.EvidenceStatus(entry.Evidence) != "read" || !entry.Observations.HasMore || len(entry.Observations.Items) != 1 || !entry.Evidence.OwnEcho || !entry.Evidence.ServerError {
		t.Fatal(entry, err)
	}
	count := 1
	cursor := entry.Observations.NextCursor
	for cursor != nil {
		page, err := db.Outbound().Read(t.Context(), "", r.Key, o.Account.PN, 2, *cursor)
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Observations.Items)
		cursor = page.Observations.NextCursor
	}
	if count != 4 {
		t.Fatal("semantic dedupe/page count", count)
	}
	// The older protocol timestamp arriving later does not erase a read fact.
	echoOnly := db.Outbound()
	_, r2 := outboundFixture(t, db, 2, DraftTextKind, o.Account.PN)
	other, err := echoOnly.Reserve(t.Context(), r2)
	if err != nil {
		t.Fatal(err)
	}
	other, err = echoOnly.Observe(t.Context(), other.ID, other.Account, other.MessageID, fixtureFact(other, OutboundOwnEcho, other.UpdatedAt.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := echoOnly.Read(t.Context(), other.ID, "", "", 20, "")
	if err != nil || e.Evidence.Accepted != "unknown" || e.Operation.EvidenceStatus(e.Evidence) != "incomplete" {
		t.Fatal("echo invented acceptance", e, err)
	}
}

func TestOutboundAcceptedAckAtomicAndGroupScope(t *testing.T) {
	db := openTestDB(t)
	rev, r := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	p := rev.Payload().Data()
	p.Recipient = DraftRecipient{JID: "120363000000001@g.us"}
	payload, err := NewDraftPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	rid := strings.Repeat("f", 32)
	rev2, err := NewDraftRevision(rev.DraftID(), rid, rev.CreatedAt().Add(time.Second), payload, DraftReviewSnapshot{RequestedRaw: p.Recipient.JID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDraft(t.Context(), rev2, rev.ID()); err != nil {
		t.Fatal(err)
	}
	r.RevisionID, r.Hash = rid, payload.Hash()
	o, err := db.Outbound().Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	o = outboundStep(t, db, o, OutboundPreparing, OutboundPending)
	o = outboundStep(t, db, o, OutboundDispatchPossible, OutboundPending)
	ch := OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: OutboundFinalized, Result: OutboundAccepted, At: o.UpdatedAt.Add(time.Second)}
	_, err = db.Outbound().Checkpoint(t.Context(), ch)
	assertOutboundCode(t, err, "invalid_arguments")
	f := fixtureFact(o, OutboundRead, ch.At)
	f.ActorJID = "15550000003@s.whatsapp.net"
	o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, f)
	if err != nil {
		t.Fatal(err)
	}
	ch.Generation = o.Generation
	ch.At = o.UpdatedAt.Add(time.Second)
	ack := fixtureFact(o, OutboundAck, ch.At)
	ch.Ack = &ack
	o, err = db.Outbound().Checkpoint(t.Context(), ch)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := db.Outbound().Read(t.Context(), o.ID, "", "", 20, "")
	if err != nil || entry.Evidence.Delivered != "unknown" || entry.Evidence.ReadParticipants == nil || *entry.Evidence.ReadParticipants != 1 || entry.Evidence.DeliveredParticipants == nil || *entry.Evidence.DeliveredParticipants != 1 || entry.Evidence.Read != "unknown" || entry.Operation.EvidenceStatus(entry.Evidence) != "accepted" {
		t.Fatal(entry, err)
	}
	if _, err = db.sql.Exec("UPDATE outbound_observations SET fact='delivered'"); err == nil {
		t.Fatal("mutable fact")
	}
	if _, err = db.sql.Exec("DELETE FROM outbound_observations"); err == nil {
		t.Fatal("deleted fact")
	}
}

func TestOutboundPagesAccountStoreAndCorruption(t *testing.T) {
	db := openTestDB(t)
	for i := 1; i <= 4; i++ {
		account := "15550000001@s.whatsapp.net"
		if i == 4 {
			account = "15550000009@s.whatsapp.net"
		}
		_, r := outboundFixture(t, db, i, DraftTextKind, account)
		if _, err := db.Outbound().Reserve(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.Outbound().List(t.Context(), db.path, "", 2, "")
	if err != nil || !page.HasMore {
		t.Fatal(page, err)
	}
	next, err := db.Outbound().List(t.Context(), db.path, "", 20, *page.NextCursor)
	if err != nil || len(next.Items) != 2 || next.HasMore {
		t.Fatal(next, err)
	}
	filtered, err := db.Outbound().List(t.Context(), db.path, "15550000009@s.whatsapp.net", 20, "")
	if err != nil || len(filtered.Items) != 1 {
		t.Fatal(filtered, err)
	}
	for _, scope := range []struct{ store, account string }{{"/elsewhere", ""}, {db.path, "15550000009@s.whatsapp.net"}} {
		_, err = db.Outbound().List(t.Context(), scope.store, scope.account, 2, *page.NextCursor)
		assertOutboundCode(t, err, "invalid_cursor")
	}
	for _, token := range []string{"", strings.Repeat("A", 16385), *page.NextCursor + "=", "e30"} {
		assertOutboundCode(t, ValidateOutboundCursor(token), "invalid_cursor")
	}
	_, err = db.Outbound().Read(t.Context(), page.Items[0].Operation.ID, "", "", 20, *page.NextCursor)
	assertOutboundCode(t, err, "invalid_cursor")
	// List never needs full payload; show validates its referenced revision.
	if _, err = db.sql.Exec("DROP TRIGGER draft_revision_no_update;UPDATE draft_revisions SET payload_json='{}'"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Outbound().List(t.Context(), db.path, "", 20, ""); err != nil {
		t.Fatal("list read payload", err)
	}
	_, err = db.Outbound().Read(t.Context(), page.Items[0].Operation.ID, "", "", 20, "")
	assertOutboundCode(t, err, "store_error")
	if _, err = db.sql.Exec("DROP TRIGGER outbound_transition;PRAGMA ignore_check_constraints=ON;UPDATE outbound_operations SET phase='SECRET_INVALID',generation=generation+1"); err != nil {
		t.Fatal(err)
	}
	_, err = db.Outbound().List(t.Context(), db.path, "", 20, "")
	assertOutboundCode(t, err, "store_error")
}

func TestOutboundMigration31ReadonlyOldArchiveAndAccountIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	history := historyTestAttempt()
	if err = db.BeginHistoryAttempt(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("DROP TABLE outbound_observations;DROP TABLE outbound_operations;DELETE FROM schema_migrations WHERE version=31"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	if _, err = OpenReadOnly(path); err == nil {
		t.Fatal("readonly upgraded30")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("readonly mutated30")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrateOutbound(db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ReadDraft(t.Context(), rev.DraftID(), rev.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ListHistoryRecoveryEvidence(t.Context(), []string{history.RequestedChatJID}); err != nil {
		t.Fatal(err)
	}
	for i, account := range []string{"15550000001@s.whatsapp.net", "15550000009@s.whatsapp.net"} {
		_, r := outboundFixture(t, db, i+2, DraftTextKind, account)
		r.Key = "same-key"
		if _, err = db.Outbound().Reserve(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{"15550000001@s.whatsapp.net", "15550000009@s.whatsapp.net"} {
		entry, err := db.Outbound().Read(t.Context(), "", "same-key", account, 20, "")
		if err != nil || entry.Operation.Account.PN != account {
			t.Fatal(entry, err)
		}
	}
	other := openTestDB(t)
	_, err = other.Outbound().Read(t.Context(), "", "same-key", "15550000001@s.whatsapp.net", 20, "")
	assertOutboundCode(t, err, "not_found")
}

func TestOutboundBoundedInputMessageUniqueAndCheckpointScope(t *testing.T) {
	db := openTestDB(t)
	_, r := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	for _, key := range []string{"", "space key", "\n", "não", strings.Repeat("x", 129)} {
		bad := r
		bad.Key = key
		_, err := db.Outbound().Reserve(t.Context(), bad)
		assertOutboundCode(t, err, "invalid_arguments")
	}
	o, err := db.Outbound().Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	_, second := outboundFixture(t, db, 2, DraftTextKind, r.Account.PN)
	second.MessageID = r.MessageID
	if _, err = db.Outbound().Reserve(t.Context(), second); err == nil {
		t.Fatal("message ID reused within account")
	}
	page, err := db.Outbound().List(t.Context(), db.path, "", 20, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatal("partial failed reservation", page, err)
	}
	ch := OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: "different-message", Generation: o.Generation, Phase: OutboundPreparing, Result: OutboundPending, At: o.UpdatedAt.Add(time.Second)}
	_, err = db.Outbound().Checkpoint(t.Context(), ch)
	assertOutboundCode(t, err, "checkpoint_scope_conflict")
	ch.MessageID, ch.Account.PN = o.MessageID, "15550000009@s.whatsapp.net"
	_, err = db.Outbound().Checkpoint(t.Context(), ch)
	assertOutboundCode(t, err, "checkpoint_scope_conflict")
	current, err := db.Outbound().Read(t.Context(), o.ID, "", "", 20, "")
	if err != nil || current.Operation.Generation != o.Generation {
		t.Fatal(current, err)
	}
}

func TestOutboundParticipantAliasesDevicesAndCorruptionOutsidePage(t *testing.T) {
	db := openTestDB(t)
	rev, r := outboundFixture(t, db, 1, DraftTextKind, "15550000001@s.whatsapp.net")
	p := rev.Payload().Data()
	p.Recipient = DraftRecipient{JID: "120363000000001@g.us"}
	payload, err := NewDraftPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := NewDraftRevision(rev.DraftID(), strings.Repeat("f", 32), rev.CreatedAt().Add(time.Second), payload, DraftReviewSnapshot{RequestedRaw: p.Recipient.JID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDraft(t.Context(), revision, rev.ID()); err != nil {
		t.Fatal(err)
	}
	r.RevisionID, r.Hash = revision.ID(), payload.Hash()
	o, err := db.Outbound().Reserve(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	o = outboundStep(t, db, o, OutboundPreparing, OutboundPending)
	o = outboundStep(t, db, o, OutboundDispatchPossible, OutboundPending)
	o = outboundStep(t, db, o, OutboundFinalized, OutboundUncertain)
	f := fixtureFact(o, OutboundRead, o.UpdatedAt.Add(time.Second))
	f.ActorJID, f.Device = "90003@lid", 1
	o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, f)
	if err != nil {
		t.Fatal(err)
	}
	f.ActorJID, f.ActorAlias, f.Device = "15550000003@s.whatsapp.net", "90003@lid", 2
	f.ObservedAt = o.UpdatedAt.Add(time.Second)
	o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, f)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := db.Outbound().Read(t.Context(), o.ID, "", "", 1, "")
	if err != nil || !entry.Observations.HasMore || *entry.Evidence.ReadParticipants != 1 || entry.Evidence.Read != "unknown" || entry.Operation.Result != OutboundUncertain {
		t.Fatal(entry, err)
	}
	f.ActorAlias = "90004@lid"
	_, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, f)
	assertOutboundCode(t, err, "observation_scope_conflict")
	// Simulate corrupt persisted aliases, outside the requested first page.
	if _, err = db.sql.Exec("DROP TRIGGER outbound_observation_immutable"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("UPDATE outbound_observations SET actor_jid='15550000004@s.whatsapp.net',actor_alias='90003@lid' WHERE device=1"); err != nil {
		t.Fatal(err)
	}
	_, err = db.Outbound().Read(t.Context(), o.ID, "", "", 1, "")
	assertOutboundCode(t, err, "store_error")
	_, err = db.Outbound().List(t.Context(), db.path, "", 1, "")
	assertOutboundCode(t, err, "store_error")
}
