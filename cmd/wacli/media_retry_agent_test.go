package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func retryFixtureSQL(t *testing.T, dir, query string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func seedAgentRetry(t *testing.T, cache bool) (string, []byte) {
	t.Helper()
	dir, data := seedAgentMedia(t, "audio", cache)
	retryFixtureSQL(t, dir, "UPDATE messages SET media_key=zeroblob(32),direct_path='/private-synthetic-path',file_enc_sha256=zeroblob(32)")
	return dir, data
}

func TestAgentMediaRetryGuardsBeforeAnyStoreEffects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		env, code string
	}{
		{"readonly", []string{"--read-only"}, "", "read_only"},
		{"readonly_env", nil, "1", "read_only"},
		{"before_empty", []string{"--before="}, "", "invalid_arguments"},
		{"limit_default", []string{"--limit=0"}, "", "invalid_arguments"},
		{"batch_default", []string{"--batch=32"}, "", "invalid_arguments"},
		{"wait_zero", []string{"--wait=0"}, "", "invalid_arguments"},
		{"wait_short", []string{"--wait=500ms"}, "", "invalid_arguments"},
		{"wait_long", []string{"--wait=121s"}, "", "invalid_arguments"},
		{"timeout_zero", []string{"--timeout=0"}, "", "invalid_arguments"},
		{"timeout_short", []string{"--timeout=500ms"}, "", "invalid_arguments"},
		{"timeout_long", []string{"--timeout=6m"}, "", "invalid_arguments"},
		{"events", []string{"--events"}, "", "invalid_arguments"},
		{"malformed_duration", []string{"--wait=https://private.invalid?key=SECRET"}, "", "invalid_arguments"},
		{"unknown_flag", []string{"--bad=https://private.invalid?key=SECRET"}, "", "invalid_arguments"},
		{"blank_id", []string{"--id="}, "", "invalid_arguments"},
		{"phone_not_jid", []string{"--chat=123456789"}, "", "invalid_arguments"},
		{"blank_output", []string{"--output="}, "", "invalid_arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WACLI_READONLY", tc.env)
			parent := t.TempDir()
			dir := filepath.Join(parent, "missing-store")
			args := append(mediaFixtureArgs(dir, "retry"), "--output", filepath.Join(parent, "output", "file"))
			stdout, stderr, err := runAgentTest(t, append(args, tc.args...)...)
			env := decodeAgentTest(t, stderr)
			if err == nil || stdout != "" || env.Error.Code != tc.code || env.Meta.Source != "live" || strings.Contains(stderr, "SECRET") || strings.Contains(stderr, "private.invalid") {
				t.Fatalf("%v %s %s", err, stdout, stderr)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("guard created files: %v %v", entries, err)
			}
		})
	}
	for _, flag := range []string{"--id=selected", "--output=/explicit"} {
		parent := t.TempDir()
		_, _, err := runAgentTest(t, "--store", filepath.Join(parent, "missing"), "media", "retry", flag)
		if err == nil || !strings.Contains(err.Error(), "require --agent") {
			t.Fatalf("exact flag accepted without agent: %v", err)
		}
		entries, _ := os.ReadDir(parent)
		if len(entries) != 0 {
			t.Fatal("legacy exact guard opened store")
		}
	}
}

func TestAgentMediaRetryPreflightPreservesMissingOldAndInvalidArchives(t *testing.T) {
	for _, tc := range []struct{ name, code, sql string }{
		{"missing", "store_unavailable", ""}, {"old", "store_unavailable", "DELETE FROM schema_migrations WHERE version=31"},
		{"absent_message", "not_found", "DELETE FROM messages"}, {"tombstone", "media_deleted", "UPDATE messages SET deleted_at=0"},
		{"invalid_size", "media_metadata_incomplete", "UPDATE messages SET file_length=-1"},
		{"missing_hash", "media_binding_unverified", "UPDATE messages SET file_sha256=NULL"},
		{"missing_key", "media_metadata_incomplete", "UPDATE messages SET media_key=NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedAgentRetry(t, false)
			if tc.name == "missing" {
				dir = filepath.Join(t.TempDir(), "missing")
			} else {
				retryFixtureSQL(t, dir, tc.sql)
			}
			var before map[string]localFileSnapshot
			if tc.name != "missing" {
				before = snapshotLocalStore(t, dir)
			}
			output := filepath.Join(t.TempDir(), "nested", "file")
			stdout, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "retry"), "--output", output)...)
			if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != tc.code || decodeAgentTest(t, stderr).Meta.Source != "live" {
				t.Fatalf("%v %s %s", err, stdout, stderr)
			}
			if tc.name != "missing" && !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("preflight changed archive or permissions")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight output exists: %v", err)
			}
		})
	}
}

func TestAgentMediaRetryOwnerLockNeverTouchesIPCOrWA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	dir, _ := seedAgentRetry(t, false)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	listener, err := net.Listen("unix", filepath.Join(dir, ".send.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var accepts atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			accepts.Add(1)
			_ = conn.Close()
		}
	}()
	output := filepath.Join(t.TempDir(), "nested", "file")
	stdout, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "retry"), "--output", output)...)
	env := decodeAgentTest(t, stderr)
	var typed *out.AgentError
	if err == nil || stdout != "" || !errors.As(err, &typed) || typed.ExitCode != 4 || commandExitCode(err) != 4 || env.Error.Code != "store_locked" || env.Meta.Source != "live" || env.Error.Media == nil || env.Error.Media.FilePublication != "not_written" || env.Error.Media.Recorded {
		t.Fatalf("%v %s %s", err, stdout, stderr)
	}
	_ = listener.Close()
	<-done
	if accepts.Load() != 0 {
		t.Fatal("retry contacted owner IPC")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry opened WA session")
	}
	if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry created output directories before LOCK")
	}
}

func TestAgentMediaRetryProductionCacheAndDestinationNeedNoSession(t *testing.T) {
	for _, existing := range []bool{false, true} {
		dir, data := seedAgentRetry(t, true)
		retryFixtureSQL(t, dir, "UPDATE messages SET media_key=NULL,direct_path=NULL,file_length=0")
		output := filepath.Join(t.TempDir(), "nested", "file")
		if existing {
			output = filepath.Join(t.TempDir(), "existing")
			if err := os.WriteFile(output, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stdout, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "retry"), "--output", output)...)
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		env := decodeAgentTest(t, stdout)
		var result agentMediaRetryResult
		if err := json.Unmarshal(env.Data, &result); err != nil {
			t.Fatal(err)
		}
		status, publication := "cached", "written"
		if existing {
			status, publication = "existing", "not_written"
		}
		if env.Meta.Source != "live" || result.Status != status || result.FilePublication != publication || result.Recorded == existing || result.Retry.Phone != "not_requested" || result.Remote.Current != "unknown" || result.Output == nil || result.Output.Verification != "sha256_verified" {
			t.Fatalf("%s", stdout)
		}
		got, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%q %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "session.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("local result opened session")
		}
	}
}

type mediaRetryCLIWA struct {
	agentChatStateWA
	authed      bool
	receipts    atomic.Int64
	connectHook func() error
}

func (f *mediaRetryCLIWA) IsAuthed() bool { return f.authed }
func (f *mediaRetryCLIWA) Connect(ctx context.Context, opts wa.ConnectOptions) error {
	if f.connectHook != nil {
		if err := f.connectHook(); err != nil {
			return err
		}
	}
	return f.agentChatStateWA.Connect(ctx, opts)
}
func (f *mediaRetryCLIWA) SendMediaRetryReceipt(_ context.Context, info *types.MessageInfo, _ []byte) error {
	f.receipts.Add(1)
	f.emit(&events.MediaRetry{ChatID: info.Chat, MessageID: info.ID, Error: &events.MediaRetryError{Code: 2}})
	return nil
}

func retryFixtureRunner(t *testing.T, dir string, f *mediaRetryCLIWA, writer io.Writer, download app.MediaRetryBytes) error {
	t.Helper()
	flags := &rootFlags{storeDir: dir, agent: true, agentCapability: agentMediaRecovery, detail: "compact", timeout: 3 * time.Second}
	cmd := &cobra.Command{Use: "retry"}
	cmd.SetContext(context.Background())
	return runAgentMediaRetryWith(cmd, flags, mediaFixtureChat, "media-1", filepath.Join(dir, "media", "output"), time.Second, writer,
		func(ctx context.Context, flags *rootFlags) (*app.App, *lock.Lock, error) {
			lk, err := lock.AcquireWithTimeout(ctx, dir, 0)
			if err != nil {
				return nil, nil, err
			}
			a, err := app.New(app.Options{StoreDir: dir, Events: out.NewEventWriter(io.Discard, true), WADiagnosticWriter: io.Discard, WAFactory: func(opts wa.Options) (app.WAClient, error) {
				f.opens.Add(1)
				if opts.DiagnosticWriter != io.Discard {
					t.Error("WA diagnostics not suppressed")
				}
				return f, nil
			}})
			if err != nil {
				_ = lk.Release()
			}
			return a, lk, err
		}, download)
}

func TestAgentMediaRetryAppFakeAndLocalHTTPPreserveEffects(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"success", ""}, {"unauthenticated", "not_authenticated"}, {"connect_failure", "connect_failed"},
		{"changed_after_connect", "media_changed"}, {"output_failure", "output_failed"}, {"db_after_publication", "store_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, data := seedAgentRetry(t, false)
			f := &mediaRetryCLIWA{authed: tc.name != "unauthenticated"}
			if tc.name == "connect_failure" {
				f.connectHook = func() error { return errors.New("private URL key SQL cause") }
			}
			if tc.name == "changed_after_connect" {
				f.connectHook = func() error { retryFixtureSQL(t, dir, "UPDATE messages SET file_sha256=X'01'"); return nil }
			}
			if tc.name == "db_after_publication" {
				retryFixtureSQL(t, dir, "CREATE TRIGGER fixture_mark_fail BEFORE UPDATE OF local_path ON messages BEGIN SELECT RAISE(ABORT,'private SQL cause');END")
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = w.Write(data) }))
			defer server.Close()
			download := func(ctx context.Context, info store.MediaDownloadInfo, path string, reuploaded bool) ([]byte, error) {
				if reuploaded || path != info.DirectPath || len(info.FileEncSHA256) != 32 {
					t.Fatal("fallback dropped original ciphertext identity")
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				if err != nil {
					return nil, err
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					return nil, err
				}
				defer resp.Body.Close()
				return io.ReadAll(resp.Body)
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if tc.name == "output_failure" {
				writer = brokenMediaOutput{}
			}
			err := retryFixtureRunner(t, dir, f, writer, download)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				env := decodeAgentTest(t, output.String())
				if env.Meta.Source != "live" {
					t.Fatal("source changed")
				}
			} else {
				mapped := classifyMediaRetryCommandError(err)
				if err == nil || mapped.Code != tc.code || mapped.Media == nil {
					t.Fatalf("%+v %v", mapped, err)
				}
				if tc.name == "output_failure" || tc.name == "db_after_publication" {
					if mapped.Media.FilePublication != "written" || mapped.Media.Recorded != (tc.name == "output_failure") || mapped.Media.Path == nil || mapped.Media.SHA256 == "" {
						t.Fatalf("effects lost: %+v", mapped.Media)
					}
					var stderr bytes.Buffer
					if err := out.WriteAgentError(&stderr, out.AgentAccount{}, agentMeta(&rootFlags{agentCapability: agentMediaRecovery}), mapped); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(stderr.String(), "private SQL") || strings.Contains(stderr.String(), "private-synthetic-path") {
						t.Fatal("error leaked cause")
					}
				} else if f.receipts.Load() != 0 || requests.Load() != 0 {
					t.Fatal("network after failed auth/connect/binding")
				}
			}
			if f.opens.Load() != 1 || f.closed.Load() != 1 {
				t.Fatalf("client lifetime %d/%d", f.opens.Load(), f.closed.Load())
			}
			if tc.name == "success" || tc.name == "output_failure" || tc.name == "db_after_publication" {
				if f.receipts.Load() != 1 || requests.Load() != 1 {
					t.Fatal("operation repeated")
				}
				got, err := os.ReadFile(filepath.Join(dir, "media", "output"))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("published=%q %v", got, err)
				}
			}
		})
	}
}

func TestAgentMediaRetryCacheProvesZeroWAFactoryCalls(t *testing.T) {
	dir, _ := seedAgentRetry(t, true)
	f := &mediaRetryCLIWA{authed: true}
	var output bytes.Buffer
	if err := retryFixtureRunner(t, dir, f, &output, nil); err != nil {
		t.Fatal(err)
	}
	if f.opens.Load() != 0 || f.connects.Load() != 0 || f.receipts.Load() != 0 {
		t.Fatal("cache initialized a WA client")
	}
}

func TestAgentMediaRetryMainProcessGuards(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		extra []string
		code  string
		exit  int
	}{
		{"readonly", []string{"--read-only"}, "read_only", 2}, {"bulk", []string{"--limit=0"}, "invalid_arguments", 2},
		{"missing", nil, "store_unavailable", 4}, {"parse", []string{"--wait=PRIVATE_URL_SECRET"}, "invalid_arguments", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			args := append(mediaFixtureArgs(filepath.Join(parent, "missing"), "retry"), "--output", filepath.Join(parent, "output", "file"))
			command := exec.Command(exe, append([]string{"-test.run=^TestAgentMainProcess$", "--"}, append(args, tc.extra...)...)...)
			command.Env = append(os.Environ(), "WACLI_AGENT_TEST_PROCESS=1", "WACLI_READONLY=0")
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tc.exit || stdout.Len() != 0 {
				t.Fatalf("%v %s %s", err, stdout.String(), stderr.String())
			}
			env := decodeAgentTest(t, stderr.String())
			if env.Error.Code != tc.code || env.Meta.Source != "live" || strings.Contains(stderr.String(), "PRIVATE_URL_SECRET") {
				t.Fatalf("%s", stderr.String())
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("main process created files")
			}
		})
	}
}

// Opt-in reuses synthetic stores against the built production binary. It never
// allows a network-required unlocked request, so no real handshake is possible.
func TestAgentMediaRetryProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_MEDIA_RETRY_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_MEDIA_RETRY_E2E_BINARY to the production fixture binary")
	}
	for _, tc := range []struct {
		name, code string
		exit       int
	}{
		{"readonly", "read_only", 2}, {"bulk", "invalid_arguments", 2}, {"missing", "store_unavailable", 4},
		{"old", "store_unavailable", 4}, {"owner_lock", "store_locked", 4}, {"cache", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, data := seedAgentRetry(t, tc.name == "cache")
			if tc.name == "old" {
				retryFixtureSQL(t, dir, "DELETE FROM schema_migrations WHERE version=31")
			}
			if tc.name == "missing" {
				dir = filepath.Join(t.TempDir(), "missing")
			}
			if tc.name == "owner_lock" {
				lk, err := lock.Acquire(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer lk.Release()
			}
			var before map[string]localFileSnapshot
			if tc.name != "missing" {
				before = snapshotLocalStore(t, dir)
			}
			output := filepath.Join(t.TempDir(), "nested", "out")
			args := append(mediaFixtureArgs(dir, "retry"), "--output", output)
			if tc.name == "readonly" {
				args = append(args, "--read-only")
			}
			if tc.name == "bulk" {
				args = append(args, "--batch=32")
			}
			command := exec.Command(binary, args...)
			command.Env = append(os.Environ(), "WACLI_READONLY=0", "WACLI_DEVICE_LABEL=SyntheticFixture")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			exit := 0
			if err != nil {
				var failed *exec.ExitError
				if !errors.As(err, &failed) {
					t.Fatal(err)
				}
				exit = failed.ExitCode()
			}
			if exit != tc.exit {
				t.Fatalf("exit=%d %v %s", exit, err, stderr.String())
			}
			if tc.exit != 0 {
				env := decodeAgentTest(t, stderr.String())
				if stdout.Len() != 0 || env.Error.Code != tc.code || env.Meta.Source != "live" {
					t.Fatalf("%s %s", stdout.String(), stderr.String())
				}
				if tc.name == "owner_lock" && (env.Error.Media == nil || env.Error.Media.FilePublication != "not_written" || env.Error.Media.Recorded) {
					t.Fatalf("LOCK refusal lost effect knowledge: %s", stderr.String())
				}
				if tc.name != "missing" && !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
					t.Fatal("binary failure modified store")
				}
				if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("binary failure created output directory")
				}
			} else {
				env := decodeAgentTest(t, stdout.String())
				var result agentMediaRetryResult
				if err := json.Unmarshal(env.Data, &result); err != nil {
					t.Fatal(err)
				}
				if stderr.Len() != 0 || env.Meta.Source != "live" || result.Status != "cached" || !result.Recorded || result.FilePublication != "written" {
					t.Fatalf("%s %s", stdout.String(), stderr.String())
				}
				got, err := os.ReadFile(output)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("%q %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "session.db")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("production fixture initialized WA")
			}
		})
	}
}
