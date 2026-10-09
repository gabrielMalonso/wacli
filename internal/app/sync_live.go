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
	revision           uint64
	OwnerRunID         string                  `json:"owner_run_id,omitempty"`
	SendInitialized    bool                    `json:"send_initialized"`
	Operations         SyncOperations          `json:"operations"`
	Stages             []SyncStageObservation  `json:"stages"`
	AppState           AppStateDiagnostics     `json:"app_state"`
	Observations       *DiagnosticObservations `json:"observations,omitempty"`
	State              SyncLiveState           `json:"state"`
	Ready              bool                    `json:"ready"`
	OwnerReady         bool                    `json:"owner_ready"`
	Initialized        bool                    `json:"initialized"`
	Connected          string                  `json:"connected"`
	TransportConnected string                  `json:"transport_connected"`
	Authenticated      string                  `json:"authenticated"`
	ReadinessReason    string                  `json:"readiness_reason"`
	LinkedJID          string                  `json:"linked_jid,omitempty"`
	LinkedLID          string                  `json:"linked_lid,omitempty"`
	ObservedAt         time.Time               `json:"observed_at"`
}

type syncLive struct {
	mu              sync.Mutex
	state           SyncLiveState
	ctx             context.Context
	generation      uint64
	revision        uint64 // fences SDK reads performed outside mu
	initialized     bool
	connected       bool // latest unscoped event, never proof of current authentication
	runID           string
	sendInitialized bool
	recovery        *appStateRecoveryRun
	stages          map[syncStageKey]SyncStageObservation
}

func (s *syncLive) terminal() bool {
	return s.state == SyncLiveLoggedOut || s.state == SyncLiveStopping || s.state == SyncLiveStopped || s.state == SyncLiveError
}

func (s *syncLive) begin(ctx context.Context) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	s.generation++
	s.revision++
	s.state, s.initialized, s.connected = SyncLiveInitializing, false, false
	s.runID, s.sendInitialized, s.recovery = "", false, nil
	s.stages = make(map[syncStageKey]SyncStageObservation)
	return s.generation
}

func (s *syncLive) initialize(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	s.initialized = true
	s.sendInitialized = true
}

// enableSend admits only the typed draft/outbound family after identity migration
// and lifetime/history observers exist. It does not establish authentication.
func (s *syncLive) enableSend(ctx context.Context, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.generation && !s.terminal() && ctx.Err() == nil {
		s.sendInitialized = true
	}
}

func (s *syncLive) transition(state SyncLiveState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Logout is terminal for this run, even if shutdown or a late Connected follows.
	if s.state == SyncLiveLoggedOut || s.terminal() && state != SyncLiveStopped && state != SyncLiveError {
		return
	}
	s.revision++
	s.state = state
	// Reconnect can race a Connected callback. Keep its lifecycle observation;
	// the snapshot checks transport separately and never certifies authentication.
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
	s.revision++
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

// syncLiveSnapshot separates local owner readiness and observed transport from
// current authentication. The pinned SDK retains IsLoggedIn across normal loss
// and dispatches Connected without transport identity. Current authentication
// cannot be established, so strict Ready stays false even after legitimate login.
func (a *App) syncLiveSnapshot() SyncLiveStatus {
	// Do not hold the lifecycle mutex across SDK getters: Disconnect may wait
	// for handlers while holding the client mutex those getters need.
	a.live.mu.Lock()
	revision := a.live.revision
	a.live.mu.Unlock()
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
	// Invalidate only evidence from the same observation revision. A Connected
	// event or a new run during the getter must not be erased by an older loss.
	if revision == a.live.revision && client != nil && !transportConnected && a.live.connected {
		a.live.connected = false
		a.live.revision++
		if !a.live.terminal() && a.live.state != SyncLiveReconnecting {
			a.live.state = SyncLiveDisconnected
		}
		revision = a.live.revision
	}
	v := SyncLiveStatus{
		revision:   a.live.revision,
		OwnerRunID: a.live.runID, SendInitialized: a.live.sendInitialized && !a.live.terminal() && !closed,
		State: a.live.state, Initialized: a.live.initialized,
		OwnerReady: a.live.initialized && !a.live.terminal() && !closed,
		Connected:  "unknown", TransportConnected: "unknown", Authenticated: "unknown",
		ReadinessReason: "current_authentication_unsupported",
		LinkedJID:       linkedJID, LinkedLID: linkedLID, ObservedAt: time.Now().UTC(),
	}
	if v.State == "" {
		v.State = SyncLiveInitializing
	}
	if closed && v.State != SyncLiveLoggedOut && v.State != SyncLiveError {
		v.State = SyncLiveStopped
	}
	if revision != a.live.revision {
		// Do not combine an old transport/identity read with newer event evidence.
		v.LinkedJID, v.LinkedLID = "", ""
		if a.live.connected {
			if !a.live.terminal() && !closed {
				v.State = SyncLiveUnknown
			}
		} else if client != nil {
			v.Connected = "false"
		}
		return v
	}
	if client != nil {
		if transportConnected && !closed {
			v.TransportConnected = "true"
		} else {
			v.TransportConnected = "false"
			v.Connected = "false"
		}
		if v.State == SyncLiveLoggedOut || closed {
			v.Connected = "false"
		}
		if v.OwnerReady && a.live.connected && transportConnected {
			v.State = SyncLiveUnknown
		}
	}
	return v
}
