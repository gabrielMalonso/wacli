package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

const historyEvidenceWriteTimeout = 2 * time.Second

func NewHistoryAttemptID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func ValidateHistoryAttemptID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("invalid history attempt ID")
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return fmt.Errorf("invalid history attempt ID")
		}
	}
	return nil
}

// HistoryFailure describes this recovery only. Not-dispatched never implies no
// normal sync activity or local writes; uncertain never authorizes a blind retry.
type HistoryFailure struct {
	AttemptID            string                    `json:"attempt_id,omitempty"`
	Phase                store.HistoryAttemptPhase `json:"phase"`
	Outcome              string                    `json:"outcome"`
	Code                 string                    `json:"code"`
	CorrelationConfirmed bool                      `json:"correlation_confirmed"`
}
type BackfillError struct {
	History HistoryFailure
	Cause   error
}

func (e *BackfillError) Error() string { return e.Cause.Error() }
func (e *BackfillError) Unwrap() error { return e.Cause }

func historyFailure(id string, phase store.HistoryAttemptPhase, code string, possible, confirmed bool, cause error) error {
	outcome := "not_dispatched"
	if possible {
		outcome = "uncertain"
		code = "backfill_outcome_uncertain"
	}
	return &BackfillError{History: HistoryFailure{AttemptID: id, Phase: phase, Code: code, Outcome: outcome, CorrelationConfirmed: confirmed}, Cause: cause}
}

func (a *App) saveHistoryAttempt(ctx context.Context, rec store.HistoryAttempt, begin bool) error {
	ctx, cancel := context.WithTimeout(ctx, historyEvidenceWriteTimeout)
	defer cancel()
	if begin {
		return a.db.BeginHistoryAttempt(ctx, rec)
	}
	return a.db.SaveHistoryAttempt(ctx, rec)
}

func (a *App) finishHistoryAttempt(ctx context.Context, rec *store.HistoryAttempt, res *BackfillResult, runErr *error) {
	now := nowUTC()
	rec.CheckpointAt = now
	rec.FinishedAt = &now
	rec.CountersFinal = true
	if *runErr == nil && ctx.Err() != nil {
		*runErr = ctx.Err()
	}
	if *runErr == nil {
		rec.State = store.HistorySucceeded
		rec.Phase = store.HistoryFinalizing
		rec.FinalCount = new(*rec.BaselineCount + res.MessagesAdded)
		rec.NetGrowth = new(res.MessagesAdded)
		rec.MessagesSynced = new(res.MessagesSynced)
	} else {
		rec.State = store.HistoryError
		rec.ErrorCode = "operational_error"
		if errors.Is(*runErr, context.Canceled) || errors.Is(*runErr, context.DeadlineExceeded) {
			rec.State = store.HistoryCancelled
			rec.ErrorCode = "cancelled"
		}
		var typed *BackfillError
		if errors.As(*runErr, &typed) {
			rec.ErrorCode = typed.History.Code
		}
	}
	// Synchronous bounded cleanup survives cancellation and completes before the
	// caller can release its writer. SQLite's evidence busy timeout is also bounded.
	err := a.saveHistoryAttempt(context.WithoutCancel(ctx), *rec, false)
	if err != nil {
		*runErr = historyFailure(rec.AttemptID, rec.Phase, "store_state", rec.DispatchPossible, true, errors.Join(*runErr, fmt.Errorf("persist history recovery outcome (already persisted history may remain): %w", err)))
	} else if *runErr != nil {
		code := rec.ErrorCode
		*runErr = historyFailure(rec.AttemptID, rec.Phase, code, rec.DispatchPossible, true, *runErr)
	} else {
		res.AttemptID = rec.AttemptID
		snapshot := *rec
		res.Evidence = &snapshot
	}
	if *runErr != nil {
		*res = BackfillResult{}
	}
}
