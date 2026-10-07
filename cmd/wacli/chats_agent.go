package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func runAgentChatState(flags *rootFlags, opts chatStateOptions, action app.ChatStateAction) error {
	if err := flags.requireWritable(); err != nil {
		return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects chat state mutations.", ExitCode: 2, Cause: err}
	}
	if opts.receipts || action == app.ChatStateMarkRead && (flags.timeout <= 0 || flags.timeout > 5*time.Minute) {
		return agentUsageError(errors.New("agent mark-read excludes receipts and requires a positive timeout at most 5m"))
	}
	requested, err := store.NormalizeDraftTarget(opts.chat)
	if err != nil || opts.pick != 0 {
		return agentUsageError(errors.New("--chat requires an explicit phone/DM/group JID; --pick is not supported with --agent"))
	}
	dir, err := resolveStoreDir(flags)
	if err != nil {
		return agentStoreError(err)
	}
	r := app.ChatStateRequest{Version: 1, StoreRef: dir, Requested: requested, Action: action}
	flags.agentChatStateRequest = &r
	if err := r.Validate(); err != nil {
		return agentUsageError(err)
	}
	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()
	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &r})
		if !delegated {
			return classifyChatStateAgentError(err, &r)
		}
		if delegateErr != nil {
			return delegateErr
		}
		result, err := validateAgentChatStateDelegate(r, resp)
		if err != nil {
			return err
		}
		return writeAgentChatStateResult(os.Stdout, flags, result)
	}
	defer closeApp(a, lk)
	result, err := connectAndApplyAgentChatState(ctx, a, r)
	if err != nil {
		return err
	}
	return writeAgentChatStateResult(os.Stdout, flags, result)
}

// Keep the app-state handler through disconnect, then App.Close drains before
// closing the DB and releasing LOCK, including persistence after cancellation.
func connectAndApplyAgentChatState(ctx context.Context, a *app.App, r app.ChatStateRequest) (app.ChatStateResult, error) {
	fail := func(err error) (app.ChatStateResult, error) {
		failure := app.ChatStateFailure(r, "not_dispatched", err)
		return failure.Result, failure
	}
	if a.ReadOnly() {
		failure := app.ChatStateFailure(r, "read_only", nil)
		return failure.Result, failure
	}
	if err := r.Validate(); err != nil {
		failure := app.ChatStateFailure(r, "invalid_arguments", err)
		return failure.Result, failure
	}
	if r.StoreRef != a.StoreDir() {
		failure := app.ChatStateFailure(r, "store_unavailable", nil)
		return failure.Result, failure
	}
	if err := a.EnsureAuthed(ctx); err != nil {
		failure := app.ChatStateFailure(r, "identity_unavailable", err)
		return failure.Result, failure
	}
	remove, err := a.AddChatStatePersistenceHandler(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() {
		a.WA().Disconnect()
		remove()
	}()
	if err := a.Connect(ctx, false, nil); err != nil {
		return fail(err)
	}
	return a.ApplyAgentChatState(ctx, r)
}

func writeAgentChatStateResult(dst io.Writer, flags *rootFlags, result app.ChatStateResult) error {
	meta := agentMeta(flags)
	meta.Source = "live"
	meta.Recovery = "SDK completion and local mirroring do not confirm current remote state. Inspect chats show --agent --detail full locally; do not repeat an uncertain mutation automatically."
	if err := out.WriteAgentActionJSON(dst, flags.agentAccount, meta, result); err != nil {
		return &app.ChatStateError{Code: "chat_state_output_unconfirmed", Result: result, Cause: err}
	}
	return nil
}

func classifyChatStateAgentError(err error, request *app.ChatStateRequest) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) {
		copy := *existing
		if copy.Code == "invalid_arguments" {
			copy.Message = "Invalid explicit chat state arguments."
		}
		if existing.ChatState != nil {
			return &copy
		}
		copy.ChatState = &out.AgentChatStateError{Outcome: string(app.ChatStateNotDispatched), LocalMirror: string(app.ChatStateMirrorUnknown)}
		if request != nil {
			copy.ChatState.Requested, copy.ChatState.Action = request.Requested, string(request.Action)
		}
		return &copy
	}
	var failure *app.ChatStateError
	if !errors.As(err, &failure) {
		code := "store_unavailable"
		if lock.IsLocked(err) {
			code = "store_locked"
		}
		var r app.ChatStateRequest
		if request != nil {
			r = *request
		}
		failure = app.ChatStateFailure(r, code, err)
	}
	code, message, exit := failure.Code, "Chat state invocation has no reliable result; do not repeat it automatically.", 1
	switch code {
	case "invalid_arguments", "read_only":
		exit, message = 2, "Invalid explicit chat state arguments or read-only policy rejection."
	case "store_unavailable", "identity_unavailable":
		exit, message = 4, "Selected local store or public account/identity observation is unavailable or incompatible."
	case "store_locked":
		message = "Selected store is locked and its owner is unavailable; no request was submitted."
	case "not_dispatched":
		message = "Chat state mutation was not invoked; earlier connection/recovery may have persisted state."
	case "chat_state_local_mirror_unconfirmed":
		message = "SDK completed the chat state call, but local mirroring is unconfirmed."
	case "chat_state_output_unconfirmed":
		message = "Chat state output was not confirmed; retained invocation knowledge does not imply rollback."
	case "chat_state_outcome_uncertain":
	default:
		code = "chat_state_outcome_uncertain"
	}
	r := failure.Result
	d := &out.AgentChatStateError{Requested: r.Request.Requested, Action: string(r.Request.Action), Outcome: string(r.Outcome), LocalMirror: string(r.LocalMirror)}
	if o := r.Observation; o != nil {
		d.OwnPN, d.OwnLID = o.Account.PN, o.Account.LID
		d.TargetJID, d.TargetPN, d.TargetLID = o.Target.JID, o.Target.PN, o.Target.LID
	}
	return &out.AgentError{Code: code, Message: message, ExitCode: exit, ChatState: d, Cause: err, Recovery: "Inspect local chats show --agent --detail full and account status. A missing reliable reply does not authorize automatic retry; an older owner may need an explicit restart."}
}
