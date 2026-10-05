package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
)

func cleanupFixtureRevision(t testing.TB, id, rid string, kind DraftKind, at time.Time, caption string) DraftRevision {
	t.Helper()
	p := DraftPayloadData{Account: DraftIdentity{PN: "15550000001@s.whatsapp.net"}, Recipient: DraftRecipient{JID: "15550000002@s.whatsapp.net"}, Kind: kind}
	review := DraftReviewSnapshot{RequestedRaw: p.Recipient.JID}
	if kind == DraftDocumentKind {
		p.Document = &DraftDocument{Filename: "fixture.bin", MIME: "application/octet-stream", Caption: caption, Size: 7, SHA256: strings.Repeat("a", 64)}
		review.SnapshotPath, _ = DraftSnapshotRelativePath(rid)
		review.VerifiedAtCreate = at
	} else {
		p.Text = &DraftText{Text: caption}
	}
	payload, err := NewDraftPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewDraftRevision(id, rid, at, payload, review)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cleanupSelection(r DraftRevision, head string) DraftCleanupSelection {
	return DraftCleanupSelection{DraftID: r.DraftID(), RevisionID: r.ID(), ExpectedHeadID: head, Hash: r.Payload().Hash()}
}

func assertCleanupCode(t *testing.T, err error, code DraftCleanupCode) {
	t.Helper()
	var failure *DraftCleanupError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want cleanup %s, got %v", code, err)
	}
}

func TestDraftCleanupPageEligibilityCapsAndCursors(t *testing.T) {
	db := openTestDB(t)
	id := strings.Repeat("a", 32)
	at := time.Unix(1700000000, 0)
	var revisions []DraftRevision
	head := ""
	for i := 1; i <= 205; i++ {
		kind := DraftDocumentKind
		if i == 2 {
			kind = DraftTextKind
		}
		r := cleanupFixtureRevision(t, id, fmt.Sprintf("%032x", i), kind, at.Add(time.Duration(i)*time.Second), "fixture")
		if _, err := db.WriteDraft(t.Context(), r, head); err != nil {
			t.Fatal(err)
		}
		revisions, head = append(revisions, r), r.ID()
	}
	_, err := db.Outbound().Reserve(t.Context(), OutboundReservation{Version: 1, ID: strings.Repeat("b", 32), DraftID: id, RevisionID: revisions[0].ID(), Hash: revisions[0].Payload().Hash(), Key: "fixture-key", MessageID: "fixture-id", Account: revisions[0].Payload().Data().Account, CreatedAt: at.Add(210 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 200, "")
	if err != nil || active.Counts.Active != 200 || active.Counts.Eligible != 0 {
		t.Fatal("active revisions must all be protected", active.Counts, err)
	}
	if _, err := db.DiscardDraft(t.Context(), id, head); err != nil {
		t.Fatal(err)
	}
	page, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 200, "")
	if err != nil || len(page.Items) != 200 || !page.HasMore || page.NextCursor == nil || page.Counts != (DraftCleanupCounts{Examined: 200, Eligible: 198, NonDocument: 1, OutboundProtected: 1, EligibleBytesAtCreate: 198 * 7}) {
		t.Fatal("bounded page", page.Counts, err)
	}
	if page.Items[0].Eligibility != DraftCleanupOutboundProtected || page.Items[0].OutboundRefs != 1 || page.Items[1].Eligibility != DraftCleanupNonDocument {
		t.Fatal("explicit protected reasons", page.Items[:2])
	}
	for _, item := range page.Items {
		if len(item.Hash) != 64 || item.Document != nil && len(item.Document.SHA256) != 64 {
			t.Fatal("truncated digest", item)
		}
	}
	next, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 20, *page.NextCursor)
	if err != nil || len(next.Items) != 5 || next.HasMore || next.NextCursor != nil || next.Counts.Eligible != 5 || next.Items[4].RevisionID != head {
		t.Fatal("discarded head and page-local counts", next, err)
	}
	for _, limit := range []int{0, 201} {
		_, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, limit, "")
		assertCleanupCode(t, err, DraftCleanupInvalidArguments)
	}
	for _, token := range []string{"bad", *page.NextCursor + "=", strings.Repeat("x", 16385)} {
		_, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 20, token)
		assertCleanupCode(t, err, DraftCleanupInvalidCursor)
	}
	for _, change := range []func(*draftCleanupCursor){
		func(c *draftCleanupCursor) { c.Domain = "draft-list" },
		func(c *draftCleanupCursor) { c.Store += "/other" },
		func(c *draftCleanupCursor) { c.DraftID = strings.Repeat("f", 32) },
		func(c *draftCleanupCursor) { c.Policy = "older-policy" },
	} {
		c, _ := decodeDraftCleanupCursor(*page.NextCursor)
		change(&c)
		raw, _ := json.Marshal(c)
		_, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 20, base64.RawURLEncoding.EncodeToString(raw))
		assertCleanupCode(t, err, DraftCleanupInvalidCursor)
	}
	if _, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), id, 20, ""); err != nil {
		t.Fatal("no snapshot directory exists, preview must not stat it", err)
	}
}

func TestDraftCleanupAllOutboundStatesProtectedAndBindingsRetained(t *testing.T) {
	for _, state := range []string{"reserved", "preparing", "upload_possible", "upload_returned", "dispatch_possible", "uncertain", "accepted", "rejected", "not_dispatched"} {
		t.Run(state, func(t *testing.T) {
			db := openTestDB(t)
			r, reservation := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
			o, err := db.Outbound().Reserve(t.Context(), reservation)
			if err != nil {
				t.Fatal(err)
			}
			if state != "reserved" {
				o = outboundStep(t, db, o, OutboundPreparing, OutboundPending)
			}
			if state != "reserved" && state != "preparing" && state != "not_dispatched" {
				o = outboundStep(t, db, o, OutboundUploadPossible, OutboundPending)
			}
			if state == "upload_returned" || state == "dispatch_possible" || state == "accepted" || state == "rejected" {
				o = outboundStep(t, db, o, OutboundUploadReturned, OutboundPending)
			}
			if state == "dispatch_possible" || state == "accepted" || state == "rejected" {
				o = outboundStep(t, db, o, OutboundDispatchPossible, OutboundPending)
			}
			results := map[string]OutboundResult{"uncertain": OutboundUncertain, "accepted": OutboundAccepted, "rejected": OutboundRejected, "not_dispatched": OutboundNotDispatched}
			if result, ok := results[state]; ok {
				ch := OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: OutboundFinalized, Result: result, At: o.UpdatedAt.Add(time.Second)}
				if result == OutboundAccepted {
					ack := fixtureFact(o, OutboundAck, ch.At)
					ch.Ack = &ack
				}
				o, err = db.Outbound().Checkpoint(t.Context(), ch)
				if err != nil {
					t.Fatal(err)
				}
				if result == OutboundAccepted {
					o, err = db.Outbound().Observe(t.Context(), o.ID, o.Account, o.MessageID, fixtureFact(o, OutboundRead, o.UpdatedAt.Add(time.Second)))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID()); err != nil {
				t.Fatal(err)
			}
			before, err := db.Outbound().Read(t.Context(), o.ID, "", "", 200, "")
			if err != nil {
				t.Fatal(err)
			}
			page, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), r.DraftID(), 20, "")
			if err != nil || page.Counts.OutboundProtected != 1 || page.Counts.Eligible != 0 {
				t.Fatal(page, err)
			}
			selection := cleanupSelection(r, r.ID())
			_, err = db.ReadDraftCleanup(t.Context(), selection)
			assertCleanupCode(t, err, DraftCleanupProtected)
			_, err = db.ConfirmDraftCleanup(t.Context(), selection)
			assertCleanupCode(t, err, DraftCleanupProtected)
			duplicate := reservation
			duplicate.ID, duplicate.MessageID, duplicate.Account = strings.Repeat("e", 32), "different-candidate", DraftIdentity{}
			retained, err := db.Outbound().Reserve(t.Context(), duplicate)
			if err != nil || !reflect.DeepEqual(retained, o) {
				t.Fatal("key binding lost after discard/protected cleanup", retained, err)
			}
			after, err := db.Outbound().Read(t.Context(), o.ID, "", "", 200, "")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("operation/receipt evidence changed", err)
			}
			entry, err := db.ReadDraft(t.Context(), r.DraftID(), r.ID())
			if err != nil || !bytes.Equal(entry.Revision.Payload().CanonicalJSON(), r.Payload().CanonicalJSON()) || entry.Revision.Review() != r.Review() || entry.Record.HeadRevisionID != r.ID() {
				t.Fatal("revision no longer reconstitutable", err)
			}
		})
	}
}

func TestDraftCleanupFULLReaffirmsSingleHeadAndPreservesMetadata(t *testing.T) {
	db := openTestDB(t)
	r, reservation := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
	before, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID())
	if err != nil {
		t.Fatal(err)
	}
	selection := cleanupSelection(r, r.ID())
	confirmed, err := db.confirmDraftCleanup(t.Context(), selection, outboundSQLiteIO(), r.CreatedAt())
	if err != nil || confirmed.Record.UpdatedAt.UnixNano() != before.Record.UpdatedAt.UnixNano()+1 || confirmed.Record.HeadRevisionID != r.ID() || confirmed.Record.State != "discarded" || !confirmed.Record.DiscardedAt.Equal(*before.Record.DiscardedAt) || confirmed.Revision.Review() != r.Review() || !bytes.Equal(confirmed.Revision.Payload().CanonicalJSON(), r.Payload().CanonicalJSON()) {
		t.Fatal("FULL guard must really advance only updated_at", confirmed.Record, err)
	}
	_, err = db.Outbound().Reserve(t.Context(), reservation)
	assertOutboundCode(t, err, "draft_conflict")
	updated := cleanupFixtureRevision(t, r.DraftID(), strings.Repeat("f", 32), DraftDocumentKind, time.Now(), "replacement")
	_, err = db.WriteDraft(t.Context(), updated, r.ID())
	assertDraftCode(t, err, "draft_conflict")
	for _, query := range []string{"UPDATE draft_revisions SET payload_json='{}'", "DELETE FROM draft_revisions"} {
		if _, err := db.sql.Exec(query); err == nil {
			t.Fatal("immutability protection changed", query)
		}
	}
	rows, err := db.sql.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("cleanup damaged FKs")
	}
	rows.Close()
	if _, err := db.sql.Exec("UPDATE drafts SET updated_at=? WHERE id=?", int64(math.MaxInt64), r.DraftID()); err != nil {
		t.Fatal(err)
	}
	_, err = db.ConfirmDraftCleanup(t.Context(), selection)
	assertCleanupCode(t, err, DraftCleanupStoreError)
}

func TestDraftCleanupFULLFailuresNeverReturnAuthorization(t *testing.T) {
	for _, stage := range []string{"set_full", "begin", "cancel", "commit_before", "commit_lost", "rollback", "restore", "restore_check"} {
		t.Run(stage, func(t *testing.T) {
			db := openTestDB(t)
			db.sql.SetMaxOpenConns(1)
			r, _ := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
			before, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			disk := outboundSQLiteIO()
			baseExec, baseSync := disk.exec, disk.synchronous
			var baseline int
			if err := db.sql.QueryRow("PRAGMA synchronous").Scan(&baseline); err != nil {
				t.Fatal(err)
			}
			marker := errors.New("synthetic cleanup failure")
			checks := 0
			disk.synchronous = func(ctx context.Context, c *sql.Conn) (int, error) {
				checks++
				if stage == "restore_check" && checks == 3 {
					return -1, marker
				}
				return baseSync(ctx, c)
			}
			disk.exec = func(ctx context.Context, c *sql.Conn, q string, args ...any) (sql.Result, error) {
				if stage == "set_full" && q == "PRAGMA synchronous=FULL" || stage == "begin" && q == "BEGIN IMMEDIATE" || stage == "commit_before" && q == "COMMIT" || stage == "rollback" && q == "ROLLBACK" || stage == "restore" && q == fmt.Sprintf("PRAGMA synchronous=%d", baseline) {
					return nil, marker
				}
				res, err := baseExec(ctx, c, q, args...)
				if q == "BEGIN IMMEDIATE" && (stage == "cancel" || stage == "rollback") && err == nil {
					cancel()
				}
				if q == "COMMIT" && stage == "commit_lost" && err == nil {
					return res, marker
				}
				return res, err
			}
			value, err := db.confirmDraftCleanup(ctx, cleanupSelection(r, r.ID()), disk, time.Now())
			if err == nil || value.Revision.ID() != "" {
				t.Fatal("failure returned unlink authorization", value, err)
			}
			if stage == "commit_before" || stage == "commit_lost" || stage == "restore" || stage == "restore_check" {
				assertCleanupCode(t, err, DraftCleanupCommitUncertain)
			}
			if db.sql.Stats().InUse != 0 {
				t.Fatal("leaked FULL lease")
			}
			entry, err := db.ReadDraft(context.Background(), r.DraftID(), r.ID())
			if err != nil || entry.Record.State != "discarded" || entry.Revision.Payload().Hash() != r.Payload().Hash() {
				t.Fatal("failure lost revision", err)
			}
			if stage == "commit_lost" || stage == "restore" || stage == "restore_check" {
				if !entry.Record.UpdatedAt.After(before.Record.UpdatedAt) {
					t.Fatal("fixture did not retain actual commit")
				}
			} else if !entry.Record.UpdatedAt.Equal(before.Record.UpdatedAt) {
				t.Fatal("failed transaction advanced metadata")
			}
		})
	}
}

func TestDraftCleanupReadonlyLockedWALSnapshotAndCorruption(t *testing.T) {
	db := openTestDB(t)
	r, _ := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
	dir := filepath.Dir(db.path)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	ro, err := OpenReadOnly(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	tx, err := ro.sql.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readDraftCleanupHead(t.Context(), tx, r.DraftID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID()); err != nil {
		t.Fatal(err)
	}
	old, err := previewDraftCleanup(t.Context(), tx, dir, r.DraftID(), 20, 0)
	if err != nil || old.Counts.Active != 1 {
		t.Fatal("preview mixed read snapshots", old.Counts, err)
	}
	tx.Rollback()
	next, err := ro.PreviewDraftCleanup(t.Context(), dir, r.DraftID(), 20, "")
	if err != nil || next.Counts.Eligible != 1 {
		t.Fatal("readonly reader did not see committed discard", next.Counts, err)
	}
	_, err = ro.ConfirmDraftCleanup(t.Context(), cleanupSelection(r, r.ID()))
	if err == nil {
		t.Fatal("readonly DB allowed the write barrier")
	}
	if _, err := db.sql.Exec("DROP TRIGGER draft_revision_no_update; UPDATE draft_revisions SET payload_json='{}'"); err != nil {
		t.Fatal(err)
	}
	_, err = ro.PreviewDraftCleanup(t.Context(), dir, r.DraftID(), 20, "")
	assertCleanupCode(t, err, DraftCleanupStoreError)
}

func TestDraftCleanupClosedReadonlyNoFilesystemChangesOldMissingStore(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprint("old=", old), func(t *testing.T) {
			db := openTestDB(t)
			r, _ := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
			if _, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID()); err != nil {
				t.Fatal(err)
			}
			if old {
				if _, err := db.sql.Exec("DELETE FROM schema_migrations WHERE version=31"); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			before, err := os.ReadFile(db.path)
			if err != nil {
				t.Fatal(err)
			}
			beforeNames, _ := os.ReadDir(filepath.Dir(db.path))
			ro, err := OpenReadOnly(db.path)
			if old {
				if err == nil {
					ro.Close()
					t.Fatal("old store was opened/migrated")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				page, err := ro.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), r.DraftID(), 20, "")
				ro.Close()
				if err != nil || page.Counts.Eligible != 1 {
					t.Fatal(page, err)
				}
			}
			after, _ := os.ReadFile(db.path)
			afterNames, _ := os.ReadDir(filepath.Dir(db.path))
			if !bytes.Equal(before, after) || len(beforeNames) != len(afterNames) {
				t.Fatal("readonly inspection modified closed archive")
			}
		})
	}
	dir := t.TempDir()
	if ro, err := OpenReadOnly(filepath.Join(dir, "wacli.db")); err == nil {
		ro.Close()
		t.Fatal("missing store initialized")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("missing readonly store created files", err)
	}
}

func TestDraftCleanupConcurrentReserveDiscardAndUpdateSelection(t *testing.T) {
	for i := 0; i < 8; i++ {
		db := openTestDB(t)
		r, reservation := outboundFixture(t, db, i+1, DraftDocumentKind, "15550000001@s.whatsapp.net")
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var reserveErr, discardErr error
		go func() { defer wg.Done(); <-start; _, reserveErr = db.Outbound().Reserve(t.Context(), reservation) }()
		go func() { defer wg.Done(); <-start; _, discardErr = db.DiscardDraft(t.Context(), r.DraftID(), r.ID()) }()
		close(start)
		wg.Wait()
		if discardErr != nil {
			t.Fatal(discardErr)
		}
		_, err := db.ConfirmDraftCleanup(t.Context(), cleanupSelection(r, r.ID()))
		if reserveErr == nil {
			assertCleanupCode(t, err, DraftCleanupProtected)
		} else if err != nil {
			t.Fatal("unreferenced discarded head lost eligibility", err)
		}
	}
	db := openTestDB(t)
	r, _ := outboundFixture(t, db, 50, DraftDocumentKind, "15550000001@s.whatsapp.net")
	newRevision := cleanupFixtureRevision(t, r.DraftID(), strings.Repeat("f", 32), DraftDocumentKind, time.Now(), "new head")
	if _, err := db.WriteDraft(t.Context(), newRevision, r.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardDraft(t.Context(), r.DraftID(), newRevision.ID()); err != nil {
		t.Fatal(err)
	}
	_, err := db.ConfirmDraftCleanup(t.Context(), cleanupSelection(r, r.ID()))
	assertCleanupCode(t, err, DraftCleanupConflict)
	if _, err := db.ConfirmDraftCleanup(t.Context(), cleanupSelection(r, newRevision.ID())); err != nil {
		t.Fatal("explicit older discarded revision rejected", err)
	}
}

func TestDraftCleanupCorruptRecordsNeverBecomeEligible(t *testing.T) {
	for _, corruption := range []string{"payload", "hash", "review", "document_digest", "record_time", "missing_head"} {
		t.Run(corruption, func(t *testing.T) {
			db := openTestDB(t)
			db.sql.SetMaxOpenConns(1)
			r, _ := outboundFixture(t, db, 1, DraftDocumentKind, "15550000001@s.whatsapp.net")
			if _, err := db.DiscardDraft(t.Context(), r.DraftID(), r.ID()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.sql.Exec("DROP TRIGGER draft_revision_no_update"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch corruption {
			case "payload":
				_, err = db.sql.Exec("UPDATE draft_revisions SET payload_json='{}'")
			case "hash":
				_, err = db.sql.Exec("UPDATE draft_revisions SET payload_hash=?", strings.Repeat("f", 64))
			case "review":
				_, err = db.sql.Exec("UPDATE draft_revisions SET review_json='{}'")
			case "document_digest":
				raw := bytes.Replace(r.Payload().CanonicalJSON(), []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("X", 64)), 1)
				_, err = db.sql.Exec("UPDATE draft_revisions SET payload_json=?", string(raw))
			case "record_time":
				_, err = db.sql.Exec("UPDATE drafts SET updated_at=created_at-1")
			case "missing_head":
				if _, err := db.sql.Exec("PRAGMA foreign_keys=OFF"); err != nil {
					t.Fatal(err)
				}
				_, err = db.sql.Exec("UPDATE drafts SET head_revision_id=?", strings.Repeat("f", 32))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.PreviewDraftCleanup(t.Context(), filepath.Dir(db.path), r.DraftID(), 20, "")
			assertCleanupCode(t, err, DraftCleanupStoreError)
			entry, err := db.ConfirmDraftCleanup(t.Context(), cleanupSelection(r, r.ID()))
			assertCleanupCode(t, err, DraftCleanupStoreError)
			if entry.Revision.ID() != "" {
				t.Fatal("corrupt archive authorized unlink")
			}
		})
	}
}
