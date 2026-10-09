package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
)

// DiagnosticObservations contains historical checkpoints, never current liveness.
// Connection and Sync are independent bounded slots and may describe different runs.
type DiagnosticObservations struct {
	Version    int                      `json:"version"`
	Historical bool                     `json:"historical"`
	Connection *ConnectionObservation   `json:"connection"`
	Sync       *SyncObservation         `json:"sync"`
	Error      *AppStateDiagnosticError `json:"error,omitempty"`
}

type ConnectionObservation struct {
	ExecutionID            string     `json:"execution_id"`
	StartedAt              time.Time  `json:"started_at"`
	ObservedAt             time.Time  `json:"observed_at"`
	ClosedAt               *time.Time `json:"closed_at"`
	LastEvent              string     `json:"last_event"`
	LoginConfirmedAt       *time.Time `json:"login_confirmed_at"`
	DisconnectedAt         *time.Time `json:"disconnected_at"`
	LoggedOutAt            *time.Time `json:"logged_out_at"`
	RejectedAt             *time.Time `json:"rejected_at"`
	ErrorAt                *time.Time `json:"error_at"`
	PersistenceUnconfirmed bool       `json:"persistence_unconfirmed"`
}

type HistorySyncObservation struct {
	ObservedAt time.Time `json:"observed_at"`
	SyncType   string    `json:"sync_type"`
	ChunkOrder *uint32   `json:"chunk_order"`
	Progress   *uint32   `json:"progress"`
}

type SyncObservation struct {
	ExecutionID            string                        `json:"execution_id"`
	ConnectionExecutionID  string                        `json:"connection_execution_id,omitempty"`
	Mode                   SyncMode                      `json:"mode"`
	StartedAt              time.Time                     `json:"started_at"`
	ObservedAt             time.Time                     `json:"observed_at"`
	StoppedAt              *time.Time                    `json:"stopped_at"`
	CleanupAt              *time.Time                    `json:"cleanup_at"`
	State                  string                        `json:"state"`
	StopReason             string                        `json:"stop_reason,omitempty"`
	MessagesStored         *int64                        `json:"messages_stored"`
	LastHistorySync        *HistorySyncObservation       `json:"last_history_sync"`
	Ingestion              *IngestionObservation         `json:"ingestion,omitempty"`
	OfflinePreviewAt       *time.Time                    `json:"offline_preview_at"`
	OfflineCompletedAt     *time.Time                    `json:"offline_completed_at"`
	OfflineCompletedCount  *int                          `json:"offline_completed_count"`
	RecoveryObservations   []AppStateRecoveryObservation `json:"recovery_observations"`
	PersistenceUnconfirmed bool                          `json:"persistence_unconfirmed"`
}

// Writes are synchronous, bounded checkpoints under the existing writer/owner.
// No heartbeat worker or message-by-message diagnostic writes are introduced.
type diagnosticRecoveryWriter interface {
	RecoverDiagnosticSnapshot(context.Context, string, string, string, []byte) error
}

type diagnosticRun struct {
	mu                  sync.Mutex
	app                 *App
	recoveryWriter      diagnosticRecoveryWriter
	connection          *ConnectionObservation
	sync                *SyncObservation
	recovery            *appStateRecoveryRun
	connectionRun       *diagnosticRun // connection lifetime used by this Sync; immutable after binding
	closed              bool
	started             bool
	previousExecutionID *string
}

func newDiagnosticRun(a *App, mode SyncMode, recovery *appStateRecoveryRun) *diagnosticRun {
	at := nowUTC()
	id := randomDiagnosticID()
	r := &diagnosticRun{app: a, recovery: recovery, recoveryWriter: a.db}
	if recovery == nil {
		r.connection = &ConnectionObservation{ExecutionID: id, StartedAt: at, ObservedAt: at, LastEvent: "unobserved"}
	} else {
		r.sync = &SyncObservation{ExecutionID: id, Mode: mode, StartedAt: at, ObservedAt: at, State: "unfinalized", RecoveryObservations: []AppStateRecoveryObservation{}}
		r.sync.Ingestion = &IngestionObservation{ExecutionID: id, StartedAt: at, ObservedAt: at}
	}
	r.mu.Lock()
	r.persistLocked(true)
	r.mu.Unlock()
	return r
}

func randomDiagnosticID() string {
	var id [16]byte
	// crypto/rand.Read cannot return an error on supported Go versions.
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func diagnosticTime() *time.Time { at := nowUTC(); return &at }

func (r *diagnosticRun) persistLocked(start bool) {
	if r.closed {
		return
	}
	slot, id, value := "sync", "", any(r.sync)
	if r.connection != nil {
		slot, id, value = "connection", r.connection.ExecutionID, r.connection
	} else {
		id = r.sync.ExecutionID
		r.sync.RecoveryObservations = r.recovery.snapshot()
	}
	raw, err := json.Marshal(value)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if start {
			previous, readErr := r.app.db.DiagnosticSnapshotExecutionID(ctx, slot)
			if readErr == nil {
				r.previousExecutionID = &previous
			}
			err = r.app.db.StartDiagnosticSnapshot(ctx, slot, id, raw)
		} else if !r.started {
			current := true
			if r.recovery != nil {
				// Serialize the check AND recovery write with Sync's fence advance.
				// Otherwise two failed starts can claim the same prior execution.
				r.app.waMu.Lock()
				current = r.app.appStateRecoveryOnClose == r.recovery
			}
			if current {
				// Without a readable prior token, only an absent slot or our own ID can match.
				previous := ""
				if r.previousExecutionID != nil {
					previous = *r.previousExecutionID
				}
				err = r.recoveryWriter.RecoverDiagnosticSnapshot(ctx, slot, id, previous, raw)
			} else {
				err = errors.New("diagnostic execution superseded")
			}
			if r.recovery != nil {
				r.app.waMu.Unlock()
			}
		} else {
			err = r.app.db.SaveDiagnosticSnapshot(ctx, slot, id, raw)
		}
		if err == nil {
			r.started = true
		}
		cancel()
	}
	if err != nil {
		first := false
		if r.connection != nil {
			first = !r.connection.PersistenceUnconfirmed
			r.connection.PersistenceUnconfirmed = true
		} else {
			first = !r.sync.PersistenceUnconfirmed
			r.sync.PersistenceUnconfirmed = true
		}
		if first {
			r.app.emitWarning("diagnostics_persistence_unconfirmed", "warning: "+slot+" diagnostics for execution "+id+" could not be retained; saved checkpoints may be older or absent", map[string]any{"slot": slot, "execution_id": id, "observed_at": nowUTC()})
		}
	}
}

func (r *diagnosticRun) observeConnection(evt any, revoked bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	at := diagnosticTime()
	switch v := evt.(type) {
	case *events.Connected:
		if revoked {
			return
		}
		r.connection.LoginConfirmedAt, r.connection.LastEvent = at, "login_confirmed"
	case *events.Disconnected:
		r.connection.DisconnectedAt = at
		if r.connection.LoggedOutAt == nil {
			r.connection.LastEvent = "disconnected"
		}
	case *events.LoggedOut:
		r.connection.LoggedOutAt, r.connection.LastEvent = at, "logged_out"
	case *events.ConnectFailure:
		r.connection.RejectedAt = at
		if r.connection.LoggedOutAt == nil {
			r.connection.LastEvent = "login_rejected"
		}
		if v.Reason.IsLoggedOut() {
			r.connection.LoggedOutAt, r.connection.LastEvent = at, "logged_out"
		}
	case *events.ClientOutdated, *events.TemporaryBan:
		r.connection.RejectedAt = at
		if r.connection.LoggedOutAt == nil {
			r.connection.LastEvent = "login_rejected"
		}
	case *events.StreamReplaced, *events.CATRefreshError, *events.StreamError, *events.ManualLoginReconnect:
		r.connection.ErrorAt = at
		if r.connection.LoggedOutAt == nil {
			r.connection.LastEvent = "connection_error"
		}
	default:
		return
	}
	r.connection.ObservedAt = *at
	r.persistLocked(false)
}

func (r *diagnosticRun) updateSync(update func(*SyncObservation)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	update(r.sync)
	r.sync.ObservedAt = nowUTC()
	r.persistLocked(false)
}

func syncDiagnosticRun(ctx context.Context) *diagnosticRun {
	recovery, _ := ctx.Value(appStateRecoveryRunKey{}).(*appStateRecoveryRun)
	if recovery == nil {
		return nil
	}
	return recovery.diagnostic
}

func observeHistorySync(ctx context.Context, data *waHistorySync.HistorySync) {
	r := syncDiagnosticRun(ctx)
	if r == nil || data == nil {
		return
	}
	r.updateSync(func(s *SyncObservation) {
		s.LastHistorySync = &HistorySyncObservation{ObservedAt: nowUTC(), SyncType: data.GetSyncType().String(), ChunkOrder: data.ChunkOrder, Progress: data.Progress}
	})
}

func (r *diagnosticRun) finishSync(err, contextErr error, messages int64) {
	r.updateSync(func(s *SyncObservation) {
		s.StoppedAt = diagnosticTime()
		s.State = "stopped"
		s.MessagesStored = &messages
		switch {
		case errors.Is(err, context.Canceled):
			s.StopReason = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			s.StopReason = "deadline_exceeded"
		case err != nil:
			s.StopReason = "failed"
		case s.StopReason == "logged_out":
		case errors.Is(contextErr, context.DeadlineExceeded):
			s.StopReason = "deadline_exceeded"
		case contextErr != nil:
			s.StopReason = "cancelled"
		default:
			s.StopReason = "idle"
		}
	})
}

func (r *diagnosticRun) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	at := diagnosticTime()
	if r.connection != nil {
		r.connection.ClosedAt, r.connection.ObservedAt = at, *at
	} else {
		r.sync.CleanupAt, r.sync.ObservedAt = at, *at
	}
	r.persistLocked(false)
	r.closed = true
	if r.sync != nil {
		r.app.emitEvent("sync_observations_finalized", map[string]any{"execution_id": r.sync.ExecutionID, "observed_at": *at, "observation": r.sync})
	}
}

func ReadDiagnosticObservations(db *store.DB) DiagnosticObservations {
	result := DiagnosticObservations{Version: 1, Historical: true}
	fail := func() DiagnosticObservations {
		return DiagnosticObservations{Version: 1, Historical: true, Error: &AppStateDiagnosticError{Code: "diagnostics_unavailable"}}
	}
	if db == nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	records, err := db.ReadDiagnosticSnapshots(ctx)
	if err != nil {
		return fail()
	}
	if raw := records["connection"]; raw != nil {
		var c ConnectionObservation
		if decodeDiagnostic(raw, &c) != nil || c.StartedAt.IsZero() || c.ObservedAt.IsZero() || !validDiagnosticID(c.ExecutionID) || !slices.Contains([]string{"unobserved", "login_confirmed", "disconnected", "logged_out", "login_rejected", "connection_error"}, c.LastEvent) {
			return fail()
		}
		result.Connection = &c
	}
	if raw := records["sync"]; raw != nil {
		var s SyncObservation
		if decodeDiagnostic(raw, &s) != nil || s.StartedAt.IsZero() || s.ObservedAt.IsZero() || s.RecoveryObservations == nil || !validDiagnosticID(s.ExecutionID) || s.ConnectionExecutionID != "" && !validDiagnosticID(s.ConnectionExecutionID) || !slices.Contains([]SyncMode{SyncModeBootstrap, SyncModeOnce, SyncModeFollow}, s.Mode) || !slices.Contains([]string{"unfinalized", "stopped"}, s.State) || !slices.Contains([]string{"", "idle", "failed", "cancelled", "deadline_exceeded", "logged_out"}, s.StopReason) || !validRecoveryObservations(s.RecoveryObservations) {
			return fail()
		}
		if h := s.LastHistorySync; h != nil {
			if _, ok := waHistorySync.HistorySync_HistorySyncType_value[h.SyncType]; !ok {
				return fail()
			}
		}
		if !validIngestionObservation(s.Ingestion, s.ExecutionID) {
			return fail()
		}
		result.Sync = &s
	}
	return result
}

func decodeDiagnostic(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid diagnostic snapshot")
	}
	return nil
}
func validDiagnosticID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16
}
func validRecoveryObservations(observations []AppStateRecoveryObservation) bool {
	if len(observations) > 35 {
		return false
	}
	for _, o := range observations {
		if !slices.Contains(diagnosticAppStateCollections[:], o.Collection) || !slices.Contains(diagnosticAppStatePhases[:], o.Phase) || len(o.Outcomes) > 4 || len(o.ErrorCodes) > 6 {
			return false
		}
		for _, outcome := range o.Outcomes {
			if !slices.Contains(diagnosticAppStateOutcomes[:], outcome) {
				return false
			}
		}
		for _, code := range o.ErrorCodes {
			if !slices.Contains(diagnosticAppStateErrorCodes[:], code) {
				return false
			}
		}
	}
	return true
}

func (r SyncResult) ObservationsSnapshot() DiagnosticObservations {
	result := DiagnosticObservations{Version: 1, Historical: true, Error: &AppStateDiagnosticError{Code: "diagnostics_unavailable"}}
	if r.storeDir != "" {
		if db, err := store.OpenReadOnly(filepath.Join(r.storeDir, "wacli.db")); err == nil {
			result = ReadDiagnosticObservations(db)
			_ = db.Close()
		}
	}
	if r.recovery != nil && r.recovery.diagnostic != nil {
		run := r.recovery.diagnostic
		run.mu.Lock()
		defer run.mu.Unlock()
		// Copy the invocation's facts rather than attributing a newer persisted run.
		raw, _ := json.Marshal(run.sync)
		var s SyncObservation
		_ = json.Unmarshal(raw, &s)
		s.RecoveryObservations = r.recovery.snapshot()
		result.Sync = &s
		if c := run.connectionRun; c != nil {
			c.mu.Lock()
			raw, _ := json.Marshal(c.connection)
			c.mu.Unlock()
			var current ConnectionObservation
			_ = json.Unmarshal(raw, &current)
			result.Connection = &current
		}
	}
	return result
}

func (a *App) syncEventData(ctx context.Context, data map[string]any) map[string]any {
	r := syncDiagnosticRun(ctx)
	if r == nil {
		return data
	}
	if data == nil {
		data = map[string]any{}
	}
	r.mu.Lock()
	data["execution_id"] = r.sync.ExecutionID
	data["observed_at"] = nowUTC()
	r.mu.Unlock()
	return data
}
func (a *App) emitSyncObservationEvent(ctx context.Context, event string, data map[string]any) {
	a.emitEvent(event, a.syncEventData(ctx, data))
}
func (a *App) emitOrPrintSync(ctx context.Context, event string, data map[string]any, format string, args ...any) {
	a.emitOrPrint(event, a.syncEventData(ctx, data), format, args...)
}

// InvocationConnectionObservation copies this App's observed client facts even
// when its persisted slot is older or absent. It never implies current liveness.
func (a *App) InvocationConnectionObservation() *ConnectionObservation {
	if a == nil {
		return nil
	}
	a.waMu.Lock()
	state := a.sessionState
	a.waMu.Unlock()
	if state == nil || state.diagnostic == nil {
		return nil
	}
	r := state.diagnostic
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(r.connection)
	var observed ConnectionObservation
	_ = json.Unmarshal(raw, &observed)
	return &observed
}
