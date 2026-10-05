package wa

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Only transport is synthetic: ID generation and handler dispatch/removal use
// the pinned SDK. No socket, connection, encryption or account is exercised.
type recoveryTransport struct {
	*whatsmeow.Client
	send     func(context.Context, types.JID, *waE2E.Message, whatsmeow.SendRequestExtra) error
	mu       sync.Mutex
	callback whatsmeow.EventHandler
}

func (s *recoveryTransport) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	s.mu.Lock()
	s.callback = h
	s.mu.Unlock()
	return s.Client.AddEventHandler(h)
}
func (s *recoveryTransport) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	if len(extra) != 1 || !extra[0].Peer || extra[0].ID == "" || msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetSyncdCollectionFatalRecoveryRequest().GetCollectionName() != "regular_low" {
		panic("invalid synthetic recovery send")
	}
	return whatsmeow.SendResponse{ID: extra[0].ID}, s.send(ctx, to, msg, extra[0])
}
func recoveryResponse(own types.JID, id types.MessageID) *events.Message {
	return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: own, Sender: own, IsFromMe: true}}, Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE.Enum(), PeerDataOperationRequestResponseMessage: &waE2E.PeerDataOperationRequestResponseMessage{StanzaID: proto.String(id), PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY.Enum()}}}}
}
func recoveryFixture(t *testing.T) *recoveryTransport {
	t.Helper()
	c, err := New(Options{StorePath: newPairedSessionStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &recoveryTransport{Client: c.client}
}
func TestRecoveryObservedBeforeACKAndSendError(t *testing.T) {
	for _, kind := range []string{"ack", "error", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s := recoveryFixture(t)
			own := s.Store.GetJID().ToNonAD()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sendErr := errors.New("synthetic send error")
			var sentID types.MessageID
			s.send = func(_ context.Context, to types.JID, _ *waE2E.Message, extra whatsmeow.SendRequestExtra) error {
				sentID = extra.ID
				if to != own {
					t.Fatal("wrong peer destination")
				}
				s.DangerousInternals().DispatchEvent(recoveryResponse(own, extra.ID))
				for range 20 {
					s.DangerousInternals().DispatchEvent(recoveryResponse(own, extra.ID))
				}
				if kind == "cancel" {
					cancel()
					return ctx.Err()
				}
				if kind == "error" {
					return sendErr
				}
				return nil
			}
			ackCalls := 0
			exchange, err := observeAppStateRecovery(ctx, s, own, types.EmptyJID, appstate.WAPatchRegularLow, func(types.MessageID) { ackCalls++ })
			if !exchange.ResponseReceived || exchange.ACKConfirmed != (kind == "ack") || ackCalls != map[bool]int{true: 1, false: 0}[kind == "ack"] {
				t.Fatalf("facts=%+v ack calls=%d", exchange, ackCalls)
			}
			switch kind {
			case "ack":
				if !errors.Is(err, ErrAppStateCompletionUnconfirmed) {
					t.Fatal(err)
				}
			case "error":
				if !errors.Is(err, sendErr) {
					t.Fatal(err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			s.mu.Lock()
			late := s.callback
			s.mu.Unlock()
			late(recoveryResponse(own, sentID))
			raw, _ := json.Marshal(exchange)
			if string(raw) != "{\"ack_confirmed\":"+map[bool]string{true: "true", false: "false"}[exchange.ACKConfirmed]+",\"response_received\":true}" {
				t.Fatal("unexpected exchange fields")
			}
		})
	}
}
func TestRecoveryObservedRejectsWrongIdentityScopeAndType(t *testing.T) {
	for _, kind := range []string{"id", "sender", "device", "chat", "group", "from_me", "history", "rerequest", "protocol", "request_type", "unknown_lid", "nil", "global"} {
		t.Run(kind, func(t *testing.T) {
			s := recoveryFixture(t)
			own := s.Store.GetJID().ToNonAD()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.send = func(_ context.Context, _ types.JID, _ *waE2E.Message, extra whatsmeow.SendRequestExtra) error {
				e := recoveryResponse(own, extra.ID)
				switch kind {
				case "id":
					e.Message.ProtocolMessage.PeerDataOperationRequestResponseMessage.StanzaID = proto.String("old-request")
				case "sender":
					e.Info.Sender = types.NewJID("15550000001", types.DefaultUserServer)
				case "device":
					e.Info.Sender.Device = 7
				case "chat":
					e.Info.Chat = types.NewJID("15550000001", types.DefaultUserServer)
				case "group":
					e.Info.IsGroup = true
				case "from_me":
					e.Info.IsFromMe = false
				case "history":
					e.SourceWebMsg = &waWeb.WebMessageInfo{}
				case "rerequest":
					e.UnavailableRequestID = "old-request"
				case "protocol":
					e.Message.ProtocolMessage.Type = waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum()
				case "request_type":
					e.Message.ProtocolMessage.PeerDataOperationRequestResponseMessage.PeerDataOperationRequestType = waE2E.PeerDataOperationRequestType_PLACEHOLDER_MESSAGE_RESEND.Enum()
				case "unknown_lid":
					e.Info.Sender = types.NewJID("300", types.HiddenUserServer)
				case "nil":
					e = nil
				case "global":
					s.DangerousInternals().DispatchEvent(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularLow, Version: 81, Recovery: true})
					cancel()
					return nil
				}
				s.DangerousInternals().DispatchEvent(e)
				cancel()
				return nil
			}
			exchange, err := observeAppStateRecovery(ctx, s, own, types.EmptyJID, appstate.WAPatchRegularLow, nil)
			if !exchange.ACKConfirmed || exchange.ResponseReceived || !errors.Is(err, context.Canceled) {
				t.Fatalf("facts=%+v err=%v", exchange, err)
			}
		})
	}
}
func TestRecoveryObservedOwnLIDInterleavingAndDeadline(t *testing.T) {
	for _, respond := range []bool{false, true} {
		t.Run(map[bool]string{false: "ack_only", true: "known_lid"}[respond], func(t *testing.T) {
			s := recoveryFixture(t)
			own := s.Store.GetJID().ToNonAD()
			lid := types.NewJID("300", types.HiddenUserServer)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			s.send = func(_ context.Context, _ types.JID, _ *waE2E.Message, extra whatsmeow.SendRequestExtra) error {
				s.DangerousInternals().DispatchEvent(recoveryResponse(own, "old-request"))
				if respond {
					s.DangerousInternals().DispatchEvent(recoveryResponse(lid, extra.ID))
				}
				s.DangerousInternals().DispatchEvent(recoveryResponse(own, "other-request"))
				return nil
			}
			exchange, err := observeAppStateRecovery(ctx, s, own, lid, appstate.WAPatchRegularLow, nil)
			if !exchange.ACKConfirmed || exchange.ResponseReceived != respond {
				t.Fatalf("facts=%+v", exchange)
			}
			if respond && !errors.Is(err, ErrAppStateCompletionUnconfirmed) || !respond && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryObservedResponseDoesNotCertifyPayloadCollection(t *testing.T) {
	s := recoveryFixture(t)
	own := s.Store.GetJID().ToNonAD()
	s.send = func(_ context.Context, _ types.JID, _ *waE2E.Message, extra whatsmeow.SendRequestExtra) error {
		response := recoveryResponse(own, extra.ID)
		// Opaque data can be empty, malformed, or name another collection: only reception is observed.
		response.Message.ProtocolMessage.PeerDataOperationRequestResponseMessage.PeerDataOperationResult = []*waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult{{SyncdSnapshotFatalRecoveryResponse: &waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult_SyncDSnapshotFatalRecoveryResponse{CollectionSnapshot: []byte("opaque wrong-collection snapshot")}}}
		s.DangerousInternals().DispatchEvent(response)
		return nil
	}
	exchange, err := observeAppStateRecovery(t.Context(), s, own, types.EmptyJID, appstate.WAPatchRegularLow, nil)
	if !exchange.ResponseReceived || !errors.Is(err, ErrAppStateCompletionUnconfirmed) {
		t.Fatalf("facts=%+v err=%v", exchange, err)
	}
}
