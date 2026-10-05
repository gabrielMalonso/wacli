package store

import (
	"encoding/json"
	"time"
)

const DraftMediaDirectory = "draft-media"

// DraftReviewSnapshot is immutable observation metadata, outside the effective
// payload hash. File verification describes creation only, not current bytes.
type DraftReviewSnapshot struct {
	RequestedRaw     string    `json:"requested_raw"`
	RequestedJID     string    `json:"requested_jid"`
	RecipientName    string    `json:"recipient_name"`
	AccountName      string    `json:"account_name"`
	SnapshotPath     string    `json:"snapshot_path,omitempty"`
	VerifiedAtCreate time.Time `json:"verified_at_create,omitzero"`
}

type DraftRevision struct {
	draftID, revisionID string
	createdAt           time.Time
	payload             DraftPayload
	review              DraftReviewSnapshot
}

func NewDraftRevision(draftID, revisionID string, createdAt time.Time, payload DraftPayload, review DraftReviewSnapshot) (DraftRevision, error) {
	if err := ValidateDraftID(draftID); err != nil {
		return DraftRevision{}, err
	}
	if err := ValidateDraftID(revisionID); err != nil {
		return DraftRevision{}, err
	}
	if createdAt.IsZero() || payload.hash == "" {
		return DraftRevision{}, invalidDraft("revision", "timestamp and validated payload are required")
	}
	for _, field := range []struct{ name, value string }{
		{"requested_raw", review.RequestedRaw}, {"recipient_name", review.RecipientName},
		{"account_name", review.AccountName},
	} {
		if err := checkDraftString(field.name, field.value, false); err != nil {
			return DraftRevision{}, err
		}
	}
	requested, err := NormalizeDraftTarget(review.RequestedRaw)
	if err != nil {
		return DraftRevision{}, err
	}
	data := payload.Data()
	target := data.Recipient
	if requested != target.JID && requested != target.PN && requested != target.LID {
		return DraftRevision{}, invalidDraft("requested_raw", "does not match frozen recipient or observed alias")
	}
	if review.RequestedJID != "" && review.RequestedJID != requested {
		return DraftRevision{}, invalidDraft("requested_jid", "does not match requested input")
	}
	review.RequestedJID = requested
	if data.Kind.HasUpload() {
		path, err := DraftSnapshotRelativePath(revisionID)
		if err != nil || review.SnapshotPath != path || review.VerifiedAtCreate.IsZero() {
			return DraftRevision{}, invalidDraft("snapshot", "matching revision path and creation verification are required")
		}
		review.VerifiedAtCreate = review.VerifiedAtCreate.UTC()
	} else if review.SnapshotPath != "" || !review.VerifiedAtCreate.IsZero() {
		return DraftRevision{}, invalidDraft("snapshot", "only document/image revisions reference a snapshot")
	}
	encoded, err := json.Marshal(review)
	if err != nil || len(encoded) > MaxDraftPayloadBytes {
		return DraftRevision{}, invalidDraft("review", "encoded observation metadata exceeds 256 KiB")
	}
	return DraftRevision{draftID, revisionID, createdAt.UTC(), payload, review}, nil
}

func (r DraftRevision) DraftID() string             { return r.draftID }
func (r DraftRevision) ID() string                  { return r.revisionID }
func (r DraftRevision) CreatedAt() time.Time        { return r.createdAt }
func (r DraftRevision) Payload() DraftPayload       { return r.payload }
func (r DraftRevision) Review() DraftReviewSnapshot { return r.review }

// DraftSnapshotRelativePath cannot construct traversal paths from external IDs.
func DraftSnapshotRelativePath(revisionID string) (string, error) {
	if err := ValidateDraftID(revisionID); err != nil {
		return "", err
	}
	return DraftMediaDirectory + "/" + revisionID + ".blob", nil
}
