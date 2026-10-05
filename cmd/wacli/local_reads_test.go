package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

const localReadPN = "15550000001@s.whatsapp.net"
const localReadLID = "100000000001@lid"

// These invoke the real CLI without --read-only, including every local command
// family. Live channel commands remain remote; channel archives use chats/messages.
func localReadCommands(dir string) [][]string {
	return [][]string{
		{"messages", "list", "--chat", localReadPN},
		{"messages", "search", "fixture", "--chat", localReadPN},
		{"messages", "starred"},
		{"messages", "show", "--chat", localReadPN, "--id", "m1"},
		{"messages", "context", "--chat", localReadPN, "--id", "m1"},
		{"messages", "export", "--chat", localReadPN},
		{"chats", "list"},
		{"chats", "show", "--jid", localReadPN},
		{"contacts", "list"},
		{"contacts", "search", "Fixture Alias"},
		{"contacts", "show", "--jid", localReadLID},
		{"contacts", "resolve", localReadLID},
		{"contacts", "import-system", "--dry-run", "--clear"},
		{"contacts", "import-system", "--dry-run", "--input", filepath.Join(dir, "contacts-input.json")},
		{"groups", "list"},
		{"groups", "participants", "list", "--jid", "123@g.us"},
		{"polls", "list", "--chat", localReadPN},
		{"poll", "show", "--to", localReadPN, "--id", "p1"},
		{"calls", "list", "--chat", localReadPN},
		{"history", "coverage", "--include-blocked"},
		{"history", "fill", "--dry-run"},
		{"store", "stats"},
	}
}

func seedLocalReadStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, chat := range []struct{ jid, kind string }{{localReadLID, "dm"}, {"123@g.us", "group"}, {"456@newsletter", "newsletter"}} {
		if err := db.UpsertChat(chat.jid, chat.kind, "Fixture Chat", ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertContact(localReadPN, "15550000001", "Fixture Contact", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAlias([]string{localReadPN}, "Fixture Alias"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "m1", SenderJID: localReadLID, Text: "fixture message", Timestamp: ts}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPoll(store.Poll{ChatJID: localReadLID, MsgID: "p1", Question: "Fixture poll?", Options: []string{"yes", "no"}, SelectableCount: 1, CreatedAt: ts}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Deliberately incomplete/old session schema: a writable whatsmeow opener
	// would upgrade it. Only its persisted identity map is needed for these reads.
	sessionPath := filepath.Join(dir, "session.db")
	writeTestSessionLIDMap(t, sessionPath, "100000000001", "15550000001")
	session, err := sql.Open("sqlite3", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec("CREATE TABLE whatsmeow_device (jid TEXT, lid TEXT); INSERT INTO whatsmeow_device VALUES ('15550000009:4@s.whatsapp.net', '100000000009@lid')"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contacts-input.json"), []byte(`[{"full_name":"Fixture System Name","phones":["15550000001"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runLocalRead(t *testing.T, dir string, args []string) (string, error) {
	t.Helper()
	var err error
	var stdout string
	captureRootStderr(t, func() {
		stdout = captureRootStdout(t, func() {
			err = execute(append([]string{"--store", dir, "--json"}, args...))
		})
	})
	return stdout, err
}

type localFileSnapshot struct {
	Mode fs.FileMode
	Hash [32]byte
}

func snapshotLocalStore(t *testing.T, dir string) map[string]localFileSnapshot {
	t.Helper()
	files := map[string]localFileSnapshot{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Normal readonly SQLite may create SHM and empty WAL bookkeeping even
		// before a writer appears. Retain nonempty WAL bytes to catch SQL effects.
		for _, db := range []string{"wacli.db", "session.db"} {
			if path == filepath.Join(dir, db+"-shm") || (path == filepath.Join(dir, db+"-wal") && info.Size() == 0) {
				return nil
			}
		}
		state := localFileSnapshot{Mode: info.Mode()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state.Hash = sha256.Sum256(data)
		}
		files[path] = state
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestLocalReadsDefaultPreserveStoreAndSession(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	for _, path := range []string{dir, filepath.Join(dir, "wacli.db"), filepath.Join(dir, "session.db")} {
		mode := fs.FileMode(0o644)
		if path == dir {
			mode = 0o755
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotLocalStore(t, dir)
	commands := append(localReadCommands(dir), []string{"doctor"}, []string{"auth", "status"})
	for _, args := range commands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, err := runLocalRead(t, dir, args)
			if err != nil {
				t.Fatal(err)
			}
			if args[0] == "contacts" && args[1] == "import-system" && args[3] == "--input" {
				var preview struct {
					Data struct {
						Matched int  `json:"matched"`
						DryRun  bool `json:"dry_run"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(stdout), &preview); err != nil || preview.Data.Matched != 1 || !preview.Data.DryRun {
					t.Fatalf("input fixture preview must match a contact without applying: %v %s", err, stdout)
				}
			}
			var envelope struct {
				Success bool            `json:"success"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || !envelope.Success || len(envelope.Data) == 0 {
				t.Fatalf("invalid legacy JSON envelope: %v %s", err, stdout)
			}
			if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
				t.Fatalf("local read changed files, bytes or permissions: before=%v after=%v", before, got)
			}
		})
	}
}

func TestLocalReadsDefaultDoNotInitializeMissingStore(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := filepath.Join(t.TempDir(), "missing")
	for _, args := range localReadCommands(dir) {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runLocalRead(t, dir, args)
			if err == nil || !strings.Contains(err.Error(), "initialize explicitly") {
				t.Fatalf("want actionable missing-store error, got %v", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("read created store directory: %v", err)
			}
		})
	}
	for _, args := range [][]string{{"doctor"}, {"auth", "status"}} {
		stdout, err := runLocalRead(t, dir, args)
		if err != nil {
			t.Fatal(err)
		}
		if args[0] == "doctor" && !strings.Contains(stdout, "store_error") {
			t.Fatalf("doctor must report the missing store: %s", stdout)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("status created store directory: %v", err)
		}
	}
}

func TestLocalReadsDefaultWorkWithWriterLockAndLiveWAL(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedLocalReadStore(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "live-wal", Text: "fixture WAL", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, args := range append(localReadCommands(dir), []string{"doctor"}, []string{"auth", "status"}) {
		stdout, err := runLocalRead(t, dir, args)
		if err != nil {
			t.Fatalf("%v with writer lock: %v", args, err)
		}
		if args[0] == "messages" && args[1] == "list" && !strings.Contains(stdout, "live-wal") {
			t.Fatalf("reader missed live WAL row: %s", stdout)
		}
		if args[0] == "doctor" && !strings.Contains(stdout, `"lock_held":true`) {
			t.Fatalf("doctor missed held writer lock: %s", stdout)
		}
	}
	// SQLite may touch WAL/shared-memory bookkeeping while a writer is active.
	// The archive/session, permissions, and the writer's LOCK must stay unchanged.
	after := snapshotLocalStore(t, dir)
	for _, files := range []map[string]localFileSnapshot{before, after} {
		for path := range files {
			if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
				delete(files, path)
			}
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("local reads changed store/session/lock beyond SQLite WAL bookkeeping")
	}
}

func TestLocalReadsDefaultResolvePersistedIdentityAndAlias(t *testing.T) {
	dir := seedLocalReadStore(t)
	for _, args := range [][]string{
		{"messages", "list", "--chat", localReadPN},
		{"messages", "show", "--chat", localReadPN, "--id", "m1"},
		{"contacts", "show", "--jid", localReadLID},
		{"contacts", "search", "Fixture Alias"},
	} {
		stdout, err := runLocalRead(t, dir, args)
		if err != nil || !strings.Contains(stdout, "Fixture Alias") {
			t.Fatalf("persisted alias/identity lost for %v: %v %s", args, err, stdout)
		}
	}
	stdout, err := runLocalRead(t, dir, []string{"chats", "show", "--jid", localReadPN})
	if err != nil || !strings.Contains(stdout, localReadPN) {
		t.Fatalf("persisted identity lost in chat display: %v %s", err, stdout)
	}
}

func TestLocalReadsDefaultRejectPendingMigrationWithoutChangingStore(t *testing.T) {
	dir := seedLocalReadStore(t)
	path := filepath.Join(dir, "wacli.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	// Migration 27 repairs chat activity, and 28 creates a table. Leave both
	// pending to exercise data and schema migration risks through the real CLI.
	if _, err := raw.Exec(`
		DELETE FROM schema_migrations WHERE version >= 27;
		DROP TABLE unavailable_app_state_keys;
		UPDATE messages SET text = '', display_text = '(message)', ts = 100;
		UPDATE chats SET last_message_ts = 100 WHERE jid = '100000000001@lid';
	`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, args := range localReadCommands(dir) {
		_, err := runLocalRead(t, dir, args)
		if err == nil || !strings.Contains(err.Error(), "explicit writable upgrade") {
			t.Fatalf("%v: want upgrade error, got %v", args, err)
		}
	}
	stdout, err := runLocalRead(t, dir, []string{"doctor"})
	if err != nil || !strings.Contains(stdout, "explicit writable upgrade") {
		t.Fatalf("doctor should report pending migration: %v %s", err, stdout)
	}
	if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
		t.Fatal("local queries migrated the old store or session")
	}
	// The normal writable boundary still upgrades the fixture explicitly.
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := db.GetChat(localReadLID)
	if err != nil || !chat.LastMessageTS.IsZero() {
		t.Fatalf("explicit migration should repair placeholder activity: %v %+v", err, chat)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runLocalRead(t, dir, []string{"store", "stats"}); err != nil {
		t.Fatalf("read after explicit upgrade: %v", err)
	}
}

func TestLocalReadChangesPreserveExplicitReadOnlyMutationBarrier(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	for _, args := range [][]string{
		{"contacts", "alias", "set", "--jid", localReadPN, "--alias", "Blocked"},
		{"groups", "refresh"},
		{"channels", "list"},
		{"channels", "info", "--jid", "456@newsletter"},
		{"history", "backfill", "--chat", localReadPN},
		{"doctor", "--connect"},
	} {
		_, err := runLocalRead(t, dir, append([]string{"--read-only"}, args...))
		if err == nil || !strings.Contains(err.Error(), "read-only mode") {
			t.Fatalf("%v: want read-only rejection, got %v", args, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("blocked mutation created store: %v", err)
	}
}

func TestLocalReadsDefaultRejectUnsupportedSchemasAndKeepDoctorDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
		want   string
	}{
		{"newer", "INSERT INTO schema_migrations(version, name, applied_at) SELECT MAX(version) + 1, 'future migration', 1 FROM schema_migrations", "newer than supported"},
		{"unversioned", "DROP TABLE schema_migrations", "explicit writable upgrade"},
		{"missing earlier migration", "DELETE FROM schema_migrations WHERE version = 27", "explicit writable upgrade"},
		{"zero replacing migration", "UPDATE schema_migrations SET version = 0 WHERE version = 27", "unknown migration versions"},
		{"negative replacing migration", "UPDATE schema_migrations SET version = -1 WHERE version = 1", "unknown migration versions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedLocalReadStore(t)
			raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotLocalStore(t, dir)
			for _, args := range localReadCommands(dir) {
				if _, err := runLocalRead(t, dir, args); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%v: want compatibility error, got %v", args, err)
				}
			}
			stdout, err := runLocalRead(t, dir, []string{"doctor"})
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Success bool         `json:"success"`
				Data    doctorReport `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Success || !strings.Contains(report.Data.StoreError, tc.want) || !report.Data.Authed || report.Data.LinkedJID != "15550000009@s.whatsapp.net" || report.Data.Connected {
				t.Fatalf("doctor must retain offline auth diagnostic while reporting schema error: %s", stdout)
			}
			if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
				t.Fatal("unsupported-schema read changed store/session")
			}
		})
	}
}
