package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

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
