package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
)

func seedSessionConsumerFailure(t *testing.T, failure string) string {
	t.Helper()
	dir := seedLocalReadStore(t)
	path := filepath.Join(dir, "session.db")
	switch failure {
	case "corrupt":
		if err := os.WriteFile(path, []byte("synthetic invalid session source"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "stat loop":
		if runtime.GOOS == "windows" {
			t.Skip("requires unprivileged Unix symlinks")
		}
		if err := os.Rename(path, path+".fixture"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("session.db", path); err != nil {
			t.Fatal(err)
		}
	case "permission":
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires Unix permissions without root bypass")
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	case "WAL bookkeeping":
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires Unix permissions without root bypass")
		}
		for _, fixture := range []struct{ name, journal string }{{"wacli.db", "DELETE"}, {"session.db", "WAL"}} {
			db, err := sql.Open("sqlite3", filepath.Join(dir, fixture.name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("PRAGMA journal_mode=" + fixture.journal); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown fixture failure %q", failure)
	}
	return dir
}

func TestMessageFilterPropagatesSessionSourceFailures(t *testing.T) {
	t.Setenv("WACLI_READONLY", "1")
	for _, failure := range []string{"stat loop", "permission", "corrupt", "WAL bookkeeping"} {
		t.Run(failure, func(t *testing.T) {
			dir := seedSessionConsumerFailure(t, failure)
			a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			before, err := a.DB().Stats()
			if err != nil {
				t.Fatal(err)
			}
			jids, err := messageChatJIDFilter(context.Background(), a, localReadPN)
			if err == nil || errors.Is(err, os.ErrNotExist) || jids != nil {
				t.Fatalf("unreadable source became a missing mapping: %v, %v", jids, err)
			}
			if failure == "permission" && !strings.Contains(err.Error(), "open session identity source") {
				t.Fatalf("expected readonly opening error: %v", err)
			}
			if failure == "stat loop" && !strings.Contains(err.Error(), "read session identity source") {
				t.Fatalf("expected stat error: %v", err)
			}
			for _, args := range [][]string{{"messages", "list", "--chat", localReadPN}, {"messages", "search", "fixture", "--chat", localReadPN}, {"messages", "context", "--chat", localReadPN, "--id", "m1"}} {
				stdout, err := runLocalRead(t, dir, args)
				if err == nil || commandExitCode(err) != 1 || stdout != "" {
					t.Fatalf("%v returned an empty success: stdout=%s err=%v", args, stdout, err)
				}
			}
			after, err := a.DB().Stats()
			if err != nil || !reflect.DeepEqual(before, after) || a.WA() != nil {
				t.Fatal("filter changed archive or opened a WhatsApp client", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "LOCK")); !os.IsNotExist(err) {
				t.Fatal("filter created writer LOCK", err)
			}
		})
	}
}

func TestSessionConsumersTolerateRealAbsence(t *testing.T) {
	t.Setenv("WACLI_READONLY", "1")
	dir := seedLocalReadStore(t)
	path := filepath.Join(dir, "session.db")
	if err := os.Rename(path, path+".fixture"); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	jids, err := messageChatJIDFilter(context.Background(), a, localReadLID)
	if err != nil || !reflect.DeepEqual(jids, []string{localReadLID}) || a.WA() != nil {
		t.Fatalf("missing session must retain literal chat: %v, %v", jids, err)
	}
	stdout, err := runLocalRead(t, dir, []string{"messages", "list", "--chat", localReadLID})
	var listed struct {
		Data struct {
			Messages []store.Message `json:"messages"`
		} `json:"data"`
	}
	if err != nil {
		t.Fatalf("archive-only list failed: %s, %v", stdout, err)
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil || len(listed.Data.Messages) != 1 || listed.Data.Messages[0].MsgID != "m1" || listed.Data.Messages[0].ChatJID != localReadLID {
		t.Fatalf("archive-only list changed: %s, %v", stdout, err)
	}
	stdout, err = runLocalRead(t, dir, []string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Data doctorReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Data.Authed || report.Data.LinkedJID != "" || report.Data.StoreError != "" || report.Data.Connected {
		t.Fatalf("missing session should be a normal unauthenticated diagnostic: %s, %v", stdout, err)
	}
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--read-only", "--agent", "doctor")
	if err != nil || stderr != "" || !decodeAgentTest(t, stdout).Success {
		t.Fatalf("agent doctor failed on absent session: %s, %s, %v", stdout, stderr, err)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("archive-only consumers changed files/schema/permissions or created session/LOCK")
	}
}

func TestDoctorOfflineReportsAuthSourceFailuresAndAgentSanitizes(t *testing.T) {
	t.Setenv("WACLI_READONLY", "1")
	for _, failure := range []string{"stat loop", "permission", "corrupt", "WAL bookkeeping"} {
		t.Run(failure, func(t *testing.T) {
			dir := seedSessionConsumerFailure(t, failure)
			archive := filepath.Join(dir, "wacli.db")
			before, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := runLocalRead(t, dir, []string{"doctor"})
			if commandExitCode(err) != 0 {
				t.Fatalf("legacy diagnostic exit changed: %s, %v", stdout, err)
			}
			var report struct {
				Data doctorReport `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(report.Data.StoreError, "read authentication source") || report.Data.Authed || report.Data.LinkedJID != "" || report.Data.Connected || report.Data.Store == nil || report.Data.Store.Messages != 1 {
				t.Fatalf("auth-source failure was hidden or discarded archive stats: %s", stdout)
			}
			for _, command := range [][]string{{"doctor"}, {"messages", "list", "--chat", localReadPN}} {
				stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir, "--read-only", "--agent"}, command...)...)
				if stdout != "" || commandExitCode(err) != 4 {
					t.Fatalf("agent source failure returned data: %v %s %s %v", command, stdout, stderr, err)
				}
				env := decodeAgentTest(t, stderr)
				if env.Error.Code != "store_unavailable" || strings.Contains(env.Error.Message, dir) || strings.Contains(stderr, "session.db") || strings.Contains(stderr, "synthetic invalid") || strings.Contains(stderr, "too many levels") {
					t.Fatalf("unsanitized source failure: %+v", env.Error)
				}
				if command[0] == "doctor" && env.Error.Message != "Selected local session state cannot be read." {
					t.Fatalf("doctor error contract changed: %+v", env.Error)
				}
				if command[0] == "messages" && env.Error.Message != "Selected local identity state cannot be read." {
					t.Fatalf("message identity error contract changed: %+v", env.Error)
				}
			}
			after, err := os.ReadFile(archive)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("offline error handling changed archive bytes", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "LOCK")); !os.IsNotExist(err) {
				t.Fatal("offline consumers created writer LOCK", err)
			}
			if failure == "permission" {
				info, err := os.Stat(filepath.Join(dir, "session.db"))
				if err != nil || info.Mode().Perm() != 0 {
					t.Fatal("offline consumers changed source permissions", err)
				}
			}
		})
	}
}

func TestDoctorOfflineKeepsKnownAuthWhenArchiveFails(t *testing.T) {
	t.Setenv("WACLI_READONLY", "1")
	for _, brokenSession := range []bool{false, true} {
		t.Run(fmt.Sprint("broken-session=", brokenSession), func(t *testing.T) {
			dir := seedLocalReadStore(t)
			if err := os.WriteFile(filepath.Join(dir, "wacli.db"), []byte("synthetic unreadable archive"), 0o600); err != nil {
				t.Fatal(err)
			}
			if brokenSession {
				if err := os.WriteFile(filepath.Join(dir, "session.db"), []byte("synthetic unreadable session"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotLocalStore(t, dir)
			stdout, err := runLocalRead(t, dir, []string{"doctor"})
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Data doctorReport `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Data.StoreError == "" || report.Data.Authed == brokenSession || report.Data.Connected {
				t.Fatalf("auth observation or diagnostic lost: %s, %v", stdout, err)
			}
			if brokenSession {
				if report.Data.LinkedJID != "" || !strings.Contains(report.Data.StoreError, "read authentication source") || !strings.Contains(report.Data.StoreError, "open read-only sqlite") {
					t.Fatalf("combined failures lost or inferred auth: %s", stdout)
				}
			} else if report.Data.LinkedJID != "15550000009@s.whatsapp.net" {
				t.Fatalf("known auth JID was lost: %s", stdout)
			}
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("offline diagnostic modified failed archive or auth source")
			}
		})
	}
}
