package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
)

func buildTranscriptionFixture(t *testing.T) string {
	t.Helper()
	name := "adapter; 'literal'"
	if runtime.GOOS == "windows" {
		name = "adapter.exe"
	}
	adapter := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", adapter, "testdata/transcription-adapter/main.go")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOTOOLCHAIN=local")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v, %s", err, data)
	}
	return adapter
}

func TestAgentTranscription(t *testing.T) { exerciseAgentTranscription(t, "") }

// Only synthetic local bytes and a freshly built, offline protocol stub.
func TestAgentTranscriptionProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_TRANSCRIBE_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_TRANSCRIBE_E2E_BINARY to a freshly built production binary")
	}
	exerciseAgentTranscription(t, binary)
}

func exerciseAgentTranscription(t *testing.T, binary string) {
	t.Helper()
	adapter := buildTranscriptionFixture(t)
	parent := t.TempDir()
	storeDir := filepath.Join(parent, "missing", "archive")
	file := filepath.Join(parent, "misleading-name.txt")
	data := append([]byte("OggS\x00"), []byte("synthetic bytes only\x00\xff")...)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	t.Setenv("XDG_STATE_HOME", filepath.Join(parent, "config-state"))
	t.Setenv("WACLI_STORE_DIR", storeDir)
	t.Setenv("WACLI_READONLY", "1")
	t.Setenv(mediaRootsEnv, parent)
	markerDir := t.TempDir()
	run := func(args []string, code string, exit int, executes bool) agentTestEnvelope {
		t.Helper()
		marker := filepath.Join(markerDir, fmt.Sprintf("call-%d", time.Now().UnixNano()))
		t.Setenv("WACLI_TRANSCRIBE_STUB_MARKER", marker)
		var stdout, stderr string
		var err error
		if binary == "" {
			stdout, stderr, err = runAgentTest(t, args...)
			if commandExitCode(err) != exit {
				t.Fatalf("want exit%d: %v, %s %s", exit, err, stdout, stderr)
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			var out, diagnostic bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &diagnostic
			err = cmd.Run()
			stdout, stderr = out.String(), diagnostic.String()
			gotExit := 0
			if err != nil {
				var status *exec.ExitError
				if !errors.As(err, &status) {
					t.Fatalf("process: %v", err)
				}
				gotExit = status.ExitCode()
			}
			if gotExit != exit {
				t.Fatalf("want exit%d got%d: %v %s %s", exit, gotExit, err, stdout, stderr)
			}
		}
		_, markerErr := os.Stat(marker)
		if executes != (markerErr == nil) {
			t.Fatalf("executed=%v want%v: %v", markerErr == nil, executes, markerErr)
		}
		raw := stdout
		if code != "" {
			raw = stderr
			if err == nil || stdout != "" {
				t.Fatalf("failure output: %v %s %s", err, stdout, stderr)
			}
		} else if err != nil || stderr != "" {
			t.Fatalf("success output: %v %s %s", err, stdout, stderr)
		}
		envelope := decodeAgentTest(t, raw)
		if envelope.Meta.Source != "local" || envelope.Meta.Completeness != "unknown" || envelope.Meta.Freshness != "unknown" {
			t.Fatalf("invalid source: %s", raw)
		}
		if code != "" && (envelope.Error.Code != code || strings.Contains(raw, "PRIVATE_") || strings.Contains(raw, adapter)) {
			t.Fatalf("unsafe error: %s", raw)
		}
		return envelope
	}
	base := []string{"--store", storeDir, "media", "transcribe", "--agent", "--file", file, "--adapter", adapter, "--expect-sha256", digest}
	for _, extra := range [][]string{nil, {"--read-only"}, {"--json"}, {"--detail", "full"}} {
		env := run(append(append([]string{}, base...), extra...), "", 0, true)
		var dto agentTranscriptionResult
		if err := json.Unmarshal(env.Data, &dto); err != nil {
			t.Fatal(err)
		}
		if dto.Text != fmt.Sprintf("%d:%s:application/ogg", len(data), digest) || dto.Input.Path != file || dto.Input.Bytes != len(data) || dto.Input.SHA256 != digest || dto.Status != app.TranscriptionCompleted || dto.Language == nil || *dto.Language != "pt-BR" || dto.TextTruncated {
			t.Fatalf("not exact observed input: %s", env.Data)
		}
	}
	for _, detail := range []string{"compact", "full"} {
		env := run(append(append([]string{}, base...), "--mime-type", "audio/ogg; mode=long", "--detail", detail), "", 0, true)
		var dto agentTranscriptionResult
		if err := json.Unmarshal(env.Data, &dto); err != nil {
			t.Fatal(err)
		}
		n := 400
		if detail == "compact" {
			n = 320
		}
		if dto.Text != strings.Repeat("界", n) || dto.TextTruncated != (detail == "compact") || dto.Input.SHA256 != digest {
			t.Fatalf("bad detail: %s", env.Data)
		}
	}
	empty := run(append(append([]string{}, base...), "--mime-type", "audio/ogg; mode=empty"), "", 0, true)
	if !bytes.Contains(empty.Data, []byte(`"language":null`)) || !bytes.Contains(empty.Data, []byte(`"status":"empty"`)) {
		t.Fatalf("false recognition: %s", empty.Data)
	}
	for _, tc := range []struct {
		name     string
		extra    []string
		code     string
		exit     int
		executes bool
	}{
		{"hash", []string{"--expect-sha256", strings.Repeat("0", 64)}, "hash_mismatch", 1, false},
		{"uppercase", []string{"--expect-sha256", strings.Repeat("AB", 32)}, "invalid_arguments", 2, false},
		{"missing_adapter", []string{"--adapter="}, "adapter_not_configured", 2, false},
		{"relative_adapter", []string{"--adapter=PRIVATE_RELATIVE"}, "invalid_arguments", 2, false},
		{"nonexistent_adapter", []string{"--adapter", filepath.Join(parent, "PRIVATE_NONEXISTENT")}, "path_not_allowed", 1, false},
		{"directory_adapter", []string{"--adapter", parent}, "path_not_allowed", 1, false},
		{"missing_input", []string{"--file", filepath.Join(parent, "absent")}, "input_not_found", 3, false},
		{"missing_flag", []string{"--file="}, "invalid_arguments", 2, false},
		{"directory", []string{"--file", parent}, "path_not_allowed", 1, false},
		{"mime", []string{"--mime-type=PRIVATE_INVALID"}, "invalid_arguments", 2, false},
		{"empty_mime", []string{"--mime-type="}, "invalid_arguments", 2, false},
		{"mime_cap", []string{"--mime-type=audio/" + strings.Repeat("x", 256)}, "invalid_arguments", 2, false},
		{"file_cap", []string{"--file=" + strings.Repeat("x", 4097)}, "invalid_arguments", 2, false},
		{"timeout_zero", []string{"--timeout=0"}, "invalid_arguments", 2, false},
		{"timeout_large", []string{"--timeout=301s"}, "invalid_arguments", 2, false},
		{"parse", []string{"--timeout=PRIVATE_PARSE"}, "invalid_arguments", 2, false},
		{"unknown_flag", []string{"--PRIVATE_UNKNOWN=x"}, "invalid_arguments", 2, false},
		{"events", []string{"--events"}, "invalid_arguments", 2, false},
		{"cursor", []string{"--cursor=PRIVATE_CURSOR"}, "invalid_arguments", 2, false},
		{"detail", []string{"--detail=PRIVATE_DETAIL"}, "invalid_arguments", 2, false},
		{"nonzero", []string{"--mime-type", "audio/ogg; mode=nonzero"}, "adapter_failed", 1, true},
		{"deadline", []string{"--timeout=100ms", "--mime-type", "audio/ogg; mode=block"}, "transcription_timeout", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run(append(append([]string{}, base...), tc.extra...), tc.code, tc.exit, tc.executes)
		})
	}
	for _, mode := range []string{"unknown", "trailing", "duplicate", "null", "version", "utf8", "oversize", "textoversize"} {
		run(append(append([]string{}, base...), "--mime-type", "audio/ogg; mode="+mode), "adapter_output_invalid", 1, true)
	}
	for _, roots := range []string{"PRIVATE_RELATIVE_ROOT", markerDir, filepath.Join(parent, "missing-root")} {
		t.Setenv(mediaRootsEnv, roots)
		code, exit := "path_not_allowed", 1
		if roots == "PRIVATE_RELATIVE_ROOT" {
			code, exit = "invalid_arguments", 2
		}
		run(base, code, exit, false)
	}
	t.Setenv(mediaRootsEnv, parent)
	unknown := filepath.Join(parent, "fake-audio.mp3")
	if err := os.WriteFile(unknown, []byte{0, 255, 0, 128}, 0o600); err != nil {
		t.Fatal(err)
	}
	unknownArgs := []string{"--store", storeDir, "media", "transcribe", "--agent", "--file", unknown, "--adapter", adapter}
	run(unknownArgs, "invalid_arguments", 2, false)
	run(append(unknownArgs, "--mime-type", `audio/aac; note="$(false); 'literal' & |"`), "", 0, true)
	large := filepath.Join(parent, "oversized")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(app.MaxTranscriptionInputBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	run(append(append([]string{}, base...), "--file", large), "input_too_large", 1, false)
	if err := os.Truncate(large, app.MaxTranscriptionInputBytes); err != nil {
		t.Fatal(err)
	}
	maxDigest := sha256.Sum256(make([]byte, app.MaxTranscriptionInputBytes))
	maxHash := hex.EncodeToString(maxDigest[:])
	maxEnv := run(append(append([]string{}, base...), "--file", large, "--expect-sha256", maxHash, "--mime-type", "application/octet-stream"), "", 0, true)
	var maxDTO agentTranscriptionResult
	if err := json.Unmarshal(maxEnv.Data, &maxDTO); err != nil || maxDTO.Input.Bytes != app.MaxTranscriptionInputBytes || maxDTO.Input.SHA256 != maxHash || !strings.HasPrefix(maxDTO.Text, fmt.Sprintf("%d:%s:", app.MaxTranscriptionInputBytes, maxHash)) {
		t.Fatalf("limit did not deliver exact stdin: %s %v", maxEnv.Data, err)
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("missing archive initialized: %v", err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(parent, "linked")
		if err := os.Symlink(file, link); err != nil {
			t.Fatal(err)
		}
		run(append(append([]string{}, base...), "--file", link), "path_not_allowed", 1, false)
		linkedDir := filepath.Join(parent, "linked-dir")
		if err := os.Symlink(parent, linkedDir); err != nil {
			t.Fatal(err)
		}
		run(append(append([]string{}, base...), "--file", filepath.Join(linkedDir, filepath.Base(file))), "path_not_allowed", 1, false)
		// A configured symlink root is the intentional PR17 boundary exception.
		alias := filepath.Join(markerDir, "allowed-root")
		if err := os.Symlink(parent, alias); err != nil {
			t.Fatal(err)
		}
		t.Setenv(mediaRootsEnv, alias)
		aliasInput := filepath.Join(alias, filepath.Base(file))
		aliasStore := filepath.Join(alias, "missing", "archive")
		env := run(append(append([]string{}, base...), "--file", aliasInput, "--store", aliasStore), "", 0, true)
		var dto agentTranscriptionResult
		if err := json.Unmarshal(env.Data, &dto); err != nil || dto.Input.Path != aliasInput || dto.Input.SHA256 != digest {
			t.Fatalf("declared alias/bytes changed: %s %v", env.Data, err)
		}
		t.Setenv(mediaRootsEnv, parent)
	}
	// Existing archive control bytes are synthetic sentinels, never opened by
	// this command. A writer owner and its socket remain untouched.
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wacli.db", "session.db"} {
		if err := os.WriteFile(filepath.Join(storeDir, name), []byte("synthetic never-open sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var ipc atomic.Int64
	if runtime.GOOS != "windows" {
		stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			ipc.Add(1)
			return sendDelegateResponse{}, fmt.Errorf("transcription cannot use IPC")
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
	}
	before := snapshotTranscriptionStore(t, storeDir)
	run(base, "", 0, true)
	run(append(append([]string{}, base...), "--file", filepath.Join(storeDir, "session.db")), "path_not_allowed", 1, false)
	if runtime.GOOS != "windows" {
		alias := filepath.Join(parent, "control-hardlink")
		if err := os.Link(filepath.Join(storeDir, "session.db"), alias); err != nil {
			t.Fatal(err)
		}
		run(append(append([]string{}, base...), "--file", alias), "path_not_allowed", 1, false)
	}
	if !reflect.DeepEqual(before, snapshotTranscriptionStore(t, storeDir)) || ipc.Load() != 0 {
		t.Fatal("archive/session/LOCK/owner mutated or IPC used")
	}
	if runtime.GOOS == "linux" {
		// Account selection reads only this synthetic registry, no credentials.
		configDir := filepath.Join(parent, "config-state", "wacli")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := &config.AccountsConfig{Accounts: map[string]config.AccountEntry{"fixture": {Store: storeDir}}}
		if err := config.SaveAccountsConfig(filepath.Join(configDir, "config.yaml"), cfg); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"--account", "fixture"}, base[2:]...)
		env := run(args, "", 0, true)
		if env.Account.Name != "fixture" || env.Account.StoreRef == nil || *env.Account.StoreRef != storeDir {
			t.Fatal("account identity changed")
		}
		run(append([]string{"--account", "not-configured"}, base[2:]...), "store_unavailable", 4, false)
		if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("PRIVATE_CONFIG_SECRET: [invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(args, "store_unavailable", 4, false)
	}
}

func snapshotTranscriptionStore(t *testing.T, dir string) map[string]localFileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]localFileSnapshot, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		state := localFileSnapshot{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			state.Hash = sha256.Sum256(data)
		}
		files[entry.Name()] = state
	}
	return files
}

func TestAgentTranscriptionLiteralFlagsAndTerminator(t *testing.T) {
	for _, args := range [][]string{
		{"media", "transcribe", "--file", "--agent", "--bogus"},
		{"media", "transcribe", "--adapter", "--agent", "--bogus"},
		{"media", "transcribe", "--", "--agent", "--bogus"},
		{"media", "transcribe", "--agent=false"},
	} {
		stdout, stderr, err := runAgentTest(t, args...)
		if err == nil || stdout != "" || strings.Contains(stderr, "schema_version") {
			t.Fatalf("literal incorrectly enabled agent: %v %s %s", err, stdout, stderr)
		}
	}
	for _, args := range [][]string{
		{"--agent", "media", "transcribe", "--", "PRIVATE_POSITIONAL"},
		{"media", "transcribe", "--mime-type", "--agent", "--agent"},
	} {
		stdout, stderr, err := runAgentTest(t, args...)
		env := decodeAgentTest(t, stderr)
		if err == nil || stdout != "" || env.Meta.Source != "local" || strings.Contains(stderr, "PRIVATE_POSITIONAL") {
			t.Fatalf("unsafe parse: %v %s %s", err, stdout, stderr)
		}
	}
}

func TestAgentTranscriptionProductionBinarySignal(t *testing.T) {
	binary := os.Getenv("WACLI_TRANSCRIBE_E2E_BINARY")
	if binary == "" || runtime.GOOS == "windows" {
		t.Skip("requires a production fixture binary and Unix signals")
	}
	adapter := buildTranscriptionFixture(t)
	parent := t.TempDir()
	file, marker := filepath.Join(parent, "audio"), filepath.Join(parent, "started")
	if err := os.WriteFile(file, []byte("OggS\x00synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--store", filepath.Join(parent, "absent"), "media", "transcribe", "--agent", "--file", file, "--adapter", adapter, "--mime-type", "audio/ogg; mode=block")
	cmd.Env = append(os.Environ(), "WACLI_MEDIA_ROOTS="+parent, "WACLI_TRANSCRIBE_STUB_MARKER="+marker)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if ctx.Err() != nil {
			_ = cmd.Wait()
			t.Fatal("fixture adapter did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout.Len() != 0 || time.Since(start) > time.Second {
		t.Fatalf("signal cancellation: %v %s %s", err, stdout.String(), stderr.String())
	}
	env := decodeAgentTest(t, stderr.String())
	if env.Error.Code != "cancelled" || env.Meta.Source != "local" || strings.Contains(stderr.String(), adapter) {
		t.Fatalf("unsafe cancellation: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(parent, "absent")); !os.IsNotExist(err) {
		t.Fatal("signal test initialized archive")
	}
}

type transcriptionBrokenOutput struct{}

func (transcriptionBrokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestAgentTranscriptionOutputFailureAndContext(t *testing.T) {
	adapter := buildTranscriptionFixture(t)
	parent := t.TempDir()
	file := filepath.Join(parent, "audio")
	if err := os.WriteFile(file, []byte("OggS\x00synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{agent: true, agentCapability: agentMediaTranscription, detail: "compact", storeDir: filepath.Join(parent, "absent"), timeout: time.Second}
	t.Setenv(mediaRootsEnv, parent)
	opts := app.TranscriptionOptions{Adapter: adapter, MIMEType: "audio/ogg", Timeout: time.Second}
	err := runAgentTranscription(context.Background(), flags, file, opts, transcriptionBrokenOutput{})
	if err == nil || classifyTranscriptionCommandError(err).Code != "output_failed" {
		t.Fatalf("broken output silently accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runAgentTranscription(ctx, flags, file, opts, io.Discard)
	if err == nil || classifyTranscriptionCommandError(err).Code != "cancelled" {
		t.Fatalf("cancel ignored: %v", err)
	}
}
