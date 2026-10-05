package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow"
)

type agentMediaObservation struct {
	State      string     `json:"state"`
	ObservedAt *time.Time `json:"observed_at"`
	Source     string     `json:"source"`
}
type agentMediaRemote struct {
	Current     string                 `json:"current"`
	Observation *agentMediaObservation `json:"observation"`
}
type agentMediaStatus struct {
	ChatJID           string             `json:"chat_jid"`
	ID                string             `json:"id"`
	Type              string             `json:"type"`
	MIMEType          string             `json:"mime_type"`
	Filename          string             `json:"filename"`
	FilenameTruncated bool               `json:"filename_truncated"`
	DeclaredBytes     *uint64            `json:"declared_bytes"`
	Local             app.MediaArtifact  `json:"local"`
	Output            *app.MediaArtifact `json:"output,omitempty"`
	Remote            agentMediaRemote   `json:"remote"`
	Binding           string             `json:"binding"`
	DownloadMetadata  string             `json:"download_metadata"`
	Status            string             `json:"status"`
	Recorded          bool               `json:"recorded"`
}

func newMediaStatusCmd(flags *rootFlags) *cobra.Command {
	var chat, id, output string
	var verify bool
	cmd := &cobra.Command{Use: "status", Short: "Observe one stored media reference and its local file", RunE: func(cmd *cobra.Command, _ []string) error {
		if !flags.agent {
			return fmt.Errorf("media status requires --agent")
		}
		return runAgentMedia(cmd, flags, chat, id, output, verify, false)
	}}
	cmd.Flags().StringVar(&chat, "chat", "", "exact stored chat JID")
	cmd.Flags().StringVar(&id, "id", "", "exact message ID")
	cmd.Flags().StringVar(&output, "output", "", "also observe an output file or directory without creating it")
	cmd.Flags().BoolVar(&verify, "verify", false, "hash local bytes (at most 100 MiB), without network access")
	return cmd
}

func validateAgentMediaSelection(chat, id string) error {
	jid, err := wa.ParseUserOrJID(chat)
	if err != nil || len(chat) > 256 || jid.String() != chat || strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id || len(id) > 512 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return fmt.Errorf("--chat must be an exact JID and --id a nonempty message ID of at most 512 bytes without controls")
	}
	return nil
}

func mediaStatusDTO(info store.MediaDownloadInfo, detail string) agentMediaStatus {
	name, cut := agentText(info.Filename, detail)
	d := agentMediaStatus{ChatJID: info.ChatJID, ID: info.MsgID, Type: info.MediaType, MIMEType: info.MimeType, Filename: name, FilenameTruncated: cut, Remote: agentMediaRemote{Current: "unknown"}, Binding: "unknown", DownloadMetadata: "incomplete", Status: "unknown", Local: app.MediaArtifact{State: "absent", Verification: "not_checked", Checks: []string{}}}
	if info.FileLength > 0 {
		n := info.FileLength
		d.DeclaredBytes = &n
	}
	if len(info.FileSHA256) == sha256.Size {
		d.Binding = "sha256_present"
	}
	if mediaNetworkMetadataValid(info) {
		d.DownloadMetadata = "present"
	}
	if !info.MediaUnavailableAt.IsZero() {
		d.Remote.Observation = &agentMediaObservation{"unavailable", agentTime(info.MediaUnavailableAt), "retained_phone_and_cdn"}
	}
	return d
}

func mediaNetworkMetadataValid(info store.MediaDownloadInfo) bool {
	_, err := wa.MediaTypeFromString(info.MediaType)
	p := strings.TrimSpace(info.DirectPath)
	return err == nil && len(info.MediaKey) == 32 && len(info.FileSHA256) == sha256.Size && (len(info.FileEncSHA256) == 0 || len(info.FileEncSHA256) == sha256.Size) && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "://")
}

func runAgentMedia(cmd *cobra.Command, flags *rootFlags, chat, id, output string, verify, download bool) error {
	signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := withTimeout(signalCtx, flags)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return mediaAgentError(err, agentMediaStatus{ChatJID: chat, ID: id}, "not_written")
	}
	a, lk, err := newReadApp(ctx, flags)
	if err != nil {
		return err
	}
	defer closeApp(a, lk)
	info, err := a.DB().GetMediaDownloadInfoContext(ctx, chat, id)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return mediaAgentError(err, agentMediaStatus{ChatJID: chat, ID: id}, "not_written")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return classifyAgentError(err)
		}
		return agentStoreError(err)
	}
	d := mediaStatusDTO(info, flags.detail)
	fail := func(err error) error { return mediaAgentError(err, d, "not_written") }
	if info.Tombstone {
		return fail(&out.AgentError{Code: "media_deleted", Message: "Stored message is a tombstone; media access is refused.", ExitCode: 3})
	}
	if info.InvalidFileLength {
		return fail(&out.AgentError{Code: "media_metadata_incomplete", Message: "Stored declared media size is invalid.", ExitCode: 3})
	}
	if _, err := wa.MediaTypeFromString(info.MediaType); err != nil {
		return fail(&out.AgentError{Code: "no_media", Message: "Requested stored message has no supported downloadable media type.", ExitCode: 3})
	}
	roots, err := outboundMediaRoots()
	if err != nil {
		return fail(mediaAgentPathError())
	}
	var cache *agentMediaLocation
	if info.LocalPath != "" {
		cache, err = openAgentMediaLocation(info.LocalPath, a.StoreDir(), roots, true, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if cache != nil {
			defer cache.close()
			d.Local, err = app.InspectMediaArtifact(ctx, cache.MediaLocation, info.FileSHA256, info.FileLength, verify)
			if err != nil {
				return fail(err)
			}
		}
	}
	var target *agentMediaLocation
	if output != "" {
		path, err := a.ResolveMediaOutputPath(info, output)
		if err != nil {
			return fail(err)
		}
		target, err = openAgentMediaLocation(path, a.StoreDir(), roots, false, false)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		artifact := app.MediaArtifact{State: "absent", Verification: "not_checked", Checks: []string{}}
		if target != nil {
			defer target.close()
			artifact, err = app.InspectMediaArtifact(ctx, target.MediaLocation, info.FileSHA256, info.FileLength, verify)
			if err != nil {
				return fail(err)
			}
		}
		d.Output = &artifact
	}
	if !download {
		if d.Output != nil && (d.Output.State == "existing" || d.Output.State == "verified") {
			d.Status = "existing"
		} else if d.Local.State == "verified" {
			d.Status = "cached"
		} else if d.Local.State == "existing" {
			d.Status = "existing"
		}
		meta := agentMeta(flags)
		meta.Recovery = "Stat is not verification; use --verify for local SHA-256 checks. Remote availability remains unknown."
		return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, d)
	}
	if d.Output != nil && d.Output.State != "absent" {
		if d.Output.State != "verified" {
			return fail(&app.MediaArtifactError{Code: "output_conflict", Publication: "not_written"})
		}
		d.Status = "existing"
		return writeAgentMediaDownload(os.Stdout, flags, d)
	}
	if len(info.FileSHA256) != sha256.Size {
		return fail(&out.AgentError{Code: "media_binding_unverified", Message: "Stored plaintext SHA-256 is missing or malformed; message-to-file binding cannot be verified.", ExitCode: 3})
	}
	if info.FileLength > wa.MaxMediaDownloadSize {
		return fail(wa.ErrMediaTooLarge)
	}
	// Validate network metadata before creating directories. A verified cache
	// needs neither a media key nor a CDN path.
	if d.Local.State != "verified" && !mediaNetworkMetadataValid(info) {
		return fail(&out.AgentError{Code: "media_metadata_incomplete", Message: "Stored network download metadata is missing or malformed.", ExitCode: 3})
	}
	path, err := a.ResolveMediaOutputPath(info, output)
	if err != nil {
		return fail(err)
	}
	if target == nil {
		target, err = openAgentMediaLocation(path, a.StoreDir(), roots, false, true)
		if err != nil {
			return fail(err)
		}
		defer target.close()
	}
	beforePublish := func() error {
		next, err := a.DB().GetMediaDownloadInfoContext(ctx, chat, id)
		if err != nil {
			return err
		}
		if next.Tombstone || next.InvalidFileLength || next.MediaType != info.MediaType || next.FileLength != info.FileLength || !bytes.Equal(next.FileSHA256, info.FileSHA256) {
			return &app.MediaArtifactError{Code: "media_changed", Publication: "not_written"}
		}
		return nil
	}
	var artifact app.MediaArtifact
	if d.Local.State == "verified" && cache != nil {
		d.Status = "cached"
		artifact, err = app.CopyMediaArtifact(ctx, cache.MediaLocation, target.MediaLocation, info.FileSHA256, info.FileLength, beforePublish)
	} else {
		var plaintext []byte
		plaintext, err = wa.DownloadMediaDirectBytes(ctx, info.DirectPath, info.FileEncSHA256, info.FileSHA256, info.MediaKey, info.FileLength, info.MediaType)
		if err == nil {
			artifact, err = app.PublishMediaArtifact(ctx, target.MediaLocation, bytes.NewReader(plaintext), info.FileSHA256, info.FileLength, beforePublish)
		}
		d.Status = "downloaded"
		if err == nil {
			artifact.Checks = append(artifact.Checks, "hmac")
			if len(info.FileEncSHA256) == sha256.Size {
				artifact.Checks = append(artifact.Checks, "ciphertext_sha256")
			}
		}
	}
	if err != nil {
		d.Status = "unknown"
		d.Output = &artifact
		return fail(err)
	}
	d.Output = &artifact
	return writeAgentMediaDownload(os.Stdout, flags, d)
}

func classifyMediaCommandError(err error) *out.AgentError {
	e := classifyAgentError(err)
	if e.Code == "invalid_arguments" {
		copy := *e
		copy.Message = "Invalid media arguments; use an exact --chat JID and --id, an explicit download --output, and a positive timeout of at most 5m."
		return &copy
	}
	return e
}

func writeAgentMediaDownload(dst io.Writer, flags *rootFlags, d agentMediaStatus) error {
	meta := agentMeta(flags)
	meta.Recovery = "Output describes observed bytes; the archive was not updated. Remote availability and future file stability remain unknown."
	if err := out.WriteAgentActionJSON(dst, flags.agentAccount, meta, d); err != nil {
		return mediaAgentError(&app.MediaArtifactError{Code: "output_failed", Publication: mediaDownloadPublication(d), Cause: err}, d, mediaDownloadPublication(d))
	}
	return nil
}
func mediaDownloadPublication(d agentMediaStatus) string {
	if d.Status == "downloaded" || d.Status == "cached" {
		return "written"
	}
	return "not_written"
}

func mediaAgentError(err error, d agentMediaStatus, publication string) *out.AgentError {
	code, message, exit := "download_failed", "Media operation failed; current remote availability is unknown.", 1
	var existing *out.AgentError
	var artifact *app.MediaArtifactError
	var httpErr whatsmeow.DownloadHTTPError
	switch {
	case errors.As(err, &existing):
		code, message, exit = existing.Code, existing.Message, existing.ExitCode
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code, message = "cancelled", "Media operation was cancelled or its deadline expired."
	case errors.Is(err, wa.ErrMediaTooLarge):
		code, message = "media_too_large", "Media exceeds the 100 MiB limit."
	case errors.As(err, &httpErr) && httpErr.Response != nil && (httpErr.Response.StatusCode == http.StatusForbidden || httpErr.Response.StatusCode == http.StatusNotFound || httpErr.Response.StatusCode == http.StatusGone):
		code, message = "media_expired", "Direct CDN request returned 403, 404 or 410; current recoverability remains unknown."
	case errors.Is(err, wa.ErrMediaDecryptFailed), errors.Is(err, whatsmeow.ErrInvalidMediaHMAC), errors.Is(err, whatsmeow.ErrInvalidMediaSHA256), errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256), errors.Is(err, whatsmeow.ErrFileLengthMismatch), errors.Is(err, whatsmeow.ErrTooShortFile):
		code, message = "integrity_failed", "Media integrity verification failed."
	case errors.As(err, &artifact):
		code = artifact.Code
		message = "Media file policy, integrity or publication check failed."
	case errors.Is(err, sql.ErrNoRows):
		code, message, exit = "not_found", "Requested item was not found in the selected local archive.", 3
	}
	if errors.As(err, &artifact) {
		publication = artifact.Publication
	}
	e := &out.AgentError{Code: code, Message: message, ExitCode: exit, Cause: err, Media: &out.AgentMediaError{ChatJID: d.ChatJID, ID: d.ID, Status: "unknown", FilePublication: publication, Recorded: false}}
	if publication != "not_written" && d.Output != nil {
		e.Media.Path = d.Output.Path
		e.Media.Bytes = d.Output.Bytes
		e.Media.SHA256 = d.Output.SHA256
	}
	if code == "media_expired" {
		e.Recovery = "Make an explicit recovery decision with media retry --agent --chat JID --id ID --output PATH. It requires a writable standalone LOCK; uncertainty never authorizes automatic replay."
	}
	if code == "output_conflict" {
		e.Recovery = "Choose a different explicit output path; existing files are never replaced by agent download."
	}
	return e
}
