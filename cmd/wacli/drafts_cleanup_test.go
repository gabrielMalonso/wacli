package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func seedCleanupDocument(t *testing.T, a *app.App, id, rid, expected string, discard bool) (app.DraftCleanupRequest, string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "document.fixture")
	if err := os.WriteFile(source, []byte("cleanup fixture bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := app.CreateDraftSnapshot(t.Context(), app.DraftSnapshotOptions{StoreDir: a.StoreDir(), RevisionID: rid, SourcePath: source, OpenSource: os.Open})
	if err != nil {
		t.Fatal(err)
	}
	doc := snapshot.Document()
	payload, err := store.NewDraftPayload(store.DraftPayloadData{Account: store.DraftIdentity{PN: localReadPN}, Recipient: store.DraftRecipient{JID: "15550000002@s.whatsapp.net"}, Kind: store.DraftDocumentKind, Document: &doc})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.NewDraftRevision(id, rid, time.Now(), payload, store.DraftReviewSnapshot{RequestedRaw: "15550000002@s.whatsapp.net", SnapshotPath: snapshot.RelativePath(), VerifiedAtCreate: snapshot.VerifiedAtCreate()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), revision, expected); err != nil {
		t.Fatal(err)
	}
	if discard {
		if _, err := a.DB().DiscardDraft(t.Context(), id, rid); err != nil {
			t.Fatal(err)
		}
	}
	return app.DraftCleanupRequest{Version: 1, StoreRef: a.StoreDir(), Selection: store.DraftCleanupSelection{DraftID: id, RevisionID: rid, ExpectedHeadID: rid, Hash: payload.Hash()}}, filepath.Join(a.StoreDir(), filepath.FromSlash(snapshot.RelativePath()))
}
func cleanupArgs(r app.DraftCleanupRequest) []string {
	s := r.Selection
	return []string{"--agent", "--store", r.StoreRef, "--timeout", "2s", "draft", "cleanup", "apply", s.DraftID, "--revision", s.RevisionID, "--if-revision", s.ExpectedHeadID, "--expect-hash", s.Hash}
}
func exerciseCleanupOwner(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
	before, err := a.DB().ReadDraft(t.Context(), r.Selection.DraftID, "")
	if err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{min: 10 * time.Second, max: 10 * time.Second}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		if req.Kind != draftCleanupKind {
			t.Error("cleanup selected a WA operation")
			return sendDelegateResponse{}, errors.New("fixture rejects WA")
		}
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, readonly := range []string{"flag", "env"} {
		args := cleanupArgs(r)
		if readonly == "flag" {
			args = append(args, "--read-only")
		}
		if readonly == "env" {
			t.Setenv("WACLI_READONLY", "1")
		}
		raw, stderr, err := runDraftBinary(t, binary, args, readonly == "env")
		t.Setenv("WACLI_READONLY", "0")
		if err == nil || raw != "" || decodeAgentTest(t, stderr).Error.Code != "read_only" || calls.Load() != 0 {
			t.Fatal("readonly reached owner", raw, stderr, err, calls.Load())
		}
	}
	previewArgs := []string{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID, "--limit", "1", "--read-only"}
	raw, stderr, err := runDraftBinary(t, binary, previewArgs, true)
	if err != nil || calls.Load() != 0 {
		t.Fatal("preview submitted owner request", raw, stderr, err)
	}
	env := decodeAgentTest(t, raw)
	var page draftCleanupPreviewDTO
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatal(err)
	}
	if env.Meta.Source != "local" || page.Counts.Examined != 1 || page.Counts.Eligible != 1 || page.Revisions[0].Hash != r.Selection.Hash || page.Revisions[0].SnapshotPath != "" {
		t.Fatal(raw)
	}
	raw, stderr, err = runDraftBinary(t, binary, cleanupArgs(r), false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	var result app.DraftCleanupResult
	if err := json.Unmarshal(decodeAgentTest(t, raw).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Effect != app.DraftCleanupRemoved || result.Outcome != app.DraftCleanupCompleted || calls.Load() != 1 {
		t.Fatal(raw, calls.Load())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("retained bytes", err)
	}
	after, err := a.DB().ReadDraft(t.Context(), r.Selection.DraftID, "")
	if err != nil || !reflect.DeepEqual(before.Revision, after.Revision) || before.Record.HeadRevisionID != after.Record.HeadRevisionID {
		t.Fatal("lost catalogue", err)
	}
	raw, stderr, err = runDraftBinary(t, binary, cleanupArgs(r), false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	if err := json.Unmarshal(decodeAgentTest(t, raw).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != app.DraftCleanupAlreadyAbsent || result.RemovedBytes == nil || *result.RemovedBytes != 0 || calls.Load() != 2 {
		t.Fatal(raw)
	}
	// A deliberate new call after a confirmed result is distinct from replay.
}
func TestDraftCleanupOwnerOfflineNoPacing(t *testing.T) { exerciseCleanupOwner(t, "") }
func TestDraftCleanupProductionBinaryOwnerOffline(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to a freshly built local binary")
	}
	exerciseCleanupOwner(t, binary)
}
func TestDraftCleanupIPCLostReplyAndCorrelationWithoutReplay(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, mode := range []string{"lost", "old", "hash", "head", "effect", "duplicate", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			dir, a := draftOwnerFixture(t, false)
			r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			ln, err := net.Listen("unix", sendDelegateSocketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			var calls atomic.Int64
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var req sendDelegateRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				calls.Add(1)
				if mode == "old" {
					_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "unknown old owner"})
					return
				}
				resp, err := executeDelegatedSend(t.Context(), a, req)
				if err != nil {
					t.Error(err)
					return
				}
				switch mode {
				case "lost":
					return
				case "hash":
					resp.DraftCleanup.RequestHash = strings.Repeat("f", 64)
				case "head":
					resp.DraftCleanup.RequestHash = draftCleanupRequestHash(app.DraftCleanupRequest{Version: 1, StoreRef: dir, Selection: store.DraftCleanupSelection{DraftID: r.Selection.DraftID, RevisionID: r.Selection.RevisionID, ExpectedHeadID: strings.Repeat("c", 32), Hash: r.Selection.Hash}})
				case "effect":
					resp.DraftCleanup.Result.RemovedBytes = nil
				case "oversized":
					resp.Error = strings.Repeat("x", draftCleanupMaxFrame)
				}
				raw, _ := json.Marshal(resp)
				if mode == "duplicate" {
					raw = append([]byte(`{"ok":false,`), raw[1:]...)
				}
				_, _ = conn.Write(append(raw, '\n'))
			}()
			stdout, stderr, err := runDraftBinary(t, "", cleanupArgs(r), false)
			if err == nil || stdout != "" {
				t.Fatal("uncertain cleanup returned success", stdout, stderr, err)
			}
			failure := decodeAgentTest(t, stderr).Error
			if failure.Code != string(store.DraftCleanupOutcomeUncertain) || failure.Cleanup == nil || failure.Cleanup.Effect != "unknown" || failure.Cleanup.RemovedBytes != nil || failure.Cleanup.DraftID != r.Selection.DraftID {
				t.Fatal(stderr)
			}
			<-done
			if calls.Load() != 1 {
				t.Fatal("repeated action", calls.Load())
			}
			if _, err := a.DB().ReadDraft(t.Context(), r.Selection.DraftID, r.Selection.RevisionID); err != nil {
				t.Fatal("lost metadata", err)
			}
			_, err = os.Lstat(path)
			if mode == "old" && err != nil || mode != "old" && !os.IsNotExist(err) {
				t.Fatal("unexpected bytes", mode, err)
			}
		})
	}
}
func TestDraftCleanupIPCQueueTimeoutDoesNotExecute(t *testing.T) {
	_, a := draftOwnerFixture(t, false)
	r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
	server, client := net.Pipe()
	defer client.Close()
	slot := make(chan struct{}, 1) // occupied by another cooperating operation
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSendDelegateConn(t.Context(), server, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			calls.Add(1)
			return sendDelegateResponse{}, nil
		}, slot, newSendPacer(sendSpacing{}))
	}()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	req := sendDelegateRequest{Version: 1, Kind: draftCleanupKind, DraftCleanup: &r, TimeoutMS: 80}
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp sendDelegateResponse
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	result, err := validateDraftCleanupDelegate(r, resp)
	if err == nil || result.Effect != app.DraftCleanupNotRemoved || calls.Load() != 0 {
		t.Fatal(result, err, calls.Load())
	}
	slot <- struct{}{}
	<-done
	if _, err := os.Lstat(path); err != nil {
		t.Fatal(err)
	}
}
func TestDraftCleanupIPCReadonlyOwnerAndInvalidEnvelope(t *testing.T) {
	dir, a := draftOwnerFixture(t, false)
	r, _ := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
	ro, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	stop, err := startSendDelegateServer(t.Context(), ro, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: draftCleanupKind, DraftCleanup: &r})
	failure := classifyDraftCleanupError(err, nil)
	if failure.Code != "read_only" || failure.Cleanup.Effect != "not_removed" {
		t.Fatal(failure)
	}
	req := sendDelegateRequest{Version: 1, Kind: draftCleanupKind, DraftCleanup: &r, Message: "unexpected legacy field"}
	if validateDraftCleanupEnvelope(req) {
		t.Fatal("accepted legacy adjunct")
	}
	req.Message = ""
	req.Kind = "text"
	if validateDraftCleanupEnvelope(req) {
		t.Fatal("cross executor")
	}
}
func TestDraftCleanupOutputFailureKeepsKnownEffect(t *testing.T) {
	r := app.DraftCleanupResult{DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), Hash: strings.Repeat("c", 64), Effect: app.DraftCleanupRemoved, Outcome: app.DraftCleanupCompleted, DirectorySync: app.DraftCleanupSyncCompleted, RemovedBytes: new(int64)}
	dir := t.TempDir()
	flags := &rootFlags{agent: true, agentAccount: out.AgentAccount{StoreRef: &dir}}
	for _, writeErr := range []error{syscall.EPIPE, io.ErrShortWrite} {
		err := writeDraftCleanupResult(draftBrokenWriter{writeErr}, flags, r)
		failure := classifyDraftCleanupError(err, nil)
		if failure.Code != string(store.DraftCleanupOutcomeUncertain) || failure.Cleanup == nil || failure.Cleanup.Effect != "removed" || failure.Cleanup.DirectorySync != "completed" {
			t.Fatal(failure)
		}
	}
}

func TestDraftCleanupPreviewReadonlyNoFilesystemInspection(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
	// Replace no file: an unreadable mode is enough to show catalogue preview
	// never needs snapshot opening or permission normalization.
	before := snapshotLocalStore(t, dir)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	raw, stderr, err := runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID, "--limit", "1", "--detail", "full"}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	var dto draftCleanupPreviewDTO
	if err := json.Unmarshal(decodeAgentTest(t, raw).Data, &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Revisions[0].SnapshotPath != path || dto.Counts.Eligible != 1 {
		t.Fatal(raw)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatal("preview normalized snapshot", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	after := snapshotLocalStore(t, dir)
	// Ignore SQLite WAL/SHM and the separately acquired LOCK, as existing helper does.
	for name, old := range before {
		if filepath.Base(name) == "LOCK" || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		if !reflect.DeepEqual(old, after[name]) {
			t.Fatal("readonly changed", name)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--limit", "201"}, {"--cursor", "bad"}, {"--cursor", "bad", "--read-only"},
	} {
		raw, stderr, err = runDraftBinary(t, "", append([]string{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID}, args...), false)
		if err == nil || raw != "" {
			t.Fatal("accepted invalid preview", raw, stderr, err)
		}
	}
}
func TestDraftCleanupRequestCanonicalDecoding(t *testing.T) {
	r := app.DraftCleanupRequest{Version: 1, StoreRef: t.TempDir(), Selection: store.DraftCleanupSelection{DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), ExpectedHeadID: strings.Repeat("b", 32), Hash: strings.Repeat("c", 64)}}
	raw, _ := json.Marshal(r)
	var next app.DraftCleanupRequest
	if err := json.Unmarshal(raw, &next); err != nil || next != r {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append([]byte(`{"version":1,`), raw[1:]...), bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1), bytes.Replace(raw, []byte(`"selection":`), []byte(`"extra":0,"selection":`), 1)} {
		if err := json.Unmarshal(bad, &next); err == nil {
			t.Fatal("accepted bad request")
		}
	}
}

func exerciseCleanupStandalone(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	_, a := draftOwnerFixture(t, false)
	r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", false)
	entry, err := a.DB().ReadDraft(t.Context(), r.Selection.DraftID, "")
	if err != nil {
		t.Fatal(err)
	}
	op, err := a.DB().Outbound().Reserve(t.Context(), store.OutboundReservation{Version: 1, ID: strings.Repeat("e", 32), DraftID: r.Selection.DraftID, RevisionID: r.Selection.RevisionID, Hash: r.Selection.Hash, Key: "protected-fixture-key", MessageID: "fixture-message", Account: entry.Revision.Payload().Data().Account, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	next, nextPath := seedCleanupDocument(t, a, r.Selection.DraftID, strings.Repeat("c", 32), r.Selection.RevisionID, true)
	before, err := a.DB().Outbound().Read(t.Context(), op.ID, "", "", 200, "")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	r.Selection.ExpectedHeadID = next.Selection.RevisionID
	raw, stderr, err := runDraftBinary(t, binary, cleanupArgs(r), false)
	if err == nil || raw != "" || decodeAgentTest(t, stderr).Error.Code != string(store.DraftCleanupProtected) {
		t.Fatal("removed protected revision", raw, stderr, err)
	}
	// Stale CAS refuses without deleting the sole unreferenced latest bytes.
	stale := next
	stale.Selection.ExpectedHeadID = r.Selection.RevisionID
	raw, stderr, err = runDraftBinary(t, binary, cleanupArgs(stale), false)
	if err == nil || raw != "" || decodeAgentTest(t, stderr).Error.Code != string(store.DraftCleanupConflict) {
		t.Fatal(raw, stderr, err)
	}
	raw, stderr, err = runDraftBinary(t, binary, cleanupArgs(next), false)
	if err != nil || decodeAgentTest(t, raw).Meta.Source != "local" {
		t.Fatal(raw, stderr, err)
	}
	if _, err := os.Lstat(nextPath); !os.IsNotExist(err) {
		t.Fatal("unreferenced bytes retained", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("bound bytes lost", err)
	}
	db, err := store.OpenReadOnly(filepath.Join(next.StoreRef, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after, err := db.Outbound().Read(t.Context(), op.ID, "", "", 200, "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("outbound evidence changed", err)
	}
	for _, revision := range []string{r.Selection.RevisionID, next.Selection.RevisionID} {
		if _, err := db.ReadDraft(t.Context(), r.Selection.DraftID, revision); err != nil {
			t.Fatal("catalogue lost", err)
		}
	}
}
func TestDraftCleanupStandaloneProtectsOutboundCatalogue(t *testing.T) {
	exerciseCleanupStandalone(t, "")
}
func TestDraftCleanupProductionBinaryStandalone(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to a freshly built local binary")
	}
	exerciseCleanupStandalone(t, binary)
}
func TestDraftCleanupMissingArchiveAndPreflightHaveNoEffects(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	missing := filepath.Join(t.TempDir(), "missing")
	r := app.DraftCleanupRequest{Version: 1, StoreRef: missing, Selection: store.DraftCleanupSelection{DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), ExpectedHeadID: strings.Repeat("b", 32), Hash: strings.Repeat("c", 64)}}
	for _, args := range [][]string{
		{"--agent", "--store", missing, "draft", "cleanup", "preview", r.Selection.DraftID},
		cleanupArgs(r), append(cleanupArgs(r), "--read-only"),
	} {
		raw, stderr, err := runDraftBinary(t, "", args, false)
		if err == nil || raw != "" {
			t.Fatal(raw, stderr, err)
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Fatal("initialized missing archive", err)
		}
	}
}

func TestDraftCleanupLegacyJSONErrorKeepsEffect(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	_, a := draftOwnerFixture(t, false)
	r, _ := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", false)
	args := cleanupArgs(r)
	args[0] = "--json"
	raw, stderr, err := runDraftBinary(t, "", args, false)
	if err == nil || raw != "" {
		t.Fatal(raw, stderr, err)
	}
	var e struct {
		Success bool           `json:"success"`
		Error   out.AgentError `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &e); err != nil {
		t.Fatal(err, stderr)
	}
	if e.Success || e.Error.Code != string(store.DraftCleanupProtected) || e.Error.Cleanup == nil || e.Error.Cleanup.Effect != "not_removed" {
		t.Fatal(stderr)
	}
}
func TestDraftCleanupIPCDeadlineAfterEffectHasUnknownResult(t *testing.T) {
	dir, a := draftOwnerFixture(t, false)
	r, path := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", true)
	ln, err := net.Listen("unix", sendDelegateSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	release := make(chan struct{})
	done := make(chan struct{})
	defer func() { close(release); _ = ln.Close(); <-done }()
	var calls atomic.Int64
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var req sendDelegateRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if _, err := executeDelegatedSend(context.Background(), a, req); err != nil {
			t.Error(err)
			return
		}
		// The bytes are gone, but the response is withheld past the caller budget.
		<-release
	}()
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 150 * time.Millisecond}, sendDelegateRequest{Kind: draftCleanupKind, DraftCleanup: &r})
	failure := classifyDraftCleanupError(err, nil)
	if err == nil || failure.Code != string(store.DraftCleanupOutcomeUncertain) || failure.Cleanup == nil || failure.Cleanup.Effect != "unknown" || calls.Load() != 1 {
		t.Fatal(err, failure)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDraftCleanupCLIPagesAndOldArchiveNoEffects(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	r, _ := seedCleanupDocument(t, a, strings.Repeat("a", 32), strings.Repeat("b", 32), "", false)
	next, _ := seedCleanupDocument(t, a, r.Selection.DraftID, strings.Repeat("c", 32), r.Selection.RevisionID, true)
	a.Close()
	raw, stderr, err := runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID, "--limit", "1"}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	first := decodeAgentTest(t, raw)
	var page draftCleanupPreviewDTO
	if err := json.Unmarshal(first.Data, &page); err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.NextCursor == nil || page.Counts.Examined != 1 || page.Revisions[0].RevisionID != r.Selection.RevisionID {
		t.Fatal(raw)
	}
	raw, stderr, err = runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID, "--limit", "200", "--cursor", *page.NextCursor, "--detail", "full"}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	if err := json.Unmarshal(decodeAgentTest(t, raw).Data, &page); err != nil {
		t.Fatal(err)
	}
	if page.HasMore || page.Counts.Examined != 1 || page.Revisions[0].RevisionID != next.Selection.RevisionID || page.Revisions[0].SnapshotPath == "" {
		t.Fatal(raw)
	}
	fixture, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec("DELETE FROM schema_migrations WHERE version=31"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, args := range [][]string{
		{"--agent", "--store", dir, "draft", "cleanup", "preview", r.Selection.DraftID}, cleanupArgs(next),
	} {
		raw, stderr, err = runDraftBinary(t, "", args, false)
		if err == nil || raw != "" {
			t.Fatal("upgraded old fixture", raw, stderr, err)
		}
	}
	after := snapshotLocalStore(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("old archive changed")
	}
}
