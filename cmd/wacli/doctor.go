package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appPkg "github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func parseLockOwnerPID(lockInfo string) int {
	for _, line := range strings.Split(lockInfo, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pid=") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid=")))
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

func doctorConnectionState(authed, connected, lockHeld, connect, sessionRevoked bool) string {
	switch {
	case sessionRevoked:
		return "logged_out"
	case connected:
		return "connected"
	case authed && lockHeld && !connect:
		return "locked_by_other_process"
	default:
		return "disconnected"
	}
}

type doctorStoreStats struct {
	StatsKnown     bool   `json:"-"`
	Messages       int64  `json:"messages"`
	Chats          int64  `json:"chats"`
	Contacts       int64  `json:"contacts"`
	Groups         int64  `json:"groups"`
	LastSyncAt     string `json:"last_sync_at,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

func (s doctorStoreStats) MarshalJSON() ([]byte, error) {
	type storeStatsJSON struct {
		Messages       *int64 `json:"messages,omitempty"`
		Chats          *int64 `json:"chats,omitempty"`
		Contacts       *int64 `json:"contacts,omitempty"`
		Groups         *int64 `json:"groups,omitempty"`
		LastSyncAt     string `json:"last_sync_at,omitempty"`
		LastActivityAt string `json:"last_activity_at,omitempty"`
	}
	out := storeStatsJSON{
		LastSyncAt:     s.LastSyncAt,
		LastActivityAt: s.LastActivityAt,
	}
	if s.StatsKnown {
		out.Messages = &s.Messages
		out.Chats = &s.Chats
		out.Contacts = &s.Contacts
		out.Groups = &s.Groups
	}
	return json.Marshal(out)
}

type doctorReport struct {
	StoreDir             string                        `json:"store_dir"`
	LockHeld             bool                          `json:"lock_held"`
	LockInfo             string                        `json:"lock_info,omitempty"`
	LockOwnerPID         int                           `json:"lock_owner_pid,omitempty"`
	Authed               bool                          `json:"authenticated"`
	SessionRevoked       bool                          `json:"session_revoked"`
	LinkedJID            string                        `json:"linked_jid,omitempty"`
	Connected            bool                          `json:"connected"`
	ConnectionState      string                        `json:"connection_state"`
	FTSEnabled           bool                          `json:"fts_enabled"`
	Store                *doctorStoreStats             `json:"store,omitempty"`
	StoreError           string                        `json:"store_error,omitempty"`
	AppState             appPkg.AppStateDiagnostics    `json:"app_state"`
	Observations         appPkg.DiagnosticObservations `json:"observations"`
	InvocationConnection *appPkg.ConnectionObservation `json:"invocation_connection,omitempty"`
}

func doctorStoreStatsFromStoreStats(stats store.StoreStats) doctorStoreStats {
	out := doctorStoreStats{
		StatsKnown: true,
		Messages:   stats.Messages,
		Chats:      stats.Chats,
		Contacts:   stats.Contacts,
		Groups:     stats.Groups,
	}
	if stats.LastMessageTS > 0 {
		out.LastSyncAt = time.Unix(stats.LastMessageTS, 0).UTC().Format(time.RFC3339)
	}
	return out
}

func writeDoctorReport(w io.Writer, rep doctorReport) {
	tw := newTableWriter(w)
	fmt.Fprintf(tw, "STORE\t%s\n", sanitize(rep.StoreDir))
	fmt.Fprintf(tw, "LOCKED\t%v\n", rep.LockHeld)
	if rep.LockHeld && rep.LockInfo != "" {
		fmt.Fprintf(tw, "LOCK_INFO\t%s\n", sanitize(rep.LockInfo))
	}
	if rep.LockOwnerPID > 0 {
		fmt.Fprintf(tw, "LOCK_OWNER_PID\t%d\n", rep.LockOwnerPID)
	}
	fmt.Fprintf(tw, "AUTHENTICATED\t%v\n", rep.Authed)
	fmt.Fprintf(tw, "SESSION_REVOKED\t%v\n", rep.SessionRevoked)
	if rep.LinkedJID != "" {
		fmt.Fprintf(tw, "LINKED_JID\t%s\n", sanitize(rep.LinkedJID))
	}
	fmt.Fprintf(tw, "CONNECTED\t%v\n", rep.Connected)
	fmt.Fprintf(tw, "CONNECTION_STATE\t%s\n", sanitize(rep.ConnectionState))
	fmt.Fprintf(tw, "FTS5\t%v\n", rep.FTSEnabled)
	if rep.Store != nil {
		if rep.Store.StatsKnown {
			fmt.Fprintf(tw, "MESSAGES\t%d\n", rep.Store.Messages)
			fmt.Fprintf(tw, "CHATS\t%d\n", rep.Store.Chats)
			fmt.Fprintf(tw, "CONTACTS\t%d\n", rep.Store.Contacts)
			fmt.Fprintf(tw, "GROUPS\t%d\n", rep.Store.Groups)
		}
		if rep.Store.LastSyncAt != "" {
			fmt.Fprintf(tw, "LAST_SYNC\t%s\n", rep.Store.LastSyncAt)
		}
		if rep.Store.LastActivityAt != "" {
			fmt.Fprintf(tw, "LAST_ACTIVITY\t%s\n", rep.Store.LastActivityAt)
		}
	}
	writeAppStateRows(tw, rep.AppState)
	writeObservationRows(tw, rep.Observations)
	writeInvocationConnectionRows(tw, rep.InvocationConnection)
	_ = tw.Flush()
}

func writeAppStateRows(w io.Writer, state appPkg.AppStateDiagnostics) {
	fmt.Fprintf(w, "APP_STATE_RECONCILIATION\t%s\n", state.Reconciliation)
	if state.PendingCollections != nil {
		fmt.Fprintf(w, "APP_STATE_PENDING_COLLECTIONS\t%s\n", sanitize(strings.Join(state.PendingCollections, ", ")))
	}
	if state.Error != nil {
		fmt.Fprintf(w, "APP_STATE_ERROR\t%s\n", state.Error.Code)
	}
	if state.RecoveryObservations == nil {
		fmt.Fprintln(w, "APP_STATE_RECOVERY\tunknown (no invocation-local outcomes)")
	} else if len(state.RecoveryObservations) == 0 {
		fmt.Fprintln(w, "APP_STATE_RECOVERY\tnone observed in this run")
	}
	for _, observation := range state.RecoveryObservations {
		outcomes := make([]string, len(observation.Outcomes))
		for i, outcome := range observation.Outcomes {
			outcomes[i] = string(outcome)
		}
		fmt.Fprintf(w, "APP_STATE_RECOVERY\t%s %s: %s", sanitize(observation.Collection), observation.Phase, strings.Join(outcomes, ", "))
		if len(observation.ErrorCodes) > 0 {
			fmt.Fprintf(w, " (%s)", strings.Join(observation.ErrorCodes, ", "))
		}
		fmt.Fprintln(w)
	}
}

func writeAppStateHint(w io.Writer, state appPkg.AppStateDiagnostics) {
	switch state.Reconciliation {
	case appPkg.AppStateReconciliationUnknown:
		fmt.Fprintln(w, "Tip: check readability and schema compatibility of the selected local archive; app-state debt is unknown.")
	case appPkg.AppStateReconciliationRequired:
		fmt.Fprintln(w, "Tip: inspect dated recovery observations and their execution for failed/cancelled collections and phases. Debt can also be preventive after normal shutdown; command success does not certify queue integrity or freshness.")
	}
}

func newDoctorCmd(flags *rootFlags) *cobra.Command {
	return newDoctorCmdWithApp(flags, newApp)
}

// The factory keeps live-command fixtures at the existing App/WA boundary.
func newDoctorCmdWithApp(flags *rootFlags, openApp func(context.Context, *rootFlags, bool, bool) (*appPkg.App, *lock.Lock, error)) *cobra.Command {
	var connect bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnostics for store/auth/search",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.agent {
				return runAgentDoctor(cmd.Context(), flags)
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			storeDir, err := resolveStoreDir(flags)
			if err != nil {
				return err
			}

			var lockHeld bool
			var lockInfo string
			if connect {
				if err := flags.requireWritable(); err != nil {
					return err
				}
			}
			if !connect {
				if held, info, err := lock.Probe(storeDir); err == nil {
					lockHeld = held
					lockInfo = info
				} else {
					lockHeld = true
					lockInfo = readDoctorLockInfo(storeDir)
				}
			} else {
				if lk, err := lock.Acquire(storeDir); err == nil {
					_ = lk.Release()
				} else {
					lockHeld = true
					lockInfo = readDoctorLockInfo(storeDir)
				}
			}

			var storeErr string
			var db *store.DB
			var a *appPkg.App
			var closeFn func()
			if !connect {
				dbPath := filepath.Join(storeDir, "wacli.db")
				roDB, err := store.OpenReadOnly(dbPath)
				if err != nil {
					storeErr = err.Error()
				} else {
					db = roDB
					closeFn = func() { _ = roDB.Close() }
				}
			} else {
				appInstance, lk, err := openApp(ctx, flags, connect, true)
				if err != nil {
					storeErr = err.Error()
				} else {
					a = appInstance
					db = appInstance.DB()
					closeFn = func() { closeApp(appInstance, lk) }
				}
			}
			if closeFn != nil {
				defer closeFn()
			}

			var authed bool
			var connected bool
			var linkedJID string
			if !connect {
				roAuthed, roLinkedJID, err := readOnlyAuthStatus(storeDir)
				if err != nil {
					if storeErr != "" {
						storeErr += "; "
					}
					storeErr += fmt.Errorf("read authentication source: %w", err).Error()
				} else {
					authed = roAuthed
					linkedJID = roLinkedJID
				}
			} else if a != nil {
				if err := a.OpenWA(); err == nil {
					authed = a.WA().IsAuthed()
					if authed {
						linkedJID = a.WA().LinkedJID()
					}
				}
				if connect && authed {
					if err := a.Connect(ctx, false, nil); err == nil {
						connected = true
					}
				}
			}
			sessionRevoked, sessionStateErr := appPkg.SessionRevoked(storeDir)
			if sessionStateErr != nil && storeErr == "" {
				storeErr = sessionStateErr.Error()
			}
			if sessionRevoked {
				authed = false
				connected = false
				linkedJID = ""
			}
			lockOwnerPID := parseLockOwnerPID(lockInfo)

			var stats *doctorStoreStats
			var lastActivityAt string
			if hb := appPkg.ReadHeartbeat(storeDir); !hb.IsZero() {
				lastActivityAt = hb.UTC().Format(time.RFC3339)
			}
			if db != nil {
				if raw, err := db.Stats(); err == nil {
					converted := doctorStoreStatsFromStoreStats(raw)
					stats = &converted
				} else if storeErr == "" {
					storeErr = err.Error()
				}
			}
			if lastActivityAt != "" {
				if stats == nil {
					stats = &doctorStoreStats{}
				}
				stats.LastActivityAt = lastActivityAt
			}

			rep := doctorReport{
				StoreDir:        storeDir,
				LockHeld:        lockHeld,
				LockInfo:        lockInfo,
				LockOwnerPID:    lockOwnerPID,
				Authed:          authed,
				SessionRevoked:  sessionRevoked,
				LinkedJID:       linkedJID,
				Connected:       connected,
				ConnectionState: doctorConnectionState(authed, connected, lockHeld, connect, sessionRevoked),
				FTSEnabled:      db != nil && db.HasFTS(),
				Store:           stats,
				StoreError:      storeErr,
				AppState:        appPkg.ReadAppStateDiagnostics(db),
				Observations:    appPkg.ReadDiagnosticObservations(db),
			}

			rep.InvocationConnection = a.InvocationConnectionObservation()

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, rep)
			}

			writeDoctorReport(os.Stdout, rep)
			writeAppStateHint(os.Stdout, rep.AppState)

			if rep.StoreError != "" {
				fmt.Fprintf(os.Stdout, "\nERROR: store could not be opened: %s\n", sanitize(rep.StoreError))
				fmt.Fprintln(os.Stdout, "Tip: check that the store directory exists and is not corrupted.")
			}
			if rep.LockHeld {
				fmt.Fprintln(os.Stdout, "\nTip: the writer lock is occupied; inspect local state read-only.")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&connect, "connect", false, "try connecting to WhatsApp (requires store lock)")
	return cmd
}

func readDoctorLockInfo(storeDir string) string {
	b, err := os.ReadFile(filepath.Join(storeDir, "LOCK"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeObservationRows(w io.Writer, observations appPkg.DiagnosticObservations) {
	fmt.Fprintln(w, "OBSERVATIONS\thistorical checkpoints; current liveness/freshness/completeness unknown")
	if observations.Error != nil {
		fmt.Fprintf(w, "OBSERVATIONS_ERROR\t%s\n", observations.Error.Code)
	}
	if c := observations.Connection; c != nil {
		fmt.Fprintf(w, "CONNECTION_EXECUTION\t%s\nCONNECTION_LAST_EVENT\t%s\nCONNECTION_CHECKPOINT\t%s\n", c.ExecutionID, c.LastEvent, c.ObservedAt.Format(time.RFC3339Nano))
		for _, field := range []struct {
			name string
			at   *time.Time
		}{{"LOGIN_CONFIRMED_AT", c.LoginConfirmedAt}, {"DISCONNECTED_AT", c.DisconnectedAt}, {"LOGGED_OUT_AT", c.LoggedOutAt}, {"CONNECTION_CLOSED_AT", c.ClosedAt}, {"CONNECTION_REJECTED_AT", c.RejectedAt}, {"CONNECTION_ERROR_AT", c.ErrorAt}} {
			if field.at != nil {
				fmt.Fprintf(w, "%s\t%s\n", field.name, field.at.Format(time.RFC3339Nano))
			}
		}
		if c.PersistenceUnconfirmed {
			fmt.Fprintln(w, "CONNECTION_PERSISTENCE\tunconfirmed; saved checkpoint may be older")
		}
	} else {
		fmt.Fprintln(w, "CONNECTION_OBSERVATION\tunknown (no retained observation)")
	}
	if s := observations.Sync; s != nil {
		fmt.Fprintf(w, "SYNC_EXECUTION\t%s (%s)\nSYNC_OBSERVED\t%s %s %s\n", s.ExecutionID, s.Mode, s.State, s.StopReason, s.ObservedAt.Format(time.RFC3339Nano))
		if s.CleanupAt != nil {
			fmt.Fprintf(w, "SYNC_CLEANUP_AT\t%s\n", s.CleanupAt.Format(time.RFC3339Nano))
		}
		if h := s.LastHistorySync; h != nil {
			fmt.Fprintf(w, "HISTORY_SYNC_OBSERVED\t%s %s\n", h.SyncType, h.ObservedAt.Format(time.RFC3339Nano))
		}
		if s.OfflineCompletedAt != nil {
			fmt.Fprintf(w, "OFFLINE_REPLAY_OBSERVED\t%s (server signal, not archive completeness)\n", s.OfflineCompletedAt.Format(time.RFC3339Nano))
		}
		if s.PersistenceUnconfirmed {
			fmt.Fprintln(w, "SYNC_PERSISTENCE\tunconfirmed; saved checkpoint may be older")
		}
		for _, o := range s.RecoveryObservations {
			outcomes := make([]string, len(o.Outcomes))
			for i, v := range o.Outcomes {
				outcomes[i] = string(v)
			}
			fmt.Fprintf(w, "RETAINED_RECOVERY\t%s %s: %s", o.Collection, o.Phase, strings.Join(outcomes, ", "))
			if len(o.ErrorCodes) > 0 {
				fmt.Fprintf(w, " (%s)", strings.Join(o.ErrorCodes, ", "))
			}
			if o.LastObservedAt != nil {
				fmt.Fprintf(w, " at %s", o.LastObservedAt.Format(time.RFC3339Nano))
			}
			fmt.Fprintln(w)
		}
	} else {
		fmt.Fprintln(w, "SYNC_OBSERVATION\tunknown (no retained observation)")
	}
}

func writeInvocationConnectionRows(w io.Writer, c *appPkg.ConnectionObservation) {
	if c == nil {
		return
	}
	fmt.Fprintln(w, "INVOCATION_CONNECTION\thistorical checkpoint from this invocation; current liveness unknown")
	fmt.Fprintf(w, "INVOCATION_CONNECTION_EXECUTION\t%s\nINVOCATION_CONNECTION_EVENT\t%s\nINVOCATION_CONNECTION_CHECKPOINT\t%s\n", c.ExecutionID, c.LastEvent, c.ObservedAt.Format(time.RFC3339Nano))
	if c.LoginConfirmedAt != nil {
		fmt.Fprintf(w, "INVOCATION_LOGIN_CONFIRMED_AT\t%s\n", c.LoginConfirmedAt.Format(time.RFC3339Nano))
	}
	if c.PersistenceUnconfirmed {
		fmt.Fprintln(w, "INVOCATION_CONNECTION_PERSISTENCE\tunconfirmed; saved slot may describe another execution")
	}
}
