package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
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

func draftCLIImageFile(t *testing.T, dir string) string {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "PRIVATE_IMAGE_SOURCE.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func exerciseDraftImageCLI(t *testing.T, binary string, owner bool) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	path := draftCLIImageFile(t, dir)
	var calls atomic.Int64
	if owner {
		lk, err := lock.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lk.Release()
		stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
			calls.Add(1)
			if req.Kind != draftWriteKind || req.Draft == nil || req.Draft.Version != 2 || req.Draft.Input.Image == nil || req.Draft.Input.File != "" {
				t.Fatal("wrong preparation contract")
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
			if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "jpeg_thumbnail") || strings.Contains(stdout, "PRIVATE_IMAGE_SOURCE") {
				t.Fatal("public blob/path or multiline JSON", stdout)
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
	// Readonly and invalid union reject before contacting the owner or reading source.
	for _, args := range [][]string{{"create", "--read-only", "--to", localReadPN, "--image", path}, {"create", "--to", localReadPN, "--image", path, "--file", path}, {"create", "--to", localReadPN, "--image", path, "--message", "x"}, {"create", "--to", localReadPN, "--image", path, "--mime", "image/png"}, {"create", "--to", localReadPN, "--image", path, "--file", ""}, {"create", "--to", localReadPN, "--image", path, "--message-file", "UNOPENED"}} {
		_, stderr, err := run(args...)
		if err == nil || calls.Load() != 0 || strings.Contains(stderr, "PRIVATE_IMAGE_SOURCE") {
			t.Fatal(err, stderr, calls.Load())
		}
	}
	first, stderr, err := run("create", "--to", localReadLID, "--image", path, "--caption", " literal\\n\n🔷 ")
	if err != nil {
		t.Fatal(err, stderr)
	}
	if first.Kind != store.DraftImageKind || first.Document != nil || first.Image == nil || first.Image.SnapshotPath != "" || first.Image.Width != 3 || first.Image.ThumbnailBytes == 0 || first.Image.Caption != " literal\\n\n🔷 " {
		t.Fatal(first)
	}
	full, stderr, err := run("show", first.ID, "--revision", first.RevisionID, "--detail", "full")
	if err != nil || full.Image.SnapshotPath == "" {
		t.Fatal(full, stderr, err)
	}
	changed, stderr, err := run("update", first.ID, "--if-revision", first.RevisionID, "--to", localReadPN, "--image", path, "--caption", strings.Repeat("x", 513))
	if err != nil || changed.Hash == first.Hash || len(changed.TruncatedFields) != 1 || !strings.Contains(changed.Recovery, "image bytes") {
		t.Fatal(changed, stderr, err)
	}
	// Archive readers retain metadata when the exact snapshot is absent.
	if err := os.Rename(full.Image.SnapshotPath, full.Image.SnapshotPath+".retained"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("show", first.ID, "--revision", first.RevisionID); err != nil {
		t.Fatal(err)
	}
	if owner && calls.Load() != 2 {
		t.Fatal("unexpected owner writes", calls.Load())
	}
}
func TestDraftImageCLIStandaloneAndOwner(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "owner"}[owner], func(t *testing.T) { exerciseDraftImageCLI(t, "", owner) })
	}
}
func TestDraftImageProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_DRAFT_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_DRAFT_E2E_BINARY to freshly built binary")
	}
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "owner"}[owner], func(t *testing.T) { exerciseDraftImageCLI(t, binary, owner) })
	}
}
func TestDraftImageOwnerEnvelopeCannotEnterLegacy(t *testing.T) {
	dir, a := draftOwnerFixture(t, false)
	input := app.DraftInput{To: localReadPN, Image: &app.DraftImageInput{Path: "UNOPENED"}}
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
		conn, err := net.Dial("unix", sendDelegateSocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		req := sendDelegateRequest{Version: 1, Kind: kind, Draft: &app.DraftWriteRequest{Version: 2, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: dir, Input: &input}, File: "UNOPENED"}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var response sendDelegateResponse
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if response.DraftFailure == nil || response.DraftFailure.Code != "invalid_arguments" || calls.Load() != 0 {
			t.Fatal(response, calls.Load())
		}
	}
	if _, err := a.DB().ReadDraftRecord(t.Context(), strings.Repeat("a", 32)); err == nil {
		t.Fatal("write slipped through")
	}
}
