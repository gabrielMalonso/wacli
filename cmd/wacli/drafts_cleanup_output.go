package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

type draftCleanupPreviewItem struct {
	store.DraftCleanupItem
	SnapshotPath string `json:"snapshot_path,omitempty"`
}
type draftCleanupPreviewDTO struct {
	DraftID        string                    `json:"draft_id"`
	State          string                    `json:"state"`
	ExpectedHeadID string                    `json:"expected_head_revision_id"`
	Revisions      []draftCleanupPreviewItem `json:"revisions"`
	Counts         store.DraftCleanupCounts  `json:"page_counts"`
	HasMore        bool                      `json:"has_more"`
	NextCursor     *string                   `json:"next_cursor"`
}

func writeDraftCleanupPreview(w io.Writer, flags *rootFlags, page store.DraftCleanupPage) error {
	d := draftCleanupPreviewDTO{DraftID: page.Record.ID, State: page.Record.State, ExpectedHeadID: page.Record.HeadRevisionID, Revisions: make([]draftCleanupPreviewItem, 0, len(page.Items)), Counts: page.Counts, HasMore: page.HasMore, NextCursor: page.NextCursor}
	for _, item := range page.Items {
		dto := draftCleanupPreviewItem{DraftCleanupItem: item}
		if item.Document != nil && (flags.agent && flags.detail == "full" || !flags.agent && flags.fullOutput) {
			rel, err := store.DraftSnapshotRelativePath(item.RevisionID)
			if err != nil {
				return classifyDraftCleanupError(err, nil)
			}
			dto.SnapshotPath = filepath.Join(*flags.agentAccount.StoreRef, filepath.FromSlash(rel))
		}
		d.Revisions = append(d.Revisions, dto)
	}
	if flags.agent {
		meta := agentMeta(flags)
		meta.Recovery = draftCleanupRecovery
		meta.Page = &out.AgentPage{Returned: len(d.Revisions), HasMore: page.HasMore, NextCursor: page.NextCursor}
		return out.WriteAgentJSON(w, flags.agentAccount, meta, d)
	}
	return out.WriteJSON(w, d)
}
func writeDraftCleanupResult(writer io.Writer, flags *rootFlags, result app.DraftCleanupResult) error {
	w := &draftOutputWriter{writer: writer}
	var err error
	if flags.agent {
		meta := agentMeta(flags)
		meta.Recovery = draftCleanupRecovery
		err = out.WriteAgentJSON(w, flags.agentAccount, meta, result)
	} else {
		err = out.WriteJSON(w, result)
	}
	if w.failure != nil {
		err = w.failure
	}
	if err != nil {
		selection := store.DraftCleanupSelection{DraftID: result.DraftID, RevisionID: result.RevisionID, Hash: result.Hash}
		return classifyDraftCleanupError(store.DraftCleanupFailure(store.DraftCleanupOutcomeUncertain, selection, err), &result)
	}
	return nil
}
func classifyDraftCleanupError(err error, result *app.DraftCleanupResult) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) {
		return existing
	}
	var action *draftCleanupActionError
	if errors.As(err, &action) {
		result = &action.Result
	}
	var failure *store.DraftCleanupError
	code := store.DraftCleanupStoreError
	if errors.As(err, &failure) {
		code = failure.Code
	}
	exit := 1
	message := "Local cleanup failed; retained catalogue evidence does not prove a filesystem effect."
	switch code {
	case store.DraftCleanupInvalidArguments, store.DraftCleanupInvalidCursor, store.DraftCleanupReadOnly:
		exit = 2
	case store.DraftCleanupNotFound:
		exit = 3
	case store.DraftCleanupStoreError, store.DraftCleanupSnapshotError:
		exit = 4
	}
	if code == store.DraftCleanupOutcomeUncertain {
		message = "Local cleanup outcome is unconfirmed; do not repeat automatically."
	}
	if code == store.DraftCleanupReadOnly {
		message = "Read-only policy rejects draft snapshot cleanup."
	}
	publicCode := string(code)
	if lock.IsLocked(err) && failure == nil {
		publicCode = "store_locked"
		exit = 4
	}
	var correlation *out.AgentDraftCleanupError
	if result != nil {
		correlation = &out.AgentDraftCleanupError{DraftID: result.DraftID, RevisionID: result.RevisionID, Hash: result.Hash, Effect: string(result.Effect), Outcome: string(result.Outcome), DirectorySync: string(result.DirectorySync), RemovedBytes: result.RemovedBytes}
	} else if failure != nil && failure.DraftID != "" {
		correlation = &out.AgentDraftCleanupError{DraftID: failure.DraftID, RevisionID: failure.RevisionID, Hash: failure.Hash, Effect: string(app.DraftCleanupNotRemoved), Outcome: string(app.DraftCleanupFailed), DirectorySync: string(app.DraftCleanupSyncNotAttempted), RemovedBytes: new(int64)}
	}
	return &out.AgentError{Code: publicCode, Message: message, Recovery: draftCleanupRecovery, ExitCode: exit, Cause: err, Cleanup: correlation}
}

// Used only for transport errors with typed filesystem knowledge.
type draftCleanupActionError struct {
	Result  app.DraftCleanupResult
	Failure *store.DraftCleanupError
}

func (e *draftCleanupActionError) Error() string { return fmt.Sprint(e.Failure) }
func (e *draftCleanupActionError) Unwrap() error { return e.Failure }
