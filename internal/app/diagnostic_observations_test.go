package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestDiagnosticRestartAndConnectionOnlyPreservesSync(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	f := newFakeWA()
	a.wa = f
	f.connectEvents = []any{&events.OfflineSyncCompleted{Count: 7}, &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), ChunkOrder: proto.Uint32(3), Progress: proto.Uint32(100)}}}
	result, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	first := result.ObservationsSnapshot()
	if first.Sync == nil || first.Connection == nil || first.Sync.ExecutionID == first.Connection.ExecutionID || first.Sync.ConnectionExecutionID != first.Connection.ExecutionID || first.Connection.LoginConfirmedAt == nil || first.Connection.ClosedAt == nil || first.Connection.DisconnectedAt != nil || first.Sync.State != "stopped" || first.Sync.StopReason != "idle" || first.Sync.CleanupAt == nil || first.Sync.LastHistorySync == nil || first.Sync.OfflineCompletedAt == nil || *first.Sync.OfflineCompletedCount != 7 || first.Sync.PersistenceUnconfirmed {
		t.Fatalf("incomplete local observation: %+v", first)
	}
	if len(first.Sync.RecoveryObservations) != 0 {
		t.Fatal("preventive debt fabricated failure")
	}
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	restarted := ReadDiagnosticObservations(ro)
	ro.Close()
	raw1, _ := json.Marshal(first)
	raw2, _ := json.Marshal(restarted)
	if string(raw1) != string(raw2) {
		t.Fatalf("restart lost evidence:\n%s\n%s", raw1, raw2)
	}
	b, err := New(Options{StoreDir: a.StoreDir()})
	if err != nil {
		t.Fatal(err)
	}
	b.opts.Events = out.NewEventWriter(io.Discard, true)
	b.wa = newFakeWA()
	if err := b.Connect(t.Context(), false, nil); err != nil {
		t.Fatal(err)
	}
	b.Close()
	ro, err = store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	after := ReadDiagnosticObservations(ro)
	if after.Sync.ExecutionID != first.Sync.ExecutionID || after.Connection.ExecutionID == first.Connection.ExecutionID || after.Sync.ConnectionExecutionID == after.Connection.ExecutionID {
		t.Fatal("connection-only invocation replaced or relabelled sync")
	}
}

func TestDiagnosticFailureCheckpointAndReadErrors(t *testing.T) {
	for _, failure := range []string{"start", "update", "read", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			a.wa = newFakeWA()
			if failure == "start" {
				evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_start BEFORE INSERT ON diagnostic_snapshots BEGIN SELECT RAISE(ABORT,'secret private 15551234'); END`)
			}
			var result SyncResult
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
				if failure == "update" {
					evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_update BEFORE UPDATE ON diagnostic_snapshots BEGIN SELECT RAISE(ABORT,'secret private 15551234'); END`)
				}
				cancel()
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if failure == "read" {
				evidenceFixtureSQL(t, a, `ALTER TABLE diagnostic_snapshots RENAME TO hidden_private`)
			}
			a.Close()
			if failure == "corrupt" {
				evidenceFixtureSQL(t, a, `UPDATE diagnostic_snapshots SET payload='{"execution_id":"secret 15551234"}'`)
			}
			current := result.ObservationsSnapshot()
			if (failure == "start" || failure == "update" || failure == "read") && !current.Sync.PersistenceUnconfirmed {
				t.Fatal("failed checkpoint claimed confirmed persistence")
			}
			ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			saved := ReadDiagnosticObservations(ro)
			if failure == "start" && (saved.Connection != nil || saved.Sync != nil) {
				t.Fatal("failed start fabricated saved evidence")
			}
			if failure == "update" && (saved.Sync.State != "unfinalized" || saved.Sync.CleanupAt != nil) {
				t.Fatal("failed cleanup certified a final snapshot")
			}
			if (failure == "read" || failure == "corrupt") && (saved.Error == nil || saved.Error.Code != "diagnostics_unavailable" || saved.Connection != nil || saved.Sync != nil) {
				t.Fatal("invalid read became known healthy")
			}
			raw, _ := json.Marshal(saved)
			if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "15551234") || strings.Contains(string(raw), "private") {
				t.Fatal("private cause leaked")
			}
		})
	}
}

func TestDiagnosticMonotonicRecoveryAndOldExecution(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	a.wa = newFakeWA()
	var firstCtx context.Context
	var first SyncResult
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(ctx context.Context) error {
			if i == 0 {
				firstCtx = ctx
				recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, context.Canceled)
				recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, errors.Join(errors.New("private"), appstate.ErrMismatchingLTHash))
				recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, nil)
			}
			cancel()
			return nil
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = result
		}
	}
	recordAppStateRecovery(firstCtx, "regular_high", appStateRecoverySnapshot, context.DeadlineExceeded)
	a.Close()
	old := first.ObservationsSnapshot().Sync
	if !old.PersistenceUnconfirmed {
		t.Fatal("old callback claimed to update retained newer slot")
	}
	var facts AppStateRecoveryObservation
	for _, o := range old.RecoveryObservations {
		if o.Collection == "regular_low" {
			facts = o
		}
	}
	if !slices.Contains(facts.Outcomes, AppStateRecoveryCompleted) || !slices.Contains(facts.Outcomes, AppStateRecoveryFailed) || !slices.Contains(facts.Outcomes, AppStateRecoveryCancelled) || facts.FirstObservedAt == nil || facts.LastObservedAt == nil || facts.FailedAt == nil || facts.CancelledAt == nil || facts.CompletedAt == nil {
		t.Fatalf("lost monotonic dated facts: %+v", facts)
	}
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	latest := ReadDiagnosticObservations(ro).Sync
	if latest.ExecutionID == old.ExecutionID {
		t.Fatal("old callback replaced or contaminated newer execution")
	}
	for _, o := range latest.RecoveryObservations {
		if slices.Contains(o.Outcomes, AppStateRecoveryFailed) || slices.Contains(o.Outcomes, AppStateRecoveryCancelled) {
			t.Fatal("old failure contaminated newer recovery")
		}
	}
}

func TestDiagnosticLogoutTerminalAndConcurrentRecovery(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	f := newFakeWA()
	a.wa = f
	result, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond, AfterConnect: func(ctx context.Context) error {
		var workers sync.WaitGroup
		for _, cause := range []error{nil, context.Canceled, context.DeadlineExceeded} {
			workers.Go(func() { recordAppStateRecovery(ctx, "regular_low", appStateRecoveryFullSync, cause) })
		}
		workers.Wait()
		f.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
		f.emit(&events.Disconnected{})
		f.emit(&events.Connected{})
		f.emit(&events.ConnectFailure{Reason: events.ConnectFailureServiceUnavailable})
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	saved := result.ObservationsSnapshot()
	if saved.Connection.LastEvent != "logged_out" || saved.Connection.LoggedOutAt == nil || saved.Sync.StopReason != "logged_out" || saved.Sync.CleanupAt == nil {
		t.Fatal("logout erased or cleanup missing")
	}
	if !saved.Historical || saved.Version != 1 || len(saved.Sync.RecoveryObservations) != 1 {
		t.Fatal("bad snapshot contract")
	}
}

func TestDiagnosticFailedNewConnectionDistinguishesOlderSavedExecution(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	a.wa = newFakeWA()
	if err := a.Connect(t.Context(), false, nil); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := New(Options{StoreDir: a.StoreDir()})
	if err != nil {
		t.Fatal(err)
	}
	b.opts.Events = out.NewEventWriter(io.Discard, true)
	b.wa = newFakeWA()
	previous := ReadDiagnosticObservations(b.db).Connection
	evidenceFixtureSQL(t, b, `CREATE TRIGGER fail_new_connection BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='connection' BEGIN SELECT RAISE(ABORT,'private'); END`)
	result, err := b.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	current := result.ObservationsSnapshot()
	if current.Connection.ExecutionID == previous.ExecutionID || current.Sync.ConnectionExecutionID != current.Connection.ExecutionID || !current.Connection.PersistenceUnconfirmed || current.Connection.LoginConfirmedAt == nil {
		t.Fatal("current connection failure was hidden by older saved slot")
	}
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	saved := ReadDiagnosticObservations(ro)
	if saved.Connection.ExecutionID != previous.ExecutionID || saved.Sync.ConnectionExecutionID == saved.Connection.ExecutionID {
		t.Fatal("failed new connection silently overwrote historical correlation")
	}
}

func TestDiagnosticReadonlySyncRefusesBeforeCheckpoint(t *testing.T) {
	a := newTestApp(t)
	a.Close()
	path := filepath.Join(a.StoreDir(), "wacli.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := New(Options{StoreDir: a.StoreDir(), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce}); err == nil {
		t.Fatal("readonly Sync admitted")
	}
	ro.Close()
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("readonly attempt wrote diagnostics")
	}
	if _, err := os.Stat(filepath.Join(a.StoreDir(), "session.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("readonly attempt opened session")
	}
}

func TestDiagnosticLifecycleEventsCorrelateAndFinalizeAfterCleanup(t *testing.T) {
	var output strings.Builder
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(&output, true)
	a.wa = newFakeWA()
	result, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	beforeClose := strings.Split(strings.TrimSpace(output.String()), "\n")
	stopped := false
	for _, line := range beforeClose {
		var e struct {
			Event string `json:"event"`
			Data  struct {
				ExecutionID  string                 `json:"execution_id"`
				Observations DiagnosticObservations `json:"observations"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal("events contain non-NDJSON")
		}
		if e.Event == "sync_stopped" {
			stopped = true
			if e.Data.ExecutionID != result.recovery.diagnostic.sync.ExecutionID || e.Data.Observations.Sync.CleanupAt != nil {
				t.Fatal("stop miscorrelated or prematurely finalized cleanup")
			}
		}
		if e.Event == "sync_observations_finalized" {
			t.Fatal("cleanup event preceded Close")
		}
	}
	if !stopped {
		t.Fatal("missing stop event")
	}
	a.Close()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var final struct {
		Event string `json:"event"`
		Data  struct {
			ExecutionID string          `json:"execution_id"`
			Observation SyncObservation `json:"observation"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &final); err != nil || final.Event != "sync_observations_finalized" || final.Data.ExecutionID != result.recovery.diagnostic.sync.ExecutionID || final.Data.Observation.CleanupAt == nil {
		t.Fatal("missing correlated final cleanup checkpoint")
	}
}

func TestDiagnosticStreamErrorDoesNotReinterpretConfirmedLogin(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	f := newFakeWA()
	a.wa = f
	if err := a.Connect(t.Context(), false, nil); err != nil {
		t.Fatal(err)
	}
	f.emit(&events.StreamError{Code: "private 15551234"})
	a.Close()
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	c := ReadDiagnosticObservations(ro).Connection
	if c == nil || c.LastEvent != "connection_error" || c.ErrorAt == nil || c.RejectedAt != nil || c.LoginConfirmedAt == nil {
		t.Fatal("stream error confused with login rejection")
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "15551234") {
		t.Fatal("stream error leaked")
	}
}
