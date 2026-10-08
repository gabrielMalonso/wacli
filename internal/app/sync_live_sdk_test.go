package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	wmStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Keep the actual pinned SDK's post-authentication work pending without a
// websocket, HTTP request, session database or real account.
type pendingLivePrekeys struct {
	*wmStore.NoopStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pendingLivePrekeys) UploadedPreKeyCount(context.Context) (int, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return 0, errors.New("fixture prekeys unavailable")
}

func TestSyncLivePinnedSDKDelayedConnected(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	generation := a.live.begin(t.Context())
	a.live.initialize(t.Context())
	setTransport := func(connected bool) {
		f.mu.Lock()
		f.connected = connected
		f.mu.Unlock()
	}
	setTransport(true)
	prekeys := &pendingLivePrekeys{NoopStore: &wmStore.NoopStore{}, started: make(chan struct{}), release: make(chan struct{})}
	jid := types.NewJID("15550000001", types.DefaultUserServer)
	sdk := whatsmeow.NewClient(&wmStore.Device{ID: &jid, PreKeys: prekeys, PrivacyTokens: &wmStore.NoopStore{}}, nil)
	callbacks := make(chan struct{}, 1)
	sdk.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			a.live.event(evt, generation)
			callbacks <- struct{}{}
		}
	})
	// B has authenticated, but its Connected callback is still pending.
	sdk.DangerousInternals().HandleConnectSuccess(t.Context(), &waBinary.Node{Tag: "success"})
	<-prekeys.started
	if !sdk.IsLoggedIn() {
		t.Fatal("SDK never accepted authentication B")
	}
	sdk.Disconnect()
	if !sdk.IsLoggedIn() {
		t.Fatal("pinned SDK changed: revisit whether current authentication is accessible")
	}
	setTransport(false)
	a.live.event(&events.Disconnected{}, generation)
	if v := a.SyncLiveSnapshot(); v.Ready || v.State != SyncLiveDisconnected {
		t.Fatalf("lost B=%+v", v)
	}
	a.live.transition(SyncLiveReconnecting)
	setTransport(true) // C has only an open, unauthenticated replacement transport.
	close(prekeys.release)
	awaitCallback := func() {
		t.Helper()
		select {
		case <-callbacks:
		case <-time.After(time.Second):
			t.Fatal("SDK did not dispatch Connected")
		}
	}
	awaitCallback()
	if v := a.SyncLiveSnapshot(); v.Ready || !v.OwnerReady || v.State != SyncLiveUnknown || v.Connected != "unknown" || v.TransportConnected != "true" || v.Authenticated != "unknown" || v.ReadinessReason != "current_authentication_unsupported" {
		t.Fatalf("delayed B callback certified C: %+v", v)
	}
	// A genuine success for C also cannot be correlated to the current transport
	// through the SDK's public evidence; do not silently promote it to ready.
	sdk.DangerousInternals().HandleConnectSuccess(t.Context(), &waBinary.Node{Tag: "success"})
	awaitCallback()
	if v := a.SyncLiveSnapshot(); v.Ready || !v.OwnerReady || v.Connected != "unknown" || v.TransportConnected != "true" {
		t.Fatalf("unscoped SDK evidence promoted readiness: %+v", v)
	}
	a.live.event(&events.LoggedOut{}, generation)
	a.live.event(&events.Connected{}, generation)
	if v := a.SyncLiveSnapshot(); v.Ready || v.OwnerReady || v.State != SyncLiveLoggedOut || v.Connected != "false" {
		t.Fatalf("late success revived logout: %+v", v)
	}
}

func TestSyncLiveRealWrapperLocalOwnerWithoutNetwork(t *testing.T) {
	a := newTestApp(t)
	client, err := wa.New(wa.Options{StorePath: filepath.Join(t.TempDir(), "isolated-session.db")})
	if err != nil {
		t.Fatal(err)
	}
	a.wa = client // The real wrapper/SDK is opened locally, never connected.
	generation := a.live.begin(t.Context())
	a.live.initialize(t.Context())
	a.live.event(&events.Connected{}, generation)
	v := a.SyncLiveSnapshot()
	if !v.OwnerReady || !v.Initialized || v.Ready || v.TransportConnected != "false" || v.Authenticated != "unknown" || v.ReadinessReason != "current_authentication_unsupported" {
		t.Fatalf("real local wrapper evidence=%+v", v)
	}
	if client.IsConnected() {
		t.Fatal("status connected the real wrapper")
	}
	// Local readiness remains positive while disconnected; it certifies local
	// initialization, not dispatch ability. Cancellation revokes local readiness.
	a.live.transition(SyncLiveStopping)
	if v := a.SyncLiveSnapshot(); v.OwnerReady || v.Ready || v.State != SyncLiveStopping {
		t.Fatalf("local shutdown=%+v", v)
	}
}

func TestSyncLivePinnedSDKCallbackDuringTransportRead(t *testing.T) {
	for _, test := range []struct {
		name string
		evt  any
		want SyncLiveState
	}{
		{"late_connected", nil, SyncLiveUnknown},
		{"logout", &events.LoggedOut{}, SyncLiveLoggedOut},
		{"failure", &events.ConnectFailure{}, SyncLiveUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newTestApp(t)
			f := &delayedTransportLiveWA{fakeWA: newFakeWA(), started: make(chan struct{}), release: make(chan struct{})}
			a.wa = f
			generation := a.live.begin(t.Context())
			a.live.initialize(t.Context())
			prekeys := &pendingLivePrekeys{NoopStore: &wmStore.NoopStore{}, started: make(chan struct{}), release: make(chan struct{})}
			jid := types.NewJID("15550000001", types.DefaultUserServer)
			sdk := whatsmeow.NewClient(&wmStore.Device{ID: &jid, PreKeys: prekeys, PrivacyTokens: &wmStore.NoopStore{}}, nil)
			callbackDone := make(chan struct{})
			sdk.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.Connected); ok {
					a.live.event(evt, generation)
					close(callbackDone)
				}
			})
			sdk.DangerousInternals().HandleConnectSuccess(t.Context(), &waBinary.Node{Tag: "success"})
			<-prekeys.started
			sdk.Disconnect()
			a.live.event(&events.Disconnected{}, generation)
			f.mu.Lock()
			f.connected = true // C is open; B's real SDK callback is pending.
			f.mu.Unlock()
			snapshots := make(chan SyncLiveStatus, 1)
			go func() { snapshots <- a.SyncLiveSnapshot() }()
			<-f.started
			close(prekeys.release)
			select {
			case <-callbackDone:
			case <-time.After(time.Second):
				close(f.release)
				t.Fatal("SDK callback blocked behind lifecycle mutex")
			}
			if test.evt != nil {
				a.live.event(test.evt, generation)
			}
			close(f.release)
			v := <-snapshots
			if v.Ready || v.State != test.want || v.TransportConnected != "unknown" || v.Authenticated != "unknown" || v.LinkedJID != "" {
				t.Fatalf("mixed SDK observations=%+v", v)
			}
			next := a.SyncLiveSnapshot()
			if next.Ready || next.TransportConnected != "true" || next.OwnerReady != (test.want != SyncLiveLoggedOut) {
				t.Fatalf("fresh local/transport observation=%+v", next)
			}
		})
	}
}
