package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

type agentMediaRetryResult struct {
	agentMediaStatus
	Retry           app.MediaRetryObservation `json:"retry"`
	RecordedAt      *time.Time                `json:"recorded_at,omitempty"`
	FilePublication string                    `json:"file_publication"`
}

func runAgentMediaRetry(cmd *cobra.Command, flags *rootFlags, chat, id, output string, wait time.Duration) error {
	return runAgentMediaRetryWith(cmd, flags, chat, id, output, wait, os.Stdout,
		func(ctx context.Context, flags *rootFlags) (*app.App, *lock.Lock, error) {
			return newApp(ctx, flags, true, true)
		}, nil)
}

// Fixture seams cover App/WAFactory and byte transport without changing the
// production opener, protocol, standalone lock ownership or publication policy.
func runAgentMediaRetryWith(cmd *cobra.Command, flags *rootFlags, chat, id, output string, wait time.Duration, dst io.Writer,
	open func(context.Context, *rootFlags) (*app.App, *lock.Lock, error), download app.MediaRetryBytes,
) error {
	if flags.isReadOnly() {
		return classifyMediaRetryCommandError(&app.MediaRetryExactError{Code: "read_only"})
	}
	signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := withTimeout(signalCtx, flags)
	defer cancel()
	result := app.MediaRetryExactResult{ChatJID: chat, MsgID: id, Status: "unknown", CurrentAvailability: "unknown", FilePublication: "not_written", Observation: app.MediaRetryObservation{Phone: "not_requested", CDN: "not_requested"}}
	d := agentMediaRetryResult{agentMediaStatus: agentMediaStatus{ChatJID: chat, ID: id}, FilePublication: "not_written"}
	fail := func(err error) error { return mediaRetryAgentError(err, result) }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	// Compatible archive and exact selection are preflighted read-only before
	// LOCK acquisition, writable open, permission changes or session creation.
	read, lk, err := newReadApp(ctx, flags)
	if err != nil {
		return fail(err)
	}
	info, err := read.DB().GetMediaDownloadInfoContext(ctx, chat, id)
	storeDir := read.StoreDir()
	closeApp(read, lk)
	if err != nil {
		return fail(err)
	}
	d.agentMediaStatus = mediaStatusDTO(info, flags.detail)
	result.UnavailableAt = agentTime(info.MediaUnavailableAt)
	if info.Tombstone {
		return fail(&app.MediaRetryExactError{Code: "media_deleted"})
	}
	if info.InvalidFileLength {
		return fail(&app.MediaRetryExactError{Code: "media_metadata_incomplete"})
	}
	if _, err := wa.MediaTypeFromString(info.MediaType); err != nil {
		return fail(&app.MediaRetryExactError{Code: "no_media"})
	}
	roots, err := outboundMediaRoots()
	if err != nil {
		return fail(mediaAgentPathError())
	}
	path, err := read.ResolveMediaOutputPath(info, output)
	if err != nil {
		return fail(err)
	}
	target, err := openAgentMediaLocation(path, storeDir, roots, false, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if target != nil {
		defer target.close()
		result.Output, err = app.InspectMediaArtifact(ctx, target.MediaLocation, info.FileSHA256, info.FileLength, true)
		if err != nil {
			return fail(err)
		}
		if result.Output.State != "absent" && result.Output.State != "verified" {
			return fail(&app.MediaRetryExactError{Code: "output_conflict"})
		}
	}
	if len(info.FileSHA256) != sha256.Size {
		return fail(&app.MediaRetryExactError{Code: "media_binding_unverified"})
	}
	if info.FileLength > wa.MaxMediaDownloadSize {
		return fail(wa.ErrMediaTooLarge)
	}
	var cache *agentMediaLocation
	if info.LocalPath != "" && result.Output.State != "verified" {
		cache, err = openAgentMediaLocation(info.LocalPath, storeDir, roots, true, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if cache != nil {
			defer cache.close()
			d.Local, err = app.InspectMediaArtifact(ctx, cache.MediaLocation, info.FileSHA256, info.FileLength, true)
			if err != nil {
				return fail(err)
			}
		}
	}
	if result.Output.State != "verified" && d.Local.State != "verified" && len(info.MediaKey) != 32 {
		return fail(&app.MediaRetryExactError{Code: "media_metadata_incomplete"})
	}
	// No owner socket path is consulted. A follow owner keeps its LOCK; retry
	// reports store_locked without stop, delegation, fallback or another client.
	a, writerLock, err := open(ctx, flags)
	if err != nil {
		if lock.IsLocked(err) {
			return fail(&app.MediaRetryExactError{Code: "store_locked"})
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fail(err)
		}
		return fail(agentStoreError(err))
	}
	defer closeApp(a, writerLock)
	next, err := a.DB().GetMediaDownloadInfoContext(ctx, chat, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(&app.MediaRetryExactError{Code: "media_changed"})
	}
	if err != nil {
		return fail(err)
	}
	if mediaRetrySelectionChanged(info, next) {
		return fail(&app.MediaRetryExactError{Code: "media_changed"})
	}
	if target == nil {
		target, err = openAgentMediaLocation(path, storeDir, roots, false, true)
		if err != nil {
			return fail(err)
		}
		defer target.close()
	}
	opts := app.RetryMediaExactOptions{ChatJID: chat, MsgID: id, Wait: wait, Output: target.MediaLocation, DownloadBytes: download}
	if cache != nil {
		opts.Cache = &cache.MediaLocation
	}
	opts.Connect = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.OpenWA(); err != nil {
			return &app.MediaRetryExactError{Code: "session_unavailable"}
		}
		// EnsureAuthed also migrates every historical LID. Exact recovery must
		// not select, migrate or remove media for unrelated messages.
		if !a.WA().IsAuthed() {
			return &app.MediaRetryExactError{Code: "not_authenticated"}
		}
		if err := a.Connect(ctx, false, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &app.MediaRetryExactError{Code: "connect_failed"}
		}
		return nil
	}
	result, err = a.RetryMediaExact(ctx, opts)
	if err != nil {
		return fail(err)
	}
	d.Status, d.Recorded, d.RecordedAt = result.Status, result.Recorded, result.RecordedAt
	d.Output, d.FilePublication, d.Retry = &result.Output, result.FilePublication, result.Observation
	if result.Status == "unavailable" {
		d.Remote.Observation = &agentMediaObservation{"unavailable", result.UnavailableAt, "phone_and_cdn"}
	}
	return writeAgentMediaRetry(dst, flags, d, result)
}

func mediaRetrySelectionChanged(a, b store.MediaDownloadInfo) bool {
	return b.Tombstone || b.InvalidFileLength || a.ChatJID != b.ChatJID || a.MsgID != b.MsgID || a.MediaType != b.MediaType || a.FileLength != b.FileLength || a.DirectPath != b.DirectPath || !bytes.Equal(a.FileSHA256, b.FileSHA256) || !bytes.Equal(a.FileEncSHA256, b.FileEncSHA256) || !bytes.Equal(a.MediaKey, b.MediaKey)
}

func writeAgentMediaRetry(dst io.Writer, flags *rootFlags, d agentMediaRetryResult, result app.MediaRetryExactResult) error {
	meta := agentMeta(flags)
	meta.Recovery = "Observations describe this explicit attempt. Current remote availability remains unknown; cancellation or lost output never authorizes automatic replay."
	if err := out.WriteAgentActionJSON(dst, flags.agentAccount, meta, d); err != nil {
		return mediaRetryAgentError(&app.MediaRetryExactError{Code: "output_failed"}, result)
	}
	return nil
}

func classifyMediaRetryCommandError(err error) *out.AgentError {
	var exact *app.MediaRetryExactError
	var existing *out.AgentError
	if errors.As(err, &existing) || !errors.As(err, &exact) {
		mapped := classifyMediaCommandError(err)
		if mapped.Code == "invalid_arguments" {
			copy := *mapped
			copy.Message = "Exact media retry requires --agent, --chat JID, --id, --output PATH, --wait 1s..120s and --timeout 1s..5m; explicit bulk flags are rejected."
			return &copy
		}
		return mapped
	}
	code, message, exit := exact.Code, "Explicit media recovery failed; current availability is unknown. Do not automatically repeat an uncertain operation.", 1
	switch code {
	case "read_only":
		message, exit = "Read-only policy rejects explicit media recovery.", 2
	case "invalid_arguments":
		message, exit = "Exact media retry requires --agent, --chat JID, --id, --output PATH, --wait 1s..120s and --timeout 1s..5m; explicit bulk flags are rejected.", 2
	case "not_found", "media_deleted", "no_media", "media_binding_unverified", "media_metadata_incomplete":
		exit = 3
	case "store_failed", "session_unavailable", "not_authenticated":
		exit = 4
	case "store_locked":
		message, exit = "Selected archive has an active writer; exact media recovery cannot run alongside it.", 4
	}
	return &out.AgentError{Code: code, Message: message, ExitCode: exit, Cause: err}
}

func mediaRetryAgentError(err error, result app.MediaRetryExactResult) *out.AgentError {
	// Reuse PR17's typed HTTP/integrity/path mapping, then preserve recovery's
	// independent file and DB knowledge instead of resetting it to no effects.
	mapped := mediaAgentError(err, agentMediaStatus{ChatJID: result.ChatJID, ID: result.MsgID}, result.FilePublication)
	var exact *app.MediaRetryExactError
	if errors.As(err, &exact) {
		mapped = classifyMediaRetryCommandError(err)
	}
	mapped.Media = &out.AgentMediaError{
		ChatJID: result.ChatJID, ID: result.MsgID, Status: result.Status,
		FilePublication: result.FilePublication, Recorded: result.Recorded, RecordedAt: result.RecordedAt,
		CurrentAvailability: "unknown", UnavailableAt: result.UnavailableAt,
		Retry: &out.AgentMediaRetryObservation{Phone: result.Observation.Phone, CDN: result.Observation.CDN, ObservedAt: result.Observation.ObservedAt},
	}
	if result.Output.Path != nil && (result.FilePublication != "not_written" || result.Status == "existing") {
		mapped.Media.Path, mapped.Media.Bytes, mapped.Media.SHA256 = result.Output.Path, result.Output.Bytes, result.Output.SHA256
	}
	return mapped
}
