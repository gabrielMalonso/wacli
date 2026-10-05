package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type DraftCleanupCode string

const (
	DraftCleanupInvalidArguments DraftCleanupCode = "invalid_arguments"
	DraftCleanupInvalidCursor    DraftCleanupCode = "invalid_cursor"
	DraftCleanupNotFound         DraftCleanupCode = "not_found"
	DraftCleanupStoreError       DraftCleanupCode = "store_error"
	DraftCleanupConflict         DraftCleanupCode = "cleanup_conflict"
	DraftCleanupProtected        DraftCleanupCode = "cleanup_protected"
	DraftCleanupCommitUncertain  DraftCleanupCode = "cleanup_commit_uncertain"
	DraftCleanupReadOnly         DraftCleanupCode = "read_only"
	DraftCleanupSnapshotChanged  DraftCleanupCode = "snapshot_changed"
	DraftCleanupSnapshotError    DraftCleanupCode = "snapshot_unavailable"
	DraftCleanupCanceled         DraftCleanupCode = "cleanup_canceled"
	DraftCleanupOutcomeUncertain DraftCleanupCode = "cleanup_outcome_uncertain"
)

// DraftCleanupError concerns local retention only, never an outbound attempt.
type DraftCleanupError struct {
	Code       DraftCleanupCode `json:"code"`
	DraftID    string           `json:"draft_id,omitempty"`
	RevisionID string           `json:"revision_id,omitempty"`
	Hash       string           `json:"hash,omitempty"`
	Cause      error            `json:"-"`
}

func (e *DraftCleanupError) Error() string { return "local draft cleanup: " + string(e.Code) }
func (e *DraftCleanupError) Unwrap() error { return e.Cause }

func DraftCleanupFailure(code DraftCleanupCode, selection DraftCleanupSelection, cause error) *DraftCleanupError {
	e := &DraftCleanupError{Code: code, Cause: cause}
	if ValidateDraftID(selection.DraftID) == nil {
		e.DraftID = selection.DraftID
	}
	if ValidateDraftID(selection.RevisionID) == nil {
		e.RevisionID = selection.RevisionID
	}
	if validDraftCleanupHash(selection.Hash) {
		e.Hash = selection.Hash
	}
	return e
}

type DraftCleanupSelection struct {
	DraftID        string `json:"draft_id"`
	RevisionID     string `json:"revision_id"`
	ExpectedHeadID string `json:"expected_head_revision_id"`
	Hash           string `json:"hash"`
}

func (s DraftCleanupSelection) Validate() error {
	if ValidateDraftID(s.DraftID) != nil || ValidateDraftID(s.RevisionID) != nil || ValidateDraftID(s.ExpectedHeadID) != nil || !validDraftCleanupHash(s.Hash) {
		return DraftCleanupFailure(DraftCleanupInvalidArguments, s, nil)
	}
	return nil
}

func validDraftCleanupHash(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

type DraftCleanupEligibility string

const (
	DraftCleanupEligible          DraftCleanupEligibility = "eligible"
	DraftCleanupActive            DraftCleanupEligibility = "active"
	DraftCleanupNonDocument       DraftCleanupEligibility = "non_document"
	DraftCleanupOutboundProtected DraftCleanupEligibility = "outbound_protected"
)

func draftCleanupEligibility(entry DraftEntry, referenced bool) DraftCleanupEligibility {
	if entry.Record.State != "discarded" {
		return DraftCleanupActive
	}
	if entry.Revision.Payload().Data().Kind != DraftDocumentKind {
		return DraftCleanupNonDocument
	}
	if referenced {
		return DraftCleanupOutboundProtected
	}
	return DraftCleanupEligible
}

// Validate the retained head too: an incompatible draft record is never a
// cleanup candidate, including when the caller selects an older revision.
func readDraftCleanupHead(ctx context.Context, reader draftReader, id string) (DraftEntry, error) {
	entry, err := readDraft(ctx, reader, id, "")
	if err != nil {
		var failure *DraftError
		if errors.As(err, &failure) && failure.Code == "not_found" {
			var exists bool
			if queryErr := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM drafts WHERE id=?)`, id).Scan(&exists); queryErr != nil {
				return DraftEntry{}, queryErr
			}
			if exists {
				return DraftEntry{}, DraftCleanupFailure(DraftCleanupStoreError, DraftCleanupSelection{DraftID: id}, err)
			}
		}
		return DraftEntry{}, err
	}
	r := entry.Record
	validState := r.State == "active" && r.DiscardedAt == nil || r.State == "discarded" && r.DiscardedAt != nil
	if r.ID != id || !validState || r.CreatedAt.UnixNano() <= 0 || r.UpdatedAt.Before(r.CreatedAt) || r.DiscardedAt != nil && (r.DiscardedAt.Before(r.CreatedAt) || r.DiscardedAt.After(r.UpdatedAt)) {
		return DraftEntry{}, DraftCleanupFailure(DraftCleanupStoreError, DraftCleanupSelection{DraftID: id}, nil)
	}
	return entry, nil
}

func readDraftCleanupSelection(ctx context.Context, reader draftReader, selection DraftCleanupSelection) (DraftEntry, error) {
	head, err := readDraftCleanupHead(ctx, reader, selection.DraftID)
	if err != nil {
		return DraftEntry{}, err
	}
	if head.Record.HeadRevisionID != selection.ExpectedHeadID {
		return DraftEntry{}, DraftCleanupFailure(DraftCleanupConflict, selection, nil)
	}
	entry := head
	if selection.RevisionID != head.Revision.ID() {
		entry, err = readDraft(ctx, reader, selection.DraftID, selection.RevisionID)
		if err != nil {
			return DraftEntry{}, err
		}
	}
	if entry.Revision.Payload().Hash() != selection.Hash {
		return DraftEntry{}, DraftCleanupFailure(DraftCleanupConflict, selection, nil)
	}
	var referenced bool
	if err = reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbound_operations WHERE revision_id=?)`, selection.RevisionID).Scan(&referenced); err != nil {
		return DraftEntry{}, err
	}
	if draftCleanupEligibility(entry, referenced) != DraftCleanupEligible {
		return DraftEntry{}, DraftCleanupFailure(DraftCleanupProtected, selection, nil)
	}
	return entry, nil
}

// ReadDraftCleanup validates catalogue eligibility, not snapshot availability.
// Callers must retain the store LOCK/owner slot across read, FULL guard and unlink.
func (d *DB) ReadDraftCleanup(ctx context.Context, selection DraftCleanupSelection) (DraftEntry, error) {
	if err := selection.Validate(); err != nil {
		return DraftEntry{}, err
	}
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DraftEntry{}, draftCleanupStoreFailure(err, selection)
	}
	defer tx.Rollback()
	entry, err := readDraftCleanupSelection(ctx, tx, selection)
	if err != nil {
		return DraftEntry{}, draftCleanupStoreFailure(err, selection)
	}
	return entry, nil
}

// Translate only at this boundary; outbound helper errors must never describe a
// send outcome for a local cleanup. Commit/restoration uncertainty takes priority.
func draftCleanupStoreFailure(err error, selection DraftCleanupSelection) *DraftCleanupError {
	var outbound *OutboundError
	if errors.As(err, &outbound) && outbound.Code == "write_uncertain" {
		return DraftCleanupFailure(DraftCleanupCommitUncertain, selection, err)
	}
	var cleanup *DraftCleanupError
	if errors.As(err, &cleanup) {
		return cleanup
	}
	var draft *DraftError
	if errors.As(err, &draft) && draft.Code == "not_found" {
		return DraftCleanupFailure(DraftCleanupNotFound, selection, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return DraftCleanupFailure(DraftCleanupCanceled, selection, err)
	}
	return DraftCleanupFailure(DraftCleanupStoreError, selection, err)
}

type DraftCleanupItem struct {
	RevisionID   string                  `json:"revision_id"`
	Hash         string                  `json:"hash"`
	Revision     int                     `json:"revision"`
	CreatedAt    time.Time               `json:"created_at"`
	Kind         DraftKind               `json:"kind"`
	Eligibility  DraftCleanupEligibility `json:"eligibility"`
	OutboundRefs int64                   `json:"outbound_references"`
	Document     *DraftCleanupDocument   `json:"document,omitempty"`
}

type DraftCleanupDocument struct {
	BytesAtCreate int64  `json:"bytes_at_create"`
	SHA256        string `json:"sha256"`
}

// Counts partition only the returned page; bytes are creation-time metadata,
// not a measurement of present files, allocated blocks or reclaimable disk.
type DraftCleanupCounts struct {
	Examined              int   `json:"examined"`
	Eligible              int   `json:"eligible"`
	Active                int   `json:"active"`
	NonDocument           int   `json:"non_document"`
	OutboundProtected     int   `json:"outbound_protected"`
	EligibleBytesAtCreate int64 `json:"eligible_bytes_at_create"`
}

type DraftCleanupPage struct {
	Record     DraftRecord
	Items      []DraftCleanupItem
	Counts     DraftCleanupCounts
	HasMore    bool
	NextCursor *string
}
