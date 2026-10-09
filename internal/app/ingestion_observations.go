package app

import (
	"context"
	"slices"
	"time"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
)

// IngestionCounts describes importer work, not distinct rows or completeness.
// Processed includes successful upserts, replays, updates and purge suppression.
// Additions and Replays remain null: the persistence API does not measure them.
type IngestionCounts struct {
	Received        int64  `json:"received"`
	Valid           int64  `json:"valid"`
	Content         int64  `json:"content"`
	Processed       int64  `json:"processed"`
	Skipped         int64  `json:"skipped"`
	Failed          int64  `json:"failed"`
	Unprocessed     int64  `json:"unprocessed"`
	Additions       *int64 `json:"additions"`
	Replays         *int64 `json:"replays"`
	PurgeSuppressed *int64 `json:"purge_suppressed"`
}

// Fixed fields bound both the retained size and the possible diagnostic reasons.
type IngestionSkipReasons struct {
	MissingInfo  int64 `json:"missing_info"`
	MissingID    int64 `json:"missing_id"`
	MissingChat  int64 `json:"missing_chat"`
	UnusableEdit int64 `json:"unusable_edit"`
}

type IngestionFailureReasons struct {
	PersistenceFailed int64 `json:"persistence_failed"`
	AuthorUnverified  int64 `json:"author_unverified"`
}

type IngestionFailure struct {
	ObservedAt time.Time `json:"observed_at"`
	Operation  string    `json:"operation"`
	Reason     string    `json:"reason"`
}

type HistoryIngestionSummary struct {
	ObservedAt time.Time `json:"observed_at"`
	SyncType   string    `json:"sync_type"`
	IngestionCounts
	SkipReasons    IngestionSkipReasons    `json:"skip_reasons"`
	FailureReasons IngestionFailureReasons `json:"failure_reasons"`
	LastFailure    *IngestionFailure       `json:"last_failure"`
}

// IngestionObservation resets for each Sync execution. Degraded is sticky within
// that run after a skip/failure; it is independent of transport/auth readiness.
type IngestionObservation struct {
	ExecutionID      string                   `json:"execution_id"`
	StartedAt        time.Time                `json:"started_at"`
	ObservedAt       time.Time                `json:"observed_at"`
	Degraded         bool                     `json:"degraded"`
	LiveReceived     int64                    `json:"live_received"`
	LiveProcessed    int64                    `json:"live_processed"`
	LiveFailures     int64                    `json:"live_failures"`
	HistoryResponses int64                    `json:"history_responses"`
	History          IngestionCounts          `json:"history"`
	SkipReasons      IngestionSkipReasons     `json:"skip_reasons"`
	FailureReasons   IngestionFailureReasons  `json:"failure_reasons"`
	LastFailure      *IngestionFailure        `json:"last_failure"`
	LastHistory      *HistoryIngestionSummary `json:"last_history"`
}

func copyIngestion(v *IngestionObservation) *IngestionObservation {
	if v == nil {
		return nil
	}
	result := *v
	result.History = copyIngestionCounts(v.History)
	if v.LastFailure != nil {
		f := *v.LastFailure
		result.LastFailure = &f
	}
	if v.LastHistory != nil {
		h := *v.LastHistory
		h.IngestionCounts = copyIngestionCounts(h.IngestionCounts)
		if h.LastFailure != nil {
			f := *h.LastFailure
			h.LastFailure = &f
		}
		result.LastHistory = &h
	}
	return &result
}

func copyIngestionCounts(c IngestionCounts) IngestionCounts {
	if c.Additions != nil {
		n := *c.Additions
		c.Additions = &n
	}
	if c.Replays != nil {
		n := *c.Replays
		c.Replays = &n
	}
	if c.PurgeSuppressed != nil {
		n := *c.PurgeSuppressed
		c.PurgeSuppressed = &n
	}
	return c
}

// IngestionSnapshot returns this App's latest Sync run only, without reading a
// database or creating a client. Nil means this invocation has no published run;
// a finished run remains dated evidence, never health for a future execution.
func (a *App) IngestionSnapshot() *IngestionObservation {
	if a == nil {
		return nil
	}
	a.waMu.Lock()
	recovery := a.appStateRecoveryOnClose
	var r *diagnosticRun
	if recovery != nil {
		r = recovery.diagnostic
	}
	a.waMu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return copyIngestion(r.sync.Ingestion)
}

func (a *App) observeLiveIngestion(ctx context.Context, err error) {
	if r := syncDiagnosticRun(ctx); r != nil {
		r.mu.Lock()
		if !r.closed {
			v := r.sync.Ingestion
			v.ObservedAt = nowUTC()
			v.LiveReceived++
			if err == nil {
				v.LiveProcessed++
			} else {
				v.LiveFailures++
				v.FailureReasons.PersistenceFailed++
				v.Degraded = true
				v.LastFailure = &IngestionFailure{ObservedAt: v.ObservedAt, Operation: "live", Reason: "persistence_failed"}
			}
		}
		r.mu.Unlock()
	}
	if err != nil {
		data := a.syncEventData(ctx, map[string]any{"operation": "live", "reason": "persistence_failed"})
		if snapshot := a.IngestionSnapshot(); snapshot != nil && snapshot.ExecutionID == data["execution_id"] {
			data["live_failures"] = snapshot.LiveFailures
		}
		a.emitWarning("ingestion_persistence_failed", "warning: live message persistence failed; ingestion is degraded for this run (no automatic recovery)", data)
	}
}

func (s *HistoryIngestionSummary) failure(reason string) {
	s.Failed++
	if reason == "author_unverified" {
		s.FailureReasons.AuthorUnverified++
	} else {
		s.FailureReasons.PersistenceFailed++
	}
	s.LastFailure = &IngestionFailure{ObservedAt: nowUTC(), Operation: "history", Reason: reason}
}

func (a *App) finishHistoryIngestion(ctx context.Context, s *HistoryIngestionSummary) {
	s.Unprocessed = s.Received - s.Processed - s.Skipped - s.Failed
	s.ObservedAt = nowUTC()
	if r := syncDiagnosticRun(ctx); r != nil {
		r.updateSync(func(sync *SyncObservation) {
			v := sync.Ingestion
			v.ObservedAt = s.ObservedAt
			v.HistoryResponses++
			v.History.Received += s.Received
			v.History.Valid += s.Valid
			v.History.Content += s.Content
			v.History.Processed += s.Processed
			v.History.Skipped += s.Skipped
			v.History.Failed += s.Failed
			v.History.Unprocessed += s.Unprocessed
			v.SkipReasons.MissingInfo += s.SkipReasons.MissingInfo
			v.SkipReasons.MissingID += s.SkipReasons.MissingID
			v.SkipReasons.MissingChat += s.SkipReasons.MissingChat
			v.SkipReasons.UnusableEdit += s.SkipReasons.UnusableEdit
			v.FailureReasons.PersistenceFailed += s.FailureReasons.PersistenceFailed
			v.FailureReasons.AuthorUnverified += s.FailureReasons.AuthorUnverified
			v.Degraded = v.Degraded || s.Skipped > 0 || s.Failed > 0 || s.Unprocessed > 0
			if s.LastFailure != nil && (v.LastFailure == nil || s.LastFailure.ObservedAt.After(v.LastFailure.ObservedAt)) {
				f := *s.LastFailure
				v.LastFailure = &f
			}
			h := *s
			v.LastHistory = &h
		})
	}
	a.emitOrPrintSync(ctx, "history_ingestion", map[string]any{"summary": s},
		"\nHistory ingestion: received=%d valid=%d content=%d processed=%d skipped=%d failed=%d unprocessed=%d; skips(missing_info=%d missing_id=%d missing_chat=%d unusable_edit=%d) failures(persistence_failed=%d author_unverified=%d); additions/replays/purge suppression not measured.\n",
		s.Received, s.Valid, s.Content, s.Processed, s.Skipped, s.Failed, s.Unprocessed,
		s.SkipReasons.MissingInfo, s.SkipReasons.MissingID, s.SkipReasons.MissingChat, s.SkipReasons.UnusableEdit, s.FailureReasons.PersistenceFailed, s.FailureReasons.AuthorUnverified)
}

func validIngestionObservation(v *IngestionObservation, executionID string) bool {
	// Old checkpoints have no ingestion field. Absence is not a healthy run.
	if v == nil {
		return true
	}
	if v.ExecutionID != executionID || v.StartedAt.IsZero() || v.ObservedAt.IsZero() || v.LiveReceived < 0 || v.LiveProcessed < 0 || v.LiveFailures < 0 || v.LiveProcessed+v.LiveFailures != v.LiveReceived || v.HistoryResponses < 0 || !validIngestionCounts(v.History) || !validIngestionReasons(v.SkipReasons, v.FailureReasons) || !validIngestionFailure(v.LastFailure) {
		return false
	}
	if h := v.LastHistory; h != nil {
		if _, ok := waHistorySync.HistorySync_HistorySyncType_value[h.SyncType]; !ok {
			return false
		}
		if h.ObservedAt.IsZero() || !validIngestionCounts(h.IngestionCounts) || !validIngestionReasons(h.SkipReasons, h.FailureReasons) || !validIngestionFailure(h.LastFailure) {
			return false
		}
	}
	return true
}

func validIngestionCounts(c IngestionCounts) bool {
	return c.Received >= 0 && c.Valid >= 0 && c.Valid <= c.Received && c.Content >= 0 && c.Content <= c.Valid && c.Processed >= 0 && c.Skipped >= 0 && c.Failed >= 0 && c.Unprocessed >= 0 && c.Processed+c.Skipped+c.Failed+c.Unprocessed == c.Received && c.Additions == nil && c.Replays == nil && c.PurgeSuppressed == nil
}

func validIngestionReasons(s IngestionSkipReasons, f IngestionFailureReasons) bool {
	return s.MissingInfo >= 0 && s.MissingID >= 0 && s.MissingChat >= 0 && s.UnusableEdit >= 0 && f.PersistenceFailed >= 0 && f.AuthorUnverified >= 0
}

func validIngestionFailure(f *IngestionFailure) bool {
	return f == nil || !f.ObservedAt.IsZero() && slices.Contains([]string{"live", "history"}, f.Operation) && slices.Contains([]string{"persistence_failed", "author_unverified"}, f.Reason)
}
