package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
)

const outboundSendKind = "outbound_send_v1"

type outboundDelegateResult struct {
	Capability  string                  `json:"capability"`
	RequestID   string                  `json:"request_id"`
	RequestHash string                  `json:"request_hash"`
	Result      *app.OutboundSendResult `json:"result,omitempty"`
	Failure     *app.OutboundSendError  `json:"failure,omitempty"`
}

func outboundRequestHash(r app.OutboundSendRequest) string {
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(append([]byte("wacli-outbound-send-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func outboundIPCUncertain(r *app.OutboundSendRequest, cause error) *app.OutboundSendError {
	var request app.OutboundSendRequest
	if r != nil {
		request = *r
	}
	return &app.OutboundSendError{Code: "outcome_uncertain", Request: request, Cause: cause}
}

func outboundRefusal(req sendDelegateRequest, code string) sendDelegateResponse {
	if req.Outbound == nil {
		return sendDelegateResponse{Outbound: &outboundDelegateResult{Capability: outboundSendKind}}
	}
	r := *req.Outbound
	return sendDelegateResponse{Outbound: &outboundDelegateResult{Capability: outboundSendKind, RequestID: r.RequestID, RequestHash: outboundRequestHash(r), Failure: &app.OutboundSendError{Code: code, Request: r}}}
}

func validateOutboundDelegate(r app.OutboundSendRequest, resp sendDelegateResponse) (app.OutboundSendResult, error) {
	fail := func() (app.OutboundSendResult, error) { return app.OutboundSendResult{}, outboundIPCUncertain(&r, nil) }
	d := resp.Outbound
	if d == nil || d.Capability != outboundSendKind || d.RequestID != r.RequestID || d.RequestHash != outboundRequestHash(r) || (d.Result == nil) == (d.Failure == nil) {
		return fail()
	}
	result := d.Result
	if d.Failure != nil {
		if resp.OK || d.Failure.Request != r || d.Failure.OperationID != "" && store.ValidateDraftID(d.Failure.OperationID) != nil || d.Failure.MessageID != "" && store.ValidateOutboundMessageID(d.Failure.MessageID) != nil {
			return fail()
		}
		switch d.Failure.Code {
		case "invalid_arguments", "read_only", "not_found", "store_error", "scope_conflict", "idempotency_conflict", "hash_conflict", "draft_conflict", "revision_required", "identity_unavailable", "preparation_failed", "not_dispatched", "rejected", "uncertain", "persistence_unconfirmed", "outcome_uncertain":
		default:
			return fail()
		}
		result = d.Failure.Result
	} else if !resp.OK {
		return fail()
	}
	if result != nil {
		o := result.Entry.Operation
		if store.ValidateOutboundOperation(o) != nil || o.Version != r.Version || o.Account.PN != r.OwnPN || o.DraftID != r.DraftID || o.RevisionID != r.RevisionID || o.Hash != r.Hash || o.Key != r.Key || (result.Persistence != "confirmed" && result.Persistence != "unconfirmed") {
			return fail()
		}
		if result.KnownResult != store.OutboundPending && result.KnownResult != store.OutboundAccepted && result.KnownResult != store.OutboundRejected && result.KnownResult != store.OutboundNotDispatched && result.KnownResult != store.OutboundUncertain {
			return fail()
		}
		if result.Persistence == "confirmed" && result.KnownResult != o.Result {
			return fail()
		}
		e := result.Entry.Evidence
		validCertainty := func(s string) bool { return s == "unknown" || s == "observed" }
		if !validCertainty(e.Accepted) || !validCertainty(e.Delivered) || !validCertainty(e.Read) || e.Read == "observed" && e.Delivered != "observed" || e.Delivered == "observed" && e.Accepted != "observed" || o.Result == store.OutboundAccepted && e.Accepted != "observed" {
			return fail()
		}
		if strings.HasSuffix(o.Recipient.JID, "@g.us") {
			if e.Scope != "participants" || e.Delivered != "unknown" || e.Read != "unknown" || e.DeliveredParticipants == nil || e.ReadParticipants == nil || *e.ReadParticipants < 0 || *e.DeliveredParticipants < *e.ReadParticipants {
				return fail()
			}
		} else if e.Scope != "recipient" || e.DeliveredParticipants != nil || e.ReadParticipants != nil {
			return fail()
		}
		page := result.Entry.Observations
		if len(page.Items) > 1 || page.NextCursor != nil && store.ValidateOutboundCursor(*page.NextCursor) != nil {
			return fail()
		}
		for _, f := range page.Items {
			if store.ValidateOutboundObservation(o, f) != nil {
				return fail()
			}
		}
		if result.KnownACK != nil {
			a := result.KnownACK
			if store.ValidateOutboundObservation(o, *a) != nil || result.KnownResult != store.OutboundAccepted || a.Fact != store.OutboundAck || a.Source != store.OutboundSendResponse || a.ChatJID != o.Recipient.JID || a.ActorJID != o.Account.PN && a.ActorJID != o.Account.LID || a.EventAt == nil {
				return fail()
			}
		}
		if resp.OK && !result.Duplicate && (result.Persistence != "confirmed" || o.Result != store.OutboundAccepted) {
			return fail()
		}
	}
	if d.Failure != nil {
		return app.OutboundSendResult{}, d.Failure
	}
	return *result, nil
}

type outboundPacerKey struct{}

func executeDelegatedOutbound(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.Outbound == nil {
		return outboundRefusal(req, "invalid_arguments"), nil
	}
	r := *req.Outbound
	p, _ := ctx.Value(outboundPacerKey{}).(*sendPacer)
	admitted := false
	before := func(ctx context.Context) error {
		if !p.wait(ctx) {
			return context.DeadlineExceeded
		}
		admitted = true
		return warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), io.Discard)
	}
	result, err := a.SendOutbound(ctx, r, before)
	if admitted {
		p.record()
	}
	d := &outboundDelegateResult{Capability: outboundSendKind, RequestID: r.RequestID, RequestHash: outboundRequestHash(r)}
	if err != nil {
		var typed *app.OutboundSendError
		if !errors.As(err, &typed) {
			return outboundRefusal(req, "store_error"), nil
		}
		copy := *typed
		copy.Cause = nil
		d.Failure = &copy
		return sendDelegateResponse{Outbound: d}, nil
	}
	d.Result = &result
	return sendDelegateResponse{OK: true, Outbound: d}, nil
}

func outboundStandalonePacing(flags *rootFlags, dir string) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var dst io.Writer = os.Stderr
		if flags.agent {
			dst = io.Discard
		}
		return warnRapidSendIfNeeded(dir, time.Now().UTC(), dst)
	}
}
