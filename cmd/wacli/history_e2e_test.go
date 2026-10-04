package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
)

// Opt-in only with a freshly built local binary and fixture-owned private socket.
// The executable never receives a real account/store or an actual WA executor.
func TestHistoryEvidenceProductionBinaryOffline(t *testing.T) {
	binary := os.Getenv("WACLI_HISTORY_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_HISTORY_E2E_BINARY to a freshly built local binary")
	}
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		if app.ValidateHistoryAttemptID(req.Backfill.AttemptID) != nil {
			t.Error("invalid attempt ID reached fixture")
		}
		return sendDelegateResponse{OK: true, Backfill: &app.BackfillResult{AttemptID: req.Backfill.AttemptID, ChatJID: req.Backfill.ChatJID, RequestsSent: 1, ResponsesSeen: 1, StopReason: app.BackfillStopEmptyResponse}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	run := func(args []string, readOnlyEnv bool) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"--store", dir, "--timeout", "1s"}, args...)...)
		cmd.Env = os.Environ()
		if readOnlyEnv {
			cmd.Env = append(cmd.Env, "WACLI_READONLY=1")
		} else {
			cmd.Env = append(cmd.Env, "WACLI_READONLY=0")
		}
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	for _, tc := range []struct {
		args []string
		env  bool
	}{
		{[]string{"--read-only", "history", "backfill", "--chat", "123@g.us"}, false},
		{[]string{"history", "backfill", "--chat", "123@g.us"}, true},
		{[]string{"--agent", "history", "backfill", "--chat", "123@g.us"}, false}, // Still unsupported until the shared capability integration.
		{[]string{"history", "backfill", "--chat", "123@g.us", "--count", "501"}, false},
	} {
		stdout, _, err := run(tc.args, tc.env)
		if err == nil || stdout != "" || calls.Load() != 0 {
			t.Fatalf("guard allowed effects: %v %q calls=%d", err, stdout, calls.Load())
		}
	}
	stdout, stderr, err := run([]string{"history", "backfill", "--chat", "123@g.us", "--json"}, false)
	if err != nil || calls.Load() != 1 {
		t.Fatalf("fixture owner: %v %s", err, stderr)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Chat       string `json:"chat"`
			StopReason string `json:"stop_reason"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(stdout), &envelope); err != nil || !envelope.Success || envelope.Data.Chat != "123@g.us" || envelope.Data.StopReason != "empty_response" {
		t.Fatalf("production output: %s %v", stdout, err)
	}
	for _, name := range []string{"wacli.db", "session.db"} {
		if _, err = os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("client opened second writer/session: %s %v", name, err)
		}
	}
	fixtureDir, chat := seedHistoryEvidence(t)
	cmd := exec.Command(binary, "--store", fixtureDir, "--agent", "history", "coverage", "--chat", chat, "--evidence")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"recovery_evidence"`) || !strings.Contains(string(output), `"latest"`) {
		t.Fatalf("production offline evidence: %s %v", output, err)
	}
}
