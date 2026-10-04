package wa

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type outboundFailedBuffer struct {
	wmstore.EventBuffer
	err error
}

type outboundNoHTTP struct{ calls *atomic.Int64 }

func (n outboundNoHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls.Add(1)
	return nil, errors.New("synthetic fixture prohibits HTTP")
}

func (b outboundFailedBuffer) AddOutgoingEvent(context.Context, types.JID, string, string, []byte) error {
	return b.err
}

func outboundSDKFixture(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{StorePath: filepath.Join(t.TempDir(), "synthetic-session.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	var httpCalls atomic.Int64
	httpClient := &http.Client{Transport: outboundNoHTTP{&httpCalls}}
	c.client.SetMediaHTTPClient(httpClient)
	c.client.SetWebsocketHTTPClient(httpClient)
	c.client.SetPreLoginHTTPClient(httpClient)
	t.Cleanup(func() {
		if httpCalls.Load() != 0 {
			t.Errorf("unexpected HTTP attempt: %d", httpCalls.Load())
		}
	})
	d := c.client.Store
	own := types.NewJID("15550000001", types.DefaultUserServer)
	d.ID = &own
	d.LID = types.NewJID("90001", types.HiddenUserServer)
	d.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := d.Save(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOutboundAdapterPinnedSDKIDAndRetryPersistenceFailure(t *testing.T) {
	c := outboundSDKFixture(t)
	id, err := c.GenerateOutboundMessageID()
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	sentinel := errors.New("synthetic retry persistence failure")
	c.client.Store.EventBuffer = outboundFailedBuffer{c.client.Store.EventBuffer, sentinel}
	to := types.NewJID("90002", types.HiddenUserServer)
	msg := &waE2E.Message{Conversation: proto.String("synthetic fixture")}
	response, err := c.SendOutbound(t.Context(), to, id, msg)
	if !errors.Is(err, sentinel) || response.ID != id {
		t.Fatal(response, err)
	}
	if !c.client.UseRetryMessageStore || !c.client.DangerousInternals().GetRecentMessage(to, id).IsEmpty() {
		t.Fatal("retry policy/cache changed")
	}
	t.Log("real SendMessage failure seam: ID retained, EventBuffer failure before cache; no Connect/transport/remote ACK exercised")
}

func TestOutboundSDKPayloadRemainsRetryableAfterApplicationUncertainty(t *testing.T) {
	c := outboundSDKFixture(t)
	id, _ := c.GenerateOutboundMessageID()
	to := types.NewJID("90002", types.HiddenUserServer)
	msg := &waE2E.Message{Conversation: proto.String("retained synthetic payload")}
	// Disconnected SDK stores the original payload before its local send fails.
	response, err := c.SendOutbound(t.Context(), to, id, msg)
	if err == nil || response.ID != id {
		t.Fatal(response, err)
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: to, Sender: to}, MessageIDs: []string{id}}
	got, err := c.client.DangerousInternals().GetMessageForRetry(t.Context(), receipt, id)
	if err != nil || got == nil || got.IsEmpty() {
		t.Fatal("SDK payload unavailable after return", got, err)
	}
	reopened := whatsmeow.NewClient(c.client.Store, nil)
	reopened.UseRetryMessageStore = true
	got, err = reopened.DangerousInternals().GetMessageForRetry(t.Context(), receipt, id)
	if err != nil || got == nil || got.IsEmpty() {
		t.Fatal("normal wa format not recoverable", got, err)
	}
	t.Log("payload cache and existing retry buffer remain readable after local failure; no retry frame sent by this test")
}
