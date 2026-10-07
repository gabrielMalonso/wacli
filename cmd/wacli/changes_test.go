package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func TestChangesCLIReadOnlyPageResumeUnderWriterLock(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	query := func(agent bool, token string, limit string) store.ChangesPage {
		t.Helper()
		args := []string{"--store", dir, "--read-only", "--json", "changes", "list", "--limit", limit}
		if agent {
			args = append(args, "--agent")
		}
		if token != "" {
			args = append(args, "--cursor", token)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		var page store.ChangesPage
		if agent {
			e := decodeAgentTest(t, stdout)
			if !e.Success || e.Meta.Source != "local" || e.Meta.Completeness != "unknown" || e.Meta.Page == nil || e.Meta.Page.NextCursor == nil {
				t.Fatalf("envelope %+v", e)
			}
			if err = json.Unmarshal(e.Data, &page); err != nil {
				t.Fatal(err)
			}
			page.NextCursor = *e.Meta.Page.NextCursor
			page.HasMore = e.Meta.Page.HasMore
		} else {
			var e struct {
				Success bool              `json:"success"`
				Data    store.ChangesPage `json:"data"`
			}
			if err = json.Unmarshal([]byte(stdout), &e); err != nil || !e.Success {
				t.Fatalf("legacy %s %v", stdout, err)
			}
			page = e.Data
		}
		return page
	}
	start := query(true, "", "1")
	if len(start.Changes) != 0 || start.NextCursor == "" {
		t.Fatalf("empty %+v", start)
	}
	chat := "123@s.whatsapp.net"
	if err = db.UpsertChat(chat, "dm", "fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	first := query(false, start.NextCursor, "1")
	if !first.HasMore || first.Changes[0].ID != "one" {
		t.Fatalf("first %+v", first)
	}
	second := query(true, first.NextCursor, "200")
	if second.HasMore || len(second.Changes) != 1 || second.Changes[0].ID != "two" {
		t.Fatalf("second %+v", second)
	}
	end := query(false, second.NextCursor, "1")
	if len(end.Changes) != 0 || end.NextCursor != second.NextCursor {
		t.Fatalf("end %+v", end)
	}
	stdout, stderr, err := runAgentTest(t, "--store", dir, "changes", "list", "--cursor", end.NextCursor)
	if err != nil || stderr != "" || !strings.Contains(stdout, "next_cursor: "+end.NextCursor) {
		t.Fatalf("table %s %s %v", stdout, stderr, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "session.db")); !os.IsNotExist(err) {
		t.Fatalf("created a session: %v", err)
	}
}

func TestChangesCLIRejectsCursorAndMissingStoreWithoutInitializing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, extra := range [][]string{{}, {"--cursor", "PRIVATE-CURSOR"}, {"--limit", "201"}, {"--chat", "123@s.whatsapp.net"}} {
		args := append([]string{"--agent", "--store", missing, "changes", "list"}, extra...)
		stdout, stderr, err := runAgentTest(t, args...)
		if err == nil || stdout != "" {
			t.Fatalf("succeeded %v", args)
		}
		e := decodeAgentTest(t, stderr)
		if strings.Contains(stderr, "PRIVATE-CURSOR") || e.Error == nil {
			t.Fatalf("unsafe error %s", stderr)
		}
		if _, err = os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("initialized missing archive %v", err)
		}
	}
}
