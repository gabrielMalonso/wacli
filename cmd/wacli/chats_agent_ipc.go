package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/openclaw/wacli/internal/app"
)

const agentChatStateKind = "agent_chat_state"
const agentChatStateMaxFrame = 16384

type agentChatStateReply struct {
	Capability string               `json:"capability"`
	Result     *app.ChatStateResult `json:"result,omitempty"`
	Failure    *app.ChatStateError  `json:"failure,omitempty"`
}

func agentChatStateUncertain(r *app.ChatStateRequest, cause error) *app.ChatStateError {
	var request app.ChatStateRequest
	if r != nil {
		request = *r
	}
	failure := app.ChatStateFailure(request, "chat_state_outcome_uncertain", cause)
	failure.Result.Outcome = app.ChatStateUncertain
	return failure
}

func agentChatStateRefusal(req sendDelegateRequest, code string) sendDelegateResponse {
	var r app.ChatStateRequest
	if req.AgentChatState != nil {
		r = *req.AgentChatState
	}
	failure := app.ChatStateFailure(r, code, nil)
	if code == "chat_state_outcome_uncertain" {
		failure.Result.Outcome = app.ChatStateUncertain
	}
	return sendDelegateResponse{AgentChatState: &agentChatStateReply{Capability: agentChatStateKind, Failure: failure}}
}

// The new family has no legacy adjuncts. Connection ownership plus exact typed
// scope is enough correlation; no request ID, hash or replay catalogue is needed.
func validateAgentChatStateEnvelope(req sendDelegateRequest) bool {
	if req.Version != sendDelegateVersion || req.Kind != agentChatStateKind || req.AgentChatState == nil || req.AgentChatState.Validate() != nil {
		return false
	}
	expected := sendDelegateRequest{AgentChatState: req.AgentChatState, Kind: agentChatStateKind, Version: sendDelegateVersion, TimeoutMS: req.TimeoutMS, DeadlineUnixMS: req.DeadlineUnixMS}
	raw, err := json.Marshal(req)
	want, wantErr := json.Marshal(expected)
	return err == nil && wantErr == nil && len(raw) <= agentChatStateMaxFrame && bytes.Equal(raw, want)
}

func validateAgentChatStateDelegate(r app.ChatStateRequest, resp sendDelegateResponse) (app.ChatStateResult, error) {
	uncertain := func() (app.ChatStateResult, error) { return app.ChatStateResult{}, agentChatStateUncertain(&r, nil) }
	d := resp.AgentChatState
	if d == nil || d.Capability != agentChatStateKind || (d.Result == nil) == (d.Failure == nil) {
		return uncertain()
	}
	expected := sendDelegateResponse{OK: resp.OK, AgentChatState: d}
	raw, err := json.Marshal(resp)
	want, wantErr := json.Marshal(expected)
	if err != nil || wantErr != nil || len(raw) > agentChatStateMaxFrame || !bytes.Equal(raw, want) {
		return uncertain()
	}
	if d.Failure != nil {
		f := d.Failure
		if resp.OK || app.ValidateChatStateResult(r, f.Result) != nil {
			return uncertain()
		}
		switch f.Code {
		case "invalid_arguments", "read_only", "store_unavailable", "identity_unavailable", "not_dispatched":
			if f.Result.Outcome != app.ChatStateNotDispatched {
				return uncertain()
			}
		case "chat_state_outcome_uncertain":
			if f.Result.Outcome != app.ChatStateUncertain {
				return uncertain()
			}
		case "chat_state_local_mirror_unconfirmed":
			if f.Result.Outcome != app.ChatStateSDKCompleted || f.Result.LocalMirror != app.ChatStateMirrorUnconfirmed {
				return uncertain()
			}
		default:
			return uncertain()
		}
		return app.ChatStateResult{}, f
	}
	result := *d.Result
	if !resp.OK || app.ValidateChatStateResult(r, result) != nil || result.Outcome != app.ChatStateSDKCompleted || result.LocalMirror != app.ChatStateMirrorPersisted {
		return uncertain()
	}
	return result, nil
}

func executeDelegatedAgentChatState(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if !validateAgentChatStateEnvelope(req) {
		return agentChatStateRefusal(req, "invalid_arguments"), nil
	}
	result, err := a.ApplyAgentChatState(ctx, *req.AgentChatState)
	d := &agentChatStateReply{Capability: agentChatStateKind}
	if err != nil {
		var failure *app.ChatStateError
		if !errors.As(err, &failure) {
			return agentChatStateRefusal(req, "chat_state_outcome_uncertain"), nil
		}
		copy := *failure
		copy.Cause = nil
		d.Failure = &copy
		return sendDelegateResponse{AgentChatState: d}, nil
	}
	d.Result = &result
	return sendDelegateResponse{OK: true, AgentChatState: d}, nil
}
