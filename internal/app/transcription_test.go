package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The only adapter executed by these tests is this test binary in stub mode.
// It consumes synthetic stdin, uses no network, and never opens App/WA/DB.
func TestMain(m *testing.M) {
	if os.Getenv("WACLI_TEST_TRANSCRIPTION_STUB") == "1" && len(os.Args) == 5 && os.Args[1] == "--protocol" {
		os.Exit(runTranscriptionStub())
	}
	os.Exit(m.Run())
}

func runTranscriptionStub() int {
	if os.Args[2] != "wacli-transcribe-v1" || os.Args[3] != "--mime-type" {
		return 71
	}
	_, params, err := mime.ParseMediaType(os.Args[4])
	if err != nil {
		return 72
	}
	if params["mode"] == "holder" {
		time.Sleep(2 * time.Second)
		return 0
	}
	if marker := os.Getenv("WACLI_TEST_TRANSCRIPTION_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("executed"), 0o600); err != nil {
			return 73
		}
	}
	if params["mode"] == "ignore_stdin" {
		time.Sleep(10 * time.Second)
		return 0
	}
	if params["mode"] == "stdout_over" {
		// Ignore stdin and continue writing after the cap. The runner must
		// terminate us and join both its blocked stdin and stdout copiers.
		for {
			if _, err := os.Stdout.Write(bytes.Repeat([]byte{'x'}, 8192)); err != nil {
				return 75
			}
		}
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 74
	}
	switch params["mode"] {
	case "valid", "argv":
		digest := sha256.Sum256(input)
		text := hex.EncodeToString(digest[:])
		if params["mode"] == "argv" {
			text = os.Args[4]
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "text": text, "language": "pt-BR"})
	case "empty":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":""}`)
	case "whitespace":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":" \n\t"}`)
	case "long", "text_limit", "text_over":
		n := 400
		if params["mode"] == "text_limit" {
			n = maxTranscriptionTextBytes / len("界")
		} else if params["mode"] == "text_over" {
			n = maxTranscriptionTextBytes/len("界") + 1
		}
		text := strings.Repeat("界", n)
		if params["mode"] == "text_limit" {
			text += strings.Repeat("x", maxTranscriptionTextBytes-len(text))
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "text": text})
	case "stdout_limit":
		output := `{"schema_version":1,"text":"ok"}`
		fmt.Fprint(os.Stdout, output+strings.Repeat(" ", maxTranscriptionOutputBytes-len(output)))
	case "stderr", "nonzero":
		fmt.Fprint(os.Stderr, strings.Repeat("secret-token-private-path\n", 10000))
		if params["mode"] == "nonzero" {
			return 76
		}
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":"ok"}`)
	case "block":
		time.Sleep(10 * time.Second)
	case "descendant", "descendant_block":
		child := exec.Command(os.Args[0], "--protocol", "wacli-transcribe-v1", "--mime-type", "application/x-wacli-stub; mode=holder")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 77
		}
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":"ok"}`)
		if params["mode"] == "descendant_block" {
			time.Sleep(10 * time.Second)
		}
	case "invalid":
		fmt.Fprint(os.Stdout, "secret-token-private-path")
	default:
		return 78
	}
	return 0
}

func transcriptionStubOptions(t *testing.T, mode string) TranscriptionOptions {
	t.Helper()
	t.Setenv("WACLI_TEST_TRANSCRIPTION_STUB", "1")
	adapter, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return TranscriptionOptions{Adapter: adapter, MIMEType: "application/x-wacli-stub; mode=" + mode, Timeout: 5 * time.Second}
}

func requireTranscriptionError(t *testing.T, err error, code TranscriptionErrorCode) {
	t.Helper()
	var failure *TranscriptionError
	if !errors.As(err, &failure) || failure.Code != code || err.Error() != string(code) {
		t.Fatalf("want %s, got %v", code, err)
	}
	data, marshalErr := json.Marshal(failure)
	if marshalErr != nil || string(data) != fmt.Sprintf(`{"code":%q}`, code) {
		t.Fatalf("public error contains more than its code: %s, %v", data, marshalErr)
	}
}

func TestTranscriptionExactInputAndResult(t *testing.T) {
	options := transcriptionStubOptions(t, "valid")
	for _, input := range [][]byte{nil, {}, {0, 255, 1, 0, 128}, bytes.Repeat([]byte("synthetic audio\x00"), 2000), make([]byte, MaxTranscriptionInputBytes)} {
		sum := sha256.Sum256(input)
		options.ExpectSHA256 = hex.EncodeToString(sum[:])
		result, err := Transcribe(context.Background(), input, options)
		if err != nil || result.Text != options.ExpectSHA256 || result.Language != "pt-BR" || result.Input.Bytes != len(input) || result.Input.SHA256 != options.ExpectSHA256 || result.AdapterStatus != TranscriptionCompleted || result.TextTruncated {
			t.Fatalf("input=%d: %+v, %v", len(input), result, err)
		}
		data, err := json.Marshal(result)
		if err != nil || bytes.Contains(data, []byte(options.Adapter)) || bytes.Contains(data, []byte("synthetic audio")) {
			t.Fatalf("unexpected envelope: %s, %v", data, err)
		}
	}
}

func TestTranscriptionArgvIsData(t *testing.T) {
	options := transcriptionStubOptions(t, "argv")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	options.MIMEType += fmt.Sprintf(`; note="$(touch %s); 'quoted' & |"`, marker)
	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "adapter; $(false) 'quoted'")
		if err := os.Symlink(options.Adapter, alias); err != nil {
			t.Fatal(err)
		}
		options.Adapter = alias
	}
	result, err := Transcribe(context.Background(), []byte("fixture"), options)
	if err != nil || result.Text != options.MIMEType {
		t.Fatalf("argv changed: %+v, %v", result, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metacharacters executed: %v", err)
	}
}

func TestTranscriptionRejectsBeforeExecution(t *testing.T) {
	base := transcriptionStubOptions(t, "valid")
	marker := filepath.Join(t.TempDir(), "execution-marker")
	t.Setenv("WACLI_TEST_TRANSCRIPTION_MARKER", marker)
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExecutable, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*TranscriptionOptions)
		input  []byte
		code   TranscriptionErrorCode
	}{
		{"missing", func(o *TranscriptionOptions) { o.Adapter = "" }, nil, TranscriptionAdapterNotConfigured},
		{"relative", func(o *TranscriptionOptions) { o.Adapter = "stub" }, nil, TranscriptionInvalidArguments},
		{"nonexistent", func(o *TranscriptionOptions) { o.Adapter = filepath.Join(t.TempDir(), "missing") }, nil, TranscriptionPathNotAllowed},
		{"directory", func(o *TranscriptionOptions) { o.Adapter = t.TempDir() }, nil, TranscriptionPathNotAllowed},
		{"not_executable", func(o *TranscriptionOptions) { o.Adapter = notExecutable }, nil, TranscriptionPathNotAllowed},
		{"mime_missing", func(o *TranscriptionOptions) { o.MIMEType = "" }, nil, TranscriptionInvalidArguments},
		{"mime_invalid", func(o *TranscriptionOptions) { o.MIMEType = "not a type" }, nil, TranscriptionInvalidArguments},
		{"mime_token", func(o *TranscriptionOptions) { o.MIMEType = "audio" }, nil, TranscriptionInvalidArguments},
		{"mime_control", func(o *TranscriptionOptions) { o.MIMEType = "audio/ogg\n" }, nil, TranscriptionInvalidArguments},
		{"mime_utf8", func(o *TranscriptionOptions) { o.MIMEType = "audio/ogg; note=\"" + string([]byte{255}) + "\"" }, nil, TranscriptionInvalidArguments},
		{"mime_long", func(o *TranscriptionOptions) { o.MIMEType = "audio/" + strings.Repeat("x", 256) }, nil, TranscriptionInvalidArguments},
		{"hash_short", func(o *TranscriptionOptions) { o.ExpectSHA256 = "ab" }, nil, TranscriptionInvalidArguments},
		{"hash_upper", func(o *TranscriptionOptions) { o.ExpectSHA256 = strings.Repeat("AB", 32) }, nil, TranscriptionInvalidArguments},
		{"hash_invalid", func(o *TranscriptionOptions) { o.ExpectSHA256 = strings.Repeat("z", 64) }, nil, TranscriptionInvalidArguments},
		{"hash_mismatch", func(o *TranscriptionOptions) { o.ExpectSHA256 = strings.Repeat("0", 64) }, []byte("synthetic"), TranscriptionHashMismatch},
		{"size", func(o *TranscriptionOptions) {}, make([]byte, MaxTranscriptionInputBytes+1), TranscriptionInputTooLarge},
		{"timeout_negative", func(o *TranscriptionOptions) { o.Timeout = -1 }, nil, TranscriptionInvalidArguments},
		{"timeout_long", func(o *TranscriptionOptions) { o.Timeout = MaxTranscriptionTimeout + 1 }, nil, TranscriptionInvalidArguments},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := base
			tc.change(&options)
			result, err := Transcribe(context.Background(), tc.input, options)
			requireTranscriptionError(t, err, tc.code)
			if result.AdapterStatus != "" || result.Text != "" {
				t.Fatalf("failed operation claims a transcript: %+v", result)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("adapter executed: %v", err)
			}
		})
	}
}

func TestTranscriptionEmptyAndCompact(t *testing.T) {
	for _, mode := range []string{"empty", "whitespace", "long", "text_limit", "stdout_limit", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			options := transcriptionStubOptions(t, mode)
			options.Timeout = 0 // Default deadline is valid without a provider default.
			result, err := Transcribe(context.Background(), []byte("fixture"), options)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "empty" || mode == "whitespace" {
				if result.AdapterStatus != TranscriptionEmpty {
					t.Fatalf("empty output claims recognition: %+v", result)
				}
			} else if result.AdapterStatus != TranscriptionCompleted {
				t.Fatalf("unexpected adapter status: %+v", result)
			}
			compact := result.Compact()
			if mode == "long" || mode == "text_limit" {
				if compact.Text != strings.Repeat("界", 320) || !compact.TextTruncated || result.TextTruncated || compact.Input != result.Input || compact.AdapterStatus != result.AdapterStatus {
					t.Fatalf("compact changed metadata or Unicode: %+v", compact)
				}
			} else if compact != result {
				t.Fatalf("short text changed: %+v", compact)
			}
		})
	}
}

func TestTranscriptionStrictOutput(t *testing.T) {
	for _, output := range []string{
		``, `null`, `[]`, `{"schema_version":1}`, `{"text":"ok"}`,
		`{"schema_version":null,"text":"ok"}`, `{"schema_version":2,"text":"ok"}`,
		`{"schema_version":1.0,"text":"ok"}`, `{"schema_version":"1","text":"ok"}`,
		`{"schema_version":1,"text":null}`, `{"schema_version":1,"text":4}`,
		`{"schema_version":1,"text":"ok","language":null}`,
		`{"schema_version":1,"text":"ok","language":4}`,
		`{"schema_version":1,"text":"ok","language":"` + strings.Repeat("x", 65) + `"}`,
		`{"schema_version":1,"text":"ok","language":"pt\nBR"}`,
		`{"schema_version":1,"text":"ok","secret":"secret-token"}`,
		`{"schema_version":1,"text":"ok","text":"other"}`,
		`{"schema_version":1,"text":"ok"} {}`, `{"schema_version":1,"text":"ok"}x`,
		`{"schema_version":1,"text":"ok"`, `{"schema_version":1,"text":"` + string([]byte{255}) + `"}`,
	} {
		if _, _, err := decodeTranscriptionOutput([]byte(output)); err == nil {
			t.Errorf("accepted malformed output: %q", output)
		}
	}
	for _, output := range []string{`{"schema_version":1,"text":""}`, " \n" + `{"language":"pt-BR","text":"界😀","schema_version":1}` + "\n\t"} {
		if _, _, err := decodeTranscriptionOutput([]byte(output)); err != nil {
			t.Errorf("valid output rejected: %q, %v", output, err)
		}
	}
}

func TestTranscriptionProcessFailuresArePrivateAndBounded(t *testing.T) {
	for _, tc := range []struct {
		mode string
		code TranscriptionErrorCode
	}{
		{"nonzero", TranscriptionProcessFailed},
		{"invalid", TranscriptionOutputInvalid},
		{"stdout_over", TranscriptionOutputInvalid},
		{"text_over", TranscriptionOutputInvalid},
		{"descendant", TranscriptionProcessFailed},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			options := transcriptionStubOptions(t, tc.mode)
			input := []byte("fixture")
			if tc.mode == "stdout_over" {
				input = make([]byte, MaxTranscriptionInputBytes)
			}
			start := time.Now()
			_, err := Transcribe(context.Background(), input, options)
			requireTranscriptionError(t, err, tc.code)
			if time.Since(start) > 1500*time.Millisecond {
				t.Fatalf("pipe/oversize wait was not bounded: %v", time.Since(start))
			}
			if tc.mode == "descendant" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("expected bounded descendant pipe wait: %v", err)
			}
		})
	}
}

func TestTranscriptionTimeoutAndCancellation(t *testing.T) {
	for _, mode := range []string{"timeout", "deadline", "cancel", "pre_cancel", "ignore_stdin", "descendant_block"} {
		t.Run(mode, func(t *testing.T) {
			options := transcriptionStubOptions(t, "block")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := TranscriptionCancelled
			marker := filepath.Join(t.TempDir(), "executed")
			t.Setenv("WACLI_TEST_TRANSCRIPTION_MARKER", marker)
			input := []byte("fixture")
			switch mode {
			case "timeout":
				options.Timeout = 100 * time.Millisecond
				code = TranscriptionTimedOut
			case "deadline":
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer deadlineCancel()
				code = TranscriptionTimedOut
			case "cancel":
				timer := time.AfterFunc(100*time.Millisecond, cancel)
				defer timer.Stop()
			case "pre_cancel":
				cancel()
			case "ignore_stdin":
				options.MIMEType = "application/x-wacli-stub; mode=ignore_stdin"
				options.Timeout = 100 * time.Millisecond
				input = make([]byte, MaxTranscriptionInputBytes)
				code = TranscriptionTimedOut
			case "descendant_block":
				options.MIMEType = "application/x-wacli-stub; mode=descendant_block"
				options.Timeout = 100 * time.Millisecond
				code = TranscriptionTimedOut
			}
			start := time.Now()
			_, err := Transcribe(ctx, input, options)
			requireTranscriptionError(t, err, code)
			if time.Since(start) > time.Second {
				t.Fatal("cancellation failed to bound execution")
			}
			if mode == "pre_cancel" {
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pre-cancelled adapter executed: %v", err)
				}
			}
		})
	}
}

func TestTranscriptionBuffersRetainOnlyTheirLimits(t *testing.T) {
	for _, limit := range []int{maxTranscriptionOutputBytes, maxTranscriptionStderrBytes} {
		ctx, cancel := context.WithCancel(context.Background())
		buffer := &transcriptionOutputBuffer{limit: limit}
		if limit == maxTranscriptionOutputBytes {
			buffer.cancel = cancel
		}
		payload := bytes.Repeat([]byte{'x'}, limit+10000)
		n, err := buffer.Write(payload)
		if buffer.data.Len() != limit || !buffer.exceeded {
			t.Fatalf("unbounded buffer: %d", buffer.data.Len())
		}
		if buffer.cancel != nil {
			if n != limit || !errors.Is(err, io.ErrShortBuffer) || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("oversize did not stop stdout: %d, %v, %v", n, err, ctx.Err())
			}
		} else if n != len(payload) || err != nil {
			t.Fatalf("stderr did not drain: %d, %v", n, err)
		}
		cancel()
	}
}
