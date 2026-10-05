package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

type historyScopeDTO struct {
	AccountJID       *string `json:"account_jid"`
	ChatJID          *string `json:"chat_jid"`
	AliasJID         *string `json:"alias_jid"`
	IdentityRelation string  `json:"identity_relation"`
}
type historyObservationDTO struct {
	AttemptID               string                    `json:"attempt_id"`
	State                   store.HistoryAttemptState `json:"state"`
	Phase                   store.HistoryAttemptPhase `json:"phase"`
	StartedAt               time.Time                 `json:"started_at"`
	FinishedAt              *time.Time                `json:"finished_at"`
	Scope                   historyScopeDTO           `json:"scope"`
	DispatchPossible        bool                      `json:"dispatch_possible"`
	StopReason              string                    `json:"stop_reason,omitempty"`
	RequestsSent            *int                      `json:"requests_sent"`
	ResponsesSeen           *int                      `json:"responses_seen"`
	NetGrowth               *int64                    `json:"net_growth"`
	ResponseChatJID         *string                   `json:"response_chat_jid"`
	ResponseObservedAt      *time.Time                `json:"response_observed_at"`
	PrimaryNoMoreObservedAt *time.Time                `json:"primary_no_more_observed_at"`
	PrimaryResponseChatJID  *string                   `json:"primary_response_chat_jid"`
	ErrorCode               string                    `json:"error_code,omitempty"`
	Full                    *historyObservationFull   `json:"full,omitempty"`
}
type historyObservationFull struct {
	ExecutionMode           string    `json:"execution_mode"`
	CheckpointAt            time.Time `json:"checkpoint_at"`
	CountersFinal           bool      `json:"counters_final"`
	CheckpointRequestsSent  int       `json:"checkpoint_requests_sent"`
	CheckpointResponsesSeen int       `json:"checkpoint_responses_seen"`
	BaselineCount           *int64    `json:"baseline_count"`
	FinalCount              *int64    `json:"final_count"`
	Count                   int       `json:"count"`
	Requests                int       `json:"requests"`
	WaitMS                  int64     `json:"wait_ms"`
	IdleMS                  int64     `json:"idle_ms"`
	FirstAnchorID           string    `json:"first_anchor_id,omitempty"`
	LastAnchorID            string    `json:"last_anchor_id,omitempty"`
	PreparedRequestChatJID  string    `json:"prepared_request_chat_jid,omitempty"`
	GlobalMessagesSynced    *int64    `json:"global_messages_synced"`
	GlobalCounterScope      string    `json:"global_counter_scope"`
}
type historyRecoveryDTO struct {
	RequestedChatJID string                 `json:"requested_chat_jid"`
	Latest           *historyObservationDTO `json:"latest"`
	LastSuccess      *historyObservationDTO `json:"last_success"`
}

func historyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func historyObservation(a *store.HistoryAttempt, current app.HistoryIdentity, full bool) *historyObservationDTO {
	if a == nil {
		return nil
	}
	d := &historyObservationDTO{AttemptID: a.AttemptID, State: a.State, Phase: a.Phase, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
		Scope:            historyScopeDTO{AccountJID: historyString(a.AccountJID), ChatJID: historyString(a.WindowChatJID), AliasJID: historyString(a.WindowAliasJID), IdentityRelation: app.HistoryIdentityRelation(*a, current)},
		DispatchPossible: a.DispatchPossible, StopReason: a.StopReason, NetGrowth: a.NetGrowth, ResponseChatJID: historyString(a.ResponseChatJID),
		ResponseObservedAt: a.ResponseObservedAt, PrimaryNoMoreObservedAt: a.PrimaryNoMoreObservedAt, PrimaryResponseChatJID: historyString(a.PrimaryResponseChatJID), ErrorCode: a.ErrorCode}
	if a.CountersFinal {
		d.RequestsSent = new(a.RequestsSent)
		d.ResponsesSeen = new(a.ResponsesSeen)
	}
	if full {
		scope := "standalone_sync_total"
		if a.ExecutionMode == "sync_owner" {
			scope = "sync_owner_window_delta"
		}
		d.Full = &historyObservationFull{ExecutionMode: a.ExecutionMode, CheckpointAt: a.CheckpointAt, CountersFinal: a.CountersFinal,
			CheckpointRequestsSent: a.RequestsSent, CheckpointResponsesSeen: a.ResponsesSeen, BaselineCount: a.BaselineCount, FinalCount: a.FinalCount,
			Count: a.Count, Requests: a.Requests, WaitMS: a.WaitMS, IdleMS: a.IdleMS, FirstAnchorID: a.FirstAnchorID, LastAnchorID: a.LastAnchorID,
			PreparedRequestChatJID: a.PreparedRequestChatJID, GlobalMessagesSynced: a.MessagesSynced, GlobalCounterScope: scope}
	}
	return d
}

func historyEvidenceInputs(chats []string) ([]string, error) {
	if len(chats) > 200 {
		return nil, fmt.Errorf("--evidence accepts at most 200 chat inputs")
	}
	inputs := make([]string, 0, len(chats))
	seen := map[string]bool{}
	for _, raw := range chats {
		jid, err := app.ParseHistoryJID(raw)
		if err != nil || jid.User == "" {
			return nil, fmt.Errorf("invalid history evidence chat JID")
		}
		key := jid.ToNonAD().String()
		if !seen[key] {
			inputs = append(inputs, key)
			seen[key] = true
		}
	}
	return inputs, nil
}

func readHistoryEvidence(ctx context.Context, a *app.App, chats []string, coverage []store.HistoryCoverage, full bool) ([]historyRecoveryDTO, error) {
	if len(chats) == 0 {
		for _, c := range coverage {
			chats = append(chats, c.ChatJID)
		}
	}
	inputs, err := historyEvidenceInputs(chats)
	if err != nil {
		return nil, err
	}
	identities, err := a.ReadHistoryIdentities(ctx, inputs)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(identities)*2)
	scopes := map[string]app.HistoryIdentity{}
	for _, identity := range identities {
		for _, key := range []string{identity.InputJID, identity.ChatJID, identity.AliasJID} {
			if key == "" {
				continue
			}
			if _, ok := scopes[key]; !ok {
				keys = append(keys, key)
				scopes[key] = identity
			}
		}
	}
	evidence, err := a.DB().ListHistoryRecoveryEvidence(ctx, keys)
	if err != nil {
		return nil, err
	}
	result := make([]historyRecoveryDTO, 0, len(evidence))
	for _, e := range evidence {
		result = append(result, historyRecoveryDTO{RequestedChatJID: e.RequestedChatJID, Latest: historyObservation(e.Latest, scopes[e.RequestedChatJID], full), LastSuccess: historyObservation(e.LastSuccess, scopes[e.RequestedChatJID], full)})
	}
	return result, nil
}

type historyCoverageEvidenceData struct {
	Coverage         []store.HistoryCoverage `json:"coverage"`
	RecoveryEvidence []historyRecoveryDTO    `json:"recovery_evidence"`
}
type historyAgentCoverageEvidenceData struct {
	Coverage         []agentCoverage      `json:"coverage"`
	RecoveryEvidence []historyRecoveryDTO `json:"recovery_evidence"`
}

// This opt-in writer leaves the existing coverage writer and DTO untouched.
func writeHistoryAgentCoverageEvidence(flags *rootFlags, cs []store.HistoryCoverage, evidence []historyRecoveryDTO, limit int) error {
	data := historyAgentCoverageEvidenceData{Coverage: make([]agentCoverage, 0, len(cs)), RecoveryEvidence: evidence}
	meta := agentMeta(flags)
	meta.Recovery = "Retained recovery observations do not prove completeness."
	for _, c := range cs {
		name, cut := agentText(c.Name, flags.detail)
		d := agentCoverage{ChatJID: c.ChatJID, Kind: c.Kind, Name: name, MessageCount: c.MessageCount, OldestAt: agentTime(c.OldestTS), NewestAt: agentTime(c.NewestTS), Status: c.Status, BlockedReason: c.BlockedReason}
		if cut {
			d.FieldsTruncated = []string{"name"}
			meta.Recovery = "Retained recovery observations do not prove completeness. " + agentCoverageRecovery
		}
		data.Coverage = append(data.Coverage, d)
	}
	meta.Limit = limit
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}

func writeHistoryEvidenceTable(dst io.Writer, records []historyRecoveryDTO) error {
	if _, err := fmt.Fprintln(dst, "Retained recovery observations (local evidence; remote completeness unknown):"); err != nil {
		return err
	}
	w := newTableWriter(dst)
	fmt.Fprintln(w, "INPUT\tSLOT\tATTEMPT\tSTATE\tSTARTED\tSTOP\tPRIMARY OBSERVED\tNET GROWTH")
	for _, r := range records {
		for _, slot := range []struct {
			name string
			a    *historyObservationDTO
		}{{"latest", r.Latest}, {"last_success", r.LastSuccess}} {
			if slot.a == nil {
				fmt.Fprintf(w, "%s\t%s\t-\tno retained record\t-\t-\t-\t-\n", r.RequestedChatJID, slot.name)
				continue
			}
			growth := "unknown"
			if slot.a.NetGrowth != nil {
				growth = fmt.Sprint(*slot.a.NetGrowth)
			}
			primary := "-"
			if slot.a.PrimaryNoMoreObservedAt != nil {
				primary = slot.a.PrimaryNoMoreObservedAt.Format(time.RFC3339Nano)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.RequestedChatJID, slot.name, slot.a.AttemptID, slot.a.State, slot.a.StartedAt.Format(time.RFC3339Nano), slot.a.StopReason, primary, growth)
		}
	}
	return w.Flush()
}
