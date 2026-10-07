package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

func TestDoctorHistoricalObservationsReadonlyWAL(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	connectionID, syncID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	c := app.ConnectionObservation{ExecutionID: connectionID, StartedAt: at, ObservedAt: at, LastEvent: "login_confirmed", LoginConfirmedAt: &at, ClosedAt: &at}
	s := app.SyncObservation{ExecutionID: syncID, ConnectionExecutionID: connectionID, Mode: app.SyncModeOnce, StartedAt: at, ObservedAt: at, State: "stopped", StopReason: "idle", StoppedAt: &at, CleanupAt: &at, RecoveryObservations: []app.AppStateRecoveryObservation{{Collection: "regular_low", Phase: "snapshot", Outcomes: []app.AppStateRecoveryOutcome{app.AppStateRecoveryCompleted, app.AppStateRecoveryFailed}, ErrorCodes: []string{"recovery_failed"}, FirstObservedAt: &at, LastObservedAt: &at, FailedAt: &at, CompletedAt: &at}}}
	for _, slot := range []struct {
		name, id string
		value    any
	}{{"connection", connectionID, c}, {"sync", syncID, s}} {
		raw, _ := json.Marshal(slot.value)
		if err := db.StartDiagnosticSnapshot(t.Context(), slot.name, slot.id, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MarkAppStateRecoveryRequired("regular_low"); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, format := range []string{"human", "json", "compact", "full"} {
		args := []string{"--store", dir, "--read-only", "doctor"}
		if format == "json" {
			args = append(args, "--json")
		}
		if format == "compact" || format == "full" {
			args = append(args, "--agent", "--detail", format)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatalf("readonly doctor %s: %v %s", format, err, stderr)
		}
		if format == "human" {
			if !strings.Contains(stdout, "historical checkpoints") || !strings.Contains(stdout, "RETAINED_RECOVERY") || !strings.Contains(stdout, "completed, failed") {
				t.Fatal("human evidence absent")
			}
			continue
		}
		var report struct {
			Data struct {
				Observations app.DiagnosticObservations `json:"observations"`
				Connected    bool                       `json:"connected"`
				Auth         agentAuth                  `json:"auth"`
				LockHeld     bool                       `json:"lock_held"`
				AppState     app.AppStateDiagnostics    `json:"app_state"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatal(err)
		}
		obs := report.Data.Observations
		if !obs.Historical || obs.Version != 1 || obs.Sync == nil || obs.Connection == nil || obs.Sync.ExecutionID != syncID || obs.Connection.ExecutionID != connectionID || report.Data.Connected || !report.Data.LockHeld || report.Data.AppState.RecoveryObservations != nil {
			t.Fatal("historical evidence reinterpreted legacy fields or correlation")
		}
		if format != "json" {
			env := decodeAgentTest(t, stdout)
			if report.Data.Auth.Connected != "unknown" || env.Meta.Source != "local" || env.Meta.Freshness != "unknown" || env.Meta.Completeness != "unknown" {
				t.Fatal("historical checkpoint certified current health")
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("readonly diagnostics changed archive/LOCK")
	}
}

func TestDoctorObservationQueryErrorKeepsExistingExit(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	retryFixtureSQL(t, dir, `ALTER TABLE diagnostic_snapshots RENAME TO hidden_private`)
	for _, format := range []string{"json", "compact", "full"} {
		args := []string{"--store", dir, "--read-only", "doctor", "--json"}
		if format != "json" {
			args = append(args, "--agent", "--detail", format)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatal("observation-only failure changed exit")
		}
		if !strings.Contains(stdout, `"code":"diagnostics_unavailable"`) || strings.Contains(stdout, "hidden_private") || strings.Contains(stdout, "no such table") {
			t.Fatal("invalid sanitized unavailable report")
		}
	}
}

func TestDoctorConnectInvocationEvidenceSurvivesCheckpointFailure(t *testing.T) {
	for _, failure := range []string{"start", "save"} {
		for _, format := range []string{"json", "human"} {
			t.Run(failure+"/"+format, func(t *testing.T) {
				dir := t.TempDir()
				db, err := store.Open(filepath.Join(dir, "wacli.db"))
				if err != nil {
					t.Fatal(err)
				}
				oldAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
				oldID, syncID := strings.Repeat("1", 32), strings.Repeat("2", 32)
				old := app.ConnectionObservation{ExecutionID: oldID, StartedAt: oldAt, ObservedAt: oldAt, LastEvent: "disconnected", DisconnectedAt: &oldAt}
				priorSync := app.SyncObservation{ExecutionID: syncID, Mode: app.SyncModeOnce, StartedAt: oldAt, ObservedAt: oldAt, State: "stopped", StopReason: "idle", RecoveryObservations: []app.AppStateRecoveryObservation{}}
				raw, _ := json.Marshal(old)
				if err := db.StartDiagnosticSnapshot(t.Context(), "connection", oldID, raw); err != nil {
					t.Fatal(err)
				}
				raw, _ = json.Marshal(priorSync)
				if err := db.StartDiagnosticSnapshot(t.Context(), "sync", syncID, raw); err != nil {
					t.Fatal(err)
				}
				db.Close()
				query := `CREATE TRIGGER fail_current_connection BEFORE INSERT ON diagnostic_snapshots WHEN NEW.slot='connection' BEGIN SELECT RAISE(ABORT,'private SDK 15551234'); END`
				if failure == "save" {
					query = `CREATE TRIGGER fail_current_connection BEFORE UPDATE ON diagnostic_snapshots WHEN NEW.slot='connection' BEGIN SELECT RAISE(ABORT,'private SDK 15551234'); END`
				}
				if failure == "start" {
					retryFixtureSQL(t, dir, query)
				}
				flags := &rootFlags{storeDir: dir, asJSON: format == "json", events: true, timeout: time.Second}
				fake := &agentChatStateWA{}
				cmd := newDoctorCmdWithApp(flags, func(ctx context.Context, flags *rootFlags, needLock, allowUnauthed bool) (*app.App, *lock.Lock, error) {
					lk, err := lock.AcquireWithTimeout(ctx, dir, flags.lockWait)
					if err != nil {
						return nil, nil, err
					}
					a, err := app.New(app.Options{StoreDir: dir, AllowUnauthed: allowUnauthed, Events: out.NewEventWriter(os.Stderr, flags.events), WAFactory: func(wa.Options) (app.WAClient, error) { return fake, nil }})
					if err != nil {
						lk.Release()
						return nil, nil, err
					}
					if failure == "save" {
						if err := a.OpenWA(); err != nil {
							a.Close()
							lk.Release()
							return nil, nil, err
						}
						retryFixtureSQL(t, dir, query)
					}
					return a, lk, nil
				})
				cmd.SetArgs([]string{"--connect"})
				var stdout string
				stderr := captureRootStderr(t, func() { stdout = captureRootStdout(t, func() { err = cmd.Execute() }) })
				if err != nil {
					t.Fatal(err)
				}
				var warning struct {
					Event string `json:"event"`
					Data  struct {
						Code        string `json:"code"`
						Slot        string `json:"slot"`
						ExecutionID string `json:"execution_id"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &warning); err != nil || warning.Event != "warning" || warning.Data.Code != "diagnostics_persistence_unconfirmed" || warning.Data.Slot != "connection" || warning.Data.ExecutionID == "" || warning.Data.ExecutionID == oldID {
					t.Fatalf("uncorrelated persistence warning: %s", stderr)
				}
				if strings.Contains(stderr, "private") || strings.Contains(stderr, "15551234") {
					t.Fatal("warning leaked private cause")
				}
				if format == "json" {
					var report struct {
						Success bool         `json:"success"`
						Data    doctorReport `json:"data"`
					}
					if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Success || !report.Data.Connected {
						t.Fatal("confirmed login changed legacy output")
					}
					current := report.Data.InvocationConnection
					saved := report.Data.Observations.Connection
					if current == nil || current.ExecutionID != warning.Data.ExecutionID || !current.PersistenceUnconfirmed || current.LoginConfirmedAt == nil || current.LastEvent != "login_confirmed" || saved == nil || report.Data.Observations.Sync.ExecutionID != syncID {
						t.Fatal("invocation evidence hidden by saved checkpoint")
					}
					if failure == "start" && (saved.ExecutionID != oldID || saved.LastEvent != "disconnected" || saved.PersistenceUnconfirmed) {
						t.Fatal("historical checkpoint reinterpreted as current")
					}
					if failure == "save" && (saved.ExecutionID != current.ExecutionID || saved.LastEvent != "unobserved" || saved.PersistenceUnconfirmed) {
						t.Fatal("failed save was reported as retained login")
					}
				} else if !strings.Contains(stdout, "INVOCATION_CONNECTION_EXECUTION") || !strings.Contains(stdout, warning.Data.ExecutionID) || !strings.Contains(stdout, "INVOCATION_CONNECTION_PERSISTENCE") || !strings.Contains(stdout, "unconfirmed") {
					t.Fatal("human invocation correlation absent")
				}
				// Offline agent/legacy readers keep only the saved historical slot, never
				// fabricate this invocation's unsaved memory or current connectivity.
				for _, detail := range []string{"json", "compact", "full"} {
					args := []string{"--store", dir, "--read-only", "doctor", "--json"}
					if detail != "json" {
						args = append(args, "--agent", "--detail", detail)
					}
					stdout, stderr, err := runAgentTest(t, args...)
					if err != nil || stderr != "" || strings.Contains(stdout, "invocation_connection") {
						t.Fatal("offline reader fabricated invocation facts")
					}
					if detail != "json" {
						env := decodeAgentTest(t, stdout)
						if env.Meta.Freshness != "unknown" || env.Meta.Completeness != "unknown" {
							t.Fatal("checkpoint failure implied freshness")
						}
					}
				}
			})
		}
	}
}
