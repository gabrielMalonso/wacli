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
	"testing/synctest"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func requireRecoveryObservation(t *testing.T, state AppStateDiagnostics, collection string, phase AppStateRecoveryPhase, outcome AppStateRecoveryOutcome, code string) {
	t.Helper()
	for _, observation := range state.RecoveryObservations {
		if observation.Collection == collection && observation.Phase == phase {
			if !slices.Contains(observation.Outcomes, outcome) || (code != "" && !slices.Contains(observation.ErrorCodes, code)) {
				t.Fatalf("unexpected recovery observation: %+v", observation)
			}
			return
		}
	}
	t.Fatalf("missing %s/%s/%s observation", collection, phase, outcome)
}

func TestSyncAppStateDiagnostics(t *testing.T) {
	for _, mode := range []string{"failed", "full_completed", "snapshot_unconfirmed", "cancelled", "preventive"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			f := &appStateContextWA{fakeWA: newFakeWA()}
			a.wa = f
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			f.fetchAppState = func(_ context.Context, stringName string, _, _ bool) error {
				if stringName != "regular_low" {
					return nil
				}
				switch mode {
				case "failed", "snapshot_unconfirmed":
					return appstate.ErrMismatchingLTHash
				case "cancelled":
					cancel()
					return context.Canceled
				default:
					return nil
				}
			}
			f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
				if mode == "snapshot_unconfirmed" {
					f.emit(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularLow, Recovery: true})
					return "fixture", nil
				}
				return "", errors.Join(context.DeadlineExceeded, errors.New("private SQL fixture /hidden/path"))
			}
			if mode != "preventive" {
				if err := a.db.MarkAppStateRecoveryRequired("regular_low"); err != nil {
					t.Fatal(err)
				}
			}
			f.connectEvents = []any{&events.OfflineSyncCompleted{Count: 12}}
			result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
			if err != nil || result.MessagesStored != 0 {
				t.Fatalf("legacy sync result: err=%v stored=%d", err, result.MessagesStored)
			}
			a.Close()
			a.Close() // Explicit close plus deferred close remains idempotent.
			state := result.AppStateSnapshot()
			if state.Reconciliation != AppStateReconciliationRequired || !slices.Equal(state.PendingCollections, []string{"regular", "regular_high", "regular_low"}) {
				t.Fatalf("missing post-close preventive debt: %+v", state)
			}
			switch mode {
			case "failed":
				assertRecoveryFailure(t, state)
			case "full_completed":
				requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryCompleted, "")
				requireRecoveryObservation(t, state, "regular_low", appStateRecoveryCheckpoint, AppStateRecoveryCompleted, "")
			case "snapshot_unconfirmed":
				requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryFailed, "lthash_mismatch")
				requireRecoveryObservation(t, state, "regular_low", appStateRecoverySnapshot, AppStateRecoveryUnconfirmed, "completion_unconfirmed")
			case "cancelled":
				requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryCancelled, "cancelled")
			case "preventive":
				if state.RecoveryObservations == nil || len(state.RecoveryObservations) != 0 {
					t.Fatal("preventive shutdown fabricated a recovery outcome")
				}
			}
			raw, err := json.Marshal(state)
			if err != nil || strings.Contains(string(raw), "private") || strings.Contains(string(raw), "/hidden") {
				t.Fatal("diagnostic leaked a private cause")
			}
		})
	}
}

func assertRecoveryFailure(t *testing.T, state AppStateDiagnostics) {
	t.Helper()
	requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryFailed, "lthash_mismatch")
	requireRecoveryObservation(t, state, "regular_low", appStateRecoverySnapshot, AppStateRecoveryFailed, "deadline_exceeded")
}

func TestSyncAppStateRetainsSDKFailureAfterRecovery(t *testing.T) {
	for _, fullSync := range []bool{false, true} {
		a := newTestApp(t)
		a.opts.Events = out.NewEventWriter(io.Discard, true)
		a.wa = newFakeWA()
		a.appStateReplayOnClose = true
		run := &appStateRecoveryRun{}
		ctx := context.WithValue(t.Context(), appStateRecoveryRunKey{}, run)
		a.handleAppStateSyncError(ctx, &events.AppStateSyncError{Name: appstate.WAPatchRegularLow, FullSync: fullSync, Error: appstate.ErrMismatchingLTHash}, &sync.Map{})
		a.appStateRecoveryWorkers.Wait()
		a.Close()
		state := (SyncResult{recovery: run, storeDir: a.StoreDir()}).AppStateSnapshot()
		phase := appStateRecoveryDelta
		if fullSync {
			phase = appStateRecoveryFullSync
		}
		requireRecoveryObservation(t, state, "regular_low", phase, AppStateRecoveryFailed, "lthash_mismatch")
		requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryCompleted, "")
		if len(state.PendingCollections) != 3 {
			t.Fatal("successful repair lost normal preventive shutdown debt")
		}
	}
}

func TestAppStateRecoveryFactsAreBoundedAndKeepEarlierFailure(t *testing.T) {
	run := &appStateRecoveryRun{}
	ctx := context.WithValue(t.Context(), appStateRecoveryRunKey{}, run)
	for range 100 {
		recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, context.DeadlineExceeded)
		recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, context.Canceled)
		recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, nil)
		recordAppStateRecovery(ctx, "unrecognized/private-collection", appStateRecoverySnapshot, errors.New("private"))
	}
	observations := run.snapshot()
	if len(observations) != 1 || !slices.Equal(observations[0].Outcomes, []AppStateRecoveryOutcome{AppStateRecoveryCompleted, AppStateRecoveryFailed, AppStateRecoveryCancelled}) || !slices.Equal(observations[0].ErrorCodes, []string{"cancelled", "deadline_exceeded"}) {
		t.Fatalf("lost facts or retained attempt history: %+v", observations)
	}
}

func TestSyncAppStateObservationsStayWithTheirExecution(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	a.wa = newFakeWA()
	var results []SyncResult
	var firstContext context.Context
	for i := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(ctx context.Context) error {
			if i == 0 {
				firstContext = ctx
			}
			cancel()
			return nil
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
	// A callback retaining the earlier context must not update the later result.
	recordAppStateRecovery(firstContext, "regular_high", appStateRecoverySnapshot, context.DeadlineExceeded)
	a.Close()
	requireRecoveryObservation(t, results[0].AppStateSnapshot(), "regular_high", appStateRecoverySnapshot, AppStateRecoveryFailed, "deadline_exceeded")
	for _, observation := range results[1].AppStateSnapshot().RecoveryObservations {
		if slices.Contains(observation.Outcomes, AppStateRecoveryFailed) {
			t.Fatal("callback was attributed to a later sync invocation")
		}
	}
}

func TestSyncAppStateSnapshotIncludesRecoveryDrainedDuringClose(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	run := &appStateRecoveryRun{}
	result := SyncResult{recovery: run, storeDir: a.StoreDir()}
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		defer a.Close()
		defer releaseOnce.Do(func() { close(release) })
		f := &recoveryCloseWA{appStateContextWA: &appStateContextWA{fakeWA: newFakeWA()}, disconnected: make(chan struct{})}
		f.fetchEvents = func(context.Context, string, bool, bool) ([]any, error) {
			close(started)
			<-release
			return nil, nil
		}
		a.wa = f
		a.appStateReplayOnClose = true
		ctx := context.WithValue(t.Context(), appStateRecoveryRunKey{}, run)
		a.handleAppStateSyncError(ctx, &events.AppStateSyncError{Name: appstate.WAPatchRegularLow, Error: appstate.ErrMismatchingLTHash}, &sync.Map{})
		<-started
		closed := make(chan struct{})
		go func() { a.Close(); close(closed) }()
		<-f.disconnected
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close did not wait for admitted recovery")
		default:
		}
		releaseOnce.Do(func() { close(release) })
		synctest.Wait()
		<-closed
	})
	state := result.AppStateSnapshot()
	requireRecoveryObservation(t, state, "regular_low", appStateRecoveryFullSync, AppStateRecoveryCompleted, "")
	if len(state.PendingCollections) != 3 {
		t.Fatal("snapshot preceded Close replay restoration")
	}
}

func TestSyncAppStateShutdownMarkerFailurePreservesLegacyError(t *testing.T) {
	for _, phase := range []string{"sync", "close"} {
		t.Run(phase, func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			a.wa = newFakeWA()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failMarker := func() {
				evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_marker BEFORE INSERT ON app_state_recovery_intents BEGIN SELECT RAISE(ABORT,'private marker failure'); END`)
			}
			result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
				if phase == "sync" {
					failMarker()
				}
				cancel()
				return nil
			}})
			if (err != nil) != (phase == "sync") {
				t.Fatalf("changed legacy marker error: %v", err)
			}
			if phase == "close" {
				for _, collection := range mirroredAppStateCollections {
					if err := a.db.ClearAppStateRecoveryRequired(string(collection)); err != nil {
						t.Fatal(err)
					}
				}
				failMarker()
			}
			a.Close()
			state := result.AppStateSnapshot()
			requireRecoveryObservation(t, state, "regular_low", appStateRecoveryShutdown, AppStateRecoveryFailed, "recovery_failed")
			if state.Reconciliation != AppStateReconciliationNoneRecorded || state.Error != nil {
				t.Fatal("successful empty debt query was confused with marker write failure")
			}
		})
	}
}

func TestSyncAppStateSnapshotUnavailableRetainsRunFacts(t *testing.T) {
	for _, mode := range []string{"query_failure", "missing", "schema"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t)
			run := &appStateRecoveryRun{}
			ctx := context.WithValue(t.Context(), appStateRecoveryRunKey{}, run)
			recordAppStateRecovery(ctx, "regular_low", appStateRecoverySnapshot, context.DeadlineExceeded)
			result := SyncResult{recovery: run, storeDir: a.StoreDir()}
			switch mode {
			case "query_failure":
				evidenceFixtureSQL(t, a, `ALTER TABLE app_state_recovery_intents RENAME TO hidden_private_fixture`)
			case "schema":
				evidenceFixtureSQL(t, a, `DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)`)
			}
			a.Close()
			if mode == "missing" {
				if err := os.Rename(filepath.Join(a.StoreDir(), "wacli.db"), filepath.Join(a.StoreDir(), "fixture.saved")); err != nil {
					t.Fatal(err)
				}
			}
			state := result.AppStateSnapshot()
			if state.Reconciliation != AppStateReconciliationUnknown || state.PendingCollections != nil || state.Error == nil || state.Error.Code != "recovery_state_unavailable" {
				t.Fatalf("unavailable observation became known empty debt: %+v", state)
			}
			requireRecoveryObservation(t, state, "regular_low", appStateRecoverySnapshot, AppStateRecoveryFailed, "deadline_exceeded")
			raw, err := json.Marshal(state)
			if err != nil || strings.Contains(string(raw), "private") || strings.Contains(string(raw), a.StoreDir()) || strings.Contains(string(raw), "no such table") {
				t.Fatal("unavailable observation exposed a SQL cause or path")
			}
		})
	}
}
