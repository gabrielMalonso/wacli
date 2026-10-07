package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Exercise successive client lifetimes against one real archive. The fake
// supplies SDK events, not a simulated inventory of what the server owes us.
func TestSyncContinuousReconnectRestartPreservesArchive(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	f := newFakeWA()
	a.wa = f
	chat := types.NewJID("15550000002", types.DefaultUserServer)
	lid := types.NewJID("20002", types.HiddenUserServer)
	self, err := types.ParseJID(f.LinkedJID())
	if err != nil {
		t.Fatal(err)
	}
	f.lids[lid] = chat
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Use the pinned SDK's unwrap: live callbacks have an unwrapped Message
	// and DeviceSentMeta, while RawMessage retains the device envelope.
	own := (&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: self, Sender: self, IsFromMe: true},
			ID:            "other-device", Timestamp: base,
		},
		RawMessage: &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{
			DestinationJID: proto.String(lid.String()),
			Message:        &waE2E.Message{Conversation: proto.String("from another device")},
		}},
	}).UnwrapRaw()
	originals := historySyncWithTextMessages(chat, base, "edited", "deleted")
	f.connectEvents = []any{own, originals}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := a.Sync(ctx, SyncOptions{
		Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet, MaxReconnect: time.Second,
		AfterConnect: func(context.Context) error {
			f.emit(&events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: chat, Sender: chat},
					ID:            "edit-event", Timestamp: base.Add(time.Minute),
				},
				Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
					Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
					Key:           &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String("edited"), FromMe: proto.Bool(false)},
					EditedMessage: &waE2E.Message{Conversation: proto.String("edited content")},
				}},
			})
			f.emit(&events.DeleteForMe{ChatJID: chat, MessageID: "deleted", Timestamp: base.Add(time.Minute)})
			// Installed last so cancellation follows all persistence callbacks.
			f.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.OfflineSyncCompleted); ok {
					cancel()
				}
			})
			f.mu.Lock()
			f.connectEvents = []any{own, originals, &events.OfflineSyncCompleted{Count: 3}}
			f.mu.Unlock()
			f.emit(&events.StreamReplaced{})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := f.connectCalls
	f.mu.Unlock()
	if calls != 2 || result.MessagesStored != 7 {
		t.Fatalf("connects=%d stored attempts=%d, want 2/7", calls, result.MessagesStored)
	}
	if err := a.db.PurgeMessage(chat.String(), "deleted"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if obs := result.ObservationsSnapshot(); obs.Sync == nil || obs.Sync.StopReason != "cancelled" || obs.Sync.CleanupAt == nil || obs.Sync.PersistenceUnconfirmed {
		t.Fatalf("cancelled/drained execution = %+v", obs)
	}

	b, err := New(Options{StoreDir: a.StoreDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.opts.Events = out.NewEventWriter(io.Discard, true)
	restarted := newFakeWA()
	restarted.lids[lid] = chat
	restarted.connectEvents = []any{own, originals, &events.OfflineSyncCompleted{Count: 3}}
	b.wa = restarted
	res, err := b.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond, PresenceMode: SyncPresenceModeQuiet})
	if err != nil || res.MessagesStored != 3 {
		t.Fatalf("restart sync = %+v, %v", res, err)
	}
	b.Close()
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if n, err := ro.CountMessages(); err != nil || n != 3 {
		t.Fatalf("distinct rows=%d, %v; want 3", n, err)
	}
	got, err := ro.GetMessage(chat.String(), "other-device")
	if err != nil || !got.FromMe || got.SenderJID != self.String() || got.Text != "from another device" {
		t.Fatalf("own device message = %+v, %v", got, err)
	}
	for _, wrongChat := range []string{self.String(), lid.String()} {
		if _, err := ro.GetMessage(wrongChat, "other-device"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("own device leaked to %s: %v", wrongChat, err)
		}
	}
	got, err = ro.GetMessage(chat.String(), "edited")
	if err != nil || !got.Edited || got.Text != "edited content" {
		t.Fatalf("older replay replaced edit = %+v, %v", got, err)
	}
	got, err = ro.GetMessage(chat.String(), "deleted")
	if err != nil || !got.DeletedForMe || got.PayloadPurgedAt == nil || got.Text != "" {
		t.Fatalf("older replay resurrected purged tombstone = %+v, %v", got, err)
	}
	listed, err := ro.ListMessages(store.ListMessagesParams{ChatJID: chat.String(), Limit: 10})
	if err != nil || len(listed) != 2 {
		t.Fatalf("visible messages=%d, %v; want 2", len(listed), err)
	}
}

func TestSyncEmptyOfflineReplayDoesNotInventCoverage(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	var output bytes.Buffer
	a.opts.Events = out.NewEventWriter(&output, true)
	f.connectEvents = []any{
		&events.OfflineSyncPreview{Total: 4, Messages: 0, Notifications: 4},
		&events.OfflineSyncCompleted{Count: 4},
	}
	result, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond, PresenceMode: SyncPresenceModeQuiet})
	if err != nil || result.MessagesStored != 0 {
		t.Fatalf("empty backlog = %+v, %v", result, err)
	}
	a.Close()
	obs := result.ObservationsSnapshot()
	if obs.Sync == nil || obs.Sync.StopReason != "idle" || obs.Sync.OfflineCompletedAt == nil || obs.Sync.OfflineCompletedCount == nil || *obs.Sync.OfflineCompletedCount != 4 || len(obs.Sync.RecoveryObservations) != 0 {
		t.Fatalf("empty replay observations = %+v", obs)
	}
	if findEventByName(t, output.String(), "offline_sync_preview")["data"].(map[string]any)["messages"] != float64(0) {
		t.Fatal("preview invented messages")
	}
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat := types.NewJID("15550000002", types.DefaultUserServer)
	if _, err := ro.GetMessage(chat.String(), "not-announced"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unobserved message = %v", err)
	}
	if n, err := ro.CountMessages(); err != nil || n != 0 {
		t.Fatalf("empty replay rows=%d, %v", n, err)
	}
	if required, err := ro.AppStateRecoveryRequired("regular_low"); err != nil || !required {
		t.Fatalf("preventive shutdown debt=%t, %v", required, err)
	}
	ro.Close()

	// A separately supplied later history event can add the absent ID. This
	// does not assert that WhatsApp will send it, or explain the earlier gap.
	b, err := New(Options{StoreDir: a.StoreDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.opts.Events = out.NewEventWriter(io.Discard, true)
	later := newFakeWA()
	history := historySyncWithTextMessages(chat, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), "not-announced")
	history.Data.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(true)
	later.connectEvents = []any{history}
	b.wa = later
	res, err := b.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
	if err != nil || res.MessagesStored != 1 {
		t.Fatalf("later admitted history = %+v, %v", res, err)
	}
	msg, err := b.db.GetMessage(chat.String(), "not-announced")
	if err != nil || !msg.FromMe || msg.SenderJID != later.LinkedJID() {
		t.Fatalf("later own message = %+v, %v", msg, err)
	}
}
