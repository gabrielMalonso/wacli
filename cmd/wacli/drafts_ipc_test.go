package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func draftOwnerFixture(t *testing.T, readOnly bool) (string, *app.App) {
	return draftOwnerFixtureOptions(t, app.Options{ReadOnly: readOnly})
}

func draftOwnerFixtureOptions(t *testing.T, opts app.Options) (string, *app.App) {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	source := seedLocalReadStore(t)
	for _, name := range []string{"wacli.db", "session.db"} {
		raw, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts.StoreDir = dir
	a, err := app.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return dir, a
}
func runDraftBinary(t *testing.T, binary string, args []string, readOnly bool) (string, string, error) {
	t.Helper()
	if binary == "" {
		stdout, stderr, err := runPresenceDelegateHelper(t, args)
		return strings.TrimSuffix(stdout, "PASS\n"), stderr, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "WACLI_READONLY=0")
	if readOnly {
		cmd.Env = append(cmd.Env, "WACLI_READONLY=1")
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
func exerciseDraftOwner(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{min: 10 * time.Second, max: 10 * time.Second}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		if req.Kind != draftWriteKind || req.Draft == nil {
			t.Error("nonlocal request reached owner")
			return sendDelegateResponse{}, errors.New("fixture rejects WA operations")
		}
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	run := func(args ...string) (draftDTO, string, error) {
		raw, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir, "--timeout", "2s", "draft"}, args...), false)
		var dto draftDTO
		if err == nil {
			e := decodeAgentTest(t, raw)
			if e.Meta.Source != "local" {
				t.Fatal("source", raw)
			}
			if err := json.Unmarshal(e.Data, &dto); err != nil {
				t.Fatal(err)
			}
		}
		return dto, stderr, err
	}
	_, stderr, err := run("create", "--read-only", "--to", localReadPN, "--message", "guard")
	if err == nil || calls.Load() != 0 || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatal("readonly effect", err, stderr)
	}
	first, stderr, err := run("create", "--to", localReadLID, "--message", "exact\\n\n👋")
	if err != nil {
		t.Fatal(err, stderr)
	}
	second, stderr, err := run("update", first.ID, "--if-revision", first.RevisionID, "--to", localReadPN, "--message", "replacement")
	if err != nil || second.Revision != 2 || calls.Load() != 2 {
		t.Fatal("local branch was paced", err, stderr)
	}
	_, stderr, err = run("discard", first.ID, "--if-revision", second.RevisionID)
	if err != nil || calls.Load() != 3 {
		t.Fatal(err, stderr)
	}
	old, stderr, err := run("show", first.ID, "--revision", first.RevisionID, "--detail", "full")
	if err != nil || old.State != "discarded" || old.Text.Text != "exact\\n\n👋" || calls.Load() != 3 {
		t.Fatal("read dispatched", err, stderr)
	}
	// A fixture owner has no WA client at all: invoking any send path would fail.
}
func TestDraftIPCRealOwnerOfflineWithoutPacing(t *testing.T) { exerciseDraftOwner(t, "") }
func TestDraftProductionBinaryOwnerOffline(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to a freshly built local binary")
	}
	exerciseDraftOwner(t, binary)
}
func TestDraftIPCReadOnlyOwner(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, true)
	stop, err := startSendDelegateServer(context.Background(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	id, rid := strings.Repeat("a", 32), strings.Repeat("b", 32)
	message := "fixture"
	req := app.DraftWriteRequest{Version: 1, Action: "create", DraftID: id, RevisionID: rid, StoreRef: dir, Input: &app.DraftInput{To: localReadPN, Message: &message}}
	_, err = delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: draftWriteKind, Draft: &req})
	if typed := classifyDraftError(err); typed.Code != "read_only" || typed.ExitCode != 2 {
		t.Fatal(err)
	}
	if _, err := a.DB().ReadDraftRecord(context.Background(), id); err == nil {
		t.Fatal("readonly owner wrote")
	}
}
func TestDraftIPCUncertainOldOwnerLostReplyAndMismatch(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, mode := range []string{"old_owner", "lost_reply", "mismatch_id", "mismatch_hash", "missing_typed"} {
		t.Run(mode, func(t *testing.T) {
			dir, a := draftOwnerFixture(t, false)
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
			done := make(chan sendDelegateRequest, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req sendDelegateRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				done <- req
				if mode == "old_owner" || mode == "missing_typed" {
					_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "unsupported old owner"})
					return
				}
				resp, err := executeDelegatedSend(context.Background(), a, req)
				if err != nil {
					t.Error(err)
					return
				}
				if mode == "lost_reply" {
					return
				}
				if mode == "mismatch_id" {
					resp.DraftResult.RevisionID = strings.Repeat("f", 32)
				} else {
					resp.DraftResult.Hash = strings.Repeat("0", 64)
				}
				_ = json.NewEncoder(conn).Encode(resp)
			}()
			stdout, stderr, err := runPresenceDelegateHelper(t, []string{"--agent", "--store", dir, "--timeout", "2s", "draft", "create", "--to", localReadPN, "--message", "persist once"})
			if err == nil || stdout != "" {
				t.Fatal("uncertainty presented success", err, stdout)
			}
			e := decodeAgentTest(t, stderr)
			if e.Error.Code != "local_write_uncertain" || e.Error.Draft == nil || e.Error.ExitCode != 0 {
				t.Fatal(stderr)
			}
			req := <-done
			if e.Error.Draft.DraftID != req.Draft.DraftID || e.Error.Draft.RevisionID != req.Draft.RevisionID {
				t.Fatal("missing IDs")
			}
			if mode != "old_owner" && mode != "missing_typed" {
				if _, err := a.DB().ReadDraft(context.Background(), req.Draft.DraftID, req.Draft.RevisionID); err != nil {
					t.Fatal("lost reply erased commit", err)
				}
			}
			page, err := a.DB().ListDrafts(context.Background(), dir, true, 20, "")
			if err != nil || len(page.Items) > 1 {
				t.Fatal("fallback/replay", err)
			}
		})
	}
}

func TestDraftProductionBinaryStandaloneDocumentAndCard(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to a freshly built local binary")
	}
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "fixture.txt")
	t.Setenv("WACLI_MEDIA_ROOTS", sourceDir)
	if err := os.WriteFile(source, []byte("local bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(readonly bool, args ...string) (draftDTO, string, error) {
		raw, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir, "draft"}, args...), readonly)
		var dto draftDTO
		if err == nil {
			e := decodeAgentTest(t, raw)
			if err := json.Unmarshal(e.Data, &dto); err != nil {
				t.Fatal(err)
			}
		}
		return dto, stderr, err
	}
	_, stderr, err := run(true, "create", "--to", localReadPN, "--file", source)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatal("env policy", err, stderr)
	}
	doc, stderr, err := run(false, "create", "--to", localReadLID, "--file", source)
	if err != nil || doc.Document.Size != 12 || doc.Document.SnapshotPath != "" {
		t.Fatal("document", err, stderr)
	}
	// The original import path is gone; full still derives the artifact path.
	if err := os.Rename(source, source+".fixture-hidden"); err != nil {
		t.Fatal(err)
	}
	full, stderr, err := run(false, "show", doc.ID, "--revision", doc.RevisionID, "--detail", "full")
	if err != nil || full.Document.SnapshotPath != filepath.Join(dir, "draft-media", doc.RevisionID+".blob") {
		t.Fatal("full artifact", err, stderr)
	}
	card, stderr, err := run(false, "update", doc.ID, "--if-revision", doc.RevisionID, "--to", localReadPN, "--contact-name", "Fixture;\nFN:inject", "--contact-phone", "+15550000003")
	if err != nil || card.Contact.Phone != "15550000003" || !strings.Contains(card.Contact.VCard, `Fixture\;\nFN:inject`) {
		t.Fatal("card", err, stderr)
	}
	_, stderr, err = run(false, "discard", doc.ID, "--if-revision", card.RevisionID)
	if err != nil {
		t.Fatal(err, stderr)
	}
	if _, stderr, err := run(false, "show", doc.ID, "--revision", doc.RevisionID, "--detail", "full"); err != nil {
		t.Fatal(err, stderr)
	}
}

func TestDraftIPCExpiresQueuedWriteWithoutDispatch(t *testing.T) {
	dir, a := draftOwnerFixture(t, false)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return draftRefusal(req, ctx.Err()), nil
			}
		}
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	makeRequest := func(id, rid string) sendDelegateRequest {
		message := "fixture"
		return sendDelegateRequest{Kind: draftWriteKind, Draft: &app.DraftWriteRequest{Version: 1, Action: "create", DraftID: id, RevisionID: rid, StoreRef: dir, Input: &app.DraftInput{To: localReadPN, Message: &message}}}
	}
	first := makeRequest(strings.Repeat("a", 32), strings.Repeat("b", 32))
	second := makeRequest(strings.Repeat("c", 32), strings.Repeat("d", 32))
	done := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 2 * time.Second}, first)
		done <- err
	}()
	<-started
	_, err = delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 50 * time.Millisecond}, second)
	if err == nil || classifyDraftError(err).Code != "local_write_not_dispatched" || calls.Load() != 1 {
		close(release)
		t.Fatal("expired request dispatched", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal("first operation", err)
	}
	if _, err := a.DB().ReadDraftRecord(context.Background(), second.Draft.DraftID); err == nil {
		t.Fatal("queued draft exists")
	}
}

func TestDraftProductionBinaryReviewRegressions(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to a freshly built local binary")
	}
	t.Setenv("WACLI_READONLY", "0")
	t.Run("DM sender before snapshot", func(t *testing.T) {
		dir := seedLocalReadStore(t)
		sourceDir := t.TempDir()
		source := filepath.Join(sourceDir, "fixture.txt")
		t.Setenv("WACLI_MEDIA_ROOTS", sourceDir)
		if err := os.WriteFile(source, []byte("local fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(filepath.Join(dir, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "third", SenderJID: "15550000003@s.whatsapp.net", Text: "real fixture quote", Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "create", "--to", localReadPN, "--file", source, "--reply-to", "third"}, false)
		var process *exec.ExitError
		if !errors.As(err, &process) || process.ExitCode() != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatal("DM quote accepted", err, stdout, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, store.DraftMediaDirectory)); !os.IsNotExist(err) {
			t.Fatal("snapshot before sender validation", err)
		}
		stdout, stderr, err = runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "create", "--to", localReadPN, "--message", "valid mapped reply", "--reply-to", "m1"}, false)
		if err != nil || !decodeAgentTest(t, stdout).Success {
			t.Fatal("verified PN/LID reply rejected", err, stderr)
		}
	})
	for _, tc := range draftCorruptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedLocalReadStore(t)
			stdout, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "create", "--to", localReadPN, "--message", "saved"}, false)
			if err != nil {
				t.Fatal(err, stderr)
			}
			e := decodeAgentTest(t, stdout)
			var dto draftDTO
			if err := json.Unmarshal(e.Data, &dto); err != nil {
				t.Fatal(err)
			}
			corruptDraftFixture(t, dir, dto.RevisionID, tc.statement)
			for _, args := range [][]string{{"show", dto.ID}, {"discard", dto.ID, "--if-revision", dto.RevisionID}} {
				stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir, "draft"}, args...), false)
				var process *exec.ExitError
				if !errors.As(err, &process) || process.ExitCode() != 4 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" || strings.Contains(stderr, "SECRET") {
					t.Fatal("corrupt archive exit/envelope", err, stdout, stderr)
				}
			}
		})
	}
}
