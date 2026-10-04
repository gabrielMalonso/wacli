package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func TestAgentChatsPaginationRawIdentitiesAndWAL(t *testing.T) {
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
	// The verified pair remains two stored rows with separate counts, never a merged view.
	for _, jid := range []string{localReadPN, localReadLID} {
		if err := db.UpsertChat(jid, "dm", "Synthetic %_\\", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
		if err := db.SetChatUnreadCount(jid, 2); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 11; i++ {
		if err := db.UpsertChat(fmt.Sprintf("tie%02d@g.us", i), "group", "Synthetic %_\\", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotLocalStore(t, dir)
	var got []string
	cursor := ""
	for n := 0; n < 10; n++ {
		limit, detail := "3", "compact"
		if n > 0 {
			limit, detail = "2", "full"
		}
		args := []string{"--store", dir, "--read-only", "--agent", "chats", "list", "--query", `%_\`, "--limit", limit, "--detail", detail}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		env := decodeAgentTest(t, stdout)
		var data agentChats
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		page := env.Meta.Page
		if page == nil || page.Returned != len(data.Chats) || page.HasMore != (page.NextCursor != nil) || env.Meta.Completeness != "unknown" || env.Meta.Freshness != "unknown" {
			t.Fatalf("metadata: %s", stdout)
		}
		for _, c := range data.Chats {
			if n > 0 && c.Full == nil {
				t.Fatal("detail change failed")
			}
			if (c.JID == localReadPN || c.JID == localReadLID) && c.UnreadCount != 2 {
				t.Fatal("counts consolidated")
			}
			got = append(got, c.JID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	want := []string{localReadLID, localReadPN}
	for i := 0; i < 11; i++ {
		want = append(want, fmt.Sprintf("tie%02d@g.us", i))
	}
	// The literal binary order of this pair is deterministic.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	// No reader mutates archive, LOCK, session or permissions; allow SQLite shm bookkeeping.
	after := snapshotLocalStore(t, dir)
	for _, files := range []map[string]localFileSnapshot{before, after} {
		for path := range files {
			if strings.HasSuffix(path, "-shm") {
				delete(files, path)
			}
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read changed archive/WAL/lock/session/permissions")
	}
}

func TestAgentChatsCursorPreflightScopeAndLegacy(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 55; i++ {
		if err := db.UpsertChat(fmt.Sprintf("j%02d@g.us", i), "group", "Fixture", time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetChatPinned("j00@g.us", true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: "j00@g.us", MsgID: "fixture1", Timestamp: time.Unix(100, 0), Text: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: "j00@g.us", MsgID: "fixture2", Timestamp: time.Unix(100, 0), Text: "fixture"}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before := snapshotLocalStore(t, dir)
	stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "chats", "list")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	first := decodeAgentTest(t, stdout)
	if first.Meta.Limit != 20 || first.Meta.Page.Returned != 20 || !first.Meta.Page.HasMore {
		t.Fatalf("default: %s", stdout)
	}
	token := *first.Meta.Page.NextCursor
	malformed := []string{"", "PRIVATE_SQL_JID_CURSOR", strings.Repeat("x", store.MaxChatsCursorBytes+1)}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	malformed = append(malformed, base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"v":1`, `"v":2`, 1))))
	missing := filepath.Join(dir, "missing")
	for _, bad := range malformed {
		assertCursorError(t, []string{"--agent", "--store", missing, "--cursor", bad, "chats", "list"}, bad)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preflight initialized store")
	}
	for _, filter := range []string{"--archived", "--no-archived", "--pinned", "--no-pinned", "--muted", "--no-muted", "--unread", "--no-unread"} {
		assertCursorError(t, []string{"--agent", "--store", dir, "chats", "list", "--cursor", token, filter}, token)
	}
	assertCursorError(t, []string{"--agent", "--store", dir, "chats", "list", "--cursor", token, "--query", "other"}, token)
	other := t.TempDir()
	otherDB, err := store.Open(filepath.Join(other, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = otherDB.Close()
	assertCursorError(t, []string{"--agent", "--store", other, "chats", "list", "--cursor", token}, token)
	for _, operation := range [][]string{{"messages", "list"}, {"messages", "search", "fixture", "--sort", "time"}} {
		assertCursorError(t, append([]string{"--agent", "--store", dir, "--cursor", token}, operation...), token)
		out, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir, "--limit", "1"}, operation...)...)
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		messageToken := *decodeAgentTest(t, out).Meta.Page.NextCursor
		assertCursorError(t, []string{"--agent", "--store", dir, "chats", "list", "--cursor", messageToken}, messageToken)
	}
	for _, args := range [][]string{{"--cursor", token, "chats", "list"}, {"--agent", "--cursor", token, "chats", "show", "--jid", "j00@g.us"}} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--store", missing}, args...)...)
		if err == nil || stdout != "" || !strings.Contains(stderr, "--cursor requires") || strings.Contains(stderr, token) {
			t.Fatalf("context: %v %s", err, stderr)
		}
	}
	for _, filter := range []string{"archived", "pinned", "muted", "unread"} {
		stdout, stderr, err := runAgentTest(t, "--agent", "--store", missing, "chats", "list", "--"+filter, "--no-"+filter)
		if commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatalf("mutually exclusive: %v %s", err, stderr)
		}
	}
	for _, limit := range []string{"0", "201", "-1"} {
		stdout, stderr, err := runAgentTest(t, "--agent", "--store", missing, "chats", "list", "--limit", limit)
		if commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatalf("limit: %v %s", err, stderr)
		}
	}
	// Legacy default, raw ordering and envelope match the existing store reader exactly.
	reader, err := store.OpenReadOnly(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := reader.ListChats("", 50)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	stdout, stderr, err = runAgentTest(t, "--store", dir, "--json", "chats", "list")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	var legacy struct {
		Data          []store.Chat
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(stdout), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.SchemaVersion != 0 || len(legacy.Data) != 50 || !reflect.DeepEqual(legacy.Data, want) {
		t.Fatalf("legacy changed: %s", stdout)
	}
	for _, query := range []string{"Fixture", "absent"} {
		stdout, stderr, err = runAgentTest(t, "--store", dir, "--agent", "chats", "list", "--query", query, "--limit", "55")
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		env := decodeAgentTest(t, stdout)
		returned := 55
		if query == "absent" {
			returned = 0
		}
		if env.Meta.Page.Returned != returned || env.Meta.Page.HasMore || env.Meta.Page.NextCursor != nil {
			t.Fatalf("local end: %s", stdout)
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("offline reads changed fixture bytes or permissions")
	}
}

func TestAgentChatsStoreSchemaErrors(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	fixture, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.Exec(`INSERT INTO schema_migrations(version,name,applied_at) SELECT MAX(version)+1,'future fixture',1 FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Close()
	before := snapshotLocalStore(t, dir)
	stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "chats", "list")
	if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
		t.Fatalf("schema error: %v %s", err, stderr)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("reader migrated incompatible archive")
	}
}

func TestAgentChatsOversizedStoredContinuationIsInternalError(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	oversized := "PRIVATE_STORED_JID_" + strings.Repeat("x", store.MaxChatsCursorBytes)
	for i, jid := range []string{oversized, "next@g.us"} {
		if err := db.UpsertChat(jid, "group", "fixture", time.Unix(int64(100-i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before := snapshotLocalStore(t, dir)
	stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "chats", "list", "--limit", "1")
	if stdout != "" || commandExitCode(err) != 1 {
		t.Fatalf("stored key failure should be internal: %v stdout=%s", err, stdout)
	}
	env := decodeAgentTest(t, stderr)
	if env.Success || env.Error.Code != "internal_error" || env.Error.Recovery != "" || env.Meta.Page != nil || strings.Contains(stderr, "PRIVATE_STORED_JID_") || strings.Contains(stderr, oversized) {
		t.Fatalf("invalid generated continuation leaked: %s", stderr)
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("failure changed fixture")
	}
	// The same data is readable at its local end, where no continuation is needed.
	stdout, stderr, err = runAgentTest(t, "--agent", "--store", dir, "chats", "list", "--limit", "2")
	if err != nil || stderr != "" {
		t.Fatalf("local-end data: %v %s", err, stderr)
	}
	end := decodeAgentTest(t, stdout)
	if end.Meta.Page.HasMore || end.Meta.Page.NextCursor != nil || end.Meta.Page.Returned != 2 {
		t.Fatal("invalid next page emitted")
	}
}
