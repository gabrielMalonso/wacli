package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

const maintenanceOldChat = "old-dm@s.whatsapp.net"
const maintenanceRecentChat = "recent-dm@s.whatsapp.net"

type maintenanceCase struct {
	name     string
	args     []string
	targets  []string
	messages int
}

func maintenanceCases() []maintenanceCase {
	return []maintenanceCase{
		{"chats default age", []string{"chats", "cleanup"}, []string{maintenanceOldChat}, 2},
		{"chats custom age", []string{"chats", "cleanup", "--days", "180"}, []string{maintenanceOldChat, "old-active@g.us", "old-left@g.us"}, 2},
		{"chats specific JID", []string{"chats", "cleanup", "--jid", maintenanceRecentChat}, []string{maintenanceRecentChat}, 1},
		{"groups default left only", []string{"groups", "prune"}, []string{"old-left@g.us", "recent-left@g.us"}, 0},
		{"groups left age", []string{"groups", "prune", "--days", "180"}, []string{"old-left@g.us"}, 0},
		{"groups include active", []string{"groups", "prune", "--days", "180", "--include-active"}, []string{"old-active@g.us", "old-left@g.us"}, 0},
		{"groups left-only false", []string{"groups", "prune", "--days", "180", "--left-only=false"}, []string{"old-active@g.us", "old-left@g.us"}, 0},
		{"store default age", []string{"store", "cleanup"}, []string{maintenanceOldChat}, 2},
		{"store custom age", []string{"store", "cleanup", "--days", "180"}, []string{maintenanceOldChat, "old-active@g.us", "old-left@g.us"}, 2},
		{"message payload", []string{"messages", "purge", "--chat", maintenanceOldChat, "--id", "deleted"}, nil, 1},
	}
}

func seedMaintenanceStore(t *testing.T) string {
	t.Helper()
	dir := seedPruneStore(t)
	db := openPruneStore(t, dir)
	defer db.Close()
	now := time.Now().UTC()
	for _, chat := range []struct {
		jid string
		ts  time.Time
	}{
		{maintenanceOldChat, now.AddDate(0, 0, -400)},
		{maintenanceRecentChat, now.AddDate(0, 0, -1)},
		{"unknown-dm@s.whatsapp.net", time.Time{}},
	} {
		if err := db.UpsertChat(chat.jid, "dm", "Synthetic maintenance fixture", chat.ts); err != nil {
			t.Fatal(err)
		}
		if chat.ts.IsZero() {
			continue
		}
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat.jid, MsgID: "live", Text: "synthetic live payload", Timestamp: chat.ts}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: maintenanceOldChat, MsgID: "deleted", Text: "synthetic retained payload", MediaType: "image", Timestamp: now.AddDate(0, 0, -400)}); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(dir, "retained-media.jpg")
	if err := os.WriteFile(mediaPath, []byte("synthetic retained media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMediaDownloaded(maintenanceOldChat, "deleted", mediaPath, now); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageDeletedForMePreserveMedia(maintenanceOldChat, "deleted"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkMaintenancePreview(t *testing.T, tc maintenanceCase, stdout string) {
	t.Helper()
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			WouldDelete         int           `json:"would_delete"`
			WouldDeleteChats    int           `json:"would_delete_chats"`
			WouldDeleteMessages int           `json:"would_delete_messages"`
			WouldPurge          int           `json:"would_purge"`
			MessageCount        int           `json:"message_count"`
			Chats               []store.Chat  `json:"chats"`
			Chat                store.Chat    `json:"chat"`
			Groups              []store.Group `json:"groups"`
			Message             store.Message `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || !result.Success {
		t.Fatalf("invalid preview: %v %s", err, stdout)
	}
	var targets []string
	switch tc.args[0] {
	case "chats":
		if result.Data.WouldDelete != len(tc.targets) {
			t.Fatalf("wrong candidate count: %s", stdout)
		}
		for _, chat := range result.Data.Chats {
			targets = append(targets, chat.JID)
		}
		if result.Data.Chat.JID != "" {
			targets = append(targets, result.Data.Chat.JID)
			if result.Data.MessageCount != tc.messages {
				t.Fatalf("wrong specific-chat message count: %s", stdout)
			}
		}
	case "groups":
		if result.Data.WouldDelete != len(tc.targets) {
			t.Fatalf("wrong group count: %s", stdout)
		}
		for _, group := range result.Data.Groups {
			targets = append(targets, group.JID)
		}
	case "store":
		if result.Data.WouldDeleteChats != len(tc.targets) || result.Data.WouldDeleteMessages != tc.messages {
			t.Fatalf("wrong store counts: %s", stdout)
		}
		return
	case "messages":
		msg := result.Data.Message
		if result.Data.WouldPurge != 1 || msg.ChatJID != maintenanceOldChat || msg.MsgID != "deleted" || msg.Text != "synthetic retained payload" || msg.DeletedAt == nil || msg.PayloadPurgedAt != nil || msg.LocalPath == "" {
			t.Fatalf("preview lost retained payload: %s", stdout)
		}
		return
	}
	slices.Sort(targets)
	want := slices.Clone(tc.targets)
	slices.Sort(want)
	if !slices.Equal(targets, want) {
		t.Fatalf("targets = %v, want %v", targets, tc.targets)
	}
}

func TestMaintenancePreviewsPreserveFilesAndPermissions(t *testing.T) {
	for _, existingLock := range []bool{false, true} {
		dir := seedMaintenanceStore(t)
		// A deliberately old synthetic session must never be initialized/upgraded.
		writeTestSessionLIDMap(t, filepath.Join(dir, "session.db"), "100000000001", "15550000001")
		if existingLock {
			if err := os.WriteFile(filepath.Join(dir, "LOCK"), []byte("synthetic idle lock"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{dir, filepath.Join(dir, "wacli.db"), filepath.Join(dir, "session.db")} {
			mode := os.FileMode(0o644)
			if path == dir {
				mode = 0o755
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		before := snapshotLocalStore(t, dir)
		for _, mode := range []string{"default", "flag", "env"} {
			t.Setenv("WACLI_READONLY", "0")
			if mode == "env" {
				t.Setenv("WACLI_READONLY", "1")
			}
			for _, tc := range maintenanceCases() {
				t.Run(tc.name+"/"+mode, func(t *testing.T) {
					args := append(slices.Clone(tc.args), "--dry-run", "--confirm")
					if mode == "flag" {
						args = append(args, "--read-only")
					}
					stdout, err := runLocalRead(t, dir, args)
					if err != nil {
						t.Fatal(err)
					}
					checkMaintenancePreview(t, tc, stdout)
					if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
						t.Fatal("preview changed data/schema/LOCK/session/media/permissions or created files")
					}
				})
			}
		}
	}
}

func TestMaintenancePreviewsWorkWithWriterLockAndWAL(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedMaintenanceStore(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	db := openPruneStore(t, dir)
	defer db.Close()
	// Change candidate selection via committed WAL data while the writer is open.
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: maintenanceOldChat, MsgID: "wal-live", Text: "synthetic WAL row", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, tc := range maintenanceCases() {
		if _, err := runLocalRead(t, dir, append(slices.Clone(tc.args), "--confirm")); !lock.IsLocked(err) {
			t.Fatalf("execution must still require the writer lock: %v", err)
		}
		args := append(slices.Clone(tc.args), "--dry-run", "--read-only")
		stdout, err := runLocalRead(t, dir, args)
		if err != nil {
			t.Fatalf("%v with writer lock: %v", args, err)
		}
		if tc.args[0] == "chats" || tc.args[0] == "store" {
			if tc.name != "chats specific JID" {
				if len(tc.targets) == 1 {
					if !strings.Contains(stdout, `"deleted":0`) {
						t.Fatalf("preview missed WAL activity protecting the old chat: %s", stdout)
					}
					continue
				}
				tc.targets = []string{"old-active@g.us", "old-left@g.us"}
				tc.messages = 0
			}
		}
		checkMaintenancePreview(t, tc, stdout)
	}
	after := snapshotLocalStore(t, dir)
	// SQLite can update shared-memory read marks. Preserve its permissions and
	// file set, and require unchanged DB/WAL bytes (no writer runs during previews).
	for path, state := range before {
		if strings.HasSuffix(path, "-shm") {
			state.Hash = [32]byte{}
			before[path] = state
			state = after[path]
			state.Hash = [32]byte{}
			after[path] = state
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("preview changed database/WAL/LOCK/media or file permissions")
	}
}

func TestMaintenancePreviewsPreserveStoreSelectionAndPurgeValidation(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := seedMaintenanceStore(t)
	t.Setenv("WACLI_STORE_DIR", dir)
	before := snapshotLocalStore(t, dir)
	for _, tc := range maintenanceCases() {
		var err error
		var stdout string
		captureRootStderr(t, func() {
			stdout = captureRootStdout(t, func() {
				args := append([]string{"--json"}, tc.args...)
				err = execute(append(args, "--dry-run"))
			})
		})
		if err != nil {
			t.Fatalf("preview did not select WACLI_STORE_DIR: %v", err)
		}
		checkMaintenancePreview(t, tc, stdout)
		// An explicit --store still takes precedence over the environment.
		missing := filepath.Join(t.TempDir(), "missing")
		if _, err := runLocalRead(t, missing, append(slices.Clone(tc.args), "--dry-run")); err == nil || !strings.Contains(err.Error(), "initialize explicitly") {
			t.Fatalf("--store must override WACLI_STORE_DIR: %v", err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("preview initialized selected missing store: %v", err)
		}
		if _, err := runLocalRead(t, dir, append(slices.Clone(tc.args), "--dry-run", "--account", "synthetic")); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("--store/--account conflict must remain rejected: %v", err)
		}
	}
	if _, err := runLocalRead(t, dir, []string{"messages", "purge", "--chat", maintenanceOldChat, "--id", "live", "--dry-run"}); err != store.ErrMessageNotTombstoned {
		t.Fatalf("live payload must remain ineligible for purge: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("selection/validation previews changed files")
	}
}

func TestMaintenancePreviewsDoNotInitializeOrMigrate(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, missing := range []bool{true, false} {
		dir := filepath.Join(t.TempDir(), "missing")
		want := "initialize explicitly"
		if !missing {
			dir = seedMaintenanceStore(t)
			raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("DELETE FROM schema_migrations WHERE version >= 27; DROP TABLE unavailable_app_state_keys"); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			want = "explicit writable upgrade"
		}
		var before map[string]localFileSnapshot
		if !missing {
			before = snapshotLocalStore(t, dir)
		}
		for _, tc := range maintenanceCases() {
			_, err := runLocalRead(t, dir, append(slices.Clone(tc.args), "--dry-run"))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%v: want %s, got %v", tc.args, want, err)
			}
			if missing {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("preview initialized missing store: %v", err)
				}
			} else if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("preview migrated old store or changed files")
			}
		}
	}
}

func TestMaintenanceExecutionStillRejectsReadOnly(t *testing.T) {
	dir := seedMaintenanceStore(t)
	before := snapshotLocalStore(t, dir)
	for _, env := range []bool{false, true} {
		t.Setenv("WACLI_READONLY", "0")
		if env {
			t.Setenv("WACLI_READONLY", "1")
		}
		for _, tc := range maintenanceCases() {
			args := append(slices.Clone(tc.args), "--confirm")
			if !env {
				args = append(args, "--read-only")
			}
			_, err := runLocalRead(t, dir, args)
			if err == nil || !strings.Contains(err.Error(), "read-only mode") {
				t.Fatalf("%v: want read-only barrier, got %v", args, err)
			}
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("blocked mutation touched fixture")
			}
		}
	}
}

func TestMaintenanceExecutionStillMutatesOnlySelectedFixtureRows(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, tc := range maintenanceCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedMaintenanceStore(t)
			stdout, err := runLocalRead(t, dir, append(slices.Clone(tc.args), "--confirm"))
			if err != nil || !strings.Contains(stdout, `"success":true`) {
				t.Fatalf("fixture execution failed: %v %s", err, stdout)
			}
			if _, err := os.Stat(filepath.Join(dir, "LOCK")); err != nil {
				t.Fatalf("execution did not take writer path: %v", err)
			}
			db, err := store.OpenReadOnly(filepath.Join(dir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, jid := range []string{maintenanceOldChat, maintenanceRecentChat, "unknown-dm@s.whatsapp.net", "old-left@g.us", "recent-left@g.us", "old-active@g.us"} {
				_, err := db.GetChat(jid)
				if slices.Contains(tc.targets, jid) {
					if err != sql.ErrNoRows {
						t.Fatalf("selected chat %s survives: %v", jid, err)
					}
					if count, err := db.CountChatMessages(jid); err != nil || count != 0 {
						t.Fatalf("selected chat messages survive: %d %v", count, err)
					}
				} else if err != nil {
					t.Fatalf("unselected chat %s removed: %v", jid, err)
				}
			}
			if tc.args[0] == "groups" {
				groups, err := db.ListPrunableGroups(1, true)
				if err != nil || len(groups) != 3-len(tc.targets) {
					t.Fatalf("group metadata not pruned: %v %v", groups, err)
				}
			}
			if tc.args[0] == "messages" {
				msg, err := db.GetMessage(maintenanceOldChat, "deleted")
				if err != nil || msg.Text != "" || msg.PayloadPurgedAt == nil || msg.DeletedAt == nil {
					t.Fatalf("payload not purged with tombstone retained: %+v %v", msg, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "retained-media.jpg")); !os.IsNotExist(err) {
					t.Fatalf("retained fixture media not removed: %v", err)
				}
				live, err := db.GetMessage(maintenanceOldChat, "live")
				if err != nil || live.Text != "synthetic live payload" {
					t.Fatalf("purge changed live message: %+v %v", live, err)
				}
			}
		})
	}
}
