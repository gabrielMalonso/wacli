package wa

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// ErrAppStateCompletionUnconfirmed means a recovery response was observed, but
// the SDK's public events cannot attribute application or completion to it.
var ErrAppStateCompletionUnconfirmed = errors.New("app state recovery completion unconfirmed")

// AppStateRecoveryExchange contains invocation-local facts, not snapshot contents
// or proof of application. False means not confirmed/observed, not remote absence.
type AppStateRecoveryExchange struct {
	ACKConfirmed     bool `json:"ack_confirmed"`
	ResponseReceived bool `json:"response_received"`
}

type appStateRecoveryClient interface {
	GenerateMessageID() types.MessageID
	AddEventHandler(whatsmeow.EventHandler) uint32
	RemoveEventHandler(uint32) bool
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// RequestAppStateRecoveryObserved observes a primary-device response separately
// from the server ACK. Neither fact certifies snapshot completion.
func (c *Client) RequestAppStateRecoveryObserved(ctx context.Context, name string, onAcknowledged func(types.MessageID)) (AppStateRecoveryExchange, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return AppStateRecoveryExchange{}, fmt.Errorf("not connected")
	}
	name = strings.TrimSpace(name)
	if !slices.Contains(appstate.AllPatchNames[:], appstate.WAPatchName(name)) {
		return AppStateRecoveryExchange{}, fmt.Errorf("invalid app state collection")
	}
	return observeAppStateRecovery(ctx, cli, cli.Store.GetJID().ToNonAD(), cli.Store.GetLID().ToNonAD(), appstate.WAPatchName(name), onAcknowledged)
}

func observeAppStateRecovery(ctx context.Context, cli appStateRecoveryClient, ownPN, ownLID types.JID, name appstate.WAPatchName, onAcknowledged func(types.MessageID)) (exchange AppStateRecoveryExchange, err error) {
	if err := ctx.Err(); err != nil {
		return exchange, err
	}
	if ownPN.IsEmpty() || ownPN.Server != types.DefaultUserServer {
		return exchange, whatsmeow.ErrNotLoggedIn
	}
	requestID := cli.GenerateMessageID()
	var mu sync.Mutex
	closed := false
	responseReceived := false
	received := make(chan struct{}, 1)
	isOwn := func(jid types.JID) bool {
		return jid == ownPN || (!ownLID.IsEmpty() && ownLID.Server == types.HiddenUserServer && jid == ownLID)
	}
	handler := cli.AddEventHandler(func(evt any) {
		msg, ok := evt.(*events.Message)
		if !ok || msg == nil || msg.SourceWebMsg != nil || msg.UnavailableRequestID != "" ||
			!msg.Info.IsFromMe || msg.Info.IsGroup || msg.Info.Sender.Device != 0 ||
			!isOwn(msg.Info.Sender) || !isOwn(msg.Info.Chat) {
			return
		}
		protocol := msg.Message.GetProtocolMessage()
		response := protocol.GetPeerDataOperationRequestResponseMessage()
		if protocol.GetType() != waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE ||
			response.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY ||
			response.GetStanzaID() != requestID {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		responseReceived = true
		select {
		case received <- struct{}{}:
		default:
		}
	})
	// Close logical admission before removal, outside both the callback and mutex.
	// The defer also retains a response delivered before a failed/cancelled ACK.
	defer func() {
		mu.Lock()
		closed = true
		exchange.ResponseReceived = responseReceived
		mu.Unlock()
		cli.RemoveEventHandler(handler)
	}()
	_, err = cli.SendMessage(ctx, ownPN, whatsmeow.BuildAppStateRecoveryRequest(name), whatsmeow.SendRequestExtra{Peer: true, ID: requestID})
	if err != nil {
		return exchange, err
	}
	mu.Lock()
	exchange.ACKConfirmed = true
	mu.Unlock()
	if onAcknowledged != nil {
		onAcknowledged(requestID)
	}
	select {
	case <-received:
		err = errors.Join(ErrAppStateCompletionUnconfirmed, ctx.Err())
	case <-ctx.Done():
		err = ctx.Err()
	}
	return exchange, err
}
