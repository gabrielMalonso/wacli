package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/wa"
)

// This helper also compiles unchanged against the pre-image base. Its socket
// runs the actual base decoder, validation and executor, never a canned reply.
func TestImageBaseOwnerProcess(t *testing.T) {
	dir := os.Getenv("WACLI_IMAGE_BASE_OWNER_STORE")
	if dir == "" {
		t.Skip("fixture subprocess only")
	}
	var opens atomic.Int64
	a, err := app.New(app.Options{StoreDir: dir, WAFactory: func(wa.Options) (app.WAClient, error) { opens.Add(1); return nil, errors.New("fixture prohibits WA") }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	fmt.Fprintln(os.Stdout, "READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if opens.Load() != 0 {
		t.Fatal("base opened WA", opens.Load())
	}
}

func TestDraftImageRealBaseOwnerAndOldClient(t *testing.T) {
	binary := os.Getenv("WACLI_IMAGE_BASE_TEST_BINARY")
	oldCLI := os.Getenv("WACLI_IMAGE_BASE_BINARY")
	if binary == "" || oldCLI == "" {
		t.Skip("build base binaries and set WACLI_IMAGE_BASE_TEST_BINARY/WACLI_IMAGE_BASE_BINARY")
	}
	t.Setenv("WACLI_READONLY", "0")
	dir, _ := draftOwnerFixture(t, false)
	source := draftCLIImageFile(t, dir)
	// Freeze one image before the old owner takes LOCK, to exercise old dispatch.
	stdout, stderr, err := runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "create", "--to", localReadLID, "--image", source}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	var draft draftDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &draft); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	child := exec.CommandContext(ctx, binary, "-test.run=^TestImageBaseOwnerProcess$")
	child.Env = append(os.Environ(), "WACLI_IMAGE_BASE_OWNER_STORE="+dir)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	child.Stderr = &diagnostics
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if err := child.Wait(); err != nil {
			t.Error("base owner", err, diagnostics.String())
		}
	})
	ready, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || ready != "READY\n" {
		t.Fatal("base owner readiness", ready, err)
	}
	_, stderr, err = runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "create", "--to", localReadLID, "--image", source}, false)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "local_write_uncertain" {
		t.Fatal("old owner refusal/correlation", stderr, err)
	}
	_, stderr, err = runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "update", draft.ID, "--if-revision", draft.RevisionID, "--to", localReadLID, "--image", source}, false)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "local_write_uncertain" {
		t.Fatal("old owner update", stderr, err)
	}
	_, stderr, err = runDraftBinary(t, "", []string{"--agent", "--store", dir, "outbound", "send", draft.ID, "--revision", draft.RevisionID, "--expect-hash", draft.Hash, "--key", "base-refusal"}, false)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "store_error" {
		t.Fatal("old owner dispatched image", stderr, err)
	}
	// Refused v2 creates/updates must not publish blobs or advance the head.
	reader, lk, err := newReadApp(t.Context(), &rootFlags{storeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer closeApp(reader, lk)
	page, err := reader.DB().ListDrafts(t.Context(), dir, false, 20, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].RevisionID != draft.RevisionID {
		t.Fatal(page, err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "draft-media"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	missing := filepath.Join(t.TempDir(), "unopened")
	_, _, err = runDraftBinary(t, oldCLI, []string{"--store", missing, "draft", "create", "--to", localReadLID, "--image", source}, false)
	if err == nil {
		t.Fatal("old client accepted unsupported selector")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("old client store effect", err)
	}
}
