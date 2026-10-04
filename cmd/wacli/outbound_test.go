package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func outboundCLISeed(t *testing.T, dir string, n int) (*store.DB, store.OutboundOperation) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p, err := store.NewDraftPayload(store.DraftPayloadData{Account: store.DraftIdentity{PN: "15550000001@s.whatsapp.net", LID: "90001@lid"}, Recipient: store.DraftRecipient{JID: "15550000002@s.whatsapp.net", PN: "15550000002@s.whatsapp.net", LID: "90002@lid"}, Kind: store.DraftDocumentKind, Document: &store.DraftDocument{Filename: "fixture.txt", MIME: "text/plain", Size: 7, SHA256: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	id, rid := fmt.Sprintf("%032x", n), fmt.Sprintf("%032x", 10000+n)
	relative, _ := store.DraftSnapshotRelativePath(rid)
	at := time.Unix(1700000000, 0)
	revision, err := store.NewDraftRevision(id, rid, at, p, store.DraftReviewSnapshot{RequestedRaw: p.Data().Recipient.JID, SnapshotPath: relative, VerifiedAtCreate: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.WriteDraft(t.Context(), revision, ""); err != nil {
		t.Fatal(err)
	}
	o, err := db.Outbound().Reserve(t.Context(), store.OutboundReservation{Version: 1, ID: fmt.Sprintf("%032x", 20000+n), DraftID: id, RevisionID: rid, Hash: p.Hash(), Key: fmt.Sprintf("KEY-%d", n), MessageID: fmt.Sprintf("3EB0SYNTHETIC%d", n), Account: p.Data().Account, CreatedAt: at.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return db, o
}
func outboundCLI(t *testing.T, dir string, args ...string) (agentTestEnvelope, string, error) {
	t.Helper()
	raw, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir, "outbound"}, args...)...)
	if err != nil {
		return agentTestEnvelope{}, stderr, err
	}
	return decodeAgentTest(t, raw), stderr, nil
}

func TestOutboundCLIReadonlyWithoutSessionMediaLockOrOwner(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := t.TempDir()
	db, o := outboundCLISeed(t, dir, 1)
	// Deliberately malformed session and no managed media: either being opened
	// would fail. Only the existing archive is used by these commands.
	if err := os.WriteFile(filepath.Join(dir, "session.db"), []byte("NEVER_OPEN_SYNTHETIC_SESSION"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	before := snapshotLocalStore(t, dir)
	for _, detail := range []string{"compact", "full"} {
		for _, args := range [][]string{{"show", o.ID}, {"show", "--key", o.Key, "--account-jid", o.Account.PN}, {"list"}, {"list", "--account-jid", o.Account.PN}} {
			e, stderr, err := outboundCLI(t, dir, append(args, "--detail", detail)...)
			if err != nil || stderr != "" || e.Meta.Source != "local" || e.Meta.Freshness != "unknown" || e.Meta.Completeness != "unknown" {
				t.Fatal(e, err, stderr)
			}
			if strings.Contains(string(e.Data), "snapshot_path") || strings.Contains(string(e.Data), "NEVER_OPEN") || strings.Contains(string(e.Data), "running") {
				t.Fatal("private/invented DTO", string(e.Data))
			}
			if args[0] == "show" {
				var data struct {
					Operation outboundDTO `json:"operation"`
				}
				if err = json.Unmarshal(e.Data, &data); err != nil {
					t.Fatal(err)
				}
				if data.Operation.ID != o.ID || data.Operation.Status != "incomplete" || data.Operation.Evidence.Accepted != "unknown" || data.Operation.ErrorCode != nil || data.Operation.ArchiveContinuity != "unknown" || (detail == "full") != (data.Operation.Checkpoints != nil) {
					t.Fatal(data)
				}
				if detail == "full" && data.Operation.Checkpoints.DispatchPossibleAt != nil {
					t.Fatal("unmeasured checkpoint")
				}
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("readonly query changed archive/session/LOCK/WAL/permissions")
	}
	// Active WAL is read by the existing readonly opener, not a writer migration.
	db, err = store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: store.OutboundPreparing, Result: store.OutboundPending, At: o.UpdatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	activeBefore := snapshotLocalStore(t, dir)
	if _, stderr, err := outboundCLI(t, dir, "show", o.ID); err != nil {
		t.Fatal(err, stderr)
	}
	activeAfter := snapshotLocalStore(t, dir)
	delete(activeBefore, filepath.Join(dir, "wacli.db-shm"))
	delete(activeAfter, filepath.Join(dir, "wacli.db-shm"))
	if !reflect.DeepEqual(activeBefore, activeAfter) {
		t.Fatal("active WAL query changed data/files beyond SQLite SHM bookkeeping")
	}
}

func TestOutboundCLISelectionPagesCapsAndNoCreate(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := t.TempDir()
	_, o := outboundCLISeed(t, dir, 1)
	outboundCLISeed(t, dir, 2)
	outboundCLISeed(t, dir, 3)
	e, stderr, err := outboundCLI(t, dir, "list", "--limit", "1")
	if err != nil || !e.Meta.Page.HasMore {
		t.Fatal(err, stderr)
	}
	page, stderr, err := outboundCLI(t, dir, "list", "--limit", "200", "--cursor", *e.Meta.Page.NextCursor, "--detail", "full")
	if err != nil || page.Meta.Page.Returned != 2 || page.Meta.Page.HasMore {
		t.Fatal(page, err, stderr)
	}
	for _, args := range [][]string{{"show", o.ID, "--cursor", *e.Meta.Page.NextCursor}, {"list", "--account-jid", o.Account.PN, "--cursor", *e.Meta.Page.NextCursor}, {"list", "--cursor", "e30"}, {"list", "--cursor", ""}, {"list", "--limit", "201"}, {"show", o.ID, "--key", o.Key}, {"show", "--key", " bad ", "--account-jid", o.Account.PN}, {"show", "--key", o.Key}, {"show", "../SECRET"}, {"list", "--account-jid", ""}, {"send"}, {"recover", o.ID}} {
		_, stderr, err = outboundCLI(t, dir, args...)
		if err == nil || commandExitCode(err) != 2 {
			t.Fatal(args, err, stderr)
		}
	}
	for _, args := range [][]string{{"list"}, {"show", o.ID}} {
		missing := filepath.Join(t.TempDir(), "missing")
		_, stderr, err = outboundCLI(t, missing, args...)
		if err == nil || commandExitCode(err) != 4 {
			t.Fatal(err, stderr)
		}
		if _, err = os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("created missing archive")
		}
	}
	_, stderr, err = outboundCLI(t, dir, "show", strings.Repeat("a", 32))
	if err == nil || commandExitCode(err) != 3 {
		t.Fatal(err, stderr)
	}
	other := t.TempDir()
	db, err := store.Open(filepath.Join(other, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, stderr, err = outboundCLI(t, other, "show", o.ID)
	if err == nil || commandExitCode(err) != 3 {
		t.Fatal("cross-store lookup", err, stderr)
	}
	// The shared agent encoder enforces both limits before writing any bytes.
	for _, detail := range []string{"compact", "full"} {
		var b strings.Builder
		err := out.WriteAgentJSON(&b, out.AgentAccount{}, out.AgentMeta{Detail: detail}, strings.Repeat("x", 9<<20))
		var e *out.AgentError
		if err == nil || !errors.As(err, &e) || e.Code != "payload_too_large" || b.Len() != 0 {
			t.Fatal("envelope cap", detail, err)
		}
	}
}

func TestOutboundCLICorruptRecordsAndOldSchemaUseStoreErrors(t *testing.T) {
	for _, query := range []string{
		"DROP TRIGGER outbound_scope_immutable;PRAGMA ignore_check_constraints=ON;UPDATE outbound_operations SET payload_hash='SECRET_BAD_HASH',generation=generation+1",
		"DROP TRIGGER outbound_transition;PRAGMA ignore_check_constraints=ON;UPDATE outbound_operations SET phase='SECRET_BAD_PHASE',generation=generation+1",
		"DROP TRIGGER draft_revision_no_update;UPDATE draft_revisions SET summary_json='{}'",
	} {
		t.Run(query[:20], func(t *testing.T) {
			dir := t.TempDir()
			db, o := outboundCLISeed(t, dir, 1)
			db.Close()
			raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = raw.Exec(query); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			for _, args := range [][]string{{"show", o.ID}, {"list"}} {
				_, stderr, err := outboundCLI(t, dir, args...)
				if err == nil || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_error" || strings.Contains(stderr, "SECRET") {
					t.Fatal(err, stderr)
				}
			}
		})
	}
	dir := t.TempDir()
	db, o := outboundCLISeed(t, dir, 1)
	db.Close()
	raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec("DELETE FROM schema_migrations WHERE version=31"); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	before := snapshotLocalStore(t, dir)
	_, stderr, err := outboundCLI(t, dir, "show", o.ID)
	if err == nil || commandExitCode(err) != 4 {
		t.Fatal(err, stderr)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("readonly migration")
	}
}

func TestOutboundProductionBinaryOwnerFixtureReadonly(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_OUTBOUND_E2E_BINARY to a freshly built local binary")
	}
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	_, o := outboundCLISeed(t, dir, 1)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		return sendDelegateResponse{}, fmt.Errorf("readonly query reached fake owner")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, readonly := range []bool{false, true} {
		for _, args := range [][]string{{"show", o.ID}, {"list"}, {"show", "--key", o.Key, "--account-jid", o.Account.PN}} {
			stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir, "outbound"}, args...), readonly)
			if err != nil || stderr != "" {
				t.Fatal(err, stderr)
			}
			e := decodeAgentTest(t, stdout)
			if e.Meta.Source != "local" {
				t.Fatal(e)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("delegated readonly query")
	}
	for _, name := range []string{"session.db", "draft-media"} {
		if _, err = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("created session/media", name, err)
		}
	}
}
