package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type outboundLifecycleFake struct {
	*fakeWA
	adapter *outboundFake
}

func (f *outboundLifecycleFake) LinkedJID() string { return f.adapter.LinkedJID() }
func (f *outboundLifecycleFake) LinkedLID() string { return f.adapter.LinkedLID() }
func (f *outboundLifecycleFake) GenerateOutboundMessageID() (string, error) {
	return f.adapter.GenerateOutboundMessageID()
}
func (f *outboundLifecycleFake) SendOutbound(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return f.adapter.SendOutbound(ctx, to, id, msg)
}
func (f *outboundLifecycleFake) Upload(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return f.adapter.Upload(ctx, data, kind)
}

func TestOutboundStandaloneAppClientLifetime(t *testing.T) {
	for _, kind := range []store.DraftKind{store.DraftTextKind, store.DraftContactKind, store.DraftDocumentKind} {
		t.Run(string(kind), func(t *testing.T) {
			var client *outboundLifecycleFake
			t.Cleanup(func() {
				if client != nil {
					client.mu.Lock()
					defer client.mu.Unlock()
					if client.connected || len(client.handlers) != 0 {
						t.Error("App.Close did not release client/observer")
					}
				}
			})
			a, _, r, adapter, rev := outboundAppFixture(t, kind)
			client = &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: adapter}
			opens, admissions := 0, 0
			a.opts.WAFactory = func(opts wa.Options) (WAClient, error) {
				opens++
				if opts.StorePath != filepath.Join(a.StoreDir(), "session.db") || opts.KeyStateStore != a.DB() {
					t.Fatal("client options", opts)
				}
				return client, nil
			}
			result, err := a.SendOutbound(t.Context(), r, func(context.Context) error { admissions++; return nil })
			if err != nil || result.KnownResult != store.OutboundAccepted || result.KnownACK == nil || result.Persistence != "confirmed" {
				t.Fatal(result, err)
			}
			entry, err := a.DB().Outbound().Read(t.Context(), result.Entry.Operation.ID, "", "", 20, "")
			if err != nil || entry.Operation.MessageID != adapter.id || entry.Operation.Result != store.OutboundAccepted || len(entry.Observations.Items) != 1 || entry.Observations.Items[0].Fact != store.OutboundAck {
				t.Fatal("ACK not retained", entry, err)
			}
			_, want, err := prepareOutboundPayload(rev.Payload().Data(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if kind != store.DraftDocumentKind && !proto.Equal(adapter.sent, want) {
				t.Fatal("exact payload", adapter.sent, want)
			}
			if kind == store.DraftDocumentKind && (adapter.uploads != 1 || string(adapter.bytes) != "same bytes\x00\n" || adapter.sent.GetDocumentMessage().GetFileLength() != uint64(len(adapter.bytes))) {
				t.Fatal("document buffer/upload", adapter)
			}
			if err := a.OpenWA(); err != nil {
				t.Fatal(err)
			}
			if opens != 1 || client.connectCalls != 1 || len(client.handlers) != 1 || a.sessionState == nil || a.connectGate == nil || adapter.sends != 1 || adapter.ids != 1 || admissions != 1 {
				t.Fatal("OpenWA/Connect/adapter composition", opens, client.connectCalls, len(client.handlers), adapter, admissions)
			}
			again, err := a.SendOutbound(t.Context(), r, func(context.Context) error { t.Fatal("duplicate admission"); return nil })
			if err != nil || !again.Duplicate || again.Entry.Operation.ID != entry.Operation.ID || opens != 1 || client.connectCalls != 1 || adapter.sends != 1 {
				t.Fatal("standalone replay", again, err)
			}
		})
	}
	t.Run("connect failure", func(t *testing.T) {
		a, _, r, adapter, _ := outboundAppFixture(t, store.DraftTextKind)
		client := &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: adapter}
		client.connectErrs = []error{errors.New("synthetic connection failure")}
		a.opts.WAFactory = func(wa.Options) (WAClient, error) { return client, nil }
		result, err := a.SendOutbound(t.Context(), r, nil)
		if err == nil || result.KnownResult != store.OutboundNotDispatched || result.Entry.Operation.DispatchPossibleAt != nil || adapter.sends != 0 || client.connectCalls != 1 {
			t.Fatal(result, err, adapter.sends, client.connectCalls)
		}
	})
}
