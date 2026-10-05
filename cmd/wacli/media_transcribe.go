package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

type agentTranscriptionInput struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Explicit public DTO: no executable path, raw streams, binary or private cause.
type agentTranscriptionResult struct {
	Text          string                         `json:"text"`
	Language      *string                        `json:"language"`
	Status        app.TranscriptionAdapterStatus `json:"status"`
	Input         agentTranscriptionInput        `json:"input"`
	TextTruncated bool                           `json:"text_truncated"`
}

func newMediaTranscribeCmd(flags *rootFlags) *cobra.Command {
	var file string
	var options app.TranscriptionOptions
	cmd := &cobra.Command{
		Use: "transcribe", Short: "Transcribe local bytes using an explicitly selected executable adapter",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !flags.agent {
				return transcriptionUsageError()
			}
			options.Timeout = flags.timeout
			return runAgentTranscription(cmd.Context(), flags, file, options, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "explicit local regular file (at most 25 MiB)")
	cmd.Flags().StringVar(&options.Adapter, "adapter", "", "absolute executable adapter path; required, never discovered or installed")
	cmd.Flags().StringVar(&options.ExpectSHA256, "expect-sha256", "", "optional lowercase SHA-256 of the exact bytes supplied on stdin")
	cmd.Flags().StringVar(&options.MIMEType, "mime-type", "", "explicit MIME type; otherwise detect from observed bytes, never filename")
	return cmd
}

func validateAgentTranscription(cmd *cobra.Command, flags *rootFlags) error {
	file, _ := cmd.Flags().GetString("file")
	adapter, _ := cmd.Flags().GetString("adapter")
	mimeType, _ := cmd.Flags().GetString("mime-type")
	hash, _ := cmd.Flags().GetString("expect-sha256")
	if adapter == "" {
		return classifyTranscriptionCommandError(&app.TranscriptionError{Code: app.TranscriptionAdapterNotConfigured})
	}
	if file == "" || len(file) > 4096 || strings.Contains(file, "://") || strings.IndexFunc(file, unicode.IsControl) >= 0 || len(adapter) > 4096 || strings.IndexFunc(adapter, unicode.IsControl) >= 0 || flags.timeout <= 0 || flags.timeout > app.MaxTranscriptionTimeout {
		return transcriptionUsageError()
	}
	if _, err := outboundMediaRoots(); err != nil {
		return transcriptionUsageError()
	}
	// Byte detection is deferred until the confined read. This placeholder
	// only permits configuration validation before reading any input file.
	if mimeType == "" && !cmd.Flags().Changed("mime-type") {
		mimeType = "application/octet-stream"
	}
	if err := app.ValidateTranscriptionOptions(app.TranscriptionOptions{Adapter: adapter, MIMEType: mimeType, ExpectSHA256: hash, Timeout: flags.timeout}); err != nil {
		return classifyTranscriptionCommandError(err)
	}
	return nil
}

func runAgentTranscription(parent context.Context, flags *rootFlags, path string, options app.TranscriptionOptions, dst io.Writer) error {
	signalCtx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := withTimeout(signalCtx, flags)
	defer cancel()
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return agentStoreError(err)
	}
	roots, err := outboundMediaRoots()
	if err != nil {
		return transcriptionUsageError()
	}
	loc, err := openAgentMediaLocation(path, storeDir, roots, false, false)
	if err != nil {
		return classifyTranscriptionCommandError(err)
	}
	defer loc.close()
	input, err := app.ReadTranscriptionInput(ctx, loc.MediaLocation)
	if err != nil {
		return classifyTranscriptionCommandError(err)
	}
	if options.MIMEType == "" {
		options.MIMEType = http.DetectContentType(input)
		if options.MIMEType == "application/octet-stream" {
			return transcriptionUsageError()
		}
	}
	result, err := app.Transcribe(ctx, input, options)
	if err != nil {
		return classifyTranscriptionCommandError(err)
	}
	if flags.detail != "full" {
		result = result.Compact()
	}
	var language *string
	if result.Language != "" {
		language = &result.Language
	}
	dto := agentTranscriptionResult{
		Text: result.Text, Language: language, Status: result.AdapterStatus,
		Input:         agentTranscriptionInput{Path: path, Bytes: result.Input.Bytes, SHA256: result.Input.SHA256},
		TextTruncated: result.TextTruncated,
	}
	meta := agentMeta(flags)
	if dto.TextTruncated {
		meta.Recovery = "Transcript text was truncated. No transcript was stored; obtaining full text requires another explicit adapter execution with --detail full. Do not repeat automatically."
	}
	if err := out.WriteAgentActionJSON(dst, flags.agentAccount, meta, dto); err != nil {
		return &out.AgentError{Code: "output_failed", Message: "Transcription output could not be written; no transcript was stored.", ExitCode: 1}
	}
	return nil
}

func transcriptionUsageError() *out.AgentError {
	return &out.AgentError{Code: "invalid_arguments", Message: "Transcription requires --agent, --file PATH, an explicit absolute executable --adapter, optional canonical --expect-sha256 and valid --mime-type, with --timeout positive and at most 5m. Unknown bytes require explicit --mime-type.", ExitCode: 2}
}

func classifyTranscriptionCommandError(err error) *out.AgentError {
	if err == nil {
		return nil
	}
	var existing *out.AgentError
	if errors.As(err, &existing) {
		if existing.Code == "invalid_arguments" {
			return transcriptionUsageError()
		}
		return existing
	}
	code := app.TranscriptionInputUnreadable
	var transcription *app.TranscriptionError
	var media *app.MediaArtifactError
	switch {
	case errors.As(err, &transcription):
		code = transcription.Code
	case errors.As(err, &media):
		code = app.TranscriptionPathNotAllowed
	case errors.Is(err, context.DeadlineExceeded):
		code = app.TranscriptionTimedOut
	case errors.Is(err, context.Canceled):
		code = app.TranscriptionCancelled
	case errors.Is(err, os.ErrNotExist):
		code = app.TranscriptionInputNotFound
	}
	message, exit := "Local transcription input could not be read.", 1
	switch code {
	case app.TranscriptionInvalidArguments:
		return transcriptionUsageError()
	case app.TranscriptionAdapterNotConfigured:
		message, exit = "No transcription adapter is configured. Supply an explicit --adapter executable.", 2
	case app.TranscriptionInputNotFound:
		message, exit = "Requested local transcription input was not found.", 3
	case app.TranscriptionPathNotAllowed:
		message = "Transcription requires an allowed regular input file and an absolute executable adapter."
	case app.TranscriptionHashMismatch:
		message = "Observed input bytes do not match --expect-sha256; the adapter was not executed."
	case app.TranscriptionInputTooLarge:
		message = "Transcription input exceeds 25 MiB; the adapter was not executed."
	case app.TranscriptionProcessFailed:
		message = "The selected transcription adapter failed."
	case app.TranscriptionTimedOut:
		message = "Local transcription timed out; external adapter effects are not rolled back."
	case app.TranscriptionCancelled:
		message = "Local transcription was cancelled; external adapter effects are not rolled back."
	case app.TranscriptionOutputInvalid:
		message = "The selected adapter returned invalid or oversized transcription output."
	}
	return &out.AgentError{Code: string(code), Message: message, ExitCode: exit}
}

// A missing selection is identity only, never an invitation to create it.
// Resolve existing ancestors so the shared PR17 control-path check still works.
func transcriptionStoreBoundary(path string) (string, error) {
	var suffix []string
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, lstatErr := os.Lstat(path); !errors.Is(lstatErr, os.ErrNotExist) {
			return "", err // Existing/dangling links are not missing components.
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}
