package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func draftFixtureRevision(t *testing.T, id, rid, text string, at time.Time) DraftRevision {
	t.Helper()
	payload, err := NewDraftPayload(DraftPayloadData{Account: DraftIdentity{PN: "15550000001@s.whatsapp.net"}, Recipient: DraftRecipient{JID: "15550000002@s.whatsapp.net"}, Kind: DraftTextKind, Text: &DraftText{Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := NewDraftRevision(id, rid, at, payload, DraftReviewSnapshot{RequestedRaw: "+15550000002"})
	if err != nil {
		t.Fatal(err)
	}
	return rev
}
func assertDraftCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *DraftError
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("want %s: %v", code, err)
	}
}
func TestDraftPersistenceCASReopenAndImmutable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, rid, rid2 := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	first := draftFixtureRevision(t, id, rid, "Olá 👋\n\\n", time.Now())
	entry, err := db.WriteDraft(ctx, first, "")
	if err != nil || entry.Number != 1 {
		t.Fatalf("create: %+v %v", entry, err)
	}
	second := draftFixtureRevision(t, id, rid2, "replaced", time.Now().Add(time.Second))
	_, err = db.WriteDraft(ctx, second, strings.Repeat("d", 32))
	assertDraftCode(t, err, "draft_conflict")
	entry, err = db.WriteDraft(ctx, second, rid)
	if err != nil || entry.Number != 2 {
		t.Fatalf("update: %+v %v", entry, err)
	}
	for _, sql := range []string{"UPDATE draft_revisions SET payload_json='{}'", "DELETE FROM draft_revisions", "UPDATE drafts SET account_jid='different'"} {
		if _, err := db.sql.Exec(sql); err == nil {
			t.Fatalf("allowed %s", sql)
		}
	}
	if _, err := db.DiscardDraft(ctx, id, rid); err == nil {
		t.Fatal("stale discard")
	}
	if _, err := db.DiscardDraft(ctx, id, rid2); err != nil {
		t.Fatal(err)
	}
	_, err = db.WriteDraft(ctx, draftFixtureRevision(t, id, strings.Repeat("e", 32), "forbidden", time.Now()), rid2)
	assertDraftCode(t, err, "draft_conflict")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entry, err = db.ReadDraft(ctx, id, rid)
	if err != nil || entry.Revision.Payload().Hash() != first.Payload().Hash() || entry.Record.State != "discarded" {
		t.Fatalf("old reopen: %+v %v", entry, err)
	}
}
func TestDraftPagesLiveScopeAndSummaryOnly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	at := time.Now()
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("%032x", i)
		rid := fmt.Sprintf("%032x", i+100)
		_, err := db.WriteDraft(ctx, draftFixtureRevision(t, id, rid, strings.Repeat("λ", 30000), at), "")
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListDrafts(ctx, "/fixture", false, 2, "")
	if err != nil || !page.HasMore || len(page.Items) != 2 || !page.Items[0].Summary.Truncated {
		t.Fatalf("page: %+v %v", page, err)
	}
	// List only needs persisted summaries, even when a payload is corrupt.
	if _, err := db.sql.Exec("DROP TRIGGER draft_revision_no_update"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("UPDATE draft_revisions SET payload_json='{}' WHERE draft_id=?", fmt.Sprintf("%032x", 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardDraft(ctx, fmt.Sprintf("%032x", 3), fmt.Sprintf("%032x", 103)); err != nil {
		t.Fatal(err)
	}
	next, err := db.ListDrafts(ctx, "/fixture", false, 2, *page.NextCursor)
	if err != nil || len(next.Items) != 1 || next.Items[0].Record.ID != fmt.Sprintf("%032x", 4) {
		t.Fatalf("live status: %+v %v", next, err)
	}
	if _, err := db.ReadDraft(ctx, fmt.Sprintf("%032x", 4), ""); err == nil {
		t.Fatal("corrupt payload allowed")
	}
	for _, scope := range []struct {
		store   string
		discard bool
	}{{"/relocated", false}, {"/fixture", true}} {
		if _, err := db.ListDrafts(ctx, scope.store, scope.discard, 2, *page.NextCursor); err == nil {
			t.Fatal("cross scope cursor")
		}
	}
	for _, token := range []string{"", strings.Repeat("A", 16385), *page.NextCursor + "=", "e30"} {
		if ValidateDraftCursor(token) == nil {
			t.Fatal("invalid token")
		}
	}
}
func TestDraftWriterAccountScope(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, rid := strings.Repeat("a", 32), strings.Repeat("b", 32)
	_, err := db.WriteDraft(ctx, draftFixtureRevision(t, id, rid, "first", time.Now()), "")
	if err != nil {
		t.Fatal(err)
	}
	rev := draftFixtureRevision(t, id, strings.Repeat("c", 32), "changed", time.Now())
	data := rev.Payload().Data()
	data.Account.PN = "15550000003@s.whatsapp.net"
	payload, err := NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	rev, err = NewDraftRevision(id, rev.ID(), rev.CreatedAt(), payload, rev.Review())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.WriteDraft(ctx, rev, rid)
	assertDraftCode(t, err, "identity_unavailable")
}

func TestDraftMigration30FromHistoryArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	attempt := historyTestAttempt()
	if err := db.BeginHistoryAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContact("15550000002@s.whatsapp.net", "15550000002", "fixture", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DROP TABLE draft_revisions;DROP TABLE drafts;DELETE FROM schema_migrations WHERE version=30`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("readonly upgraded 29")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("readonly modified 29", err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	history, err := db.ListHistoryRecoveryEvidence(ctx, []string{attempt.RequestedChatJID})
	if err != nil || history[0].Latest.AttemptID != attempt.AttemptID {
		t.Fatal("lost PR10 evidence", err)
	}
	if _, err := db.GetContact("15550000002@s.whatsapp.net"); err != nil {
		t.Fatal("lost PR9 contact", err)
	}
	if err := migrateDrafts(db); err != nil {
		t.Fatal("non-idempotent migration", err)
	}
	if _, err := db.WriteDraft(ctx, draftFixtureRevision(t, strings.Repeat("a", 32), strings.Repeat("b", 32), "new", time.Now()), ""); err != nil {
		t.Fatal(err)
	}
}

func TestDraftQuoteEmptyAliasCannotSelectAnotherChat(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, chat := range []string{"", "15550000003@s.whatsapp.net"} {
		if err := db.UpsertChat(chat, "dm", "fixture", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "collision", SenderJID: "15550000003@s.whatsapp.net", Text: "outside selected chat", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := db.ReadDraftQuote(ctx, "15550000002@s.whatsapp.net", "", "collision")
	if err != nil || len(records) != 0 {
		t.Fatal("quote escaped chat scope", records, err)
	}
}
