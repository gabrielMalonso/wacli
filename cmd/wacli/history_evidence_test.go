package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func seedHistoryEvidence(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	chat := "300@lid"
	attempt := store.HistoryAttempt{RequestedChatJID: chat, AttemptID: "0123456789abcdef0123456789abcdef", StartedAt: now, CheckpointAt: now,
		State: store.HistoryUnfinalized, Phase: store.HistoryPreparing, ExecutionMode: "standalone", Count: 50, Requests: 1, WaitMS: 1000, IdleMS: 1}
	if err = db.BeginHistoryAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	return dir, chat
}
func TestHistoryCoverageEvidenceWithoutChatAndLegacyUnchanged(t *testing.T) {
	dir, chat := seedHistoryEvidence(t)
	for _, agent := range []bool{false, true} {
		for _, flag := range []bool{false, true} {
			args := []string{"--store", dir, "history", "coverage", "--chat", chat}
			if agent {
				args = append(args, "--agent")
			} else {
				args = append(args, "--json")
			}
			if flag {
				args = append(args, "--evidence")
			}
			stdout, stderr, err := runAgentTest(t, args...)
			if err != nil || stderr != "" {
				t.Fatalf("query: %v %s", err, stderr)
			}
			var envelope struct {
				Data struct {
					Coverage         []json.RawMessage    `json:"coverage"`
					RecoveryEvidence []historyRecoveryDTO `json:"recovery_evidence"`
				} `json:"data"`
			}
			if err = json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Data.Coverage) != 0 {
				t.Fatal("invented chat row")
			}
			if flag {
				records := envelope.Data.RecoveryEvidence
				if len(records) != 1 || records[0].Latest == nil || records[0].Latest.State != store.HistoryUnfinalized || records[0].LastSuccess != nil || records[0].Latest.RequestsSent != nil || records[0].Latest.Full != nil {
					t.Fatalf("evidence: %s", stdout)
				}
			} else if strings.Contains(stdout, "recovery_evidence") {
				t.Fatalf("legacy changed: %s", stdout)
			}
		}
	}
	cmd := newHistoryCoverageCmd(&rootFlags{storeDir: dir})
	cmd.SetArgs([]string{"--chat", chat, "--evidence"})
	raw := captureRootStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(raw, chat) || !strings.Contains(raw, "unfinalized") || !strings.Contains(raw, "no retained record") {
		t.Fatalf("human evidence hidden: %s", raw)
	}
}
func TestHistoryCoverageEvidenceInputsAndDetails(t *testing.T) {
	dir, chat := seedHistoryEvidence(t)
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "history", "coverage", "--chat", chat+", "+chat+",400@g.us", "--evidence", "--detail", "full")
	if err != nil || stderr != "" {
		t.Fatalf("full: %v %s", err, stderr)
	}
	var envelope struct {
		Data struct {
			RecoveryEvidence []historyRecoveryDTO `json:"recovery_evidence"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	records := envelope.Data.RecoveryEvidence
	if len(records) != 2 || records[0].Latest.Full == nil || records[1].Latest != nil || records[1].LastSuccess != nil {
		t.Fatalf("dedup/absent/full: %s", stdout)
	}
	stdout, _, err = runAgentTest(t, "--store", dir, "--agent", "history", "coverage", "--evidence")
	if err != nil || !strings.Contains(stdout, `"recovery_evidence":[]`) {
		t.Fatalf("default scanned catalog: %s %v", stdout, err)
	}
	for _, args := range [][]string{{"--chat", "bad"}, {"--limit", "201"}, {"--chat", strings.Repeat("300@lid,", 200) + "300@lid"}} {
		stdout, stderr, err = runAgentTest(t, append([]string{"--store", dir, "--agent", "history", "coverage", "--evidence"}, args...)...)
		if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatalf("bounds: %v %s %s", err, stdout, stderr)
		}
	}
}

func TestHistoryEvidencePayloadBounds(t *testing.T) {
	dir, chat := seedHistoryEvidence(t)
	a, lk, err := newReadApp(context.Background(), &rootFlags{storeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer closeApp(a, lk)
	records, err := readHistoryEvidence(context.Background(), a, []string{chat}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]historyRecoveryDTO, 400)
	for i := range rows {
		rows[i] = records[0]
	}
	flags := &rootFlags{agent: true, detail: "compact"}
	stdout := captureRootStdout(t, func() {
		if err := writeHistoryAgentCoverageEvidence(flags, nil, rows, 200); err != nil {
			t.Fatal(err)
		}
	})
	if len(stdout) > 1<<20 {
		t.Fatalf("bounded keys exceeded compact quota: %d", len(stdout))
	}
	// Full detail retains anchors only when requested; the existing envelope
	// cap still rejects oversized retained fields before writing success bytes.
	fullRecords, fullErr := readHistoryEvidence(context.Background(), a, []string{chat}, nil, true)
	if fullErr != nil {
		t.Fatal(fullErr)
	}
	rows[0] = fullRecords[0]
	rows[0].Latest.Full.LastAnchorID = strings.Repeat("x", 9<<20)
	flags.detail = "full"
	stdout = captureRootStdout(t, func() { err = writeHistoryAgentCoverageEvidence(flags, nil, rows[:1], 1) })
	if stdout != "" || err == nil || classifyAgentError(err).Code != "payload_too_large" {
		t.Fatalf("partial oversized output: %d %v", len(stdout), err)
	}
}
