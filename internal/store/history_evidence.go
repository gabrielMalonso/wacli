package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// History evidence records observations of bounded recovery operations, never
// completeness. Slots grow with distinct inputs, with at most two per input.
type HistoryAttemptState string
type HistoryAttemptPhase string

const (
	HistoryUnfinalized      HistoryAttemptState = "unfinalized"
	HistorySucceeded        HistoryAttemptState = "succeeded"
	HistoryError            HistoryAttemptState = "error"
	HistoryCancelled        HistoryAttemptState = "cancelled"
	HistoryPreparing        HistoryAttemptPhase = "preparing"
	HistoryObserving        HistoryAttemptPhase = "observing"
	HistoryDispatchPossible HistoryAttemptPhase = "dispatch_possible"
	HistoryFinalizing       HistoryAttemptPhase = "finalizing"
)

// Nullable measurements distinguish unavailable observations from measured zero.
type HistoryAttempt struct {
	RequestedChatJID        string
	AttemptID               string
	StartedAt               time.Time
	CheckpointAt            time.Time
	FinishedAt              *time.Time
	State                   HistoryAttemptState
	Phase                   HistoryAttemptPhase
	ExecutionMode           string
	AccountJID              string
	WindowChatJID           string
	WindowAliasJID          string
	DispatchPossible        bool
	Count                   int
	Requests                int
	WaitMS                  int64
	IdleMS                  int64
	BaselineCount           *int64
	FinalCount              *int64
	NetGrowth               *int64
	RequestsSent            int
	ResponsesSeen           int
	CountersFinal           bool
	StopReason              string
	ResponseChatJID         string
	ResponseObservedAt      *time.Time
	PrimaryNoMoreObservedAt *time.Time
	PrimaryResponseChatJID  string
	FirstAnchorID           string
	LastAnchorID            string
	PreparedRequestChatJID  string
	MessagesSynced          *int64
	ErrorCode               string
}

type HistoryRecoveryEvidence struct {
	RequestedChatJID string
	Latest           *HistoryAttempt
	LastSuccess      *HistoryAttempt
}

func migrateHistoryEvidence(d *DB) error {
	_, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS history_recovery_evidence (
 requested_chat_jid TEXT NOT NULL CHECK(length(requested_chat_jid)>0),
 slot TEXT NOT NULL CHECK(slot IN ('latest','last_success')),
 attempt_id TEXT NOT NULL CHECK(length(attempt_id)=32 AND attempt_id NOT GLOB '*[^0-9a-f]*'),
 started_at INTEGER NOT NULL, checkpoint_at INTEGER NOT NULL, finished_at INTEGER,
 state TEXT NOT NULL CHECK(state IN ('unfinalized','succeeded','error','cancelled')),
 phase TEXT NOT NULL CHECK(phase IN ('preparing','observing','dispatch_possible','finalizing')),
 execution_mode TEXT NOT NULL CHECK(execution_mode IN ('standalone','sync_owner')),
 account_jid TEXT NOT NULL, window_chat_jid TEXT NOT NULL, window_alias_jid TEXT NOT NULL,
 dispatch_possible INTEGER NOT NULL CHECK(dispatch_possible IN (0,1)),
 batch_count INTEGER NOT NULL CHECK(batch_count BETWEEN 1 AND 500),
 batch_requests INTEGER NOT NULL CHECK(batch_requests BETWEEN 1 AND 100),
 wait_ms INTEGER NOT NULL CHECK(wait_ms BETWEEN 1 AND 300000),
 idle_ms INTEGER NOT NULL CHECK(idle_ms BETWEEN 1 AND 300000),
 baseline_count INTEGER CHECK(baseline_count>=0), final_count INTEGER CHECK(final_count>=0),
 net_growth INTEGER CHECK(net_growth>=0),
 requests_sent INTEGER NOT NULL CHECK(requests_sent BETWEEN 0 AND 400),
 responses_seen INTEGER NOT NULL CHECK(responses_seen BETWEEN 0 AND requests_sent),
 counters_final INTEGER NOT NULL CHECK(counters_final IN (0,1)),
 stop_reason TEXT NOT NULL CHECK(stop_reason IN ('','requested_batch_limit','no_progress','empty_response','primary_no_more_messages')),
 response_chat_jid TEXT NOT NULL, response_observed_at INTEGER, primary_no_more_observed_at INTEGER, primary_response_chat_jid TEXT NOT NULL,
 first_anchor_id TEXT NOT NULL, last_anchor_id TEXT NOT NULL, prepared_request_chat_jid TEXT NOT NULL,
 messages_synced INTEGER CHECK(messages_synced>=0), error_code TEXT NOT NULL CHECK(error_code IN ('','operational_error','cancelled','no_local_anchor','backfill_not_dispatched','store_state','backfill_outcome_uncertain')),
 PRIMARY KEY(requested_chat_jid,slot),
 CHECK((state='unfinalized' AND finished_at IS NULL AND counters_final=0) OR
       (state!='unfinalized' AND finished_at IS NOT NULL AND counters_final=1)),
 CHECK(slot!='last_success' OR state='succeeded'),
 CHECK(state NOT IN ('error','cancelled') OR error_code!=''),
 CHECK((window_chat_jid='' AND window_alias_jid='' AND baseline_count IS NULL) OR (window_chat_jid!='' AND baseline_count IS NOT NULL)),
 CHECK(state!='succeeded' OR (phase='finalizing' AND window_chat_jid!='' AND requests_sent>0 AND responses_seen>0 AND messages_synced IS NOT NULL AND
       baseline_count IS NOT NULL AND final_count IS NOT NULL AND net_growth IS NOT NULL AND
       final_count>=baseline_count AND net_growth=final_count-baseline_count AND
       stop_reason!='' AND error_code='' AND dispatch_possible=1)),
 CHECK(primary_no_more_observed_at IS NULL OR (response_observed_at IS NOT NULL AND primary_response_chat_jid!='')),
 CHECK(stop_reason!='primary_no_more_messages' OR primary_no_more_observed_at IS NOT NULL)
 )`)
	return err
}

const historyAttemptColumns = `requested_chat_jid,attempt_id,started_at,checkpoint_at,finished_at,state,phase,execution_mode,
 account_jid,window_chat_jid,window_alias_jid,dispatch_possible,batch_count,batch_requests,wait_ms,idle_ms,
 baseline_count,final_count,net_growth,requests_sent,responses_seen,counters_final,stop_reason,
 response_chat_jid,response_observed_at,primary_no_more_observed_at,primary_response_chat_jid,first_anchor_id,last_anchor_id,
 prepared_request_chat_jid,messages_synced,error_code`

func historyAttemptArgs(a HistoryAttempt) []any {
	return []any{a.RequestedChatJID, a.AttemptID, a.StartedAt.UnixMilli(), a.CheckpointAt.UnixMilli(), historyTimeMS(a.FinishedAt), a.State, a.Phase, a.ExecutionMode,
		a.AccountJID, a.WindowChatJID, a.WindowAliasJID, a.DispatchPossible, a.Count, a.Requests, a.WaitMS, a.IdleMS,
		a.BaselineCount, a.FinalCount, a.NetGrowth, a.RequestsSent, a.ResponsesSeen, a.CountersFinal, a.StopReason,
		a.ResponseChatJID, historyTimeMS(a.ResponseObservedAt), historyTimeMS(a.PrimaryNoMoreObservedAt), a.PrimaryResponseChatJID, a.FirstAnchorID, a.LastAnchorID,
		a.PreparedRequestChatJID, a.MessagesSynced, a.ErrorCode}
}
func historyTimeMS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
func historyTime(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := time.UnixMilli(*ms).UTC()
	return &t
}

// Evidence writes use a short SQLite busy timeout as well as the context budget.
// A cancelled command can finalize synchronously before releasing its writer lock.
func (d *DB) historyConn(ctx context.Context) (*sql.Conn, error) {
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=50`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
func closeHistoryConn(conn *sql.Conn) {
	// Restoring a connection-local pragma neither acquires a database write lock nor waits on busy.
	_, _ = conn.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`)
	_ = conn.Close()
}

func (d *DB) BeginHistoryAttempt(ctx context.Context, a HistoryAttempt) error {
	if a.State != HistoryUnfinalized || a.Phase != HistoryPreparing || a.DispatchPossible || a.FinishedAt != nil {
		return fmt.Errorf("invalid initial history attempt")
	}
	conn, err := d.historyConn(ctx)
	if err != nil {
		return err
	}
	defer closeHistoryConn(conn)
	columns := strings.Split(strings.ReplaceAll(historyAttemptColumns, "\n", ""), ",")
	updates := make([]string, 0, len(columns))
	for _, c := range columns {
		c = strings.TrimSpace(c)
		if c != "requested_chat_jid" {
			updates = append(updates, c+"=excluded."+c)
		}
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO history_recovery_evidence(slot,`+historyAttemptColumns+`) VALUES('latest',`+strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")+`) ON CONFLICT(requested_chat_jid,slot) DO UPDATE SET `+strings.Join(updates, ","), historyAttemptArgs(a)...)
	return err
}

var ErrHistoryAttemptSuperseded = errors.New("history attempt no longer owns latest evidence")

// Save conditions every update on the expected ID and unfinalized state. Scope
// cannot be reassigned, and dispatch uncertainty never decreases within an attempt.
func (d *DB) SaveHistoryAttempt(ctx context.Context, a HistoryAttempt) error {
	conn, err := d.historyConn(ctx)
	if err != nil {
		return err
	}
	defer closeHistoryConn(conn)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns := strings.Split(strings.ReplaceAll(historyAttemptColumns, "\n", ""), ",")
	sets := make([]string, 0, len(columns))
	for _, c := range columns {
		c = strings.TrimSpace(c)
		sets = append(sets, c+"=?")
	}
	args := historyAttemptArgs(a)
	args = append(args, a.RequestedChatJID, a.AttemptID, a.WindowChatJID, a.WindowAliasJID, a.BaselineCount, a.AccountJID, a.DispatchPossible)
	res, err := tx.ExecContext(ctx, `UPDATE history_recovery_evidence SET `+strings.Join(sets, ",")+` WHERE requested_chat_jid=? AND slot='latest' AND attempt_id=? AND state='unfinalized'
 AND (window_chat_jid='' OR (window_chat_jid=? AND window_alias_jid=? AND baseline_count=? AND account_jid=?)) AND dispatch_possible<=?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrHistoryAttemptSuperseded
	}
	if a.State == HistorySucceeded {
		updates := make([]string, 0, len(columns))
		for _, c := range columns {
			c = strings.TrimSpace(c)
			if c != "requested_chat_jid" {
				updates = append(updates, c+"=excluded."+c)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO history_recovery_evidence(slot,`+historyAttemptColumns+`) SELECT 'last_success',`+historyAttemptColumns+` FROM history_recovery_evidence WHERE requested_chat_jid=? AND slot='latest' AND attempt_id=? ON CONFLICT(requested_chat_jid,slot) DO UPDATE SET `+strings.Join(updates, ","), a.RequestedChatJID, a.AttemptID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// List reads only selected PKs, independently of chats or local anchors. An empty
// slot is absence of retained evidence, not evidence of an untouched archive.
func (d *DB) ListHistoryRecoveryEvidence(ctx context.Context, inputs []string) ([]HistoryRecoveryEvidence, error) {
	keys := make([]string, 0, len(inputs))
	seen := map[string]bool{}
	for _, key := range inputs {
		if key == "" {
			return nil, fmt.Errorf("empty history evidence identity")
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) > 400 {
		return nil, fmt.Errorf("history evidence accepts at most 400 selected identities")
	}
	result := make([]HistoryRecoveryEvidence, len(keys))
	indexes := make(map[string]int, len(keys))
	for i, key := range keys {
		result[i].RequestedChatJID = key
		indexes[key] = i
	}
	if len(keys) == 0 {
		return result, nil
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT slot,`+historyAttemptColumns+` FROM history_recovery_evidence WHERE requested_chat_jid IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a HistoryAttempt
		var slot string
		var started, checkpoint int64
		var finished, response, primary *int64
		err = rows.Scan(&slot, &a.RequestedChatJID, &a.AttemptID, &started, &checkpoint, &finished, &a.State, &a.Phase, &a.ExecutionMode,
			&a.AccountJID, &a.WindowChatJID, &a.WindowAliasJID, &a.DispatchPossible, &a.Count, &a.Requests, &a.WaitMS, &a.IdleMS,
			&a.BaselineCount, &a.FinalCount, &a.NetGrowth, &a.RequestsSent, &a.ResponsesSeen, &a.CountersFinal, &a.StopReason,
			&a.ResponseChatJID, &response, &primary, &a.PrimaryResponseChatJID, &a.FirstAnchorID, &a.LastAnchorID, &a.PreparedRequestChatJID, &a.MessagesSynced, &a.ErrorCode)
		if err != nil {
			return nil, err
		}
		a.StartedAt = time.UnixMilli(started).UTC()
		a.CheckpointAt = time.UnixMilli(checkpoint).UTC()
		a.FinishedAt = historyTime(finished)
		a.ResponseObservedAt = historyTime(response)
		a.PrimaryNoMoreObservedAt = historyTime(primary)
		i := indexes[a.RequestedChatJID]
		if slot == "latest" {
			result[i].Latest = &a
		} else {
			result[i].LastSuccess = &a
		}
	}
	return result, rows.Err()
}
