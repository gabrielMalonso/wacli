package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func TestDoctorAppStateDiagnostics(t *testing.T) {
	for _, mode := range []string{"empty", "pending", "query_failure", "missing", "schema"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if mode != "missing" {
				db, err := store.Open(filepath.Join(dir, "wacli.db"))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "pending" {
					for _, collection := range []string{"regular_low", "regular_high", "regular", "regular_low"} {
						if err := db.MarkAppStateRecoveryRequired(collection); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if mode == "query_failure" || mode == "schema" {
					raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
					if err != nil {
						t.Fatal(err)
					}
					query := "ALTER TABLE app_state_recovery_intents RENAME TO hidden_fixture_intents"
					if mode == "schema" {
						query = "DELETE FROM schema_migrations WHERE version = (SELECT MAX(version) FROM schema_migrations)"
					}
					_, err = raw.Exec(query)
					_ = raw.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			before := snapshotLocalStore(t, dir)
			for _, format := range []string{"human", "json", "compact", "full"} {
				args := []string{"--store", dir, "--read-only", "doctor"}
				switch format {
				case "json":
					args = append(args, "--json")
				case "compact", "full":
					args = append(args, "--agent", "--detail", format)
				}
				stdout, stderr, err := runAgentTest(t, args...)
				if (mode == "missing" || mode == "schema") && (format == "compact" || format == "full") {
					if commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" || stdout != "" {
						t.Fatal("changed agent unavailable-store contract")
					}
					continue
				}
				if err != nil || stderr != "" {
					t.Fatalf("doctor %s changed exit: %v", format, err)
				}
				want := app.AppStateReconciliationUnknown
				if mode == "empty" {
					want = app.AppStateReconciliationNoneRecorded
				} else if mode == "pending" {
					want = app.AppStateReconciliationRequired
				}
				if format == "human" {
					if !strings.Contains(stdout, "APP_STATE_RECONCILIATION") || !strings.Contains(stdout, string(want)) {
						t.Fatal("missing human app-state diagnostic")
					}
					if want == app.AppStateReconciliationUnknown && !strings.Contains(stdout, "recovery_state_unavailable") {
						t.Fatal("missing sanitized human observation error")
					}
					continue
				}
				var envelope struct {
					Success bool `json:"success"`
					Data    struct {
						AppState app.AppStateDiagnostics `json:"app_state"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || !envelope.Success {
					t.Fatal("invalid doctor envelope")
				}
				state := envelope.Data.AppState
				if state.Reconciliation != want || state.RecoveryObservations != nil {
					t.Fatalf("doctor invented recovery history: %+v", state)
				}
				if want == app.AppStateReconciliationUnknown {
					if state.PendingCollections != nil || state.Error == nil || state.Error.Code != "recovery_state_unavailable" {
						t.Fatal("unavailable query became an empty debt list")
					}
				} else {
					if state.PendingCollections == nil || state.Error != nil {
						t.Fatal("successful query lacks known collection list")
					}
					if mode == "pending" && !slices.Equal(state.PendingCollections, []string{"regular", "regular_high", "regular_low"}) {
						t.Fatal("pending collections are not sorted and distinct")
					}
				}
				if format == "compact" || format == "full" {
					env := decodeAgentTest(t, stdout)
					if env.Meta.Source != "local" || env.Meta.Freshness != "unknown" || env.Meta.Completeness != "unknown" {
						t.Fatal("diagnostic fabricated remote freshness/completeness")
					}
				}
				if mode == "query_failure" && (strings.Contains(stdout, "no such table") || strings.Contains(stdout, "hidden_fixture") || strings.Contains(stdout, "list app state recovery")) {
					t.Fatal("diagnostic leaked SQL details")
				}
			}
			if !reflect.DeepEqual(snapshotLocalStore(t, dir), before) {
				t.Fatal("diagnostic changed fixture hashes, modes or file set")
			}
		})
	}
}

func TestDoctorAppStateReadonlyWithLiveWALAndOwnerLock(t *testing.T) {
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
	if err := db.MarkAppStateRecoveryRequired("regular_low"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "wacli.db-wal"))
	if err != nil || info.Size() == 0 {
		t.Fatal("fixture lacks live WAL debt")
	}
	before := snapshotLocalStore(t, dir)
	for _, format := range []string{"json", "compact", "full"} {
		args := []string{"--store", dir, "--read-only", "doctor", "--json"}
		if format != "json" {
			args = append(args, "--agent", "--detail", format)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatal("readonly diagnostic refused an occupied writer lock")
		}
		var env struct {
			Data struct {
				LockHeld bool                    `json:"lock_held"`
				AppState app.AppStateDiagnostics `json:"app_state"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil || !env.Data.LockHeld || !slices.Equal(env.Data.AppState.PendingCollections, []string{"regular_low"}) {
			t.Fatal("diagnostic missed committed debt in WAL")
		}
	}
	if !reflect.DeepEqual(snapshotLocalStore(t, dir), before) {
		t.Fatal("diagnostic changed live WAL data, LOCK or permissions")
	}
	if _, err := os.Stat(filepath.Join(dir, "session.db")); !os.IsNotExist(err) {
		t.Fatal("app-state diagnosis initialized a WhatsApp session")
	}
	if held, _, err := lock.Probe(dir); err != nil || !held {
		t.Fatal("diagnostic released the fixture owner's lock")
	}
}

func TestSyncAppStateResultOutput(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		state := app.ReadAppStateDiagnostics(nil)
		if !unavailable {
			state = app.AppStateDiagnostics{Reconciliation: app.AppStateReconciliationNoneRecorded, PendingCollections: []string{}, RecoveryObservations: []app.AppStateRecoveryObservation{}}
		}
		var output bytes.Buffer
		if err := writeSyncResult(&output, true, app.SyncResult{}, state); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Success bool `json:"success"`
			Data    struct {
				Synced         bool                    `json:"synced"`
				MessagesStored int64                   `json:"messages_stored"`
				AppState       app.AppStateDiagnostics `json:"app_state"`
			} `json:"data"`
		}
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || !envelope.Success || !envelope.Data.Synced || envelope.Data.MessagesStored != 0 {
			t.Fatal("changed legacy successful-stop contract")
		}
		if (envelope.Data.AppState.PendingCollections == nil) != unavailable {
			t.Fatal("unknown/null and known/empty collections were conflated")
		}
		output.Reset()
		if err := writeSyncResult(&output, false, app.SyncResult{}, state); err != nil || !strings.Contains(output.String(), "Messages stored: 0") || !strings.Contains(output.String(), "APP_STATE_RECONCILIATION") {
			t.Fatal("missing human sync result")
		}
	}
}
