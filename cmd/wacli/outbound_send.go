package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newOutboundSendCmd(flags *rootFlags) *cobra.Command {
	var revision, hash, key string
	c := &cobra.Command{Use: "send DRAFT_ID", Short: "Dispatch one explicit frozen revision; retain the result for inspection", Args: cobra.ExactArgs(1)}
	c.Flags().StringVar(&revision, "revision", "", "exact immutable revision ID (required)")
	c.Flags().StringVar(&hash, "expect-hash", "", "exact canonical payload SHA-256 (required)")
	c.Flags().StringVar(&key, "key", "", "literal idempotency key, 1–128 ASCII ! through ~ (required)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if flags.isReadOnly() {
			return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects outbound dispatch.", ExitCode: 2}
		}
		if err := app.ValidateOutboundSelection(args[0], revision, hash, key); err != nil {
			return agentUsageError(err)
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		requestID, err := store.NewDraftID()
		if err != nil {
			return err
		}
		r := app.OutboundSendRequest{Version: 1, RequestID: requestID, StoreRef: flags.storeDir, DraftID: args[0], RevisionID: revision, Hash: hash, Key: key}
		flags.agentOutboundRequest = &r
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		// Only frozen archive identity is used here, including for duplicates.
		reader, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyOutboundActionError(err, &r)
		}
		record, err := reader.DB().ReadDraftRecord(ctx, r.DraftID)
		if err != nil {
			closeApp(reader, lk)
			return classifyOutboundActionError(err, &r)
		}
		r.OwnPN = record.AccountID
		old, found, err := app.LookupOutboundSend(ctx, reader.DB().Outbound(), r)
		closeApp(reader, lk)
		if err != nil {
			return classifyOutboundActionError(err, &r)
		}
		if found {
			return writeOutboundSendResult(os.Stdout, flags, r, old)
		}
		writer, writerLock, err := newApp(ctx, flags, true, true)
		if err != nil {
			resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
			if !delegated {
				return classifyOutboundActionError(delegateErr, &r)
			}
			if delegateErr != nil {
				return classifyOutboundActionError(delegateErr, &r)
			}
			result, err := validateOutboundDelegate(r, resp)
			if err != nil {
				return classifyOutboundActionError(err, &r)
			}
			return writeOutboundSendResult(os.Stdout, flags, r, result)
		}
		defer closeApp(writer, writerLock)
		result, err := writer.SendOutbound(ctx, r, outboundStandalonePacing(flags, writer.StoreDir()))
		if err != nil {
			return classifyOutboundActionError(err, &r)
		}
		return writeOutboundSendResult(os.Stdout, flags, r, result)
	}
	return c
}

type outboundSendDTO struct {
	RequestID       string                     `json:"request_id"`
	Operation       outboundDTO                `json:"operation"`
	Duplicate       bool                       `json:"duplicate"`
	Persistence     string                     `json:"persistence"`
	KnownResult     store.OutboundResult       `json:"known_result"`
	KnownACK        *store.OutboundObservation `json:"known_ack,omitempty"`
	HistoryWarning  bool                       `json:"history_warning"`
	ProtocolRetries string                     `json:"protocol_retries"`
}

func writeOutboundSendResult(dst io.Writer, flags *rootFlags, r app.OutboundSendRequest, result app.OutboundSendResult) error {
	data := outboundSendDTO{r.RequestID, projectOutbound(result.Entry.Operation, result.Entry.Evidence, outboundFull(flags)), result.Duplicate, result.Persistence, result.KnownResult, result.KnownACK, result.HistoryWarning, "sdk_managed_same_message_id"}
	meta := agentMeta(flags)
	meta.Source = "live"
	meta.Recovery = fmt.Sprintf("Inspect outbound show %s or --key with --account-jid; SDK protocol retries may continue after this action. Accepted does not prove delivery.", result.Entry.Operation.ID)
	var err error
	if flags.agent {
		err = out.WriteAgentActionJSON(dst, flags.agentAccount, meta, data)
	} else {
		err = out.WriteActionJSON(dst, struct {
			Account  out.AgentAccount `json:"account"`
			Outbound outboundSendDTO  `json:"outbound"`
		}{flags.agentAccount, data})
	}
	if err != nil {
		return classifyOutboundActionError(&app.OutboundSendError{Code: "output_unconfirmed", Request: r, Result: &result, Cause: err}, &r)
	}
	return nil
}

func classifyOutboundActionError(err error, request *app.OutboundSendRequest) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) && existing.Outbound != nil {
		return existing
	}
	code, exit := "store_error", 4
	var failure *app.OutboundSendError
	var result *app.OutboundSendResult
	if errors.As(err, &failure) {
		code = failure.Code
		request = &failure.Request
		result = failure.Result
	} else if errors.As(err, &existing) {
		code = existing.Code
		exit = existing.ExitCode
	} else {
		var draft *store.DraftError
		if errors.As(err, &draft) && draft.Code == "not_found" {
			code = "not_found"
		}
		if lock.IsLocked(err) {
			code = "locked"
		}
	}
	switch code {
	case "invalid_arguments", "read_only":
		exit = 2
	case "not_found":
		exit = 3
	case "store_error", "persistence_unconfirmed":
		exit = 4
	default:
		exit = 1
	}
	d := &out.AgentOutboundError{Phase: "preflight", AttemptResult: "not_dispatched", KnownResult: "not_dispatched", Persistence: "unconfirmed"}
	if request != nil {
		d.RequestID = request.RequestID
		d.DraftID = request.DraftID
		d.RevisionID = request.RevisionID
		d.Hash = request.Hash
		d.Key = request.Key
		d.OwnPN = request.OwnPN
	}
	if code == "outcome_uncertain" {
		d.AttemptResult = "uncertain"
		d.KnownResult = "uncertain"
		d.Phase = "dispatch_possible"
	}
	if failure != nil {
		d.OperationID, d.MessageID = failure.OperationID, failure.MessageID
	}
	if result != nil {
		o := result.Entry.Operation
		d.OperationID = o.ID
		d.MessageID = o.MessageID
		d.Phase = string(o.Phase)
		d.AttemptResult = string(o.Result)
		d.KnownResult = string(result.KnownResult)
		d.Persistence = result.Persistence
		d.HistoryWarning = result.HistoryWarning
		if result.KnownACK != nil {
			d.KnownACK = true
			d.ACKAt = result.KnownACK.EventAt
		}
	}
	message := "Outbound action did not confirm a successful retained dispatch."
	if code == "read_only" {
		message = "Read-only policy rejects outbound dispatch."
	} else if d.OperationID != "" {
		message += fmt.Sprintf(" Inspect outbound show %s; known result=%s, persistence=%s.", d.OperationID, d.KnownResult, d.Persistence)
	} else if d.Key != "" && d.OwnPN != "" {
		message += fmt.Sprintf(" Inspect key %q for own PN %s.", d.Key, d.OwnPN)
	}
	return &out.AgentError{Code: code, Message: message, Recovery: "Inspect outbound show by operation ID or key plus frozen own PN; do not create a new attempt to resolve uncertainty. SDK protocol retries may continue.", ExitCode: exit, Cause: err, Outbound: d}
}
