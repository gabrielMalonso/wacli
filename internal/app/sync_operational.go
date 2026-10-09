package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
)

type SyncOperationReason string

const (
	SyncAttemptPermitted     SyncOperationReason = "attempt_permitted"
	SyncOwnerNotRequired     SyncOperationReason = "owner_not_required"
	SyncOwnerUnavailable     SyncOperationReason = "owner_unavailable"
	SyncOwnerInitializing    SyncOperationReason = "owner_initializing"
	SyncOwnerStopping        SyncOperationReason = "owner_stopping"
	SyncTransportUnavailable SyncOperationReason = "transport_unavailable"
	SyncTransportUnknown     SyncOperationReason = "transport_unknown"
	SyncOwnerReconnecting    SyncOperationReason = "owner_reconnecting"
)

// Admission allows an authorized attempt, never authorization, authentication or an
// outcome. Local reads do not require an owner; archive availability is unchecked.
type SyncOperation struct {
	Attemptable bool                `json:"attemptable"`
	Reason      SyncOperationReason `json:"reason"`
}

type SyncLocalRead struct {
	OwnerRequired bool                `json:"owner_required"`
	Availability  string              `json:"availability"`
	Reason        SyncOperationReason `json:"reason"`
}

type SyncOperations struct {
	LocalRead      SyncLocalRead `json:"local_read"`
	DraftWrite     SyncOperation `json:"draft_write"`
	SendAttempt    SyncOperation `json:"send_attempt"`
	ChatStateWrite SyncOperation `json:"chat_state_write"`
}

func UnknownSyncOperations() SyncOperations {
	blocked := SyncOperation{Reason: SyncOwnerUnavailable}
	return SyncOperations{LocalRead: SyncLocalRead{Availability: "not_checked", Reason: SyncOwnerNotRequired}, DraftWrite: blocked, SendAttempt: blocked, ChatStateWrite: blocked}
}

// SyncOperationAdmission is shared by the status projection and owner IPC gate.
// It is conservative about transport/lifecycle; execution keeps its own guards.
func SyncOperationAdmission(v SyncLiveStatus) SyncOperations {
	o := UnknownSyncOperations()
	reason := SyncOwnerInitializing
	switch {
	case v.State == SyncLiveStopping || v.State == SyncLiveStopped || v.State == SyncLiveLoggedOut || v.State == SyncLiveError:
		reason = SyncOwnerStopping
	case !v.SendInitialized:
	case v.State == SyncLiveReconnecting:
		reason = SyncOwnerReconnecting
	case v.State == SyncLiveDisconnected || v.TransportConnected == "false":
		reason = SyncTransportUnavailable
	case v.TransportConnected != "true":
		reason = SyncTransportUnknown
	default:
		reason = SyncAttemptPermitted
	}
	o.SendAttempt = SyncOperation{Attemptable: reason == SyncAttemptPermitted, Reason: reason}
	// Draft preparation is local but still needs the migrated writer and its
	// lifetime. It need not wait for a disconnected socket to reconnect.
	draftReason := reason
	if v.SendInitialized && reason != SyncOwnerStopping {
		draftReason = SyncAttemptPermitted
	}
	o.DraftWrite = SyncOperation{Attemptable: draftReason == SyncAttemptPermitted, Reason: draftReason}
	o.ChatStateWrite = o.SendAttempt
	if !v.OwnerReady && reason == SyncAttemptPermitted {
		o.ChatStateWrite = SyncOperation{Reason: SyncOwnerInitializing}
	}
	return o
}

// SyncStageObservation retains only the latest WACLI invocation of each phase
// per collection. Elapsed includes lock/wait/persistence inside that invocation;
// internal SDK mutex/blob/CDN timings cannot be separated by this observation.
type SyncStageObservation struct {
	Phase      string     `json:"phase"`
	Collection string     `json:"collection,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	ElapsedMS  int64      `json:"elapsed_ms"`
	BudgetMS   *int64     `json:"budget_ms"`
	ErrorCode  string     `json:"error_code,omitempty"`
}

type syncStageKey struct{ phase, collection string }

func (a *App) measureSyncStage(ctx context.Context, phase, collection string) func(error) {
	// SDK collection names are an external boundary. Unknown names never become
	// labels, retained error text or an unbounded metric cardinality.
	if collection != "" && !slices.Contains(diagnosticAppStateCollections[:], collection) {
		return func(error) {}
	}
	switch phase {
	case "bootstrap", "connect", "lid_migration", "metadata", "refresh_contacts", "refresh_groups", "refresh_channels", "lock_wait", "delta", "delta_fetch", "full_sync", "snapshot":
	default:
		return func(error) {}
	}
	a.live.mu.Lock()
	if a.live.recovery == nil || a.live.recovery.diagnostic == nil {
		a.live.mu.Unlock()
		return func(error) {}
	}
	a.live.mu.Unlock()
	started := time.Now()
	var budget *int64
	if deadline, ok := ctx.Deadline(); ok {
		ms := max(0, time.Until(deadline).Milliseconds())
		budget = &ms
	}
	a.live.mu.Lock()
	generation := a.live.generation
	if a.live.recovery == nil || a.live.recovery.diagnostic == nil {
		generation = 0
	}
	if run, _ := ctx.Value(appStateRecoveryRunKey{}).(*appStateRecoveryRun); run != nil && run != a.live.recovery {
		generation = 0
	}
	key := syncStageKey{phase, collection}
	observation := SyncStageObservation{Phase: phase, Collection: collection, StartedAt: started.UTC(), BudgetMS: budget}
	if generation != 0 {
		a.live.stages[key] = observation
	}
	a.live.mu.Unlock()
	return func(err error) {
		finished := time.Now().UTC()
		observation.FinishedAt, observation.ElapsedMS = &finished, time.Since(started).Milliseconds()
		observation.ErrorCode = syncStageErrorCode(err)
		a.live.mu.Lock()
		defer a.live.mu.Unlock()
		if generation == 0 || generation != a.live.generation || a.live.stages[key].StartedAt != observation.StartedAt {
			return
		}
		a.live.stages[key] = observation
	}
}

func syncStageErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, wa.ErrAppStateCompletionUnconfirmed):
		return "completion_unconfirmed"
	case errors.Is(err, appstate.ErrMismatchingLTHash):
		return "lthash_mismatch"
	case errors.Is(err, wa.ErrEmptyAppStateKeyShare), errors.Is(err, appstate.ErrKeyNotFound):
		return "key_unavailable"
	default:
		return "operation_failed"
	}
}

// SyncAdmissionSnapshot uses only lifecycle/SDK observations, without diagnostic
// DB reads or historical copies. Action admission must not wait on metadata reads;
// execution still validates its archive and identity before dispatch.
func (a *App) SyncAdmissionSnapshot() SyncLiveStatus {
	v := a.syncLiveSnapshot()
	v.Operations = SyncOperationAdmission(v)
	return v
}

// SyncLiveSnapshot adds bounded local/historical observations to the lifecycle
// snapshot. No historical event or successful operation establishes current auth.
func (a *App) SyncLiveSnapshot() SyncLiveStatus {
	v := a.syncLiveSnapshot()
	// Query only the owner's already-open DB. The status caller opens no store.
	v.AppState = ReadAppStateDiagnostics(a.db)
	a.live.mu.Lock()
	run := a.live.recovery
	v.Stages = []SyncStageObservation{}
	for _, stage := range a.live.stages {
		if stage.BudgetMS != nil {
			budget := *stage.BudgetMS
			stage.BudgetMS = &budget
		}
		if stage.FinishedAt != nil {
			finished := *stage.FinishedAt
			stage.FinishedAt = &finished
		}
		if stage.FinishedAt == nil {
			stage.ElapsedMS = max(0, time.Since(stage.StartedAt).Milliseconds())
		}
		v.Stages = append(v.Stages, stage)
	}
	currentID := a.live.runID
	a.live.mu.Unlock()
	slices.SortFunc(v.Stages, func(x, y SyncStageObservation) int {
		if n := x.StartedAt.Compare(y.StartedAt); n != 0 {
			return n
		}
		if x.Phase < y.Phase {
			return -1
		}
		if x.Phase > y.Phase {
			return 1
		}
		return 0
	})
	if run != nil && run.diagnostic != nil {
		r := run.diagnostic
		r.mu.Lock()
		// Keep dated facts defensive, including nested optional observations.
		copy := *r.sync
		copy.Ingestion = nil // Project the run-scoped getter after releasing r.mu.
		raw, _ := json.Marshal(copy)
		var s SyncObservation
		_ = json.Unmarshal(raw, &s)
		s.RecoveryObservations = run.snapshot()
		connection := r.connectionRun
		r.mu.Unlock()
		if ingestion := a.IngestionSnapshot(); ingestion != nil && ingestion.ExecutionID == v.OwnerRunID && s.ExecutionID == v.OwnerRunID {
			s.Ingestion = ingestion
		}
		v.AppState.RecoveryObservations = s.RecoveryObservations
		v.Observations = &DiagnosticObservations{Version: 1, Historical: true, Sync: &s}
		if connection != nil {
			connection.mu.Lock()
			raw, _ := json.Marshal(connection.connection)
			var c ConnectionObservation
			_ = json.Unmarshal(raw, &c)
			connection.mu.Unlock()
			v.Observations.Connection = &c
		}
	}
	a.waMu.Lock()
	closed := a.closed
	a.waMu.Unlock()
	a.live.mu.Lock()
	changed := v.OwnerRunID != currentID || currentID != a.live.runID || v.revision != a.live.revision
	cancelled := a.live.ctx != nil && a.live.ctx.Err() != nil
	currentState, terminal := a.live.state, a.live.terminal()
	a.live.mu.Unlock()
	if changed {
		v.OwnerReady, v.SendInitialized, v.Initialized = false, false, false
		v.State, v.TransportConnected = SyncLiveUnknown, "unknown"
		v.OwnerRunID, v.LinkedJID, v.LinkedLID = "", "", ""
		v.Observations, v.Stages = nil, nil
		v.AppState.RecoveryObservations = nil
		if terminal {
			// Drop stale evidence without erasing the current terminal lifecycle.
			v.State = currentState
		}
	}
	if cancelled {
		v.OwnerReady, v.SendInitialized = false, false
		if v.State != SyncLiveLoggedOut && v.State != SyncLiveStopped && v.State != SyncLiveError {
			v.State = SyncLiveStopping
		}
	}
	if closed {
		v.OwnerReady, v.SendInitialized, v.Connected = false, false, "false"
		if v.State != SyncLiveLoggedOut && v.State != SyncLiveError {
			v.State = SyncLiveStopped
		}
	}
	v.Operations = SyncOperationAdmission(v)
	return v
}
