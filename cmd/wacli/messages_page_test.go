package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func TestAgentMessagesPaginationLocalPNLIDAndWAL(t *testing.T) {
	dir := seedLocalReadStore(t)
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
	if err := db.UpsertChat(localReadPN, "dm", "Fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	// Keep writer + WAL open; all input is synthetic, and no reader takes LOCK.
	for i := 0; i < 11; i++ {
		chat := localReadPN
		if i%2 == 0 {
			chat = localReadLID
		}
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: fmt.Sprintf("page%d", i), SenderJID: localReadPN, Timestamp: time.Unix(100, 0), FromMe: true, IsForwarded: true}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetStarred(store.SetStarredParams{ChatJID: chat, MsgID: fmt.Sprintf("page%d", i), Starred: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "tombstone", Timestamp: time.Unix(100, 0), FromMe: true, IsForwarded: true, Revoked: true}); err != nil {
		t.Fatal(err)
	}
	for _, asc := range []bool{false, true} {
		var got []string
		cursor := ""
		for n := 0; n < 10; n++ {
			limit := "3"
			detail := "compact"
			sender := "15550000001"
			chat := "15550000001"
			after := "1970-01-01T00:01:39Z"
			if n > 0 {
				limit = "2"
				detail = "full"
				sender = localReadPN
				chat = localReadPN
				after = "1970-01-01T01:01:39+01:00"
			}
			args := []string{"--store", dir, "--agent", "--detail", detail, "messages", "list", "--chat", chat, "--sender", sender, "--from-me", "--forwarded", "--starred", "--after", after, "--before", "1970-01-01T00:01:41Z", "--limit", limit}
			if asc {
				args = append(args, "--asc")
			}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			stdout, stderr, err := runAgentTest(t, args...)
			if err != nil || stderr != "" {
				t.Fatalf("%v %s", err, stderr)
			}
			value := decodeAgentTest(t, stdout)
			var data agentMessages
			if err := json.Unmarshal(value.Data, &data); err != nil {
				t.Fatal(err)
			}
			page := value.Meta.Page
			if page == nil || page.Returned != len(data.Messages) || page.HasMore != (page.NextCursor != nil) || value.Meta.Freshness != "unknown" || value.Meta.Completeness != "unknown" {
				t.Fatalf("bad metadata: %s", stdout)
			}
			for _, m := range data.Messages {
				if m.ID == "tombstone" {
					t.Fatal("tombstone leaked")
				}
				if n > 0 && m.Full == nil {
					t.Fatal("detail change lost")
				}
				got = append(got, m.ID)
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		want := make([]string, 11)
		for i := range want {
			j := 10 - i
			if asc {
				j = i
			}
			want[i] = fmt.Sprintf("page%d", j)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("asc=%v got=%v want=%v", asc, got, want)
		}
	}
	// Legacy envelope/defaults/order remain available alongside the agent surface.
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--json", "messages", "list", "--chat", localReadPN)
	if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") || strings.Contains(stdout, "next_cursor") {
		t.Fatalf("legacy: %v %s %s", err, stdout, stderr)
	}
	var legacy struct {
		Data struct {
			Messages []store.Message
			FTS      bool `json:"fts"`
		}
	}
	if err := json.Unmarshal([]byte(stdout), &legacy); err != nil || len(legacy.Data.Messages) != 12 || legacy.Data.Messages[0].MsgID != "m1" {
		t.Fatalf("legacy defaults/order changed: %v %s", err, stdout)
	}
}

func TestAgentMessagesCursorMismatchAndMappingChange(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: id, Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	base := []string{"--store", dir, "--agent", "messages", "list", "--chat", localReadPN, "--limit", "1"}
	stdout, _, err := runAgentTest(t, base...)
	if err != nil {
		t.Fatal(err)
	}
	token := *decodeAgentTest(t, stdout).Meta.Page.NextCursor
	for _, extra := range [][]string{{"--asc"}, {"--sender", localReadPN}, {"--after", "1970-01-01"}, {"--before", "2030-01-01"}, {"--from-me"}, {"--from-them"}, {"--forwarded"}, {"--starred"}, {"--chat", localReadLID}, {"--store", seedLocalReadStore(t)}} {
		args := append(append([]string{}, base...), "--cursor", token)
		args = append(args, extra...)
		assertCursorError(t, args, token)
	}
	session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec("DELETE FROM whatsmeow_lid_map"); err != nil {
		t.Fatal(err)
	}
	session.Close()
	assertCursorError(t, append(base, "--cursor", token), token)
}

func assertCursorError(t *testing.T, args []string, token string) {
	t.Helper()
	stdout, stderr, err := runAgentTest(t, args...)
	if err == nil || commandExitCode(err) != 2 || stdout != "" || (token != "" && strings.Contains(stderr, token)) {
		t.Fatalf("cursor error: %v %s %s", err, stdout, stderr)
	}
	value := decodeAgentTest(t, stderr)
	if value.Error.Code != "invalid_cursor" {
		t.Fatalf("code: %s", stderr)
	}
}

func TestCursorPreflightAndFlagIntent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, token := range []string{"", "secret-token-fragment", strings.Repeat("x", store.MaxMessagesCursorBytes+1)} {
		assertCursorError(t, []string{"--agent", "--store", missing, "--cursor", token, "messages", "list"}, token)
	}
	for _, args := range [][]string{
		{"--cursor", "secret-token-fragment", "send", "text"},
		{"--agent", "--cursor", "secret-token-fragment", "send", "text"},
		{"--agent", "messages", "search", "query", "--cursor", "secret-token-fragment"},
		{"--agent", "--cursor", "secret-token-fragment", "chats", "list"},
		{"--agent=false", "--cursor", "secret-token-fragment", "messages", "list"},
	} {
		stdout, stderr, err := runAgentTest(t, args...)
		if err == nil || commandExitCode(err) != 2 || stdout != "" || !strings.Contains(stderr, "--cursor requires --agent messages list") || strings.Contains(stderr, "secret-token-fragment") {
			t.Fatalf("context preflight: %v %s %s", err, stdout, stderr)
		}
	}
	// A cursor value may look like a flag; intent must consume it, including on
	// an earlier parse failure. A terminator leaves subsequent text positional.
	for _, args := range [][]string{
		{"--cursor", "--agent", "--bogus", "messages", "list"},
		{"--cursor=--agent", "messages", "list"},
		{"messages", "search", "--", "--cursor", "--agent"},
	} {
		_, stderr, err := runAgentTest(t, args...)
		if err == nil || strings.Contains(stderr, "schema_version") {
			t.Fatalf("wrong agent intent: %v %s", err, stderr)
		}
	}
	_, stderr, err := runAgentTest(t, "--agent", "--bogus", "--cursor", "--agent", "messages", "list")
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
		t.Fatalf("lost agent intent: %v %s", err, stderr)
	}
	stdout, stderr, err := runAgentTest(t, "--store", missing, "--agent", "messages", "list", "--", "--cursor", "TOKEN")
	if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
		t.Fatalf("agent terminator: %v %s %s", err, stdout, stderr)
	}
	dir := seedLocalReadStore(t)
	stdout, stderr, err = runAgentTest(t, "--store", dir, "--json", "messages", "search", "--", "--cursor")
	if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") {
		t.Fatalf("terminator: %v %s %s", err, stdout, stderr)
	}
}
