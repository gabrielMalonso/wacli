package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/openclaw/wacli/internal/app"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run a test binary compiled from the unchanged image/pre-image tree. Its
// existing TestImageBaseOwnerProcess starts the actual decoder and executor.
func TestDraftVoiceRealOlderOwnersAndOldClient(t *testing.T) {
	imageOwner, imageCLI := os.Getenv("WACLI_VOICE_IMAGE_OWNER_TEST_BINARY"), os.Getenv("WACLI_VOICE_IMAGE_OWNER_BINARY")
	if imageOwner == "" || imageCLI == "" {
		t.Skip("build unchanged image owner/client fixture binaries")
	}
	cases := []struct{ name, binary, cli string }{{"image", imageOwner, imageCLI}}
	if pre := os.Getenv("WACLI_IMAGE_BASE_TEST_BINARY"); pre != "" {
		cases = append(cases, struct{ name, binary, cli string }{"pre-image", pre, os.Getenv("WACLI_IMAGE_BASE_BINARY")})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("WACLI_READONLY", "0")
			dir, _ := draftOwnerFixture(t, false)
			path := draftCLIVoiceFile(t, dir)
			stdout, stderr, err := runDraftBinary(t, "", []string{"--agent", "--store", dir, "draft", "create", "--to", localReadLID, "--voice", path}, false)
			if err != nil {
				t.Fatal(stderr, err)
			}
			var draft draftDTO
			if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &draft); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			t.Cleanup(cancel)
			child := exec.CommandContext(ctx, c.binary, "-test.run=^TestImageBaseOwnerProcess$")
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
					t.Error("old owner", err, diagnostics.String())
				}
			})
			ready, err := bufio.NewReader(output).ReadString('\n')
			if err != nil || ready != "READY\n" {
				t.Fatal(ready, err)
			}
			for _, args := range [][]string{{"draft", "create", "--to", localReadLID, "--voice", path}, {"draft", "update", draft.ID, "--if-revision", draft.RevisionID, "--to", localReadLID, "--voice", path}} {
				_, stderr, err := runDraftBinary(t, "", append([]string{"--agent", "--store", dir}, args...), false)
				if err == nil || decodeAgentTest(t, stderr).Error.Code != "local_write_uncertain" {
					t.Fatal("old owner voice refusal/correlation", stderr, err)
				}
			}
			// Unknown nested voice must fail old decoders even when its version names
			// the old image contract, or its outer kind suggests legacy file/voice.
			for _, version := range []int{1, 2, 3} {
				for _, kind := range []string{draftWriteKind, "voice", "file", "unknown"} {
					req := sendDelegateRequest{Version: 1, Kind: kind, File: "UNOPENED", Draft: &app.DraftWriteRequest{Version: version, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: dir, Input: &app.DraftInput{To: localReadPN, Voice: &app.DraftVoiceInput{Path: "UNOPENED"}}}}
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
					if resp.OK {
						t.Fatal("old owner accepted voice")
					}
				}
			}
			_, stderr, err = runDraftBinary(t, "", []string{"--agent", "--store", dir, "outbound", "send", draft.ID, "--revision", draft.RevisionID, "--expect-hash", draft.Hash, "--key", "voice-base-refusal"}, false)
			if err == nil || decodeAgentTest(t, stderr).Error.Code != "store_error" {
				t.Fatal("old owner dispatched stored voice", stderr, err)
			}
			reader, lk, err := newReadApp(t.Context(), &rootFlags{storeDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer closeApp(reader, lk)
			page, err := reader.DB().ListDrafts(t.Context(), dir, false, 20, "")
			if err != nil || len(page.Items) != 1 || page.Items[0].RevisionID != draft.RevisionID {
				t.Fatal("old owner effects", page, err)
			}
			files, err := os.ReadDir(filepath.Join(dir, "draft-media"))
			if err != nil || len(files) != 1 {
				t.Fatal(files, err)
			}
			if c.cli != "" {
				missing := filepath.Join(t.TempDir(), "unopened")
				_, _, err := runDraftBinary(t, c.cli, []string{"--store", missing, "draft", "create", "--to", localReadLID, "--voice", path}, false)
				if err == nil {
					t.Fatal("old client accepted --voice")
				}
				if _, err := os.Stat(missing); !os.IsNotExist(err) {
					t.Fatal("old client store effect", err)
				}
			}
		})
	}
}
