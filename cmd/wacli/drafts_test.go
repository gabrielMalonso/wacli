package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func draftCLI(t *testing.T, dir string, args ...string) (draftDTO, string, error) {
	t.Helper()
	raw, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir, "draft"}, args...)...)
	var dto draftDTO
	if err == nil {
		e := decodeAgentTest(t, raw)
		if e.Meta.Source != "local" || e.Account.StoreRef == nil || *e.Account.StoreRef != dir {
			t.Fatalf("scope/source: %s", raw)
		}
		if err := json.Unmarshal(e.Data, &dto); err != nil {
			t.Fatal(err)
		}
	}
	return dto, stderr, err
}
func TestDraftCLICreateUpdateDiscardReadArchive(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	first, stderr, err := draftCLI(t, dir, "create", "--to", localReadLID, "--message", "Olá 👋\n\\n", "--reply-to", "m1", "--mention", localReadPN)
	if err != nil {
		t.Fatal(err, stderr)
	}
	if first.Text.Text != "Olá 👋\n\\n" || first.AccountIdentity.PN != "15550000009@s.whatsapp.net" || first.Recipient.PN != localReadPN || first.RequestedRaw != localReadLID || first.Reply.ID != "m1" {
		t.Fatalf("identity/content: %+v", first)
	}
	_, stderr, err = draftCLI(t, dir, "update", first.ID, "--if-revision", strings.Repeat("f", 32), "--to", localReadPN, "--message", "complete")
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "draft_conflict" {
		t.Fatal("CAS", err, stderr)
	}
	second, stderr, err := draftCLI(t, dir, "update", first.ID, "--if-revision", first.RevisionID, "--to", localReadPN, "--contact-name", "Name;\\\nFN:evil", "--contact-phone", "+15550000003")
	if err != nil || second.Revision != 2 || second.Hash == first.Hash || second.Contact.DisplayName != "Name;\\\nFN:evil" {
		t.Fatal("update", err, stderr)
	}
	// Reading and abandoning an archive must not need the current session.
	if err := os.Rename(filepath.Join(dir, "session.db"), filepath.Join(dir, "session.fixture-hidden")); err != nil {
		t.Fatal(err)
	}
	discarded, stderr, err := draftCLI(t, dir, "discard", first.ID, "--if-revision", second.RevisionID)
	if err != nil || discarded.State != "discarded" {
		t.Fatal("discard", err, stderr)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	before := snapshotLocalStore(t, dir)
	old, stderr, err := draftCLI(t, dir, "show", first.ID, "--revision", first.RevisionID)
	if err != nil || old.Hash != first.Hash || old.Text.Text != first.Text.Text {
		t.Fatal("old revision", err, stderr)
	}
	raw, stderr, err := runAgentTest(t, "--agent", "--store", dir, "draft", "list", "--include-discarded")
	if err != nil {
		t.Fatal(err, stderr)
	}
	e := decodeAgentTest(t, raw)
	var list []draftListDTO
	if err := json.Unmarshal(e.Data, &list); err != nil || len(list) != 1 || list[0].State != "discarded" {
		t.Fatal(raw, err)
	}
	if after := snapshotLocalStore(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatal("read changed bytes/perms/LOCK/WAL")
	}
}
func TestDraftDocumentFullExpectedPathAndRelocation(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	source := filepath.Join(t.TempDir(), "original.txt")
	if err := os.WriteFile(source, []byte("fixture bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	doc, stderr, err := draftCLI(t, dir, "create", "--to", localReadPN, "--file", source)
	if err != nil {
		t.Fatal(err, stderr)
	}
	if doc.Document.SnapshotPath != "" || doc.Document.VerifiedAtCreate.IsZero() || !strings.Contains(doc.Recovery, "--detail full") {
		t.Fatalf("compact: %+v", doc)
	}
	full, stderr, err := draftCLI(t, dir, "show", doc.ID, "--revision", doc.RevisionID, "--detail", "full")
	expected := filepath.Join(dir, store.DraftMediaDirectory, doc.RevisionID+".blob")
	if err != nil || full.Document.SnapshotPath != expected || strings.Contains(full.Document.SnapshotPath, source) {
		t.Fatal("full path", err, stderr)
	}
	if err := os.Rename(expected, expected+".fixture-hidden"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, source+".fixture-hidden"); err != nil {
		t.Fatal(err)
	}
	updated, stderr, err := draftCLI(t, dir, "update", doc.ID, "--if-revision", doc.RevisionID, "--to", localReadPN, "--message", "new text")
	if err != nil {
		t.Fatal(err, stderr)
	}
	if _, stderr, err := draftCLI(t, dir, "discard", doc.ID, "--if-revision", updated.RevisionID); err != nil {
		t.Fatal(err, stderr)
	}
	full, stderr, err = draftCLI(t, dir, "show", doc.ID, "--revision", doc.RevisionID, "--detail", "full")
	if err != nil || full.Document.SnapshotPath != expected {
		t.Fatal("show opened missing bytes", err, stderr)
	}
	// Move a closed fixture DB alone. No session or snapshot exists in this store.
	relocated := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relocated, "wacli.db"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, relocated)
	full, stderr, err = draftCLI(t, relocated, "show", doc.ID, "--revision", doc.RevisionID, "--detail", "full")
	if err != nil || full.Document.SnapshotPath != filepath.Join(relocated, store.DraftMediaDirectory, doc.RevisionID+".blob") {
		t.Fatal("relocation", err, stderr)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, relocated)) {
		t.Fatal("read created artifact")
	}
}
func TestDraftCLIInputPolicyAndCompact(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, args := range [][]string{
		{"create", "--read-only"}, {"create", "--to", "a name", "--message", "secret"}, {"create", "--to", "status@broadcast", "--message", "secret"}, {"create", "--to", localReadPN, "--message", "x", "--file", "unknown"}, {"list", "--limit", "201"}, {"show", "../invalid"}, {"discard", strings.Repeat("a", 32)}, {"send"},
	} {
		dir := t.TempDir()
		_, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir, "draft"}, args...)...)
		if err == nil || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Meta.Source != "local" {
			t.Fatal(args, err, stderr)
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Fatal("invalid input touched archive")
		}
	}
	dir := seedLocalReadStore(t)
	text := strings.Repeat("👋", 16000)
	dto, stderr, err := draftCLI(t, dir, "create", "--to", localReadPN, "--message", text)
	if err != nil || len(dto.TruncatedFields) != 1 || dto.Text.Text == text {
		t.Fatal("compact", err, stderr)
	}
	full, stderr, err := draftCLI(t, dir, "show", dto.ID, "--detail", "full")
	if err != nil || full.Text.Text != text || len(full.TruncatedFields) != 0 {
		t.Fatal("full", err, stderr)
	}
	other := seedLocalReadStore(t)
	_, stderr, err = draftCLI(t, other, "show", dto.ID)
	if err == nil || commandExitCode(err) != 3 {
		t.Fatal("cross store", err, stderr)
	}
}

type draftBrokenWriter struct{ err error }

func (w draftBrokenWriter) Write([]byte) (int, error) { return 0, w.err }
func TestDraftOutputFailureAfterWriteIsUncertain(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := store.NewDraftPayload(store.DraftPayloadData{Account: store.DraftIdentity{PN: "15550000009@s.whatsapp.net"}, Recipient: store.DraftRecipient{JID: localReadPN}, Kind: store.DraftTextKind, Text: &store.DraftText{Text: "saved"}})
	if err != nil {
		t.Fatal(err)
	}
	id, rid := strings.Repeat("a", 32), strings.Repeat("b", 32)
	revision, err := store.NewDraftRevision(id, rid, time.Now(), p, store.DraftReviewSnapshot{RequestedRaw: localReadPN})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := db.WriteDraft(context.Background(), revision, "")
	if err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{agent: true, detail: "compact", agentAccount: out.AgentAccount{StoreRef: &dir}}
	if err := writeDraftEntryTo(draftBrokenWriter{syscall.EPIPE}, flags, entry, false); err != nil {
		t.Fatal("read pipe semantics changed", err)
	}
	for _, failure := range []error{syscall.EPIPE, io.ErrShortWrite} {
		err := writeDraftEntryTo(draftBrokenWriter{failure}, flags, entry, true)
		var typed *out.AgentError
		if !errors.As(err, &typed) || typed.Code != "local_write_uncertain" || typed.Draft.Hash != p.Hash() || typed.ExitCode != 1 {
			t.Fatal("output uncertainty", err)
		}
	}
}

func TestDraftEnvironmentStoreSelectionAndLiveWALReads(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	t.Setenv("WACLI_STORE_DIR", dir)
	raw, stderr, err := runAgentTest(t, "--agent", "draft", "create", "--to", localReadPN, "--message", "env selected")
	if err != nil {
		t.Fatal(err, stderr)
	}
	e := decodeAgentTest(t, raw)
	var dto draftDTO
	if err := json.Unmarshal(e.Data, &dto); err != nil {
		t.Fatal(err)
	}
	if e.Account.StoreRef == nil || *e.Account.StoreRef != dir {
		t.Fatal("required explicit store", raw)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	writer, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.UpsertChat("100@g.us", "group", "fixture WAL", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, args := range [][]string{{"show", dto.ID}, {"list"}} {
		_, stderr, err := runAgentTest(t, append([]string{"--agent", "--read-only", "draft"}, args...)...)
		if err != nil {
			t.Fatal("read under writer LOCK/WAL", err, stderr)
		}
	}
	after := snapshotLocalStore(t, dir)
	// Preserve the existing PR10 boundary: SQLite can update active SHM marks.
	for _, files := range []map[string]localFileSnapshot{before, after} {
		for path := range files {
			if strings.HasSuffix(path, "-shm") {
				delete(files, path)
			}
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reads changed archive/session/LOCK/WAL/perms")
	}
}

func draftCorruptionCases() []struct{ name, statement string } {
	return []struct{ name, statement string }{
		{"malformed payload", `UPDATE draft_revisions SET payload_json='{' WHERE id=?`},
		{"payload version", `UPDATE draft_revisions SET payload_json=replace(payload_json,'"version":1','"version":2') WHERE id=?`},
		{"row version", `UPDATE draft_revisions SET payload_version=2 WHERE id=?`},
		{"noncanonical payload", `UPDATE draft_revisions SET payload_json=' '||payload_json WHERE id=?`},
		{"hash mismatch", `UPDATE draft_revisions SET payload_hash='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE id=?`},
		{"malformed review", `UPDATE draft_revisions SET review_json='{"SECRET_INTERNAL_PATH":' WHERE id=?`},
		{"incompatible review", `UPDATE draft_revisions SET review_json=replace(review_json,'"requested_jid":','"requested_jid":"SECRET_INTERNAL_PATH","ignored":') WHERE id=?`},
		{"noncanonical review", `UPDATE draft_revisions SET review_json=' '||review_json WHERE id=?`},
		{"unknown review field", `UPDATE draft_revisions SET review_json=replace(review_json,'{','{"SECRET_INTERNAL_PATH":0,') WHERE id=?`},
	}
}
func corruptDraftFixture(t *testing.T, dir, rid, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("DROP TRIGGER draft_revision_no_update;PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statement, rid); err != nil {
		t.Fatal(err)
	}
}
func TestDraftPersistedCorruptionUsesStoreBoundaryForShowAndDiscard(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, tc := range draftCorruptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedLocalReadStore(t)
			dto, stderr, err := draftCLI(t, dir, "create", "--to", localReadPN, "--message", "saved")
			if err != nil {
				t.Fatal(err, stderr)
			}
			corruptDraftFixture(t, dir, dto.RevisionID, tc.statement)
			for _, args := range [][]string{{"show", dto.ID}, {"discard", dto.ID, "--if-revision", dto.RevisionID}} {
				_, stderr, err := draftCLI(t, dir, args...)
				if err == nil || commandExitCode(err) != 4 {
					t.Fatal("persisted corruption classified as caller input", err, stderr)
				}
				e := decodeAgentTest(t, stderr)
				if e.Error.Code != "store_unavailable" || strings.Contains(stderr, "SECRET") || strings.Contains(e.Error.Message+e.Error.Recovery, dir) || strings.Contains(e.Error.Recovery, "complete bounded input") {
					t.Fatal("unsafe store boundary", stderr)
				}
			}
			db, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var state string
			if err := db.QueryRow("SELECT state FROM drafts WHERE id=?", dto.ID).Scan(&state); err != nil || state != "active" {
				t.Fatal("corrupt discard mutated record", state, err)
			}
		})
	}
	// A typed store boundary takes priority while retaining the validator cause.
	cause := &store.DraftValidationError{Field: "payload", Reason: "SECRET"}
	err := store.DraftFailure("store_unavailable", strings.Repeat("a", 32), strings.Repeat("b", 32), "", cause)
	var preserved *store.DraftValidationError
	if !errors.As(err, &preserved) || classifyDraftError(err).ExitCode != 4 {
		t.Fatal("lost store provenance", err)
	}
}
