package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func changesWatchFixture(t *testing.T) (string, *store.DB, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	writer, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lk.Release() })
	reader, err := store.OpenReadOnly(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return dir, writer, reader
}

func insertWatchMessage(t *testing.T, db *store.DB, chat, id string) {
	t.Helper()
	if err := db.UpsertChat(chat, "dm", "fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: time.Unix(100, 0), Text: "fixture payload", SenderJID: chat}); err != nil {
		t.Fatal(err)
	}
}

func TestChangesWatchEmptyReadThenCommitBeforeWait(t *testing.T) {
	_, writer, reader := changesWatchFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var frames []store.ChangesPage
	started := time.Now()
	err := watchChanges(ctx, reader, "fixture", 1, "", 100*time.Millisecond, func(p store.ChangesPage) error {
		frames = append(frames, p)
		if len(frames) == 1 {
			if len(p.Changes) != 0 || p.NextCursor == "" || store.ValidateChangesCursor(p.NextCursor) != nil {
				t.Fatalf("initial: %+v", p)
			}
			// Commit after the empty snapshot was released, before the timer is installed.
			insertWatchMessage(t, writer, "123@s.whatsapp.net", "between-read-and-wait")
		} else {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(frames) != 2 || len(frames[1].Changes) != 1 || frames[1].Changes[0].ID != "between-read-and-wait" || frames[0].NextCursor == frames[1].NextCursor {
		t.Fatalf("frames=%+v err=%v", frames, err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("poll latency %s", elapsed)
	}
}

func TestChangesWatchBacklogResumeReceiptRepairAndPurge(t *testing.T) {
	_, writer, reader := changesWatchFixture(t)
	lid, pn := "400@lid", "123@s.whatsapp.net"
	insertWatchMessage(t, writer, lid, "one")
	insertWatchMessage(t, writer, pn, "two")
	receipt := store.ChangeReceipt{Type: "read", ActorJID: pn, EventAt: time.Unix(101, 0)}
	for range 2 {
		if err := writer.RecordChangeReceipts(t.Context(), pn, []string{"receipt-only"}, true, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.MarkMessageDeletedForMe(lid, "one", lid, false, time.Unix(110, 0)); err != nil {
		t.Fatal(err)
	}
	if err := writer.PurgeMessage(lid, "one"); err != nil {
		t.Fatal(err)
	}
	if err := writer.MigrateLIDToPN(lid, pn); err != nil {
		t.Fatal(err)
	}
	expected, err := reader.ListChanges(t.Context(), "fixture", 200, "")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, c := range expected.Changes {
		kinds[c.Kind] = true
	}
	for _, kind := range []string{"receipt", "identity_mapping", "message_tombstone", "message_delete"} {
		if !kinds[kind] {
			t.Fatalf("missing fixture kind %s", kind)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var frames []store.ChangesPage
	err = watchChanges(ctx, reader, "fixture", 2, "", time.Minute, func(p store.ChangesPage) error {
		frames = append(frames, p)
		if !p.HasMore {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(frames) < 3 {
		t.Fatalf("frames=%d err=%v", len(frames), err)
	}
	var got []store.Change
	seen := map[string]bool{}
	for i, p := range frames {
		if len(p.Changes) > 2 || p.NextCursor == "" || (i > 0 && p.NextCursor == frames[i-1].NextCursor) {
			t.Fatalf("page %+v", p)
		}
		for _, c := range p.Changes {
			if seen[c.EventID] {
				t.Fatal("duplicate event")
			}
			seen[c.EventID] = true
			got = append(got, c)
		}
	}
	if !reflect.DeepEqual(got, expected.Changes) {
		t.Fatalf("stream=%+v list=%+v", got, expected.Changes)
	}
	// Only the first frame was processed before a simulated consumer crash.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	var suffix []store.Change
	err = watchChanges(ctx2, reader, "fixture", 3, frames[0].NextCursor, time.Minute, func(p store.ChangesPage) error {
		suffix = append(suffix, p.Changes...)
		if !p.HasMore {
			cancel2()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(suffix, expected.Changes[2:]) {
		t.Fatalf("suffix=%+v err=%v", suffix, err)
	}
	// Resuming a processed final page still produces one useful empty checkpoint.
	ctx3, cancel3 := context.WithCancel(t.Context())
	defer cancel3()
	err = watchChanges(ctx3, reader, "fixture", 1, expected.NextCursor, time.Minute, func(p store.ChangesPage) error {
		if len(p.Changes) != 0 || p.NextCursor != expected.NextCursor {
			t.Fatalf("resume end %+v", p)
		}
		cancel3()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestChangesWatchCursorContinuityRefusals(t *testing.T) {
	dir, writer, reader := changesWatchFixture(t)
	insertWatchMessage(t, writer, "123@s.whatsapp.net", "one")
	first, err := reader.ListChanges(t.Context(), "fixture", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, other := changesWatchFixture(t)
	for _, test := range []struct {
		reader       *store.DB
		scope, token string
	}{{reader, "fixture", "PRIVATE-CURSOR"}, {reader, "other", first.NextCursor}, {other, "fixture", first.NextCursor}} {
		err = watchChanges(t.Context(), test.reader, test.scope, 1, test.token, time.Second, func(store.ChangesPage) error { t.Fatal("unexpected frame"); return nil })
		if classifyAgentError(err).Code != "invalid_cursor" {
			t.Fatal(err)
		}
	}
	sqlDB, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	for _, test := range []struct{ statement, code string }{
		{"UPDATE archive_changes SET event_id=lower(hex(randomblob(16))) WHERE seq=1", "cursor_expired"},
		{"UPDATE change_feed_identity SET epoch=lower(hex(randomblob(16)))", "invalid_cursor"},
	} {
		frames := 0
		err = watchChanges(t.Context(), reader, "fixture", 1, "", 100*time.Millisecond, func(p store.ChangesPage) error {
			frames++
			if _, e := sqlDB.Exec(test.statement); e != nil {
				t.Fatal(e)
			}
			return nil
		})
		if classifyAgentError(err).Code != test.code || frames != 1 {
			t.Fatalf("frames=%d err=%v", frames, err)
		}
	}
}

type shortChangesWriter struct {
	calls int
	bytes bytes.Buffer
}

func (w *shortChangesWriter) Write(b []byte) (int, error) {
	w.calls++
	return w.bytes.Write(b[:len(b)/2])
}

func TestChangesWatchOutputFailureStopsWithoutCheckpoint(t *testing.T) {
	dir, writer, reader := changesWatchFixture(t)
	insertWatchMessage(t, writer, "123@s.whatsapp.net", "one")
	insertWatchMessage(t, writer, "123@s.whatsapp.net", "two")
	for _, agent := range []bool{false, true} {
		w := &shortChangesWriter{}
		flags := &rootFlags{agent: agent, agentAccount: out.AgentAccount{StoreRef: &dir}}
		err := watchChanges(t.Context(), reader, "fixture", 1, "", time.Second, func(p store.ChangesPage) error { return writeChangesFrame(w, flags, 1, p) })
		if !errors.Is(err, io.ErrShortWrite) || classifyAgentError(err).Code != "output_failed" || w.calls != 1 || json.Valid(w.bytes.Bytes()) {
			t.Fatalf("err=%v calls=%d bytes=%s", err, w.calls, w.bytes.String())
		}
	}
}

func TestChangesWatchIdleTimeoutAndValidation(t *testing.T) {
	dir, _, _ := changesWatchFixture(t)
	t.Setenv("WACLI_READONLY", "1")
	started := time.Now()
	stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "--timeout", "350ms", "changes", "watch", "--interval", "100ms")
	if commandExitCode(err) != 1 || decodeAgentTest(t, stderr).Error.Code != "timeout" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("%s %s %v", stdout, stderr, err)
	}
	frame := decodeAgentTest(t, stdout)
	if frame.Meta.Page == nil || frame.Meta.Page.NextCursor == nil || *frame.Meta.Page.NextCursor == "" || frame.Meta.Limit != 20 {
		t.Fatal(stdout)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout did not wake poll")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	for _, extra := range [][]string{{}, {"--cursor", "PRIVATE-CURSOR"}, {"--limit", "201"}, {"--interval", "0"}, {"--interval", "99ms"}, {"--interval", "61s"}, {"--timeout", "-1s"}, {"--timeout", "1ms"}, {"--timeout", "25h"}, {"--chat", "123@s.whatsapp.net"}} {
		stdout, stderr, err = runAgentTest(t, append([]string{"--agent", "--store", missing, "changes", "watch"}, extra...)...)
		if err == nil || stdout != "" || strings.Contains(stderr, "PRIVATE-CURSOR") {
			t.Fatalf("%v %s %s %v", extra, stdout, stderr, err)
		}
		if _, err = os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("created missing store: %v", err)
		}
	}
}

// The helper runs production main/execute, including output signal policy.
// A supplied freshly built binary exercises the identical fixture externally.
func TestChangesWatchProcessHelper(t *testing.T) {
	raw := os.Getenv("WACLI_CHANGES_WATCH_TEST_ARGS")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"wacli"}, args...)
	main()
	os.Exit(0)
}

func changesWatchProcess(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	t.Cleanup(cancel)
	binary := os.Getenv("WACLI_CHANGES_E2E_BINARY")
	var cmd *exec.Cmd
	if binary != "" {
		cmd = exec.CommandContext(ctx, binary, args...)
	} else {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestChangesWatchProcessHelper$")
		cmd.Env = append(os.Environ(), "WACLI_CHANGES_WATCH_TEST_ARGS="+string(raw))
	}
	return cmd
}

func TestChangesWatchCLIStreamsLiveWALAndInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt subprocess signal unsupported on Windows")
	}
	dir, writer, _ := changesWatchFixture(t)
	for _, mode := range []string{"default", "json", "agent"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"--store", dir, "--read-only", "--timeout", "0", "changes", "watch", "--interval", "100ms", "--limit", "1"}
			if mode != "default" {
				args = append(args, "--"+mode)
			}
			cmd := changesWatchProcess(t, args...)
			cmd.Env = append(cmd.Environ(), "WACLI_READONLY=1")
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(pipe)
			read := func() store.ChangesPage {
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err != nil {
					t.Fatalf("frame: %v", err)
				}
				var page store.ChangesPage
				if mode == "agent" {
					e := decodeAgentTest(t, string(raw))
					if e.Account.StoreRef == nil || *e.Account.StoreRef != dir || e.Meta.Source != "local" || e.Meta.Completeness != "unknown" || e.Meta.Freshness != "unknown" || e.Meta.Page == nil || e.Meta.Page.NextCursor == nil {
						t.Fatal(string(raw))
					}
					if err := json.Unmarshal(e.Data, &page); err != nil {
						t.Fatal(err)
					}
					page.NextCursor = *e.Meta.Page.NextCursor
					page.HasMore = e.Meta.Page.HasMore
					if len(page.Changes) != e.Meta.Page.Returned {
						t.Fatal("incorrect returned")
					}
				} else {
					var e struct {
						Success bool
						Data    store.ChangesPage
					}
					if err := json.Unmarshal(raw, &e); err != nil || !e.Success {
						t.Fatalf("frame %s %v", raw, err)
					}
					page = e.Data
				}
				if page.NextCursor == "" || len(page.Changes) > 1 {
					t.Fatal("invalid frame")
				}
				return page
			}
			p := read()
			for p.HasMore {
				p = read()
			}
			if err = writer.RecordChangeReceipts(t.Context(), "123@s.whatsapp.net", []string{mode}, false, store.ChangeReceipt{Type: "read", ActorJID: "123@s.whatsapp.net", EventAt: time.Unix(102, 0)}); err != nil {
				t.Fatal(err)
			}
			p = read()
			if len(p.Changes) != 1 || p.Changes[0].Kind != "receipt" || p.Changes[0].ID != mode {
				t.Fatalf("missed WAL commit %+v", p)
			}
			if err = cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			var trailing json.RawMessage
			if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected final frame %s %v", trailing, err)
			}
			if err = cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 1 {
				t.Fatalf("exit=%v", err)
			}
			if mode == "agent" {
				if decodeAgentTest(t, stderr.String()).Error.Code != "cancelled" {
					t.Fatal(stderr.String())
				}
			} else if !json.Valid(bytes.TrimSpace(stderr.Bytes())) {
				t.Fatal(stderr.String())
			}
		})
	}
	for _, name := range []string{"session.db", "HEARTBEAT", "SYNC.sock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("reader created %s: %v", name, err)
		}
	}
}

func TestChangesWatchCLIClosedStdout(t *testing.T) {
	dir, _, _ := changesWatchFixture(t)
	for _, agent := range []bool{false, true} {
		args := []string{"--store", dir, "--timeout", "0", "changes", "watch"}
		if agent {
			args = append(args, "--agent")
		}
		cmd := changesWatchProcess(t, args...)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		defer w.Close()
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err = cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 1 {
			t.Fatalf("output failure lost: %v %s", err, stderr.String())
		}
		if agent {
			e := decodeAgentTest(t, stderr.String())
			if e.Error.Code != "output_failed" || e.Meta.Page != nil {
				t.Fatal(stderr.String())
			}
		} else if !strings.Contains(stderr.String(), "Change frame output failed") {
			t.Fatal(stderr.String())
		}
	}
}

func TestChangesWatchCLIBlockedStdoutCancels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows inherited stdout handles do not support write deadlines")
	}
	dir, writer, _ := changesWatchFixture(t)
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = strings.Repeat("x", 400) + time.Unix(int64(i), 0).Format(time.RFC3339)
	}
	if err := writer.RecordChangeReceipts(t.Context(), "123@s.whatsapp.net", ids, false, store.ChangeReceipt{Type: "read", ActorJID: "123@s.whatsapp.net", EventAt: time.Unix(102, 0)}); err != nil {
		t.Fatal(err)
	}
	for _, interrupt := range []bool{false, true} {
		timeout := "350ms"
		expected := "timeout"
		if interrupt {
			timeout = "0"
			expected = "cancelled"
		}
		cmd := changesWatchProcess(t, "--store", dir, "--agent", "--timeout", timeout, "changes", "watch", "--limit", "200")
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var prefix []byte
		if interrupt {
			prefix = make([]byte, 4096)
			n, err := r.Read(prefix)
			if err != nil {
				t.Fatal(err)
			}
			prefix = prefix[:n]
			if err = cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		if err = cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 1 || time.Since(started) > 2*time.Second {
			t.Fatalf("blocked output did not cancel: %v stderr=%s", err, stderr.String())
		}
		e := decodeAgentTest(t, stderr.String())
		if e.Error.Code != expected || e.Meta.Page != nil {
			t.Fatal(stderr.String())
		}
		w.Close()
		tail, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		partial := append(prefix, tail...)
		if len(partial) == 0 || bytes.Contains(partial, []byte("\n")) || json.Valid(partial) {
			t.Fatalf("expected partial frame, bytes=%d", len(partial))
		}
	}
}

func TestChangesWatchCLICompatibilityScopeAndHelp(t *testing.T) {
	dir, writer, _ := changesWatchFixture(t)
	insertWatchMessage(t, writer, "123@s.whatsapp.net", "one")
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--read-only", "--agent", "changes", "list")
	if err != nil || stderr != "" {
		t.Fatalf("list: %v %s", err, stderr)
	}
	cursor := *decodeAgentTest(t, stdout).Meta.Page.NextCursor
	other, _, _ := changesWatchFixture(t)
	stdout, stderr, err = runAgentTest(t, "--store", other, "--agent", "changes", "watch", "--cursor", cursor)
	if stdout != "" || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_cursor" {
		t.Fatalf("cross-store: %v %s %s", err, stdout, stderr)
	}
	// A list cursor is accepted unchanged by the real watch command at the same selection.
	stdout, stderr, err = runAgentTest(t, "--store", dir, "--agent", "changes", "watch", "--cursor", cursor, "--timeout", "100ms")
	if strings.Count(stdout, "\n") != 1 || decodeAgentTest(t, stdout).Meta.Page.NextCursor == nil || *decodeAgentTest(t, stdout).Meta.Page.NextCursor != cursor || decodeAgentTest(t, stderr).Error.Code != "timeout" {
		t.Fatalf("list/watch resume: %v %s %s", err, stdout, stderr)
	}
	stdout, stderr, err = runAgentTest(t, "--store", other, "--agent", "changes", "watch", "--help")
	if err != nil || stderr != "" || !strings.Contains(stdout, "NDJSON") || !strings.Contains(stdout, "--interval") || !strings.Contains(stdout, "100ms..24h") {
		t.Fatalf("help: %v %s %s", err, stdout, stderr)
	}
	for _, statement := range []string{"DROP TRIGGER changes_message_update", "UPDATE schema_migrations SET version=34 WHERE version=33"} {
		fixture, _, _ := changesWatchFixture(t)
		sqlDB, err := sql.Open("sqlite3", filepath.Join(fixture, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sqlDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
		sqlDB.Close()
		before, err := os.ReadFile(filepath.Join(fixture, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err = runAgentTest(t, "--read-only", "--store", fixture, "--agent", "changes", "watch")
		if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" || strings.Contains(stderr, statement) {
			t.Fatalf("incompatible: %v %s %s", err, stdout, stderr)
		}
		after, err := os.ReadFile(filepath.Join(fixture, "wacli.db"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("reader changed incompatible database")
		}
	}
}
