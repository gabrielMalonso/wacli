package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

type ChatStateAction string

const (
	ChatStateMarkUnread ChatStateAction = "mark-unread"
	ChatStateArchive    ChatStateAction = "archive"
	ChatStateUnarchive  ChatStateAction = "unarchive"
)

type ChatStateOutcome string
type ChatStateMirror string

const (
	ChatStateNotDispatched     ChatStateOutcome = "not_dispatched"
	ChatStateSDKCompleted      ChatStateOutcome = "sdk_completed"
	ChatStateUncertain         ChatStateOutcome = "uncertain"
	ChatStateMirrorUnknown     ChatStateMirror  = "unknown"
	ChatStateMirrorPersisted   ChatStateMirror  = "persisted"
	ChatStateMirrorUnconfirmed ChatStateMirror  = "unconfirmed"
)

// ChatStateRequest is one invocation, not a replay token or durable operation.
// Requested is normalized syntax only; no PN/LID discovery is performed.
type ChatStateRequest struct {
	Version   int             `json:"version"`
	StoreRef  string          `json:"store_ref"`
	Requested string          `json:"requested"`
	Action    ChatStateAction `json:"action"`
}

func (r ChatStateRequest) Validate() error {
	if r.Version != 1 || !filepath.IsAbs(r.StoreRef) || len(r.StoreRef) > 4096 || !utf8.ValidString(r.StoreRef) || strings.ContainsAny(r.StoreRef, "\x00?#") {
		return fmt.Errorf("invalid chat state scope")
	}
	jid, err := store.NormalizeDraftTarget(r.Requested)
	if err != nil || jid != r.Requested {
		return fmt.Errorf("an explicit canonical phone/DM/group JID is required")
	}
	switch r.Action {
	case ChatStateMarkUnread, ChatStateArchive, ChatStateUnarchive:
		return nil
	default:
		return fmt.Errorf("unsupported agent chat state action")
	}
}

type ChatStateObservation struct {
	Account store.DraftIdentity  `json:"account_identity"`
	Target  store.DraftRecipient `json:"target"`
}

// Outcome is SDK-call knowledge; mirror is the local command's persistence.
// Neither is confirmation of current remote state or an atomic account binding.
type ChatStateResult struct {
	Request     ChatStateRequest      `json:"request"`
	Observation *ChatStateObservation `json:"observation,omitempty"`
	Outcome     ChatStateOutcome      `json:"outcome"`
	LocalMirror ChatStateMirror       `json:"local_mirror"`
}

type ChatStateError struct {
	Code   string          `json:"code"`
	Result ChatStateResult `json:"result"`
	Cause  error           `json:"-"`
}

func (e *ChatStateError) Error() string { return "chat state action: " + e.Code }
func (e *ChatStateError) Unwrap() error { return e.Cause }

func ChatStateFailure(r ChatStateRequest, code string, cause error) *ChatStateError {
	return &ChatStateError{Code: code, Cause: cause, Result: ChatStateResult{Request: r, Outcome: ChatStateNotDispatched, LocalMirror: ChatStateMirrorUnknown}}
}

// ApplyAgentChatState needs the existing writable, authenticated connected App.
// Standalone connection belongs to the CLI; owner requests never reconnect here.
func (a *App) ApplyAgentChatState(ctx context.Context, r ChatStateRequest) (ChatStateResult, error) {
	failure := ChatStateFailure(r, "invalid_arguments", nil)
	if err := r.Validate(); err != nil {
		failure.Cause = err
		return failure.Result, failure
	}
	if a.ReadOnly() {
		failure.Code = "read_only"
		return failure.Result, failure
	}
	if r.StoreRef != a.StoreDir() {
		failure.Code = "store_unavailable"
		return failure.Result, failure
	}
	client := a.WA()
	if client == nil || !client.IsAuthed() || !client.IsConnected() {
		failure.Code = "not_dispatched"
		return failure.Result, failure
	}
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularLow)
	if err != nil {
		failure.Code, failure.Cause = "not_dispatched", err
		return failure.Result, failure
	}
	defer release()
	// Connection and recovery can learn/change the map. Observe once after
	// those stages, then reuse this exact target for mutation, mirror and output.
	identities, err := a.readLocalIdentitiesPolicy(ctx, []string{r.Requested}, 1, true)
	if err != nil || len(identities) != 1 {
		failure.Code, failure.Cause = "identity_unavailable", err
		return failure.Result, failure
	}
	i := identities[0]
	if i.AccountJID == "" || i.AccountJID != client.LinkedJID() || i.AccountAliasJID != "" && i.AccountAliasJID != client.LinkedLID() {
		failure.Code = "identity_unavailable"
		return failure.Result, failure
	}
	failure.Result.Observation = &ChatStateObservation{Account: store.DraftIdentity{PN: i.AccountJID, LID: i.AccountAliasJID}, Target: store.DraftRecipientFromPublic(i.ChatJID, i.AliasJID)}
	if err := ValidateChatStateResult(r, failure.Result); err != nil {
		failure.Code, failure.Cause = "identity_unavailable", err
		failure.Result.Observation = nil
		return failure.Result, failure
	}
	// Builders index target.String() directly. Unlike outbound SendMessage,
	// these operations do not require translating a PN to a LID.
	jid, err := ParseHistoryJID(r.Requested)
	if err != nil {
		failure.Code, failure.Cause = "identity_unavailable", err
		return failure.Result, failure
	}
	beforeSend := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !client.IsConnected() || client.LinkedJID() != i.AccountJID || i.AccountAliasJID != "" && client.LinkedLID() != i.AccountAliasJID {
			return fmt.Errorf("connected account changed or unavailable")
		}
		return nil
	}
	var outcome ChatStateOutcome
	var mirror ChatStateMirror
	if r.Action == ChatStateMarkUnread {
		outcome, mirror, err = a.markChatReadResolved(ctx, jid, i.ChatJID, false, beforeSend)
	} else {
		outcome, mirror, err = a.archiveChatResolved(ctx, jid, i.ChatJID, r.Action == ChatStateArchive, beforeSend)
	}
	failure.Result.Outcome, failure.Result.LocalMirror = outcome, mirror
	if err == nil {
		return failure.Result, nil
	}
	failure.Code, failure.Cause = "chat_state_outcome_uncertain", err
	if outcome == ChatStateNotDispatched {
		failure.Code = "not_dispatched"
	} else if outcome == ChatStateSDKCompleted {
		failure.Code = "chat_state_local_mirror_unconfirmed"
	}
	return failure.Result, failure
}

// ValidateChatStateResult checks structural correlation, not remote identity or
// freshness. Each transport connection belongs to only one request.
func ValidateChatStateResult(r ChatStateRequest, result ChatStateResult) error {
	if r.Validate() != nil || result.Request != r {
		return fmt.Errorf("chat state response scope mismatch")
	}
	switch result.Outcome {
	case ChatStateNotDispatched, ChatStateUncertain:
		if result.LocalMirror != ChatStateMirrorUnknown {
			return fmt.Errorf("inconsistent chat state result")
		}
	case ChatStateSDKCompleted:
		if result.Observation == nil || result.LocalMirror != ChatStateMirrorPersisted && result.LocalMirror != ChatStateMirrorUnconfirmed {
			return fmt.Errorf("inconsistent completed chat state result")
		}
	default:
		return fmt.Errorf("unknown chat state outcome")
	}
	if o := result.Observation; o != nil {
		valid := func(jid, server string) bool {
			value, err := store.NormalizeDraftTarget(jid)
			return err == nil && value == jid && strings.HasSuffix(value, "@"+server)
		}
		if !valid(o.Account.PN, types.DefaultUserServer) || o.Account.LID != "" && !valid(o.Account.LID, types.HiddenUserServer) {
			return fmt.Errorf("invalid observed account")
		}
		t := o.Target
		if t.PN != "" && !valid(t.PN, types.DefaultUserServer) || t.LID != "" && !valid(t.LID, types.HiddenUserServer) {
			return fmt.Errorf("invalid observed pair")
		}
		expected := store.DraftRecipientFromPublic(r.Requested, "")
		if t.PN != "" && t.LID != "" {
			if r.Requested != t.PN && r.Requested != t.LID {
				return fmt.Errorf("observed pair does not contain requested identity")
			}
			expected = store.DraftRecipient{JID: t.PN, PN: t.PN, LID: t.LID}
		}
		if t != expected {
			return fmt.Errorf("observed target does not match requested identity")
		}
	}
	return nil
}
