package app

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func appChanges(t *testing.T, a *App, cursor string) store.ChangesPage {
	t.Helper()
	p, err := a.db.ListChanges(t.Context(), a.StoreDir(), 200, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChangeFeedProductionLiveHistoryReplayEditAndDelete(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	chat := types.NewJID("123", types.DefaultUserServer)
	f.contacts[chat] = types.ContactInfo{Found: true, FullName: "Alice"}
	base := time.Unix(1700000000, 0).UTC()
	live := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "original", Timestamp: base, PushName: "Alice"}, Message: &waE2E.Message{Conversation: proto.String("original-body")}}
	var count, last atomic.Int64
	for range 2 {
		a.handleLiveSyncMessage(t.Context(), SyncOptions{}, live, &count, func(string, string) {}, nil)
	}
	first := appChanges(t, a, "")
	if len(first.Changes) != 1 || first.Changes[0].Kind != "message_insert" {
		t.Fatalf("live replay %+v", first)
	}
	key := func(id string) *waCommon.MessageKey {
		return &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(false), ID: proto.String(id)}
	}
	edit := &waWeb.WebMessageInfo{Key: key("edit-event"), MessageTimestamp: proto.Uint64(uint64(base.Add(time.Minute).Unix())), Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: key("original"), EditedMessage: &waE2E.Message{Conversation: proto.String("edited-body")}}}}
	original := &waWeb.WebMessageInfo{Key: key("original"), MessageTimestamp: proto.Uint64(uint64(base.Unix())), Message: live.Message}
	history := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_FULL.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: edit}, {Message: original}}}}}}
	for range 2 {
		a.handleHistorySync(t.Context(), SyncOptions{}, history, &count, &last, func(string, string) {})
	}
	changed := appChanges(t, a, first.NextCursor)
	if len(changed.Changes) != 1 || changed.Changes[0].Kind != "message_update" || changed.Changes[0].ID != "original" {
		t.Fatalf("history/edit replay %+v", changed)
	}
	stored, err := a.db.GetMessage(chat.String(), "original")
	if err != nil || stored.Text != "edited-body" {
		t.Fatalf("stored %+v %v", stored, err)
	}
	if err = a.handleDeleteForMeEvent(t.Context(), &events.DeleteForMe{ChatJID: chat, MessageID: "original", Timestamp: base.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	deleted := appChanges(t, a, changed.NextCursor)
	if len(deleted.Changes) != 1 || deleted.Changes[0].Kind != "message_tombstone" {
		t.Fatalf("delete %+v", deleted)
	}
}

func TestChangeFeedClientLifetimeReceiptsAndCloseGate(t *testing.T) {
	a, f, _, _ := outboundEventFixture(t, false, false)
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: types.NewJID("400", types.HiddenUserServer), Sender: types.NewADJID("300", 0, 7), SenderAlt: types.NewJID("123", types.DefaultUserServer), RecipientAlt: types.NewJID("400", types.DefaultUserServer)}, Type: types.ReceiptTypeRead, Timestamp: time.Unix(1700000000, 0), MessageIDs: []string{"unrelated-message"}}
	// Real client-lifetime callback, independent of whether an outbound operation matches.
	f.emit(receipt)
	f.emit(receipt)
	page := appChanges(t, a, "")
	if len(page.Changes) != 1 || page.Changes[0].Kind != "receipt" || page.Changes[0].Receipt.ActorDevice != 7 || page.Changes[0].Receipt.SenderAlt != receipt.SenderAlt.String() {
		t.Fatalf("callback %+v", page)
	}
	observer := a.outboundEvents
	observer.closeAdmissions()
	late := *receipt
	late.MessageIDs = []string{"late"}
	observer.event(a.sessionState, &late)
	if p := appChanges(t, a, page.NextCursor); len(p.Changes) != 0 {
		t.Fatalf("late write %+v", p)
	}
}

func TestChangeFeedReceiptLimitsCancellationAndSanitizedWarning(t *testing.T) {
	a := newTestApp(t)
	var log bytes.Buffer
	a.opts.Events = out.NewEventWriter(&log, true)
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: types.NewJID("400", types.HiddenUserServer), Sender: types.NewJID("300", types.HiddenUserServer)}, Type: types.ReceiptTypePlayed, Timestamp: time.Unix(1700000000, 0), MessageIDs: ids}
	a.recordReceiptChange(t.Context(), receipt)
	first := appChanges(t, a, "")
	if len(first.Changes) != 200 || !strings.Contains(log.String(), "change_receipt_incomplete") || !strings.Contains(log.String(), `"skipped":1`) {
		t.Fatalf("limit %d %s", len(first.Changes), log.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt.MessageIDs = []string{"PRIVATE-MESSAGE-ID"}
	a.recordReceiptChange(ctx, receipt)
	if p := appChanges(t, a, first.NextCursor); len(p.Changes) != 0 {
		t.Fatalf("cancelled %+v", p)
	}
	if strings.Contains(log.String(), "PRIVATE-MESSAGE-ID") || strings.Contains(log.String(), "context canceled") || !strings.Contains(log.String(), `"persistence_failed":true`) {
		t.Fatalf("unsafe warning: %s", log.String())
	}
	// Control/error receipts remain outside the public observation vocabulary.
	for _, kind := range []types.ReceiptType{types.ReceiptTypeRetry, types.ReceiptTypeServerError, types.ReceiptTypeInactive, types.ReceiptTypePeerMsg, types.ReceiptTypeHistorySync, "PRIVATE-UNKNOWN"} {
		receipt.Type = kind
		a.recordReceiptChange(t.Context(), receipt)
	}
	if p := appChanges(t, a, first.NextCursor); len(p.Changes) != 0 {
		t.Fatalf("control receipts %+v", p)
	}
}
