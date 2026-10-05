package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestCountCleanupPreviewRetainsZeroAndCountsOnce(t *testing.T) {
	chats := []store.Chat{{JID: "first"}, {JID: "empty"}, {JID: "last"}}
	var calls []string
	counts, total, err := countCleanupPreview(chats, func(jid string) (int64, error) {
		calls = append(calls, jid)
		return map[string]int64{"first": 2, "empty": 0, "last": 3}[jid], nil
	})
	if err != nil || total != 5 || !reflect.DeepEqual(counts, []int64{2, 0, 3}) || !reflect.DeepEqual(calls, []string{"first", "empty", "last"}) {
		t.Fatalf("counts=%v total=%d calls=%v err=%v", counts, total, calls, err)
	}
}

func TestCountCleanupPreviewPropagatesErrorWithoutPartialCounts(t *testing.T) {
	wantErr := errors.New("synthetic counting failure")
	var calls int
	counts, total, err := countCleanupPreview([]store.Chat{{JID: "first"}, {JID: "broken"}, {JID: "unreached"}}, func(jid string) (int64, error) {
		calls++
		if jid == "broken" {
			return 0, wantErr
		}
		return 2, nil
	})
	if !errors.Is(err, wantErr) || counts != nil || total != 0 || calls != 2 {
		t.Fatalf("counts=%v total=%d calls=%d err=%v", counts, total, calls, err)
	}
}

func TestStoreCleanupPreviewRetainsTombstonesZeroChatsAndReadonlyEffects(t *testing.T) {
	t.Setenv("WACLI_READONLY", "1")
	dir := seedMaintenanceStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	const emptyJID = "empty@s.whatsapp.net"
	if err := db.UpsertChat(emptyJID, "dm", "", time.Now().AddDate(0, 0, -450)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, asJSON := range []bool{false, true} {
		var stdout string
		stderr := captureRootStderr(t, func() {
			stdout = captureRootStdout(t, func() {
				args := []string{"--store", dir, "--read-only", "store", "cleanup", "--dry-run", "--confirm"}
				if asJSON {
					args = append(args, "--json")
				}
				err = execute(args)
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var result struct {
				Data struct {
					Chats    int `json:"would_delete_chats"`
					Messages int `json:"would_delete_messages"`
					Days     int `json:"days"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil || result.Data.Chats != 2 || result.Data.Messages != 2 || result.Data.Days != 365 || stderr != "" {
				t.Fatalf("JSON changed: %s stderr=%s err=%v", stdout, stderr, err)
			}
		} else {
			want := "Would delete 2 chat(s) with 2 total message(s) (older than 365 days):\n" +
				"  - " + emptyJID + " (" + emptyJID + ", 0 messages)\n" +
				"  - Synthetic maintenance fixture (" + maintenanceOldChat + ", 2 messages)\n" +
				"\nRun without --dry-run to actually delete.\n"
			if stdout != "" || stderr != want {
				t.Fatalf("text changed: stdout=%s stderr=%s", stdout, stderr)
			}
		}
		if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
			t.Fatal("readonly preview changed rows, schema, media, permissions or created a DB/LOCK")
		}
	}
}
