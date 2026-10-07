package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func feedPage(t *testing.T, db *DB, cursor string, limit int) ChangesPage {
	t.Helper()
	p, err := db.ListChanges(t.Context(), "fixture-selection", limit, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func feedMessage(t *testing.T, db *DB, chat, id string) UpsertMessageParams {
	t.Helper()
	if err := db.UpsertChat(chat, "dm", "fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	p := UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: time.Unix(100, 0), SenderJID: chat, Text: "private-body", MediaKey: []byte("private-media-key"), DirectPath: "private-direct-path"}
	if err := db.UpsertMessage(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChangesCommittedStateReplayEditTombstoneEnrichmentAndDelete(t *testing.T) {
	db := openTestDB(t)
	start := feedPage(t, db, "", 20)
	if len(start.Changes) != 0 || start.HasMore || start.NextCursor == "" {
		t.Fatalf("empty: %+v", start)
	}
	p := feedMessage(t, db, "123@s.whatsapp.net", "same-id")
	if err := db.UpsertMessage(p); err != nil {
		t.Fatal(err)
	}
	p.Text = "edit"
	p.Edited = true
	p.Timestamp = time.Unix(110, 0)
	if err := db.UpsertMessage(p); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(p); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageRevoked(p.ChatJID, p.MsgID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageRevoked(p.ChatJID, p.MsgID); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMediaDownloaded(p.ChatJID, p.MsgID, "private-file", time.Unix(150, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM messages WHERE msg_id=?`, p.MsgID); err != nil {
		t.Fatal(err)
	}
	page := feedPage(t, db, start.NextCursor, 20)
	want := []string{"message_insert", "message_update", "message_tombstone", "message_tombstone", "message_delete"}
	if len(page.Changes) != len(want) {
		t.Fatalf("changes: %+v", page)
	}
	for i, c := range page.Changes {
		if c.Kind != want[i] || c.ID != p.MsgID || c.ChatJID != p.ChatJID || c.EventID == "" {
			t.Fatalf("change %d: %+v", i, c)
		}
	}
	b, _ := json.Marshal(page)
	for _, secret := range []string{"private-body", "private-media-key", "private-direct-path", "private-file"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("secret %s leaked", secret)
		}
	}
	end := feedPage(t, db, page.NextCursor, 20)
	if len(end.Changes) != 0 || end.NextCursor != page.NextCursor {
		t.Fatalf("terminal: %+v", end)
	}
}

func TestChangesTransactionRollbackAndFeedFailureAbortMutation(t *testing.T) {
	db := openTestDB(t)
	p := feedMessage(t, db, "123@s.whatsapp.net", "original")
	start := feedPage(t, db, "", 20)
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE messages SET text='rollback' WHERE msg_id=?`, p.MsgID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM archive_changes`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("inside transaction: %d %v", n, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := feedPage(t, db, start.NextCursor, 20); len(got.Changes) != 0 {
		t.Fatalf("phantom %+v", got)
	}
	if _, err = db.sql.Exec(`CREATE TRIGGER fixture_feed_failure BEFORE INSERT ON archive_changes BEGIN SELECT RAISE(ABORT,'private SQL cause'); END`); err != nil {
		t.Fatal(err)
	}
	p.Text = "failed"
	if err = db.UpsertMessage(p); err == nil {
		t.Fatal("feed failure did not abort write")
	}
	msg, err := db.GetMessage(p.ChatJID, p.MsgID)
	if err != nil || msg.Text != "private-body" {
		t.Fatalf("mutated outside feed: %+v %v", msg, err)
	}
}

func TestChangesCursorRestartIsolationAndRestoredBranch(t *testing.T) {
	db := openTestDB(t)
	feedMessage(t, db, "123@s.whatsapp.net", "one")
	feedMessage(t, db, "123@s.whatsapp.net", "two")
	first := feedPage(t, db, "", 1)
	if !first.HasMore {
		t.Fatal("expected next page")
	}
	ro, err := OpenReadOnly(db.path)
	if err != nil {
		t.Fatal(err)
	}
	second := feedPage(t, ro, first.NextCursor, 1)
	if second.HasMore || len(second.Changes) != 1 || second.Changes[0].ID != "two" {
		t.Fatalf("resume %+v", second)
	}
	ro.Close()
	if _, err = db.ListChanges(t.Context(), "other-selection", 20, first.NextCursor); err == nil {
		t.Fatal("cross-selection cursor")
	}
	other := openTestDB(t)
	if _, err = other.ListChanges(t.Context(), "fixture-selection", 20, first.NextCursor); err == nil {
		t.Fatal("cross-file cursor")
	}
	// A restored divergent history can reuse sequence numbers, but not the random anchor.
	if _, err = db.sql.Exec(`UPDATE archive_changes SET event_id=lower(hex(randomblob(16))) WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	_, err = db.ListChanges(t.Context(), "fixture-selection", 20, first.NextCursor)
	var cursorErr *ChangesCursorError
	if !errors.As(err, &cursorErr) || !cursorErr.Expired {
		t.Fatalf("divergence: %v", err)
	}
	if _, err = db.sql.Exec(`DELETE FROM archive_changes WHERE seq=2`); err != nil {
		t.Fatal(err)
	}
	_, err = db.ListChanges(t.Context(), "fixture-selection", 20, second.NextCursor)
	if !errors.As(err, &cursorErr) || !cursorErr.Expired {
		t.Fatalf("truncation: %v", err)
	}
	for _, token := range []string{"", "private-token", first.NextCursor + "=", strings.Repeat("A", 513)} {
		if err := ValidateChangesCursor(token); err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("malformed cursor: %v", err)
		}
	}
}

func TestChangesReadOnlyLiveWALDoesNotSeeUncommittedMutation(t *testing.T) {
	db := openTestDB(t)
	p := feedMessage(t, db, "123@s.whatsapp.net", "one")
	ro, err := OpenReadOnly(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	start := feedPage(t, ro, "", 20)
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE messages SET text='committed-later' WHERE msg_id=?`, p.MsgID); err != nil {
		t.Fatal(err)
	}
	if got := feedPage(t, ro, start.NextCursor, 20); len(got.Changes) != 0 {
		t.Fatal("reader saw uncommitted event")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := feedPage(t, ro, start.NextCursor, 20); len(got.Changes) != 1 {
		t.Fatalf("reader missed WAL commit: %+v", got)
	}
}

func TestChangesReceiptAtomicDedupAndNoMessageRequired(t *testing.T) {
	db := openTestDB(t)
	r := ChangeReceipt{Type: "read", ActorJID: "300@lid", ActorDevice: 7, SenderAlt: "123@s.whatsapp.net", RecipientAlt: "own@lid", EventAt: time.Unix(100, 0).UTC()}
	for range 2 {
		if err := db.RecordChangeReceipts(t.Context(), "400@lid", []string{"unknown", "unknown", "second"}, false, r); err != nil {
			t.Fatal(err)
		}
	}
	p := feedPage(t, db, "", 20)
	if len(p.Changes) != 2 || p.Changes[0].Receipt == nil || *p.Changes[0].Receipt != r {
		t.Fatalf("receipts %+v", p)
	}
	// A failure on the second ID must remove the first ID too.
	if _, err := db.sql.Exec(`CREATE TRIGGER receipt_fixture_failure BEFORE INSERT ON archive_changes WHEN NEW.msg_id='fail' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordChangeReceipts(t.Context(), "400@lid", []string{"rollback", "fail"}, false, r); err == nil {
		t.Fatal("expected batch failure")
	}
	if got := feedPage(t, db, p.NextCursor, 20); len(got.Changes) != 0 {
		t.Fatalf("partial receipt batch %+v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.RecordChangeReceipts(ctx, "400@lid", []string{"cancelled"}, false, r); err == nil {
		t.Fatal("cancelled write")
	}
}

func TestChangesPNLIDRewriteRetainsMappingAndTombstone(t *testing.T) {
	db := openTestDB(t)
	lid, pn := "400@lid", "123@s.whatsapp.net"
	p := feedMessage(t, db, lid, "one")
	if err := db.MarkMessageDeletedForMe(lid, p.MsgID, lid, false, time.Unix(120, 0)); err != nil {
		t.Fatal(err)
	}
	start := feedPage(t, db, "", 20)
	if err := db.MigrateLIDToPN(lid, pn); err != nil {
		t.Fatal(err)
	}
	page := feedPage(t, db, start.NextCursor, 20)
	if len(page.Changes) != 3 || page.Changes[0].Kind != "identity_mapping" || page.Changes[0].PreviousChatJID != lid || page.Changes[0].ChatJID != pn || (page.Changes[1].Tombstone == nil || !*page.Changes[1].Tombstone) || page.Changes[2].Kind != "message_delete" {
		t.Fatalf("rewrite: %+v", page)
	}
	if err := db.MigrateLIDToPN(lid, pn); err != nil {
		t.Fatal(err)
	}
	if got := feedPage(t, db, page.NextCursor, 20); len(got.Changes) != 0 {
		t.Fatalf("rewrite replay %+v", got)
	}
}

func TestChangesUpgradeHasNoRetroactiveRowsAndReadOnlyRejectsOldOrBrokenSchema(t *testing.T) {
	db := openTestDB(t)
	feedMessage(t, db, "123@s.whatsapp.net", "predates-feed")
	// Reproduce a pre-feed archive using real prior schema/production row writes.
	_, err := db.sql.Exec(`DROP TRIGGER changes_message_insert; DROP TRIGGER changes_message_update; DROP TRIGGER changes_message_delete; DROP TABLE archive_changes; DROP TABLE change_feed_identity; DELETE FROM schema_migrations WHERE version=33`)
	if err != nil {
		t.Fatal(err)
	}
	path := db.path
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ro, err := OpenReadOnly(path); err == nil {
		ro.Close()
		t.Fatal("old readonly migrated")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("readonly changed old database")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	page := feedPage(t, db, "", 20)
	if len(page.Changes) != 0 {
		t.Fatalf("retroactive feed %+v", page)
	}
	if _, err = db.sql.Exec(`DROP TRIGGER changes_message_update`); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.ListChanges(t.Context(), "fixture-selection", 20, ""); err == nil {
		t.Fatal("broken feed reported success")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if writer, err := Open(path); err == nil {
		writer.Close()
		t.Fatal("writer reopened without feed coverage")
	}
}

func TestChangesActualRestartAndCloneSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	feedMessage(t, db, "123@s.whatsapp.net", "one")
	page := feedPage(t, db, "", 20)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	// Bytes after a clean close are a coherent clone; no real archive is accessed.
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	clonePath := filepath.Join(t.TempDir(), "wacli.db")
	if err = os.WriteFile(clonePath, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	clone, err := OpenReadOnly(clonePath)
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	if _, err = clone.ListChanges(t.Context(), "clone-selection", 20, page.NextCursor); err == nil {
		t.Fatal("clone selection accepted cursor")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	feedMessage(t, db, "123@s.whatsapp.net", "after-restart")
	resumed := feedPage(t, db, page.NextCursor, 20)
	if len(resumed.Changes) != 1 || resumed.Changes[0].ID != "after-restart" {
		t.Fatalf("restart %+v", resumed)
	}
}

func TestChangesReceiptPublicScopeEnrichmentAndDistinctMessageKeys(t *testing.T) {
	db := openTestDB(t)
	feedMessage(t, db, "123@s.whatsapp.net", "same-id")
	feedMessage(t, db, "456@s.whatsapp.net", "same-id")
	start := feedPage(t, db, "", 20)
	if len(start.Changes) != 2 || start.Changes[0].ChatJID == start.Changes[1].ChatJID {
		t.Fatalf("message identity %+v", start)
	}
	r := ChangeReceipt{Type: "delivered", ActorJID: "300@lid", EventAt: time.Unix(100, 0).UTC()}
	for i := range 3 {
		if i == 1 {
			r.SenderAlt = "123@s.whatsapp.net"
		}
		if i == 2 {
			r.ActorDevice = 7
		}
		if err := db.RecordChangeReceipts(t.Context(), "400@lid", []string{"same-id"}, false, r); err != nil {
			t.Fatal(err)
		}
	}
	p := feedPage(t, db, start.NextCursor, 20)
	if len(p.Changes) != 3 || p.Changes[0].Receipt.SenderAlt != "" || p.Changes[1].Receipt.SenderAlt != r.SenderAlt || p.Changes[2].Receipt.ActorDevice != 7 {
		t.Fatalf("receipt scope %+v", p)
	}
	if p.Changes[0].Tombstone != nil || p.Changes[0].FromMe == nil {
		t.Fatal("receipt implied a message tombstone")
	}
}

func TestChangesMigrationFailureRollsBackNewFeedObjects(t *testing.T) {
	db := openTestDB(t)
	_, err := db.sql.Exec(`DROP TRIGGER changes_message_insert; DROP TRIGGER changes_message_update; DROP TRIGGER changes_message_delete; DROP TABLE archive_changes; DROP TABLE change_feed_identity; CREATE TABLE archive_changes(wrong_column TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateChanges(db); err == nil {
		t.Fatal("incompatible migration succeeded")
	}
	exists, err := db.tableExists("change_feed_identity")
	if err != nil || exists {
		t.Fatalf("partial migration retained identity: %t %v", exists, err)
	}
}
