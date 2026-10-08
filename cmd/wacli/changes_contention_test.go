package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

// Real SQLite fixture, with no live store/session. Exclusive WAL locking mode
// additionally prevents opening a reader, unlike an ordinary WAL writer txn.
func changesContentionFixture(t *testing.T, journal string) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wacli.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lk.Release() })
	blocker, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blocker.Exec("ROLLBACK"); blocker.Close() })
	blocker.SetMaxOpenConns(1)
	if _, err = blocker.Exec("PRAGMA journal_mode=" + journal); err != nil {
		t.Fatal(err)
	}
	return dir, blocker
}

func holdChangesExclusive(t *testing.T, blocker *sql.DB, opening bool) {
	t.Helper()
	if opening {
		if _, err := blocker.Exec("PRAGMA locking_mode=EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := blocker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
}

func checkChangesContextExit(t *testing.T, stderr string, exit int, code string, elapsed time.Duration) {
	t.Helper()
	e := decodeAgentTest(t, stderr)
	if exit != 1 || e.Error.Code != code || e.Meta.Page != nil || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if elapsed > time.Second {
		t.Fatalf("SQLite cancellation took %s (expected <1s)", elapsed)
	}
	t.Logf("code=%s latency=%s", code, elapsed)
}

func TestChangesWatchCLIExclusiveOpenTimeout(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		t.Run(journal, func(t *testing.T) {
			dir, blocker := changesContentionFixture(t, journal)
			holdChangesExclusive(t, blocker, true)
			cmd := changesWatchProcess(t, "--store", dir, "--read-only", "--agent", "--timeout", "100ms", "changes", "watch")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			started := time.Now()
			err := cmd.Run()
			elapsed := time.Since(started)
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("unexpected success/frame: %v %s", err, stdout.String())
			}
			checkChangesContextExit(t, stderr.String(), cmd.ProcessState.ExitCode(), "timeout", elapsed)
		})
	}
}

func TestChangesWatchCLIExclusiveOpenInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt subprocess signal unsupported on Windows")
	}
	dir, blocker := changesContentionFixture(t, "WAL")
	holdChangesExclusive(t, blocker, true)
	cmd := changesWatchProcess(t, "--store", dir, "--agent", "--timeout", "0", "changes", "watch")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Allow the real process to install its signal handler and enter opening.
	<-time.After(150 * time.Millisecond)
	started := time.Now()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || stdout.Len() != 0 {
		t.Fatalf("unexpected success/frame: %v %s", err, stdout.String())
	}
	checkChangesContextExit(t, stderr.String(), cmd.ProcessState.ExitCode(), "cancelled", time.Since(started))
}

func TestChangesWatchCLIExclusivePollTimeout(t *testing.T) {
	dir, blocker := changesContentionFixture(t, "DELETE")
	cmd := changesWatchProcess(t, "--store", dir, "--agent", "--timeout", "350ms", "changes", "watch", "--interval", "100ms")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	started := time.Now()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Read exactly one NDJSON line, so any additional bytes remain observable.
	reader := bufio.NewReader(pipe)
	frame, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	e := decodeAgentTest(t, frame)
	if e.Meta.Page == nil || e.Meta.Page.NextCursor == nil || *e.Meta.Page.NextCursor == "" || e.Meta.Page.Returned != 0 {
		t.Fatal(frame)
	}
	holdChangesExclusive(t, blocker, false)
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil || len(tail) != 0 {
		t.Fatalf("unexpected checkpoint after blocked poll: %v %s", err, tail)
	}
	checkChangesContextExit(t, stderr.String(), cmd.ProcessState.ExitCode(), "timeout", time.Since(started))
}

func TestChangesWatchCLITransientContentionResumes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt subprocess signal unsupported on Windows")
	}
	for _, opening := range []bool{true, false} {
		name, journal := "poll", "DELETE"
		if opening {
			name, journal = "open", "WAL"
		}
		t.Run(name, func(t *testing.T) {
			dir, blocker := changesContentionFixture(t, journal)
			if opening {
				holdChangesExclusive(t, blocker, true)
			}
			cmd := changesWatchProcess(t, "--store", dir, "--agent", "--timeout", "0", "changes", "watch", "--interval", "100ms")
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
			readFrame := func() agentTestEnvelope {
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err != nil {
					t.Fatal(err)
				}
				return decodeAgentTest(t, string(raw))
			}
			cursor := ""
			if !opening {
				first := readFrame()
				cursor = *first.Meta.Page.NextCursor
				holdChangesExclusive(t, blocker, false)
				// Production message triggers append the event in the held transaction.
				if _, err = blocker.Exec("INSERT INTO messages(chat_jid,msg_id,ts,from_me,text) VALUES('123@s.whatsapp.net','after-contention',100,0,'fixture')"); err != nil {
					t.Fatal(err)
				}
			}
			released := make(chan error, 1)
			timer := time.AfterFunc(180*time.Millisecond, func() {
				_, err := blocker.Exec("COMMIT")
				// Exclusive WAL locking mode retains its file lock until normal close.
				if opening {
					errClose := blocker.Close()
					if err == nil {
						err = errClose
					}
				}
				released <- err
			})
			t.Cleanup(func() {
				if !timer.Stop() {
					if err := <-released; err != nil {
						t.Error(err)
					}
				}
			})
			next := readFrame()
			if next.Meta.Page == nil || next.Meta.Page.NextCursor == nil || *next.Meta.Page.NextCursor == "" {
				t.Fatal("missing resume cursor")
			}
			if !opening {
				var p store.ChangesPage
				if err = json.Unmarshal(next.Data, &p); err != nil {
					t.Fatal(err)
				}
				if *next.Meta.Page.NextCursor == cursor || len(p.Changes) != 1 || p.Changes[0].ID != "after-contention" {
					t.Fatalf("lost event/scope: %+v", p)
				}
			}
			interruptStarted := time.Now()
			if err = cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			var extra json.RawMessage
			if err = decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("extra frame %s %v", extra, err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected cancellation")
			}
			checkChangesContextExit(t, stderr.String(), cmd.ProcessState.ExitCode(), "cancelled", time.Since(interruptStarted))
		})
	}
}
