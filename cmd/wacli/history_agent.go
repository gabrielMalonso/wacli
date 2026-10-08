package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

// Action failures never echo transport, SQLite, anchor, or argument text. The
// typed history member supplies correlation without parsing recovery prose.
func classifyHistoryAgentError(err error, attemptID string) *out.AgentError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		var existing *app.BackfillError
		if !errors.As(err, &existing) {
			err = &app.BackfillError{History: app.HistoryFailure{AttemptID: attemptID, Phase: store.HistoryPreparing, Outcome: "not_dispatched", Code: "cancelled"}, Cause: err}
		}
	}
	var failure *app.BackfillError
	if errors.As(err, &failure) {
		h := failure.History
		phase := string(h.Phase)
		switch h.Phase {
		case store.HistoryPreparing, store.HistoryObserving, store.HistoryDispatchPossible, store.HistoryFinalizing:
		default:
			phase = string(store.HistoryDispatchPossible)
		}
		id := h.AttemptID
		if app.ValidateHistoryAttemptID(id) != nil {
			id = attemptID
		}
		code, message, exit := "backfill_not_dispatched", "History recovery did not dispatch a request.", 1
		outcome := "not_dispatched"
		recovery := "Inspect history coverage --chat JID --evidence before deciding whether to request recovery. Normal sync activity may have persisted."
		if h.Outcome != "not_dispatched" || h.Phase == store.HistoryDispatchPossible || h.Phase == store.HistoryFinalizing {
			code, message, outcome = "backfill_outcome_uncertain", "History recovery has no reliable delivered outcome.", "uncertain"
			recovery = "Inspect history coverage --chat JID --evidence; compare attempt_id before deciding whether to retry. Do not infer rollback or zero effects."
		} else {
			switch h.Code {
			case "invalid_arguments":
				code, message, exit = "invalid_arguments", "Invalid history recovery arguments refused before dispatch.", 2
			case "no_local_anchor":
				code, message, exit = "no_local_anchor", "No local recovery anchor was available before dispatch.", 3
			case "store_state":
				code, message, exit = "store_state", "Local recovery state could not be read or persisted before dispatch.", 4
			}
		}
		return &out.AgentError{Code: code, Message: message, Recovery: recovery, ExitCode: exit, Cause: err,
			History: &out.AgentHistoryError{AttemptID: id, Phase: phase, Outcome: outcome, CorrelationConfirmed: h.CorrelationConfirmed}}
	}
	var typed *out.AgentError
	if errors.As(err, &typed) {
		copy := *typed
		switch copy.Code {
		case "invalid_arguments":
			copy.Message = "Invalid history recovery arguments."
		case "read_only":
		case "store_unavailable":
			copy.Code, copy.Message = "store_state", "Selected recovery archive or configuration is unavailable."
		default:
			copy.Message = "History recovery preflight failed."
		}
		return &copy
	}
	return &out.AgentError{Code: "store_state", Message: "Selected recovery archive could not be opened or delegated before dispatch.", ExitCode: 4, Cause: err,
		History: &out.AgentHistoryError{AttemptID: attemptID, Phase: string(store.HistoryPreparing), Outcome: "not_dispatched", CorrelationConfirmed: false}}
}

type historyAgentBackfillData struct {
	BeforeID            string                 `json:"before_id,omitempty"`
	MessagesAddedBefore *int64                 `json:"messages_added_before,omitempty"`
	Chat                string                 `json:"chat"`
	AttemptID           string                 `json:"attempt_id"`
	RequestsSent        int                    `json:"requests_sent"`
	ResponsesSeen       int                    `json:"responses_seen"`
	MessagesAdded       int64                  `json:"messages_added"`
	StopReason          app.BackfillStopReason `json:"stop_reason"`
	Evidence            *historyObservationDTO `json:"evidence"`
}

func writeHistoryBackfillResult(flags *rootFlags, res app.BackfillResult) error {
	if !flags.agent {
		return writeBackfillResult(os.Stdout, res, flags.asJSON)
	}
	// The owner must supply this operation's successful persisted snapshot. An
	// older/incompatible reply cannot establish retention just by echoing an ID.
	if res.AttemptID != flags.agentHistoryAttemptID || app.ValidateHistoryAttemptID(res.AttemptID) != nil || res.Evidence == nil || res.Evidence.AttemptID != res.AttemptID || res.Evidence.State != store.HistorySucceeded {
		return historyIPCUncertain(sendDelegateRequest{Backfill: &backfillDelegateOptions{AttemptID: flags.agentHistoryAttemptID}}, fmt.Errorf("missing correlated recovery observation"))
	}
	current := app.HistoryIdentity{} // No post-action identity query or freshness assertion.
	data := historyAgentBackfillData{BeforeID: res.BeforeID, MessagesAddedBefore: res.MessagesAddedBefore, Chat: res.ChatJID, AttemptID: res.AttemptID, RequestsSent: res.RequestsSent, ResponsesSeen: res.ResponsesSeen,
		MessagesAdded: res.MessagesAdded, StopReason: res.StopReason, Evidence: historyObservation(res.Evidence, current, flags.detail == "full")}
	meta := agentMeta(flags)
	meta.Recovery = "Inspect history coverage --chat JID --evidence for retained observations; primary end markers do not certify remote completeness."
	if err := out.WriteAgentActionJSON(os.Stdout, flags.agentAccount, meta, data); err != nil {
		return &app.BackfillError{History: app.HistoryFailure{AttemptID: res.AttemptID, Phase: store.HistoryFinalizing, Outcome: "uncertain", Code: "backfill_outcome_uncertain", CorrelationConfirmed: true}, Cause: err}
	}
	return nil
}
