package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxTranscriptionInputBytes  = 25 << 20
	maxTranscriptionOutputBytes = 256 << 10
	maxTranscriptionTextBytes   = 128 << 10
	maxTranscriptionStderrBytes = 8 << 10
	MaxTranscriptionTimeout     = 300 * time.Second
	transcriptionWaitDelay      = 250 * time.Millisecond
)

type TranscriptionErrorCode string

const (
	TranscriptionInvalidArguments     TranscriptionErrorCode = "invalid_arguments"
	TranscriptionAdapterNotConfigured TranscriptionErrorCode = "adapter_not_configured"
	TranscriptionPathNotAllowed       TranscriptionErrorCode = "path_not_allowed"
	TranscriptionHashMismatch         TranscriptionErrorCode = "hash_mismatch"
	TranscriptionInputTooLarge        TranscriptionErrorCode = "input_too_large"
	TranscriptionInputNotFound        TranscriptionErrorCode = "input_not_found"
	TranscriptionInputUnreadable      TranscriptionErrorCode = "input_unreadable"
	TranscriptionProcessFailed        TranscriptionErrorCode = "adapter_failed"
	TranscriptionTimedOut             TranscriptionErrorCode = "transcription_timeout"
	TranscriptionCancelled            TranscriptionErrorCode = "cancelled"
	TranscriptionOutputInvalid        TranscriptionErrorCode = "adapter_output_invalid"
)

// TranscriptionError exposes only a stable code. Causes are for internal
// classification; callers must never serialize them or adapter output.
type TranscriptionError struct {
	Code  TranscriptionErrorCode `json:"code"`
	cause error
}

func (e *TranscriptionError) Error() string { return string(e.Code) }
func (e *TranscriptionError) Unwrap() error { return e.cause }

func transcriptionFailure(code TranscriptionErrorCode, cause error) error {
	return &TranscriptionError{Code: code, cause: cause}
}

type TranscriptionOptions struct {
	Adapter      string
	MIMEType     string
	ExpectSHA256 string
	// Zero selects 300 seconds; positive values may only shorten that limit.
	Timeout time.Duration
}

type TranscriptionAdapterStatus string

const (
	TranscriptionCompleted TranscriptionAdapterStatus = "completed"
	// Empty means the adapter returned no non-whitespace text. It does not
	// establish silence, successful recognition, or absence of speech.
	TranscriptionEmpty TranscriptionAdapterStatus = "empty"
)

type TranscriptionInput struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type TranscriptionResult struct {
	Text          string                     `json:"text"`
	Language      string                     `json:"language,omitempty"`
	Input         TranscriptionInput         `json:"input"`
	AdapterStatus TranscriptionAdapterStatus `json:"adapter_status"`
	TextTruncated bool                       `json:"text_truncated"`
}

// Compact preserves metadata and limits text to 320 Unicode code points.
func (r TranscriptionResult) Compact() TranscriptionResult {
	if utf8.RuneCountInString(r.Text) > 320 {
		r.Text = string([]rune(r.Text)[:320])
		r.TextTruncated = true
	}
	return r
}

// Transcribe snapshots already-confined observed bytes and supplies exactly
// that snapshot on stdin. It does not open an App, archive, session, or media
// path, discover providers, install software, persist, or retry. The explicitly
// selected executable is NOT a network/effects sandbox; it can have its own
// network and filesystem behavior. Cancellation kills the direct process and
// bounds pipe waits, but does not promise to terminate arbitrary descendants.
// The caller must not mutate input concurrently with this call.
func Transcribe(ctx context.Context, input []byte, options TranscriptionOptions) (TranscriptionResult, error) {
	var result TranscriptionResult
	if err := ValidateTranscriptionOptions(options); err != nil {
		return result, err
	}
	if len(input) > MaxTranscriptionInputBytes {
		return result, transcriptionFailure(TranscriptionInputTooLarge, nil)
	}
	if err := transcriptionContextError(ctx); err != nil {
		return result, err
	}
	snapshot := bytes.Clone(input)
	sum := sha256.Sum256(snapshot)
	digest := hex.EncodeToString(sum[:])
	if options.ExpectSHA256 != "" && options.ExpectSHA256 != digest {
		return result, transcriptionFailure(TranscriptionHashMismatch, nil)
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = MaxTranscriptionTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, options.Adapter, "--protocol", "wacli-transcribe-v1", "--mime-type", options.MIMEType)
	cmd.WaitDelay = transcriptionWaitDelay
	cmd.Stdin = bytes.NewReader(snapshot)
	stdout := &transcriptionOutputBuffer{limit: maxTranscriptionOutputBytes, cancel: cancel}
	stderr := &transcriptionOutputBuffer{limit: maxTranscriptionStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	// Oversized output initiated cancellation and remains an output error.
	if stdout.exceeded {
		return result, transcriptionFailure(TranscriptionOutputInvalid, nil)
	}
	if contextErr := transcriptionContextError(runCtx); contextErr != nil {
		return result, contextErr
	}
	if err != nil {
		return result, transcriptionFailure(TranscriptionProcessFailed, err)
	}
	text, language, err := decodeTranscriptionOutput(stdout.data.Bytes())
	if err != nil {
		return result, transcriptionFailure(TranscriptionOutputInvalid, nil)
	}
	result = TranscriptionResult{
		Text: text, Language: language,
		Input:         TranscriptionInput{Bytes: len(snapshot), SHA256: digest},
		AdapterStatus: TranscriptionCompleted,
	}
	if strings.TrimSpace(text) == "" {
		result.AdapterStatus = TranscriptionEmpty
	}
	return result, nil
}

// ValidateTranscriptionOptions preflights explicit configuration without
// executing anything or reading media bytes. Transcribe rechecks it at use.
func ValidateTranscriptionOptions(o TranscriptionOptions) error {
	if o.Adapter == "" {
		return transcriptionFailure(TranscriptionAdapterNotConfigured, nil)
	}
	if !filepath.IsAbs(o.Adapter) {
		return transcriptionFailure(TranscriptionInvalidArguments, nil)
	}
	if o.Timeout < 0 || o.Timeout > MaxTranscriptionTimeout {
		return transcriptionFailure(TranscriptionInvalidArguments, nil)
	}
	if len(o.MIMEType) == 0 || len(o.MIMEType) > 256 || !utf8.ValidString(o.MIMEType) || strings.ContainsAny(o.MIMEType, "\x00\r\n") {
		return transcriptionFailure(TranscriptionInvalidArguments, nil)
	}
	mediaType, _, err := mime.ParseMediaType(o.MIMEType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return transcriptionFailure(TranscriptionInvalidArguments, nil)
	}
	if o.ExpectSHA256 != "" {
		digest, err := hex.DecodeString(o.ExpectSHA256)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != o.ExpectSHA256 {
			return transcriptionFailure(TranscriptionInvalidArguments, nil)
		}
	}
	info, err := os.Stat(o.Adapter)
	if err != nil || !info.Mode().IsRegular() {
		return transcriptionFailure(TranscriptionPathNotAllowed, err)
	}
	// Windows does not report Unix executable bits. Only native executables
	// are accepted there, avoiding command scripts and shell interpretation.
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.Ext(o.Adapter), ".exe") {
			return transcriptionFailure(TranscriptionPathNotAllowed, nil)
		}
	} else if info.Mode().Perm()&0o111 == 0 {
		return transcriptionFailure(TranscriptionPathNotAllowed, nil)
	}
	return nil
}

func transcriptionContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return transcriptionFailure(TranscriptionTimedOut, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return transcriptionFailure(TranscriptionCancelled, err)
	}
	return nil
}

// Each buffer has one os/exec copying goroutine. Run joins them before reads.
// Stderr discards excess while draining; stdout excess cancels the process.
type transcriptionOutputBuffer struct {
	data     bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *transcriptionOutputBuffer) Write(p []byte) (int, error) {
	n := min(len(p), b.limit-b.data.Len())
	_, _ = b.data.Write(p[:n])
	if n < len(p) {
		b.exceeded = true
		if b.cancel != nil {
			b.cancel()
			return n, io.ErrShortBuffer
		}
	}
	return len(p), nil
}

// Decode tokens to reject duplicates as well as unknown, null, missing, and
// incorrectly typed fields. encoding/json alone repairs invalid raw UTF-8.
func decodeTranscriptionOutput(data []byte) (string, string, error) {
	invalid := errors.New("invalid transcription output")
	if !utf8.Valid(data) {
		return "", "", invalid
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", invalid
	}
	seen := make(map[string]bool, 3)
	var text, language string
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return "", "", invalid
		}
		seen[key] = true
		switch key {
		case "schema_version":
			var version *int
			if err := dec.Decode(&version); err != nil || version == nil || *version != 1 {
				return "", "", invalid
			}
		case "text", "language":
			var value *string
			if err := dec.Decode(&value); err != nil || value == nil || !utf8.ValidString(*value) {
				return "", "", invalid
			}
			if key == "text" {
				text = *value
				if len(text) > maxTranscriptionTextBytes {
					return "", "", invalid
				}
			} else {
				language = *value
				if len(language) > 64 || strings.ContainsAny(language, "\x00\r\n") {
					return "", "", invalid
				}
			}
		default:
			return "", "", invalid
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') || !seen["schema_version"] || !seen["text"] {
		return "", "", invalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", invalid
	}
	return text, language, nil
}
