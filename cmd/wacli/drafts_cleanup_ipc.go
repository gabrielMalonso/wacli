package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
)

const draftCleanupKind = "local_draft_cleanup"
const draftCleanupMaxFrame = 16384

type draftCleanupReply struct {
	Capability  string                   `json:"capability"`
	RequestHash string                   `json:"request_hash"`
	Result      app.DraftCleanupResult   `json:"result"`
	Failure     *store.DraftCleanupError `json:"failure,omitempty"`
}

func draftCleanupRequestHash(r app.DraftCleanupRequest) string {
	raw, _ := json.Marshal(r)
	digest := sha256.Sum256(append([]byte("wacli-local-draft-cleanup-v1\x00"), raw...))
	return hex.EncodeToString(digest[:])
}
func draftCleanupIPCUncertain(r *app.DraftCleanupRequest, cause error) *draftCleanupActionError {
	var selection store.DraftCleanupSelection
	if r != nil {
		selection = r.Selection
	}
	result := draftCleanupNoRemoval(selection)
	result.Effect, result.RemovedBytes, result.DirectorySync = app.DraftCleanupUnknown, nil, app.DraftCleanupSyncUnknown
	return &draftCleanupActionError{result, store.DraftCleanupFailure(store.DraftCleanupOutcomeUncertain, selection, cause)}
}
func draftCleanupRefusal(req sendDelegateRequest, code store.DraftCleanupCode) sendDelegateResponse {
	if req.DraftCleanup == nil {
		return sendDelegateResponse{Error: "invalid local draft cleanup request"}
	}
	r := *req.DraftCleanup
	return sendDelegateResponse{DraftCleanup: &draftCleanupReply{Capability: draftCleanupKind, RequestHash: draftCleanupRequestHash(r), Result: draftCleanupNoRemoval(r.Selection), Failure: store.DraftCleanupFailure(code, r.Selection, nil)}}
}
func validateDraftCleanupEnvelope(req sendDelegateRequest) bool {
	if req.Version != sendDelegateVersion || req.Kind != draftCleanupKind || req.DraftCleanup == nil || req.DraftCleanup.Validate() != nil {
		return false
	}
	expected := sendDelegateRequest{DraftCleanup: req.DraftCleanup, Kind: draftCleanupKind, Version: sendDelegateVersion, TimeoutMS: req.TimeoutMS, DeadlineUnixMS: req.DeadlineUnixMS}
	raw, err := json.Marshal(req)
	want, wantErr := json.Marshal(expected)
	return err == nil && wantErr == nil && len(raw) <= draftCleanupMaxFrame && bytes.Equal(raw, want)
}
func executeDelegatedDraftCleanup(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if !validateDraftCleanupEnvelope(req) {
		return draftCleanupRefusal(req, store.DraftCleanupInvalidArguments), nil
	}
	r := *req.DraftCleanup
	if r.StoreRef != a.StoreDir() {
		return draftCleanupRefusal(req, store.DraftCleanupInvalidArguments), nil
	}
	result, err := a.CleanupDraftSnapshot(ctx, r)
	reply := &draftCleanupReply{Capability: draftCleanupKind, RequestHash: draftCleanupRequestHash(r), Result: result}
	if err != nil {
		var failure *store.DraftCleanupError
		if !errors.As(err, &failure) {
			return sendDelegateResponse{}, draftCleanupIPCUncertain(&r, err)
		}
		copy := *failure
		copy.Cause = nil
		copy.DraftID, copy.RevisionID, copy.Hash = r.Selection.DraftID, r.Selection.RevisionID, r.Selection.Hash
		reply.Failure = &copy
	}
	return sendDelegateResponse{OK: err == nil, DraftCleanup: reply}, nil
}
func validateDraftCleanupDelegate(r app.DraftCleanupRequest, resp sendDelegateResponse) (app.DraftCleanupResult, error) {
	uncertain := func() (app.DraftCleanupResult, error) {
		failure := draftCleanupIPCUncertain(&r, nil)
		return failure.Result, failure
	}
	d := resp.DraftCleanup
	if d == nil || d.Capability != draftCleanupKind || d.RequestHash != draftCleanupRequestHash(r) {
		return uncertain()
	}
	expected := sendDelegateResponse{OK: resp.OK, DraftCleanup: d}
	raw, err := json.Marshal(resp)
	want, wantErr := json.Marshal(expected)
	if err != nil || wantErr != nil || len(raw) > draftCleanupMaxFrame || !bytes.Equal(raw, want) {
		return uncertain()
	}
	result := d.Result
	s := r.Selection
	if result.DraftID != s.DraftID || result.RevisionID != s.RevisionID || result.Hash != s.Hash {
		return uncertain()
	}
	switch result.Effect {
	case app.DraftCleanupNotRemoved:
		if result.RemovedBytes == nil || *result.RemovedBytes != 0 || result.DirectorySync != app.DraftCleanupSyncNotAttempted || result.Outcome != app.DraftCleanupFailed && result.Outcome != app.DraftCleanupAlreadyAbsent {
			return uncertain()
		}
	case app.DraftCleanupRemoved:
		if result.RemovedBytes == nil || *result.RemovedBytes < 0 || *result.RemovedBytes > store.MaxDraftFileBytes || result.Outcome != app.DraftCleanupFailed && result.Outcome != app.DraftCleanupCompleted {
			return uncertain()
		}
		if result.DirectorySync != app.DraftCleanupSyncCompleted && result.DirectorySync != app.DraftCleanupSyncUnsupported && result.DirectorySync != app.DraftCleanupSyncUnknown {
			return uncertain()
		}
	case app.DraftCleanupUnknown:
		if result.RemovedBytes != nil || result.Outcome != app.DraftCleanupFailed || result.DirectorySync != app.DraftCleanupSyncNotAttempted && result.DirectorySync != app.DraftCleanupSyncUnknown {
			return uncertain()
		}
	default:
		return uncertain()
	}
	if d.Failure == nil {
		if !resp.OK || result.Outcome == app.DraftCleanupFailed || result.Effect == app.DraftCleanupUnknown || result.DirectorySync == app.DraftCleanupSyncUnknown {
			return uncertain()
		}
		return result, nil
	}
	f := d.Failure
	if resp.OK || result.Outcome != app.DraftCleanupFailed || f.DraftID != s.DraftID || f.RevisionID != s.RevisionID || f.Hash != s.Hash {
		return uncertain()
	}
	switch f.Code {
	case store.DraftCleanupOutcomeUncertain:
		// A known successful unlink may still have unconfirmed durability/output.
	case store.DraftCleanupInvalidArguments, store.DraftCleanupNotFound, store.DraftCleanupStoreError, store.DraftCleanupConflict, store.DraftCleanupProtected, store.DraftCleanupCommitUncertain, store.DraftCleanupReadOnly, store.DraftCleanupSnapshotChanged, store.DraftCleanupSnapshotError, store.DraftCleanupCanceled:
		if result.Effect != app.DraftCleanupNotRemoved {
			return uncertain()
		}
	default:
		return uncertain()
	}
	return result, &draftCleanupActionError{result, f}
}
