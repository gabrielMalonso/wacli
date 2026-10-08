package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type historyAnchorOwnerWA struct {
	syncStatusWA
	requests atomic.Int64
	request  func(types.MessageInfo, int)
}

func (f *historyAnchorOwnerWA) LinkedLID() string                                         { return "" }
func (f *historyAnchorOwnerWA) ResolvePNToLID(_ context.Context, jid types.JID) types.JID { return jid }
func (f *historyAnchorOwnerWA) CheckPublicPair(context.Context, types.JID, types.JID) (wa.PublicPairResult, error) {
	return wa.PublicPairUnverified, nil
}
func (f *historyAnchorOwnerWA) ResolveChatName(context.Context, types.JID, string) string {
	return "fixture"
}
func (f *historyAnchorOwnerWA) RequestHistorySyncOnDemand(_ context.Context, info types.MessageInfo, count int) (types.MessageID, error) {
	f.requests.Add(1)
	f.request(info, count)
	return "fixture-request", nil
}

// Real DB, LOCK, follow runtime, serialized IPC executor and CLI client. Only
// the WA boundary is fake; the client must not open a second connection.
func TestHistoryExplicitAnchorRealOwnerIPC(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	f := &historyAnchorOwnerWA{syncStatusWA: syncStatusWA{shutdownStarted: make(chan struct{}), shutdownRelease: make(chan struct{})}}
	close(f.shutdownRelease)
	dir, a := draftOwnerFixtureOptions(t, app.Options{Events: out.NewEventWriter(io.Discard, true), WAFactory: func(wa.Options) (app.WAClient, error) { f.opens.Add(1); return f, nil }})
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	chat := "123@g.us"
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := a.DB().UpsertChat(chat, "group", "fixture", base); err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.UpsertMessageParams{
		{ChatJID: chat, MsgID: "oldest", SenderJID: localReadPN, Timestamp: base.Add(-24 * time.Hour), Text: "old"},
		{ChatJID: chat, MsgID: "selected", SenderJID: localReadPN, Timestamp: base, Text: "anchor"},
	} {
		if err := a.DB().UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	f.request = func(info types.MessageInfo, count int) {
		if info.Chat.String() != chat || info.ID != "selected" || !info.Timestamp.Equal(base) || info.Sender.String() != localReadPN || info.IsFromMe || count != 50 {
			t.Errorf("request=%+v count=%d", info, count)
		}
		if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "gap", SenderJID: localReadPN, Timestamp: base.Add(-time.Minute), Text: "recovered fixture"}); err != nil {
			t.Error(err)
		}
		f.emit(&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{{}}}}}})
	}
	ownerCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.Sync(ownerCtx, app.SyncOptions{Mode: app.SyncModeFollow, PresenceMode: app.SyncPresenceModeQuiet, AfterConnect: func(context.Context) error { close(ready); return nil }})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("owner failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("owner timeout")
	}
	stop, err := startSyncDelegateServer(ownerCtx, a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	args := []string{"--store", dir, "--agent", "--timeout", "2s", "history", "backfill", "--chat", chat, "--before-id", "selected", "--wait", "100ms", "--idle-exit", "1ms"}
	stdout, stderr, err := runDraftBinary(t, os.Getenv("WACLI_HISTORY_E2E_BINARY"), args, false)
	if err != nil || !strings.Contains(stdout, `"before_id":"selected"`) || !strings.Contains(stdout, `"messages_added_before":1`) || !strings.Contains(stdout, `"stop_reason":"requested_batch_limit"`) {
		t.Fatalf("result=%s stderr=%s err=%v", stdout, stderr, err)
	}
	if f.requests.Load() != 1 || f.opens.Load() != 1 || f.connects.Load() != 1 {
		t.Fatalf("lifetimes requests=%d opens=%d connects=%d", f.requests.Load(), f.opens.Load(), f.connects.Load())
	}
	// Flag and environment guards execute before the socket, even with a live owner.
	for _, envReadOnly := range []bool{false, true} {
		guarded := append([]string{"--read-only"}, args...)
		if envReadOnly {
			t.Setenv("WACLI_READONLY", "1")
			guarded = args
		}
		_, _, err := runDraftBinary(t, os.Getenv("WACLI_HISTORY_E2E_BINARY"), guarded, envReadOnly)
		t.Setenv("WACLI_READONLY", "0")
		if err == nil || f.requests.Load() != 1 {
			t.Fatalf("readonly contacted owner: %v calls=%d", err, f.requests.Load())
		}
	}
	oldest, err := a.DB().GetOldestMessageInfo(chat)
	if err != nil || oldest.MsgID != "oldest" {
		t.Fatalf("oldest=%+v err=%v", oldest, err)
	}
	// Owner resolves the ID. The client has no DB read/preflight or extra lock.
	for i := range args {
		if args[i] == "--before-id" {
			args[i+1] = "missing"
			break
		}
	}
	_, _, err = runDraftBinary(t, os.Getenv("WACLI_HISTORY_E2E_BINARY"), args, false)
	if err == nil || f.requests.Load() != 1 {
		t.Fatalf("missing dispatched: %v calls=%d", err, f.requests.Load())
	}
}

func TestHistoryExplicitAnchorOlderDecoderRejectsWithoutOldestFallback(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			dir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			listener, err := net.Listen("unix", sendDelegateSocketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			calls := make(chan int, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				// Shape and permissive decoder of the pre-feature owner: before_id is ignored.
				var req struct {
					Version  int    `json:"version"`
					Kind     string `json:"kind"`
					Backfill *struct {
						ChatJID string `json:"chat_jid"`
					} `json:"backfill"`
				}
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					calls <- -1
					return
				}
				dispatches := 0
				switch req.Kind {
				case historyBackfillKind:
					dispatches++
				default:
					if !lost {
						_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: fmt.Sprintf("unsupported send kind %q", req.Kind)})
					}
				}
				calls <- dispatches
			}()
			_, _, err = runPresenceDelegateHelper(t, []string{"--store", dir, "--timeout", "1s", "history", "backfill", "--chat", "123@g.us", "--before-id", "selected"})
			if err == nil || <-calls != 0 {
				t.Fatalf("older owner used default anchor: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "wacli.db")); !os.IsNotExist(err) {
				t.Fatalf("fallback opened database: %v", err)
			}
		})
	}
}

func TestHistoryExplicitAnchorKindAndGuards(t *testing.T) {
	for _, change := range []func(*sendDelegateRequest){
		func(r *sendDelegateRequest) { r.Backfill.BeforeID = "selected" }, // legacy kind carrying new field
		func(r *sendDelegateRequest) { r.Kind = historyBeforeKind },       // explicit kind without anchor
		func(r *sendDelegateRequest) {
			r.Kind = historyBeforeKind
			r.Backfill.BeforeID = "selected"
			r.Backfill.Requests = 2
		},
	} {
		server, client := net.Pipe()
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(time.Second))
		req := fixtureBackfillRequest("123@g.us")
		change(&req)
		go handleSendDelegateConn(t.Context(), server, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			t.Error("invalid explicit request executed")
			return sendDelegateResponse{}, nil
		}, make(chan struct{}), nil)
		if err := json.NewEncoder(client).Encode(req); err != nil {
			t.Fatal(err)
		}
		var res sendDelegateResponse
		if err := json.NewDecoder(client).Decode(&res); err != nil {
			t.Fatal(err)
		}
		if res.OK || res.HistoryFailure == nil || res.HistoryFailure.Outcome != "not_dispatched" {
			t.Fatalf("refusal=%+v", res)
		}
	}
	for _, args := range [][]string{
		{"--read-only", "history", "backfill", "--chat", "123@g.us", "--before-id", "selected"},
		{"--agent", "history", "backfill", "--chat", "123@g.us", "--before-id", ""},
		{"history", "backfill", "--chat", "123@g.us", "--before-id", "selected", "--requests", "2"},
		{"history", "backfill", "--chat", "123@g.us", "--before-id", "bad id"},
	} {
		dir := shortPresenceDelegateStoreDir(t)
		lk, err := lock.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = runPresenceDelegateHelper(t, append([]string{"--store", dir}, args...))
		lk.Release()
		if err == nil {
			t.Fatalf("invalid arguments accepted %v", args)
		}
	}
}
