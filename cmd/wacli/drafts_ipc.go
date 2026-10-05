package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

const draftWriteKind = "local_draft_write"

// Optional refusal advice; unknown categories retain the correlated base error.
type draftValidationField string

const (
	draftReplySenderField      draftValidationField = "reply.sender"
	draftReplyUnsupportedField draftValidationField = "reply.unsupported"
)

func draftRequestsQuote(req sendDelegateRequest) bool {
	return req.Kind == draftWriteKind && req.Draft != nil && (req.Draft.Action == "create" || req.Draft.Action == "update") && req.Draft.Input != nil && req.Draft.Input.ReplyTo != ""
}

// This echo binds a response to the complete local request; it is not dedupe.
func draftRequestHash(request app.DraftWriteRequest) string {
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(append([]byte("wacli-local-draft-request-v1\x00"), raw...))
	return hex.EncodeToString(digest[:])
}

type draftDelegateResult struct {
	RequestHash string                    `json:"request_hash"`
	Record      store.DraftRecord         `json:"record"`
	RevisionID  string                    `json:"revision_id"`
	Number      int                       `json:"number"`
	CreatedAt   time.Time                 `json:"created_at"`
	Payload     string                    `json:"payload"`
	Hash        string                    `json:"hash"`
	Review      store.DraftReviewSnapshot `json:"review"`
}

func projectDraftDelegate(request app.DraftWriteRequest, entry store.DraftEntry) *draftDelegateResult {
	return &draftDelegateResult{draftRequestHash(request), entry.Record, entry.Revision.ID(), entry.Number, entry.Revision.CreatedAt(), string(entry.Revision.Payload().CanonicalJSON()), entry.Revision.Payload().Hash(), entry.Revision.Review()}
}
func draftIPCUncertain(request *app.DraftWriteRequest, cause error) *store.DraftError {
	id, rev := "", ""
	if request != nil {
		id = request.DraftID
		rev = request.RevisionID
	}
	return store.DraftFailure("local_write_uncertain", id, rev, "", cause)
}
func validateDraftDelegateResult(request app.DraftWriteRequest, result *draftDelegateResult) (store.DraftEntry, error) {
	fail := func() (store.DraftEntry, error) { return store.DraftEntry{}, draftIPCUncertain(&request, nil) }
	if result == nil || result.RequestHash != draftRequestHash(request) || result.Record.ID != request.DraftID || result.RevisionID != request.RevisionID || result.Record.HeadRevisionID != request.RevisionID || result.Number < 1 {
		return fail()
	}
	if request.Action == "discard" {
		if result.Record.State != "discarded" || result.Record.DiscardedAt == nil {
			return fail()
		}
	} else if result.Record.State != "active" || result.Record.DiscardedAt != nil {
		return fail()
	}
	payload, err := store.DecodeDraftPayload([]byte(result.Payload))
	if err != nil || payload.Hash() != result.Hash || payload.Data().Account.PN != result.Record.AccountID {
		return fail()
	}
	revision, err := store.NewDraftRevision(request.DraftID, result.RevisionID, result.CreatedAt, payload, result.Review)
	if err != nil {
		return fail()
	}
	return store.DraftEntry{Record: result.Record, Revision: revision, Number: result.Number}, nil
}
func executeDelegatedDraft(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.Draft == nil {
		return sendDelegateResponse{}, draftIPCUncertain(nil, nil)
	}
	entry, err := a.WriteLocalDraft(ctx, *req.Draft, openOutboundMedia)
	if err != nil {
		return draftRefusal(req, err), nil
	}
	return sendDelegateResponse{OK: true, DraftResult: projectDraftDelegate(*req.Draft, entry)}, nil
}
func draftRefusal(req sendDelegateRequest, err error) sendDelegateResponse {
	var failure *store.DraftError
	if !errors.As(err, &failure) {
		code := "store_unavailable"
		var validation *store.DraftValidationError
		if errors.As(err, &validation) {
			code = "invalid_arguments"
		}
		failure = store.DraftFailure(code, "", "", "", nil)
	}
	copy := *failure
	copy.Cause = nil
	if req.Draft != nil {
		copy.DraftID = req.Draft.DraftID
		copy.RevisionID = req.Draft.RevisionID
	}
	resp := sendDelegateResponse{DraftFailure: &copy}
	if req.Draft != nil {
		resp.DraftRequestHash = draftRequestHash(*req.Draft)
	}
	var validation *store.DraftValidationError
	if copy.Code == "invalid_arguments" && draftRequestsQuote(req) && errors.As(err, &validation) {
		switch validation.Field {
		case string(draftReplySenderField), string(draftReplyUnsupportedField):
			resp.DraftValidationField = draftValidationField(validation.Field)
		}
	}
	return resp
}
func draftIPCFailure(req sendDelegateRequest, resp sendDelegateResponse) error {
	failure := resp.DraftFailure
	if req.Draft == nil || failure == nil || resp.DraftRequestHash != draftRequestHash(*req.Draft) || failure.DraftID != req.Draft.DraftID || failure.RevisionID != req.Draft.RevisionID {
		return draftIPCUncertain(req.Draft, nil)
	}
	switch failure.Code {
	case "not_found", "identity_unavailable", "store_unavailable", "document_unavailable", "read_only", "draft_conflict", "invalid_arguments", "local_write_not_dispatched", "local_write_uncertain":
	default:
		return draftIPCUncertain(req.Draft, nil)
	}
	if failure.Hash != "" {
		raw, err := hex.DecodeString(failure.Hash)
		if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != failure.Hash {
			return draftIPCUncertain(req.Draft, nil)
		}
	}
	if failure.Code == "invalid_arguments" && draftRequestsQuote(req) {
		switch resp.DraftValidationField {
		case draftReplySenderField, draftReplyUnsupportedField:
			classified := classifyDraftError(&store.DraftValidationError{Field: string(resp.DraftValidationField)})
			classified.Cause = failure
			classified.Draft = &out.AgentDraftError{DraftID: failure.DraftID, RevisionID: failure.RevisionID, Hash: failure.Hash}
			return classified
		}
	}
	return failure
}
