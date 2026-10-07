package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

// Pause an admitted recovery before SQLite prepares its conditional INSERT,
// without exposing the store's raw writer connection to app callers.
type pausedDiagnosticRecovery struct {
	*store.DB
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *pausedDiagnosticRecovery) RecoverDiagnosticSnapshot(ctx context.Context, slot, id, previous string, payload []byte) error {
	p.once.Do(func() {
		close(p.entered)
		select {
		case <-p.resume:
		case <-ctx.Done():
		}
	})
	return p.DB.RecoverDiagnosticSnapshot(ctx, slot, id, previous, payload)
}

func TestDiagnosticInFlightFailedStartRecoveryVersusNewRun(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	a.wa = newFakeWA()
	if _, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_start BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='sync' BEGIN SELECT RAISE(ABORT,'synthetic'); END`)
	var oldContext context.Context
	old, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeFollow, AfterConnect: func(ctx context.Context) error {
		oldContext = ctx
		return errors.New("synthetic old stop")
	}})
	if err == nil {
		t.Fatal("old fixture did not stop")
	}
	oldID := old.recovery.diagnostic.sync.ExecutionID
	// The older write can recover while the competing run's Start still fails.
	// Its ID is generated hex, not account or message content.
	evidenceFixtureSQL(t, a, `DROP TRIGGER fail_diagnostic_start`)
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_start BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='sync' AND NEW.execution_id!='`+oldID+`' BEGIN SELECT RAISE(ABORT,'synthetic'); END`)
	barrier := &pausedDiagnosticRecovery{DB: a.db, entered: make(chan struct{}), resume: make(chan struct{})}
	old.recovery.diagnostic.recoveryWriter = barrier
	var release sync.Once
	defer release.Do(func() { close(barrier.resume) })
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		recordAppStateRecovery(oldContext, "regular_low", appStateRecoverySnapshot, context.Canceled)
	}()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("older recovery did not reach the write barrier")
	}
	// If the fence is free, exercise the reported ordering completely: newer
	// Start/stop fail against the shared predecessor before the older write resumes.
	// If held, the competing Sync must advance after the older write finishes.
	fenceHeld := !a.waMu.TryLock()
	if !fenceHeld {
		a.waMu.Unlock()
	}
	type syncReply struct {
		result SyncResult
		err    error
	}
	latestDone := make(chan syncReply, 1)
	runLatest := func() {
		result, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
			return errors.New("synthetic latest stop")
		}})
		latestDone <- syncReply{result, err}
	}
	if fenceHeld {
		go runLatest()
	} else {
		runLatest()
	}
	release.Do(func() { close(barrier.resume) })
	<-oldDone
	latest := <-latestDone
	if latest.err == nil || !latest.result.recovery.diagnostic.sync.PersistenceUnconfirmed {
		t.Fatal("newer fixture did not retain its failed-start observation")
	}
	evidenceFixtureSQL(t, a, `DROP TRIGGER fail_diagnostic_start`)
	a.Close()
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	saved := ReadDiagnosticObservations(ro)
	if saved.Error != nil || saved.Sync == nil || saved.Sync.ExecutionID != latest.result.recovery.diagnostic.sync.ExecutionID || saved.Sync.CleanupAt == nil || !saved.Sync.PersistenceUnconfirmed {
		t.Fatalf("in-flight older recovery obstructed newer cleanup retention (fence held=%v): %+v", fenceHeld, saved.Sync)
	}
	for _, observation := range saved.Sync.RecoveryObservations {
		for _, outcome := range observation.Outcomes {
			if outcome == AppStateRecoveryCancelled {
				t.Fatal("older in-flight recovery contaminated the newer execution")
			}
		}
	}
}

func TestDiagnosticTransientStartFailureRecoversAtLaterCheckpoint(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "previous_execution"}[previous], func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			a.wa = newFakeWA()
			if previous {
				_, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
			}
			evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_start BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='sync' BEGIN SELECT RAISE(ABORT,'private start failure'); END`)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
				evidenceFixtureSQL(t, a, `DROP TRIGGER fail_diagnostic_start`)
				cancel()
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			a.Close()
			ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			saved := ReadDiagnosticObservations(ro)
			if saved.Error != nil || saved.Sync == nil || saved.Sync.ExecutionID != result.recovery.diagnostic.sync.ExecutionID || saved.Sync.State != "stopped" || saved.Sync.CleanupAt == nil || !saved.Sync.PersistenceUnconfirmed || saved.Connection == nil || saved.Sync.ConnectionExecutionID != saved.Connection.ExecutionID {
				t.Fatalf("transient start failure did not recover bounded correlated retention: %+v", saved)
			}
		})
	}
}

func TestDiagnosticFailedOlderStartCannotRecoverOverNewerExecution(t *testing.T) {
	for _, newerSaved := range []bool{false, true} {
		t.Run(map[bool]string{false: "both_starts_failed", true: "newer_start_saved"}[newerSaved], func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			a.wa = newFakeWA()
			evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_diagnostic_start BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='sync' BEGIN SELECT RAISE(ABORT,'private start failure'); END`)
			var oldContext context.Context
			old, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeFollow, AfterConnect: func(ctx context.Context) error { oldContext = ctx; return errors.New("synthetic stop") }})
			if err == nil {
				t.Fatal("fixture did not stop")
			}
			if newerSaved {
				evidenceFixtureSQL(t, a, `DROP TRIGGER fail_diagnostic_start`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			latest, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
				if !newerSaved {
					evidenceFixtureSQL(t, a, `DROP TRIGGER fail_diagnostic_start`)
				}
				recordAppStateRecovery(oldContext, "regular_low", appStateRecoverySnapshot, context.Canceled)
				cancel()
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			a.Close()
			ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			saved := ReadDiagnosticObservations(ro)
			if saved.Sync == nil || saved.Sync.ExecutionID != latest.recovery.diagnostic.sync.ExecutionID || saved.Sync.ExecutionID == old.recovery.diagnostic.sync.ExecutionID || saved.Sync.CleanupAt == nil {
				t.Fatal("older failed start replaced or obstructed newer execution")
			}
			for _, o := range saved.Sync.RecoveryObservations {
				for _, outcome := range o.Outcomes {
					if outcome == AppStateRecoveryCancelled {
						t.Fatal("old outcome contaminated newer execution")
					}
				}
			}
			if !newerSaved && !saved.Sync.PersistenceUnconfirmed {
				t.Fatal("recovered newer start erased prior persistence failure")
			}
		})
	}
}
