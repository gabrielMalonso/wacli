package app

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// SyncLiveState describes this owner run, independently of archive coverage.
type SyncLiveState string

const (
	SyncLiveInitializing SyncLiveState = "initializing"
	SyncLiveReady        SyncLiveState = "ready"
	SyncLiveDisconnected SyncLiveState = "disconnected"
	SyncLiveReconnecting SyncLiveState = "reconnecting"
	SyncLiveStopping     SyncLiveState = "stopping"
	SyncLiveStopped      SyncLiveState = "stopped"
	SyncLiveLoggedOut    SyncLiveState = "logged_out"
	SyncLiveError        SyncLiveState = "error"
	SyncLiveUnknown      SyncLiveState = "unknown"
)

// SyncLiveStatus is an in-memory snapshot of the existing owner.
type SyncLiveStatus struct {
	State       SyncLiveState `json:"state"`
	Ready       bool          `json:"ready"`
	Initialized bool          `json:"initialized"`
	Connected   string        `json:"connected"`
	LinkedJID   string        `json:"linked_jid,omitempty"`
	LinkedLID   string        `json:"linked_lid,omitempty"`
	ObservedAt  time.Time     `json:"observed_at"`
}

type syncLive struct {
	mu          sync.Mutex
	state       SyncLiveState
	ctx         context.Context
	generation  uint64
	initialized bool
	connected   bool
}

func (s *syncLive) terminal() bool {
	return s.state == SyncLiveLoggedOut || s.state == SyncLiveStopping || s.state == SyncLiveStopped || s.state == SyncLiveError
}

func (s *syncLive) begin(ctx context.Context) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	s.generation++
	s.state, s.initialized, s.connected = SyncLiveInitializing, false, false
	return s.generation
}

func (s *syncLive) initialize(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	s.initialized = true
}

func (s *syncLive) transition(state SyncLiveState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Logout is terminal for this run, even if shutdown or a late Connected follows.
	if s.state == SyncLiveLoggedOut || s.terminal() && state != SyncLiveStopped && state != SyncLiveError {
		return
	}
	s.state = state
	// Reconnect can race an already authenticated Connected callback. Keep that
	// evidence; the snapshot also checks the transport, including force-close.
	if state == SyncLiveDisconnected || state == SyncLiveLoggedOut || state == SyncLiveUnknown {
		s.connected = false
	}
}

func (s *syncLive) event(evt any, generation uint64) {
	var state SyncLiveState
	connected := false
	switch v := evt.(type) {
	case *events.Connected:
		state, connected = SyncLiveInitializing, true
	case *events.Disconnected, *events.StreamReplaced:
		state = SyncLiveDisconnected
	case *events.LoggedOut:
		state = SyncLiveLoggedOut
	case *events.ConnectFailure:
		state = SyncLiveUnknown
		if v.Reason.IsLoggedOut() {
			state = SyncLiveLoggedOut
		}
	case *events.ClientOutdated, *events.TemporaryBan, *events.StreamError, *events.CATRefreshError, *events.ManualLoginReconnect:
		state = SyncLiveUnknown
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || generation == 0 {
		return
	}
	if s.terminal() {
		// Late loss/revocation can update connection evidence without reviving or
		// replacing the terminal lifecycle state.
		if state == SyncLiveDisconnected || state == SyncLiveLoggedOut || state == SyncLiveUnknown {
			s.connected = false
		}
		return
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		s.state = SyncLiveStopping
		if state == SyncLiveDisconnected || state == SyncLiveLoggedOut || state == SyncLiveUnknown {
			s.connected = false
		}
		return
	}
	s.state, s.connected = state, connected
}

// SyncLiveSnapshot uses only this App's existing client. Connected requires an
// authenticated Connected event as well as a currently open transport.
func (a *App) SyncLiveSnapshot() SyncLiveStatus {
	// Do not hold the lifecycle mutex across SDK getters: Disconnect may wait
	// for handlers while holding the client mutex those getters need.
	a.waMu.Lock()
	client, closed := a.wa, a.closed
	a.waMu.Unlock()
	var linkedJID, linkedLID string
	transportConnected := false
	if client != nil {
		linkedJID, linkedLID = client.LinkedJID(), client.LinkedLID()
		transportConnected = client.IsConnected()
	}
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	if a.live.ctx != nil && a.live.ctx.Err() != nil && !a.live.terminal() {
		a.live.state = SyncLiveStopping
	}
	v := SyncLiveStatus{State: a.live.state, Initialized: a.live.initialized, Connected: "unknown", LinkedJID: linkedJID, LinkedLID: linkedLID, ObservedAt: time.Now().UTC()}
	if v.State == "" {
		v.State = SyncLiveInitializing
	}
	if closed && v.State != SyncLiveLoggedOut && v.State != SyncLiveError {
		v.State = SyncLiveStopped
	}
	if client != nil {
		connected := a.live.connected && transportConnected && a.live.state != SyncLiveLoggedOut && !closed
		if connected {
			v.Connected = "true"
		} else {
			v.Connected = "false"
		}
		if v.State == SyncLiveUnknown {
			v.Connected = "unknown"
		}
		v.Ready = v.Initialized && connected && !a.live.terminal()
		if v.Ready {
			v.State = SyncLiveReady
		} else if a.live.connected && !transportConnected && !a.live.terminal() && !closed && v.State != SyncLiveReconnecting {
			v.State = SyncLiveDisconnected
		}
	}
	return v
}
