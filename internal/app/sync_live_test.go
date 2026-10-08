package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestSyncLiveBootstrapLifecycle(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	connected := make(chan struct{})
	release := make(chan struct{})
	// Pause a real local bootstrap step after Connected, before the runtime is ready.
	f.appStateFetchEvent = func(string, bool, bool) any {
		select {
		case <-connected:
		default:
			close(connected)
		}
		<-release
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet})
		done <- err
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not start")
	}
	v := a.SyncLiveSnapshot()
	if v.State != "initializing" || v.Ready || v.Initialized || v.OwnerReady || v.Connected != "unknown" || v.TransportConnected != "true" {
		t.Fatalf("premature readiness: %+v", v)
	}
	close(release)
	awaitLocalOwnerReady(t, a)
	f.Disconnect()
	f.emit(&events.Disconnected{})
	// Reconnection uses the same fake client; terminal logout cannot be revived.
	awaitLocalOwnerReady(t, a)
	f.emit(&events.LoggedOut{})
	f.emit(&events.Connected{})
	awaitLiveState(t, a, "logged_out")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := f.connectCalls
	f.mu.Unlock()
	if calls != 2 {
		t.Fatalf("connections=%d; status created another connection", calls)
	}
}

func TestSyncLiveCancelAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "failure"}[fail], func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			if fail {
				f.connectErrs = []error{errors.New("fixture connect failure")}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet})
				done <- err
			}()
			if fail {
				if err := <-done; err == nil {
					t.Fatal("missing failure")
				}
				awaitLiveState(t, a, "error")
				return
			}
			awaitLocalOwnerReady(t, a)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			awaitLiveState(t, a, "stopped")
			f.emit(&events.Connected{})
			if a.SyncLiveSnapshot().Ready {
				t.Fatal("late callback revived stopped owner")
			}
		})
	}
}

func TestSyncLiveEventPrecedence(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	generation := a.live.begin(t.Context())
	a.live.event(&events.Connected{}, generation)
	a.live.initialize(t.Context())
	awaitLocalOwnerReady(t, a)
	a.live.event(&events.Disconnected{}, generation)
	awaitLiveState(t, a, "disconnected")
	a.live.transition("reconnecting")
	awaitLiveState(t, a, "reconnecting")
	a.live.event(&events.Connected{}, generation)
	awaitLocalOwnerReady(t, a)
	a.live.event(&events.ConnectFailure{}, generation)
	awaitLiveState(t, a, "unknown")
	a.live.event(&events.Connected{}, generation)
	awaitLocalOwnerReady(t, a)
	a.live.transition("stopping")
	awaitLiveState(t, a, "stopping")
	a.live.event(&events.Connected{}, generation)
	awaitLiveState(t, a, "stopping")
}

func awaitLiveState(t *testing.T, a *App, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if string(a.SyncLiveSnapshot().State) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state=%+v, want %s", a.SyncLiveSnapshot(), want)
}

func awaitLocalOwnerReady(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v := a.SyncLiveSnapshot()
		if v.OwnerReady && v.TransportConnected == "true" && v.State == SyncLiveUnknown {
			if v.Ready || v.Authenticated != "unknown" || v.ReadinessReason != "current_authentication_unsupported" {
				t.Fatalf("unsupported authentication became positive: %+v", v)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("local owner/transport not available: %+v", a.SyncLiveSnapshot())
}

func TestSyncLivePriorRunCallbackIsFenced(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	old := a.live.begin(t.Context())
	current := a.live.begin(t.Context())
	a.live.event(&events.LoggedOut{}, old)
	if a.SyncLiveSnapshot().State != SyncLiveInitializing {
		t.Fatal("old logout altered new run")
	}
	a.live.event(&events.LoggedOut{}, current)
	if a.SyncLiveSnapshot().State != SyncLiveLoggedOut {
		t.Fatal("current logout was lost")
	}
}

type blockedLiveWA struct {
	*fakeWA
	started chan struct{}
	release chan struct{}
}

func (f *blockedLiveWA) LinkedJID() string {
	close(f.started)
	<-f.release
	return f.fakeWA.LinkedJID()
}

func TestSyncLiveSnapshotDoesNotHoldLifecycleMutexAcrossClient(t *testing.T) {
	a := newTestApp(t)
	f := &blockedLiveWA{fakeWA: newFakeWA(), started: make(chan struct{}), release: make(chan struct{})}
	a.wa = f
	generation := a.live.begin(t.Context())
	snapshots := make(chan SyncLiveStatus, 1)
	go func() { snapshots <- a.SyncLiveSnapshot() }()
	<-f.started
	eventDone := make(chan struct{})
	go func() { a.live.event(&events.Disconnected{}, generation); close(eventDone) }()
	select {
	case <-eventDone:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("SDK getter blocked lifecycle event")
	}
	close(f.release)
	if v := <-snapshots; v.State != SyncLiveDisconnected || v.Ready {
		t.Fatalf("%+v", v)
	}
}

func TestSyncLiveCancellationPrecedesLateLogout(t *testing.T) {
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	generation := a.live.begin(ctx)
	cancel()
	a.live.event(&events.LoggedOut{}, generation)
	if v := a.SyncLiveSnapshot(); v.State != SyncLiveStopping || v.Ready {
		t.Fatalf("late logout replaced cancellation: %+v", v)
	}
}

func TestSyncLiveForcedReconnectSeparatesLocalAndTransportReadiness(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	generation := a.live.begin(t.Context())
	a.live.event(&events.Connected{}, generation)
	a.live.initialize(t.Context())
	if v := a.SyncLiveSnapshot(); v.Ready || !v.OwnerReady || v.TransportConnected != "true" {
		t.Fatalf("initial local owner: %+v", v)
	}
	// Explicit SDK Disconnect does not emit Disconnected. Its caller records
	// loss before a new handshake so an old Connected cannot certify that socket.
	a.live.transition(SyncLiveDisconnected)
	f.Disconnect()
	a.live.transition(SyncLiveReconnecting)
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	if v := a.SyncLiveSnapshot(); v.Ready || v.State != SyncLiveReconnecting || v.TransportConnected != "true" || v.Connected != "unknown" || !v.OwnerReady {
		t.Fatalf("handshake became ready: %+v", v)
	}
	a.live.event(&events.Connected{}, generation)
	if v := a.SyncLiveSnapshot(); v.Ready || !v.OwnerReady || v.Connected != "unknown" {
		t.Fatalf("unscoped Connected certified authentication: %+v", v)
	}
	a.live.transition(SyncLiveStopping)
	if v := a.SyncLiveSnapshot(); v.Ready || v.OwnerReady || v.TransportConnected != "true" {
		t.Fatalf("cleanup connection evidence lost: %+v", v)
	}
}

func TestSyncLiveTransportLossInvalidatesPriorAuthentication(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	generation := a.live.begin(t.Context())
	a.live.initialize(t.Context())
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	a.live.event(&events.Connected{}, generation)
	awaitLocalOwnerReady(t, a)

	// The SDK can expose a lost transport before asynchronous Disconnected.
	f.Disconnect()
	lost := a.SyncLiveSnapshot()
	if lost.Ready || lost.State != SyncLiveDisconnected {
		t.Fatalf("loss=%+v", lost)
	}
	t.Logf("observed lost transport: %+v", lost)

	// Its replacement transport is up, but no authenticated Connected event
	// for that replacement has been delivered.
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	replacement := a.SyncLiveSnapshot()
	if replacement.Ready || replacement.State != SyncLiveDisconnected || replacement.TransportConnected != "true" {
		t.Fatalf("new unauthenticated transport inherited old Connected: %+v", replacement)
	}
	a.live.event(&events.Connected{}, generation)
	awaitLocalOwnerReady(t, a)
	a.live.transition(SyncLiveStopping)
	f.Disconnect()
	if v := a.SyncLiveSnapshot(); v.State != SyncLiveStopping || v.Ready || v.Connected != "false" {
		t.Fatalf("transport loss replaced terminal state: %+v", v)
	}
}

type delayedTransportLiveWA struct {
	*fakeWA
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *delayedTransportLiveWA) IsConnected() bool {
	connected := f.fakeWA.IsConnected()
	f.once.Do(func() { close(f.started); <-f.release })
	return connected
}

func TestSyncLiveTransportReadConcurrentWithNewAuthentication(t *testing.T) {
	for _, test := range []struct {
		name                   string
		priorTransport, newRun bool
	}{
		{"loss_then_new_connection", false, false},
		{"loss_then_new_run", false, true},
		{"positive_read_then_replacement_loss", true, false},
		{"positive_read_then_new_run_loss", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newTestApp(t)
			f := &delayedTransportLiveWA{fakeWA: newFakeWA(), started: make(chan struct{}), release: make(chan struct{})}
			a.wa = f
			generation := a.live.begin(t.Context())
			a.live.initialize(t.Context())
			a.live.event(&events.Connected{}, generation)
			f.mu.Lock()
			f.connected = test.priorTransport
			f.mu.Unlock()
			snapshots := make(chan SyncLiveStatus, 1)
			go func() { snapshots <- a.SyncLiveSnapshot() }()
			<-f.started
			// New authentication completes while the prior transport read is pending.
			// Neither an old negative read nor an old positive read belongs to it.
			if test.newRun {
				generation = a.live.begin(t.Context())
				a.live.initialize(t.Context())
			} else {
				a.live.event(&events.Disconnected{}, generation)
			}
			f.mu.Lock()
			f.connected = !test.priorTransport
			f.mu.Unlock()
			eventDone := make(chan struct{})
			go func() { a.live.event(&events.Connected{}, generation); close(eventDone) }()
			select {
			case <-eventDone:
			case <-time.After(time.Second):
				close(f.release)
				t.Fatal("transport getter held lifecycle mutex")
			}
			close(f.release)
			if v := <-snapshots; v.Ready || !v.OwnerReady || v.State != SyncLiveUnknown || v.Connected != "unknown" || v.TransportConnected != "unknown" {
				t.Fatalf("mixed observations certified readiness: %+v", v)
			}
			next := a.SyncLiveSnapshot()
			if !test.priorTransport {
				if next.Ready || !next.OwnerReady || next.TransportConnected != "true" {
					t.Fatalf("stale loss erased newer connection evidence: %+v", next)
				}
			} else {
				if next.Ready || next.State != SyncLiveDisconnected {
					t.Fatalf("current loss not observed: %+v", next)
				}
				f.mu.Lock()
				f.connected = true
				f.mu.Unlock()
				if v := a.SyncLiveSnapshot(); v.Ready || v.State != SyncLiveDisconnected || v.TransportConnected != "true" {
					t.Fatalf("replacement inherited lost authentication: %+v", v)
				}
			}
		})
	}
}
