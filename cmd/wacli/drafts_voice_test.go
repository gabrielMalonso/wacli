package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func draftCLIVoiceFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := hex.DecodeString("4f676753000200000000000000001818181800000000c3c6153d01134f707573486561640101000080bb00000000004f67675300000000000000000000181818180100000085270dd201194f707573546167730900000073796e746865746963000000004f6767530004c0030000000000001818181802000000290c03cf0103f8fffe")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "PRIVATE_VOICE_SOURCE.ogg")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func exerciseDraftVoiceCLI(t *testing.T, binary string, owner bool) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	path := draftCLIVoiceFile(t, dir)
	var calls atomic.Int64
	if owner {
		lk, err := lock.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lk.Release()
		stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
			calls.Add(1)
			if req.Kind != draftWriteKind || req.Draft == nil || req.File != "" || (req.Draft.Action != "discard" && (req.Draft.Version != 3 || req.Draft.Input.Voice == nil || req.Draft.Input.File != "")) || (req.Draft.Action == "discard" && (req.Draft.Version != 1 || req.Draft.Input != nil)) {
				t.Fatal("voice request contract")
			}
			return executeDelegatedSend(ctx, a, req)
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
	}
	run := func(args ...string) (draftDTO, string, error) {
		stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir, "draft"}, args...), false)
		var dto draftDTO
		if err == nil {
			if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "PRIVATE_VOICE_SOURCE") || strings.Contains(stdout, "synthetic") || strings.Contains(stdout, "waveform\"") || strings.Contains(stdout, "base64") {
				t.Fatal("private data/invalid JSON", stdout)
			}
			env := decodeAgentTest(t, stdout)
			if env.Meta.Source != "local" {
				t.Fatal(stdout)
			}
			if err := json.Unmarshal(env.Data, &dto); err != nil {
				t.Fatal(err)
			}
		}
		return dto, stderr, err
	}
	for _, option := range []string{"image", "file", "message", "message-file", "contact-name", "contact-phone", "filename", "mime", "caption", "mention"} {
		_, stderr, err := run("create", "--to", localReadPN, "--voice", path, "--"+option, "")
		if err == nil || calls.Load() != 0 || strings.Contains(stderr, "PRIVATE_VOICE_SOURCE") {
			t.Fatal(option, err, stderr, calls.Load())
		}
	}
	_, stderr, err := run("create", "--read-only", "--to", localReadPN, "--voice", "UNOPENED")
	if err == nil || calls.Load() != 0 || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatal("readonly before source/socket", stderr, err)
	}
	first, stderr, err := run("create", "--to", localReadLID, "--voice", path)
	if err != nil {
		t.Fatal(stderr, err)
	}
	if first.Kind != store.DraftVoiceKind || first.Document != nil || first.Image != nil || first.Voice == nil || !first.Voice.PTT || first.Voice.SecondsPresent || first.Voice.WaveformPresent || first.Voice.Basis != "declared_structure" || first.Voice.SnapshotPath != "" {
		t.Fatal(first)
	}
	full, stderr, err := run("show", first.ID, "--revision", first.RevisionID, "--detail", "full")
	if err != nil || full.Voice.SnapshotPath == "" || !strings.Contains(full.Recovery, "human approval") {
		t.Fatal(full, stderr, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(full.Voice.SnapshotPath)
	if err != nil || !bytes.Equal(original, snapshot) {
		t.Fatal("exact voice snapshot", err)
	}
	next, stderr, err := run("update", first.ID, "--if-revision", first.RevisionID, "--to", localReadPN, "--voice", path)
	if err != nil || next.Revision != 2 {
		t.Fatal(next, stderr, err)
	}
	if err := os.Rename(full.Voice.SnapshotPath, full.Voice.SnapshotPath+".retained"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("show", first.ID, "--revision", first.RevisionID); err != nil {
		t.Fatal("show media IO", err)
	}
	stdout, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "list"}, false)
	if err != nil || !strings.Contains(stdout, `"kind":"voice"`) {
		t.Fatal(stdout, stderr, err)
	}
	if _, stderr, err := run("discard", first.ID, "--if-revision", next.RevisionID); err != nil {
		t.Fatal(stderr, err)
	}
	if owner && calls.Load() != 3 {
		t.Fatal("unexpected writes", calls.Load())
	}
	// Cleanup never makes voice eligible or unlinks its retained snapshot.
	stdout, stderr, err = runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "cleanup", "preview", first.ID}, false)
	if err != nil || !strings.Contains(stdout, `"eligibility":"non_document"`) {
		t.Fatal(stdout, stderr, err)
	}
}
func TestDraftVoiceCLIStandaloneAndOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "owner"}[owner], func(t *testing.T) { exerciseDraftVoiceCLI(t, "", owner) })
	}
}
func TestDraftVoiceProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to freshly built binary")
	}
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "owner"}[owner], func(t *testing.T) { exerciseDraftVoiceCLI(t, binary, owner) })
	}
}
func TestDraftVoiceOwnerEnvelopeCannotEnterLegacy(t *testing.T) {
	dir, _ := draftOwnerFixture(t, false)
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		return sendDelegateResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, kind := range []string{"file", "voice", "unknown", draftWriteKind} {
		req := sendDelegateRequest{Version: 1, Kind: kind, File: "UNOPENED", Draft: &app.DraftWriteRequest{Version: 3, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: dir, Input: &app.DraftInput{To: localReadPN, Voice: &app.DraftVoiceInput{Path: "UNOPENED"}}}}
		conn, err := net.Dial("unix", sendDelegateSocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var resp sendDelegateResponse
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if resp.DraftFailure == nil || resp.DraftFailure.Code != "invalid_arguments" || calls.Load() != 0 {
			t.Fatal(resp, calls.Load())
		}
	}
}
