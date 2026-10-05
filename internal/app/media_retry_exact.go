package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// MediaRetryBytes separates authenticated download from file publication. The
// reuploaded flag is true only after a successfully decrypted phone response.
type MediaRetryBytes func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error)

type RetryMediaExactOptions struct {
	ChatJID, MsgID string
	Wait           time.Duration
	Output         MediaLocation
	Cache          *MediaLocation
	// Connect lazily opens the existing standalone client only when local bytes
	// cannot satisfy the request. The selected row is rechecked afterward.
	Connect func(context.Context) error
	// DownloadBytes is an optional fixture seam; nil uses shared WA decryption.
	DownloadBytes MediaRetryBytes
}

// MediaRetryObservation describes this operation, not current availability.
type MediaRetryObservation struct {
	Phone      string     `json:"phone"`
	CDN        string     `json:"cdn"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

type MediaRetryExactResult struct {
	ChatJID             string                `json:"chat_jid"`
	MsgID               string                `json:"msg_id"`
	Status              string                `json:"status"` // downloaded | cached | existing | no_response | unavailable | unknown
	CurrentAvailability string                `json:"current_availability"`
	Observation         MediaRetryObservation `json:"observation"`
	UnavailableAt       *time.Time            `json:"unavailable_at,omitempty"`
	Recorded            bool                  `json:"recorded"`
	RecordedAt          *time.Time            `json:"recorded_at,omitempty"`
	FilePublication     string                `json:"file_publication"`
	Output              MediaArtifact         `json:"output"`
}

// MediaRetryExactError exposes only a stable code, never protocol/store causes.
// The accompanying result retains any known publication and persistence effects.
type MediaRetryExactError struct{ Code string }

func (e *MediaRetryExactError) Error() string { return e.Code }

// RetryMediaExact selects one composite archive key, sharing RetryMedia's
// receipt engine. The caller must hold the standalone writer lock, supply a
// positive deadline of at most five minutes, and confine Output/Cache using the
// agent media roots helper. This operation never delegates to an owner or
// replays an uncertain operation. Verified local bytes can avoid the protocol.
func (a *App) RetryMediaExact(ctx context.Context, opts RetryMediaExactOptions) (MediaRetryExactResult, error) {
	result := MediaRetryExactResult{
		ChatJID: opts.ChatJID, MsgID: opts.MsgID, Status: "unknown",
		CurrentAvailability: "unknown", FilePublication: "not_written",
		Observation: MediaRetryObservation{Phone: "not_requested", CDN: "not_requested"},
		Output:      emptyMediaArtifact("absent"),
	}
	fail := func(code string) (MediaRetryExactResult, error) {
		return result, &MediaRetryExactError{Code: code}
	}
	readOnly := a.opts.ReadOnly
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WACLI_READONLY"))) {
	case "1", "true", "yes", "on":
		readOnly = true
	}
	if readOnly {
		return fail("read_only")
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled")
	}
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
		return fail("invalid_arguments")
	}
	chat, err := types.ParseJID(opts.ChatJID)
	if err != nil || chat.IsEmpty() || chat.String() != opts.ChatJID || strings.TrimSpace(opts.MsgID) == "" || opts.Output.Root == nil || opts.Output.Name == "" || opts.Output.Path == "" || opts.Output.Check == nil {
		return fail("invalid_arguments")
	}
	if opts.Wait == 0 {
		opts.Wait = 30 * time.Second
	}
	if opts.Wait < time.Second || opts.Wait > 120*time.Second {
		return fail("invalid_arguments")
	}
	info, err := a.db.GetMediaDownloadInfoContext(ctx, opts.ChatJID, opts.MsgID)
	if err != nil {
		return fail(exactMediaErrorCode(err, "store_failed"))
	}
	if !info.MediaUnavailableAt.IsZero() {
		result.UnavailableAt = &info.MediaUnavailableAt
	}
	if info.Tombstone {
		return fail("media_deleted")
	}
	if info.InvalidFileLength {
		return fail("media_metadata_incomplete")
	}
	if _, err := wa.MediaTypeFromString(info.MediaType); err != nil {
		return fail("no_media")
	}
	// Recheck the selected row before publication and before recording effects.
	// No atomic DB/filesystem snapshot is promised; a changed row never licenses
	// removal of an artifact that this attempt has already published.
	unchanged := func() error {
		next, err := a.db.GetMediaDownloadInfoContext(ctx, opts.ChatJID, opts.MsgID)
		if errors.Is(err, sql.ErrNoRows) {
			return &MediaRetryExactError{Code: "media_changed"}
		}
		if err != nil {
			return err
		}
		if !sameExactMedia(info, next) {
			return &MediaRetryExactError{Code: "media_changed"}
		}
		return nil
	}
	result.Output, err = InspectMediaArtifact(ctx, opts.Output, info.FileSHA256, info.FileLength, true)
	if err != nil {
		return fail(exactMediaErrorCode(err, "output_failed"))
	}
	if result.Output.State != "absent" {
		if result.Output.State != "verified" {
			return fail("output_conflict")
		}
		if err := unchanged(); err != nil {
			return fail(exactMediaErrorCode(err, "store_failed"))
		}
		result.Status = "existing"
		return result, nil
	}
	if len(info.FileSHA256) != sha256.Size {
		return fail("media_binding_unverified")
	}
	if info.FileLength > wa.MaxMediaDownloadSize {
		return fail("media_too_large")
	}
	finishPublication := func(artifact MediaArtifact, publishErr error, status string) error {
		result.Output = artifact
		if publishErr != nil {
			var publication *MediaArtifactError
			if errors.As(publishErr, &publication) {
				result.FilePublication = publication.Publication
			}
			return &MediaRetryExactError{Code: exactMediaErrorCode(publishErr, "output_failed")}
		}
		result.FilePublication = "written"
		if err := unchanged(); err != nil {
			return &MediaRetryExactError{Code: exactMediaErrorCode(err, "store_failed")}
		}
		at := nowUTC()
		recorded, err := a.db.MarkMediaDownloadedContext(ctx, info.ChatJID, info.MsgID, opts.Output.Path, at)
		if recorded {
			result.Recorded, result.RecordedAt = true, &at
			result.Status = status
		}
		if err != nil {
			return &MediaRetryExactError{Code: exactMediaErrorCode(err, "store_failed")}
		}
		if err := ctx.Err(); err != nil {
			return &MediaRetryExactError{Code: "cancelled"}
		}
		return nil
	}
	if opts.Cache != nil {
		cache, err := InspectMediaArtifact(ctx, *opts.Cache, info.FileSHA256, info.FileLength, true)
		if err != nil {
			return fail(exactMediaErrorCode(err, "media_changed"))
		}
		if cache.State == "verified" {
			artifact, err := CopyMediaArtifact(ctx, *opts.Cache, opts.Output, info.FileSHA256, info.FileLength, unchanged)
			err = finishPublication(artifact, err, "cached")
			return result, err
		}
	}
	if len(info.MediaKey) != 32 {
		return fail("media_metadata_incomplete")
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled")
	}
	if opts.Connect != nil {
		if err := opts.Connect(ctx); err != nil {
			return fail(exactMediaErrorCode(err, "connect_failed"))
		}
		if err := unchanged(); err != nil {
			return fail(exactMediaErrorCode(err, "store_failed"))
		}
	}
	if a.wa == nil {
		return fail("not_connected")
	}
	download := opts.DownloadBytes
	if download == nil {
		download = downloadExactMediaBytes
	}
	var classifyErr error
	classify := func(ctx context.Context, selected store.MediaDownloadInfo, id string, n retryNotif, got bool, _ *MediaRetryResult) MediaRetryOutcome {
		// The shared engine reloads the row; do not send its changed identity to
		// a downloader or publisher if it differs from our explicit selection.
		if !sameExactMedia(info, selected) {
			classifyErr = &MediaRetryExactError{Code: "media_changed"}
			return MediaRetryOutcome{}
		}
		at := nowUTC()
		result.Observation.ObservedAt = &at
		switch {
		case !got:
			result.Status, result.Observation.Phone = "no_response", "no_response"
			return MediaRetryOutcome{}
		case n.err != nil:
			result.Observation.Phone = "unknown"
			classifyErr = &MediaRetryExactError{Code: exactMediaErrorCode(n.err, "retry_failed")}
			return MediaRetryOutcome{}
		case n.code == wa.MediaRetryNotFound:
			result.Observation.Phone = "not_found"
		case n.code == wa.MediaRetrySuccess && strings.TrimSpace(n.directPath) != "":
			result.Observation.Phone = "reuploaded"
		default:
			result.Observation.Phone = "unknown"
			classifyErr = &MediaRetryExactError{Code: "retry_failed"}
			return MediaRetryOutcome{}
		}
		reuploaded := n.code == wa.MediaRetrySuccess
		if !reuploaded && len(info.FileEncSHA256) != sha256.Size {
			// Only an authenticated reupload can omit the original ciphertext
			// binding. Incomplete fallback metadata proves nothing about CDN.
			classifyErr = &MediaRetryExactError{Code: "media_metadata_incomplete"}
			return MediaRetryOutcome{}
		}
		path := info.DirectPath
		if reuploaded {
			path = n.directPath
		}
		plaintext, err := download(ctx, info, path, reuploaded)
		observed := nowUTC()
		result.Observation.ObservedAt = &observed
		if err != nil {
			result.Observation.CDN = "unknown"
			if isExpiredMediaDownload(err) {
				result.Observation.CDN = "expired"
			}
			if !reuploaded && isExpiredMediaDownload(err) {
				if err := unchanged(); err != nil {
					classifyErr = &MediaRetryExactError{Code: exactMediaErrorCode(err, "store_failed")}
				} else if err := a.db.MarkMediaUnavailable(ctx, info.ChatJID, info.MsgID, observed); err != nil {
					classifyErr = &MediaRetryExactError{Code: exactMediaErrorCode(err, "store_failed")}
				} else {
					result.Status, result.Recorded, result.RecordedAt = "unavailable", true, &observed
					result.UnavailableAt = &observed
				}
			} else {
				classifyErr = &MediaRetryExactError{Code: exactMediaErrorCode(err, "download_failed")}
			}
			return MediaRetryOutcome{}
		}
		result.Observation.CDN = "downloaded"
		artifact, err := PublishMediaArtifact(ctx, opts.Output, bytes.NewReader(plaintext), info.FileSHA256, info.FileLength, unchanged)
		if err == nil {
			artifact.Checks = append(artifact.Checks, "hmac")
			if !reuploaded && len(info.FileEncSHA256) == sha256.Size {
				artifact.Checks = append(artifact.Checks, "ciphertext_sha256")
			}
		}
		classifyErr = finishPublication(artifact, err, "downloaded")
		return MediaRetryOutcome{}
	}
	// From this point a receipt may have been dispatched even if cancellation
	// prevents classification; do not claim that no phone request occurred.
	result.Observation.Phone = "unknown"
	// Exact lookup deliberately ignores local_path and retained unavailable
	// markers. It never calls a pending/bulk query, even with limit one.
	run, err := a.retrySelectedMedia(ctx, []store.PendingMediaDownload{{ChatJID: info.ChatJID, MsgID: info.MsgID}}, RetryMediaOptions{BatchSize: 1, Wait: opts.Wait}, classify, func(selected store.MediaDownloadInfo) error {
		if !sameExactMedia(info, selected) {
			return &MediaRetryExactError{Code: "media_changed"}
		}
		return unchanged()
	})
	if err != nil {
		return fail(exactMediaErrorCode(err, "retry_failed"))
	}
	if run.Failed != 0 {
		return fail("retry_failed")
	}
	return result, classifyErr
}

func sameExactMedia(a, b store.MediaDownloadInfo) bool {
	return !b.Tombstone && !b.InvalidFileLength && a.ChatJID == b.ChatJID && a.MsgID == b.MsgID && a.MediaType == b.MediaType && a.FileLength == b.FileLength && a.DirectPath == b.DirectPath && bytes.Equal(a.FileSHA256, b.FileSHA256) && bytes.Equal(a.FileEncSHA256, b.FileEncSHA256) && bytes.Equal(a.MediaKey, b.MediaKey)
}

func downloadExactMediaBytes(ctx context.Context, info store.MediaDownloadInfo, path string, reuploaded bool) ([]byte, error) {
	if reuploaded {
		return wa.DownloadRetriedMediaBytes(ctx, path, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType)
	}
	return wa.DownloadMediaDirectBytes(ctx, path, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType)
}

func exactMediaErrorCode(err error, fallback string) string {
	var exact *MediaRetryExactError
	var artifact *MediaArtifactError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.As(err, &exact):
		return exact.Code
	case errors.As(err, &artifact):
		return artifact.Code
	case errors.Is(err, sql.ErrNoRows):
		return "not_found"
	case errors.Is(err, wa.ErrMediaMetadataInvalid):
		return "media_metadata_incomplete"
	case errors.Is(err, wa.ErrMediaTooLarge):
		return "media_too_large"
	case errors.Is(err, wa.ErrMediaDecryptFailed), errors.Is(err, whatsmeow.ErrInvalidMediaHMAC), errors.Is(err, whatsmeow.ErrInvalidMediaSHA256), errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256), errors.Is(err, whatsmeow.ErrFileLengthMismatch), errors.Is(err, whatsmeow.ErrTooShortFile):
		return "integrity_failed"
	case isExpiredMediaDownload(err):
		return "media_expired"
	default:
		return fallback
	}
}
