package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestHistorySyncMetadataPreservesAbsentOptionalValues(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	var output bytes.Buffer
	a.opts.Events = out.NewEventWriter(&output, true)
	var stored, lastEvent atomic.Int64
	a.handleHistorySync(t.Context(), SyncOptions{}, &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}}, &stored, &lastEvent, func(string, string) {})
	var evt struct {
		Event string                     `json:"event"`
		Data  map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(&output).Decode(&evt); err != nil {
		t.Fatal(err)
	}
	if evt.Event != "history_sync" || string(evt.Data["sync_type"]) != `"RECENT"` || string(evt.Data["conversations"]) != "0" || string(evt.Data["chunk_order"]) != "null" || string(evt.Data["progress"]) != "null" {
		t.Fatalf("missing optional metadata was invented: %s", output.Bytes())
	}
}

func TestSyncEventsOutputStaysNDJSONDuringProgress(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	chat := types.JID{User: "123", Server: types.DefaultUserServer}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = "m" + string(rune('a'+i))
	}
	history := historySyncWithTextMessages(chat, base, ids...)
	history.Data.SyncType = waHistorySync.HistorySync_RECENT.Enum()
	history.Data.ChunkOrder = proto.Uint32(2)
	history.Data.Progress = proto.Uint32(100)
	f.connectEvents = []any{history}

	raw := captureStderr(t, func() {
		a.opts.Events = out.NewEventWriter(os.Stderr, true)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := a.Sync(ctx, SyncOptions{
			Mode:         SyncModeOnce,
			AllowQR:      false,
			IdleExit:     50 * time.Millisecond,
			WarnNoLimits: false,
		})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})

	if strings.Contains(raw, "Synced ") || strings.Contains(raw, "Processing history sync") {
		t.Fatalf("human progress leaked into --events stderr:\n%s", raw)
	}

	var sawProgress, sawHistory bool
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("stderr line is not valid JSON %q: %v\nfull stderr:\n%s", line, err, raw)
		}
		if evt["event"] == "progress" {
			data, ok := evt["data"].(map[string]any)
			if !ok || data["messages_synced"] != float64(25) {
				t.Fatalf("unexpected progress event: %#v", evt)
			}
			sawProgress = true
		}
		if evt["event"] == "history_sync" {
			data, ok := evt["data"].(map[string]any)
			if !ok || data["conversations"] != float64(1) || data["sync_type"] != "RECENT" || data["chunk_order"] != float64(2) || data["progress"] != float64(100) {
				t.Fatalf("unexpected history metadata: %#v", evt)
			}
			sawHistory = true
		}
	}
	if !sawProgress {
		t.Fatalf("expected progress event in:\n%s", raw)
	}
	if !sawHistory {
		t.Fatalf("expected history metadata event in:\n%s", raw)
	}
}

func TestSyncTTYProgressUsesSingleStatusLine(t *testing.T) {
	oldTerminal := syncStatusTerminal
	syncStatusTerminal = func() bool { return true }
	t.Cleanup(func() { syncStatusTerminal = oldTerminal })

	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	chat := types.JID{User: "123", Server: types.DefaultUserServer}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := make([]string, 30)
	for i := range ids {
		ids[i] = "m" + string(rune('a'+i))
	}
	f.connectEvents = []any{historySyncWithTextMessages(chat, base, ids...)}

	raw := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := a.Sync(ctx, SyncOptions{
			Mode:         SyncModeOnce,
			AllowQR:      false,
			IdleExit:     time.Millisecond,
			WarnNoLimits: false,
		})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})

	if strings.Contains(raw, "\nProcessing history sync") || strings.Contains(raw, "\nSynced 25 messages") {
		t.Fatalf("TTY progress should update one status line, got:\n%q", raw)
	}
	if !strings.Contains(raw, "\rConnected. Waiting for history sync...") {
		t.Fatalf("missing connected status in:\n%q", raw)
	}
	if !strings.Contains(raw, "\rSyncing history: 1 conversations, 25 messages stored") {
		t.Fatalf("missing history progress status in:\n%q", raw)
	}
	if !strings.Contains(raw, "\rSyncing history: 1 conversations, 30 messages stored") {
		t.Fatalf("missing final history status in:\n%q", raw)
	}
}

func TestSyncTTYWarningBreaksThroughStatusLine(t *testing.T) {
	oldTerminal := syncStatusTerminal
	syncStatusTerminal = func() bool { return true }
	t.Cleanup(func() { syncStatusTerminal = oldTerminal })

	a := newTestApp(t)
	f := newFakeWA()
	f.appStateFetchErr = errors.New("not connected")
	a.wa = f

	raw := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := a.Sync(ctx, SyncOptions{
			Mode:         SyncModeOnce,
			AllowQR:      false,
			IdleExit:     time.Millisecond,
			WarnNoLimits: false,
		})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})

	if !strings.Contains(raw, "warning: failed to sync WhatsApp app state regular_high: not connected\n") {
		t.Fatalf("missing regular_high warning in:\n%q", raw)
	}
	if !strings.Contains(raw, "warning: failed to sync WhatsApp app state regular_low: not connected\n") {
		t.Fatalf("missing regular_low warning in:\n%q", raw)
	}
	if strings.Contains(raw, "\nConnected.\n") {
		t.Fatalf("connected status should not become a separate noisy line:\n%q", raw)
	}
}

func TestSyncRefreshFailuresWarnAndContinue(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.getAllContactsErr = errors.New("contacts offline")
	f.getJoinedGroupsErr = errors.New("groups offline")
	f.getSubscribedNewslettersErr = errors.New("channels offline")
	a.wa = f

	raw := captureStderr(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := a.Sync(ctx, SyncOptions{
			Mode:            SyncModeOnce,
			AllowQR:         false,
			IdleExit:        time.Millisecond,
			RefreshContacts: true,
			RefreshGroups:   true,
			RefreshChannels: true,
		})
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})

	for _, want := range []string{
		"warning: failed to refresh contacts: contacts offline\n",
		"warning: failed to refresh groups: groups offline\n",
		"warning: failed to refresh channels: channels offline\n",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing warning %q in:\n%q", want, raw)
		}
	}
}
