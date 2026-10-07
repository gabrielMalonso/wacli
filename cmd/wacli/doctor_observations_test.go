package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
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
