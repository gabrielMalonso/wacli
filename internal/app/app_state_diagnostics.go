package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
)

type AppStateReconciliation string

const (
	AppStateReconciliationRequired     AppStateReconciliation = "required"
	AppStateReconciliationNoneRecorded AppStateReconciliation = "none_recorded"
	AppStateReconciliationUnknown      AppStateReconciliation = "unknown"
)

// AppStateDiagnostics describes retained local debt, never remote integrity or freshness.
// RecoveryObservations retains its invocation-local legacy meaning. Doctor uses
// the separate versioned observations.sync slot for historical recovery facts.
type AppStateDiagnostics struct {
	Reconciliation       AppStateReconciliation        `json:"reconciliation"`
	PendingCollections   []string                      `json:"pending_collections"`
	Error                *AppStateDiagnosticError      `json:"error,omitempty"`
	RecoveryObservations []AppStateRecoveryObservation `json:"recovery_observations"`
}

type AppStateDiagnosticError struct {
	Code string `json:"code"`
}

func unknownAppStateDiagnostics() AppStateDiagnostics {
	return AppStateDiagnostics{
		Reconciliation: AppStateReconciliationUnknown,
		Error:          &AppStateDiagnosticError{Code: "recovery_state_unavailable"},
	}
}

// ReadAppStateDiagnostics needs only the archive DB; it neither opens a session
// nor acquires a writer lock. A failed query must not become an empty debt list.
func ReadAppStateDiagnostics(db *store.DB) AppStateDiagnostics {
	if db == nil {
		return unknownAppStateDiagnostics()
	}
	collections, err := db.AppStateRecoveryCollections()
	if err != nil {
		return unknownAppStateDiagnostics()
	}
	// The store query already returns distinct collections in lexical order.
	if collections == nil {
		collections = []string{}
	}
	state := AppStateReconciliationNoneRecorded
	if len(collections) > 0 {
		state = AppStateReconciliationRequired
	}
	return AppStateDiagnostics{Reconciliation: state, PendingCollections: collections}
}

// AppStateSnapshot reads retained debt after the caller closes App and drains
// its workers. Keep the writer lock until the snapshot and report are finished.
// Observation failures are embedded diagnostics, not command errors.
func (r SyncResult) AppStateSnapshot() AppStateDiagnostics {
	diagnostic := unknownAppStateDiagnostics()
	if r.storeDir == "" {
		return diagnostic
	}
	if db, err := store.OpenReadOnly(filepath.Join(r.storeDir, "wacli.db")); err == nil {
		diagnostic = ReadAppStateDiagnostics(db)
		_ = db.Close()
	}
	diagnostic.RecoveryObservations = r.recovery.snapshot()
	return diagnostic
}

type AppStateRecoveryPhase string

const (
	appStateRecoveryPrepare    AppStateRecoveryPhase = "prepare"
	appStateRecoveryFullSync   AppStateRecoveryPhase = "full_sync"
	appStateRecoverySnapshot   AppStateRecoveryPhase = "snapshot"
	appStateRecoveryPersist    AppStateRecoveryPhase = "persist"
	appStateRecoveryCheckpoint AppStateRecoveryPhase = "checkpoint"
	appStateRecoveryDelta      AppStateRecoveryPhase = "delta"
	appStateRecoveryShutdown   AppStateRecoveryPhase = "shutdown"
)

type AppStateRecoveryOutcome string

const (
	AppStateRecoveryCompleted   AppStateRecoveryOutcome = "completed"
	AppStateRecoveryFailed      AppStateRecoveryOutcome = "failed"
	AppStateRecoveryCancelled   AppStateRecoveryOutcome = "cancelled"
	AppStateRecoveryUnconfirmed AppStateRecoveryOutcome = "unconfirmed"
)

// Outcomes retain each observed kind, so a later success cannot erase a failure.
// No attempt count, raw error, payload, or unlimited attempt history is retained.
type AppStateRecoveryObservation struct {
	Collection      string                    `json:"collection"`
	Phase           AppStateRecoveryPhase     `json:"phase"`
	Outcomes        []AppStateRecoveryOutcome `json:"outcomes"`
	ErrorCodes      []string                  `json:"error_codes"`
	FirstObservedAt *time.Time                `json:"first_observed_at,omitempty"`
	LastObservedAt  *time.Time                `json:"last_observed_at,omitempty"`
	CompletedAt     *time.Time                `json:"completed_at,omitempty"`
	FailedAt        *time.Time                `json:"failed_at,omitempty"`
	CancelledAt     *time.Time                `json:"cancelled_at,omitempty"`
	UnconfirmedAt   *time.Time                `json:"unconfirmed_at,omitempty"`
}

var diagnosticAppStateCollections = [...]string{"critical_block", "critical_unblock_low", "regular", "regular_high", "regular_low"}
var diagnosticAppStatePhases = [...]AppStateRecoveryPhase{appStateRecoveryPrepare, appStateRecoveryFullSync, appStateRecoverySnapshot, appStateRecoveryPersist, appStateRecoveryCheckpoint, appStateRecoveryDelta, appStateRecoveryShutdown}
var diagnosticAppStateOutcomes = [...]AppStateRecoveryOutcome{AppStateRecoveryCompleted, AppStateRecoveryFailed, AppStateRecoveryCancelled, AppStateRecoveryUnconfirmed}
var diagnosticAppStateErrorCodes = [...]string{"cancelled", "deadline_exceeded", "lthash_mismatch", "key_unavailable", "recovery_failed", "completion_unconfirmed"}

type appStateRecoveryFacts struct {
	first, last time.Time
	dates       [len(diagnosticAppStateOutcomes)]time.Time
	outcomes    [len(diagnosticAppStateOutcomes)]bool
	codes       [len(diagnosticAppStateErrorCodes)]bool
}

type appStateRecoveryRun struct {
	diagnostic *diagnosticRun
	mu         sync.Mutex
	facts      [len(diagnosticAppStateCollections)][len(diagnosticAppStatePhases)]appStateRecoveryFacts
}

type appStateRecoveryRunKey struct{}

func recordAppStateRecovery(ctx context.Context, collection string, phase AppStateRecoveryPhase, err error) {
	run, _ := ctx.Value(appStateRecoveryRunKey{}).(*appStateRecoveryRun)
	if run == nil {
		return
	}
	c := slices.Index(diagnosticAppStateCollections[:], collection)
	p := slices.Index(diagnosticAppStatePhases[:], phase)
	if c < 0 || p < 0 {
		return
	}
	outcome, code := AppStateRecoveryCompleted, ""
	if err != nil {
		outcome, code = AppStateRecoveryFailed, "recovery_failed"
		switch {
		case errors.Is(err, context.Canceled):
			outcome, code = AppStateRecoveryCancelled, "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline_exceeded"
		case errors.Is(err, wa.ErrAppStateCompletionUnconfirmed):
			outcome, code = AppStateRecoveryUnconfirmed, "completion_unconfirmed"
		case errors.Is(err, appstate.ErrMismatchingLTHash):
			code = "lthash_mismatch"
		case errors.Is(err, wa.ErrEmptyAppStateKeyShare):
			code = "key_unavailable"
		}
	}
	run.mu.Lock()
	facts := &run.facts[c][p]
	changed := !facts.outcomes[slices.Index(diagnosticAppStateOutcomes[:], outcome)]
	at := nowUTC()
	if facts.first.IsZero() {
		facts.first = at
	}
	facts.last = at
	facts.dates[slices.Index(diagnosticAppStateOutcomes[:], outcome)] = at
	facts.outcomes[slices.Index(diagnosticAppStateOutcomes[:], outcome)] = true
	if code != "" {
		changed = changed || !facts.codes[slices.Index(diagnosticAppStateErrorCodes[:], code)]
		facts.codes[slices.Index(diagnosticAppStateErrorCodes[:], code)] = true
	}
	run.mu.Unlock()
	if changed && run.diagnostic != nil {
		run.diagnostic.updateSync(func(*SyncObservation) {})
	}
}

func (r *appStateRecoveryRun) snapshot() []AppStateRecoveryObservation {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	observations := []AppStateRecoveryObservation{}
	for c, collection := range diagnosticAppStateCollections {
		for p, phase := range diagnosticAppStatePhases {
			facts := r.facts[c][p]
			observation := AppStateRecoveryObservation{Collection: collection, Phase: phase, Outcomes: []AppStateRecoveryOutcome{}, ErrorCodes: []string{}}
			observation.FirstObservedAt, observation.LastObservedAt = observationTime(facts.first), observationTime(facts.last)
			observation.CompletedAt, observation.FailedAt, observation.CancelledAt, observation.UnconfirmedAt = observationTime(facts.dates[0]), observationTime(facts.dates[1]), observationTime(facts.dates[2]), observationTime(facts.dates[3])
			for i, observed := range facts.outcomes {
				if observed {
					observation.Outcomes = append(observation.Outcomes, diagnosticAppStateOutcomes[i])
				}
			}
			if len(observation.Outcomes) == 0 {
				continue
			}
			for i, observed := range facts.codes {
				if observed {
					observation.ErrorCodes = append(observation.ErrorCodes, diagnosticAppStateErrorCodes[i])
				}
			}
			observations = append(observations, observation)
		}
	}
	return observations
}

func observationTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
