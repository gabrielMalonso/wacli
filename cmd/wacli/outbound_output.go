package main

import (
	"errors"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

type outboundCheckpointsDTO struct {
	PreparingAt        *time.Time `json:"preparing_at"`
	UploadPossibleAt   *time.Time `json:"upload_possible_at"`
	UploadReturnedAt   *time.Time `json:"upload_returned_at"`
	DispatchPossibleAt *time.Time `json:"dispatch_possible_at"`
	FinalizedAt        *time.Time `json:"finalized_at"`
}
type outboundDTO struct {
	ID                string                  `json:"id"`
	Version           int                     `json:"version"`
	Key               string                  `json:"key"`
	DraftID           string                  `json:"draft_id"`
	RevisionID        string                  `json:"revision_id"`
	Hash              string                  `json:"hash"`
	MessageID         string                  `json:"message_id"`
	Account           store.DraftIdentity     `json:"account_identity"`
	Recipient         store.DraftRecipient    `json:"recipient"`
	Kind              store.DraftKind         `json:"kind"`
	Phase             store.OutboundPhase     `json:"phase"`
	AttemptResult     store.OutboundResult    `json:"attempt_result"`
	Status            string                  `json:"status"`
	Evidence          store.OutboundEvidence  `json:"evidence"`
	ArchiveContinuity string                  `json:"archive_continuity"`
	Generation        int64                   `json:"generation"`
	CreatedAt         time.Time               `json:"created_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
	ErrorCode         *string                 `json:"error_code"`
	Checkpoints       *outboundCheckpointsDTO `json:"checkpoints,omitempty"`
}

func projectOutbound(o store.OutboundOperation, e store.OutboundEvidence, full bool) outboundDTO {
	d := outboundDTO{ID: o.ID, Version: o.Version, Key: o.Key, DraftID: o.DraftID, RevisionID: o.RevisionID, Hash: o.Hash, MessageID: o.MessageID, Account: o.Account, Recipient: o.Recipient, Kind: o.Kind, Phase: o.Phase, AttemptResult: o.Result, Status: o.EvidenceStatus(e), Evidence: e, ArchiveContinuity: "unknown", Generation: o.Generation, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
	if o.ErrorCode != "" {
		code := o.ErrorCode
		d.ErrorCode = &code
	}
	if full {
		d.Checkpoints = &outboundCheckpointsDTO{o.PreparingAt, o.UploadPossibleAt, o.UploadReturnedAt, o.DispatchPossibleAt, o.FinalizedAt}
	}
	return d
}
func outboundFull(flags *rootFlags) bool {
	return flags.agent && flags.detail == "full" || !flags.agent && flags.fullOutput
}
func outboundMeta(flags *rootFlags, page *out.AgentPage) out.AgentMeta {
	m := agentMeta(flags)
	m.Page = page
	m.Recovery = "Inspect the exact draft_id/revision_id with draft show --revision; pending is not a running-process assertion. Do not resend an operation to resolve uncertainty."
	return m
}

type outboundEntryDTO struct {
	Operation    outboundDTO                 `json:"operation"`
	Observations []store.OutboundObservation `json:"observations"`
}

func writeOutboundEntry(flags *rootFlags, entry store.OutboundEntry) error {
	data := outboundEntryDTO{projectOutbound(entry.Operation, entry.Evidence, outboundFull(flags)), entry.Observations.Items}
	page := &out.AgentPage{Returned: len(entry.Observations.Items), HasMore: entry.Observations.HasMore, NextCursor: entry.Observations.NextCursor}
	if flags.agent {
		return out.WriteAgentJSON(os.Stdout, flags.agentAccount, outboundMeta(flags, page), data)
	}
	return out.WriteJSON(os.Stdout, struct {
		Account out.AgentAccount `json:"account"`
		Data    outboundEntryDTO `json:"outbound"`
		Page    *out.AgentPage   `json:"page"`
	}{flags.agentAccount, data, page})
}
func writeOutboundList(flags *rootFlags, page store.OutboundPage) error {
	items := make([]outboundDTO, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, projectOutbound(item.Operation, item.Evidence, outboundFull(flags)))
	}
	data := struct {
		Operations []outboundDTO `json:"operations"`
	}{items}
	p := &out.AgentPage{Returned: len(items), HasMore: page.HasMore, NextCursor: page.NextCursor}
	if flags.agent {
		return out.WriteAgentJSON(os.Stdout, flags.agentAccount, outboundMeta(flags, p), data)
	}
	return out.WriteJSON(os.Stdout, struct {
		Account    out.AgentAccount `json:"account"`
		Operations []outboundDTO    `json:"operations"`
		Page       *out.AgentPage   `json:"page"`
	}{flags.agentAccount, items, p})
}
func classifyOutboundError(err error) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) {
		return existing
	}
	var failure *store.OutboundError
	code, message, exit := "store_error", "Selected outbound archive is unavailable or contains incompatible/corrupt records.", 4
	if errors.As(err, &failure) {
		switch failure.Code {
		case "invalid_arguments":
			code, message, exit = "invalid_arguments", "Invalid bounded outbound query or canonical frozen own PN filter.", 2
		case "invalid_cursor":
			code, message, exit = "invalid_cursor", "Invalid or mismatched outbound cursor; restart without --cursor.", 2
		case "not_found":
			code, message, exit = "not_found", "Requested operation was not found in the selected retained catalogue.", 3
		}
	}
	return &out.AgentError{Code: code, Message: message, ExitCode: exit, Cause: err}
}
