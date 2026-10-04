package main

import (
	"bytes"
	"encoding/json"
	"github.com/openclaw/wacli/internal/app"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestHistoryCoverageCommandListsReadyAndBlockedChats(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(storeDir + "/wacli.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := db.UpsertChat("ready@s.whatsapp.net", "dm", "Ready", base); err != nil {
		t.Fatalf("UpsertChat ready: %v", err)
	}
	if err := db.UpsertChat("blocked@s.whatsapp.net", "dm", "Blocked", base); err != nil {
		t.Fatalf("UpsertChat blocked: %v", err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:   "ready@s.whatsapp.net",
		MsgID:     "m1",
		Timestamp: base,
		Text:      "hello",
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	_ = db.Close()

	cmd := newHistoryCoverageCmd(&rootFlags{storeDir: storeDir, timeout: time.Minute})
	cmd.SetArgs([]string{"--include-blocked"})
	raw := captureRootStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(raw, "Ready") || !strings.Contains(raw, "Blocked") || !strings.Contains(raw, "no_local_anchor") {
		t.Fatalf("coverage output missing expected rows: %q", raw)
	}
}

func TestHistoryFillRequiresDryRun(t *testing.T) {
	cmd := newHistoryFillCmd(&rootFlags{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("expected --dry-run error, got %v", err)
	}
}

func TestHistoryFillDryRunSelectsReadyChats(t *testing.T) {
	storeDir := t.TempDir()
	db, err := store.Open(storeDir + "/wacli.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	base := time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)
	if err := db.UpsertChat("ready@s.whatsapp.net", "dm", "Ready", base); err != nil {
		t.Fatalf("UpsertChat ready: %v", err)
	}
	if err := db.UpsertChat("blocked@s.whatsapp.net", "dm", "Blocked", base); err != nil {
		t.Fatalf("UpsertChat blocked: %v", err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{
		ChatJID:   "ready@s.whatsapp.net",
		MsgID:     "m1",
		Timestamp: base,
		Text:      "hello",
	}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	_ = db.Close()

	cmd := newHistoryFillCmd(&rootFlags{storeDir: storeDir, timeout: time.Minute})
	cmd.SetArgs([]string{"--dry-run"})
	raw := captureRootStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(raw, "Selected 1 chats") || !strings.Contains(raw, "yes") || !strings.Contains(raw, "no") {
		t.Fatalf("dry-run output missing selection markers: %q", raw)
	}
}

func TestBackfillResultOutputPreservesScopeAndStopEvidence(t *testing.T) {
	res := app.BackfillResult{
		ChatJID: "123@g.us", RequestsSent: 2, ResponsesSeen: 1,
		MessagesAdded: 0, MessagesSynced: 7, StopReason: app.BackfillStopPrimaryNoMore,
	}
	var dst bytes.Buffer
	if err := writeBackfillResult(&dst, res, true); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Chat           string                 `json:"chat"`
			RequestsSent   int                    `json:"requests_sent"`
			ResponsesSeen  int                    `json:"responses_seen"`
			MessagesAdded  int64                  `json:"messages_added"`
			MessagesSynced int64                  `json:"messages_synced"`
			StopReason     app.BackfillStopReason `json:"stop_reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(dst.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Success || envelope.Data.Chat != res.ChatJID || envelope.Data.RequestsSent != 2 || envelope.Data.ResponsesSeen != 1 || envelope.Data.MessagesAdded != 0 || envelope.Data.MessagesSynced != 7 || envelope.Data.StopReason != res.StopReason {
		t.Fatalf("JSON result = %s", &dst)
	}
	dst.Reset()
	if err := writeBackfillResult(&dst, res, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dst.String(), "primary_no_more_messages") || !strings.Contains(dst.String(), "Local conversation grew by 0 messages (2 requests)") || strings.Contains(dst.String(), "complete") {
		t.Fatalf("human result = %s", &dst)
	}
}
