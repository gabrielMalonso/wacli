package main

import (
	"database/sql"
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

func TestAgentSearchPaginationLocalPNLIDAndWAL(t *testing.T) {
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
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: fmt.Sprintf("page%d", i), SenderJID: localReadPN, Timestamp: time.Unix(100, 0), Text: "needle", MediaType: "document", FromMe: true, IsForwarded: true}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetStarred(store.SetStarredParams{ChatJID: chat, MsgID: fmt.Sprintf("page%d", i), Starred: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "tombstone", Timestamp: time.Unix(100, 0), Text: "needle", MediaType: "document", FromMe: true, IsForwarded: true, Revoked: true}); err != nil {
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
			args := []string{"--store", dir, "--agent", "--detail", detail, "messages", "search", "needle", "--sort", "time", "--chat", chat, "--from", sender, "--has-media", "--type", "DOCUMENT", "--forwarded", "--starred", "--after", after, "--before", "1970-01-01T00:01:41Z", "--limit", limit}
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
			mode, order := "like", "time_desc"
			if db.HasFTS() {
				mode = "fts5"
			}
			if asc {
				order = "time_asc"
			}
			if data.SearchMode != mode || data.Order != order {
				t.Fatal(stdout)
			}
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
	// Compare both legacy output and default agent relevance with the actual
	// store ordering; FTS must retain rank rather than silently become temporal.
	want, err := db.SearchMessages(store.SearchMessagesParams{Query: "needle", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--json", "messages", "search", "needle")
	if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") || strings.Contains(stdout, "next_cursor") {
		t.Fatalf("legacy: %v %s %s", err, stdout, stderr)
	}
	var legacy struct {
		Data struct {
			Messages []store.Message
			FTS      bool
		}
	}
	if err := json.Unmarshal([]byte(stdout), &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy.Data.Messages) != len(want) {
		t.Fatal(stdout)
	}
	for i, m := range want {
		if legacy.Data.Messages[i].MsgID != m.MsgID || legacy.Data.Messages[i].Snippet != m.Snippet {
			t.Fatal("legacy changed", stdout)
		}
	}
	stdout, stderr, err = runAgentTest(t, "--store", dir, "--agent", "messages", "search", "needle")
	if err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	value := decodeAgentTest(t, stdout)
	var data agentMessages
	if err := json.Unmarshal(value.Data, &data); err != nil {
		t.Fatal(err)
	}
	mode, order := "like", "time_desc"
	if db.HasFTS() {
		mode, order = "fts5", "relevance"
	}
	if value.Meta.Page != nil || data.SearchMode != mode || data.Order != order {
		t.Fatal(stdout)
	}
	for i, m := range want {
		if data.Messages[i].ID != m.MsgID {
			t.Fatal("agent default order changed", stdout)
		}
	}
}

func TestAgentSearchCursorMismatchAndMappingChange(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: id, Timestamp: time.Unix(100, 0), Text: "needle"}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	base := []string{"--store", dir, "--agent", "messages", "search", "needle", "--sort", "time", "--chat", localReadPN, "--limit", "1"}
	stdout, _, err := runAgentTest(t, base...)
	if err != nil {
		t.Fatal(err)
	}
	token := *decodeAgentTest(t, stdout).Meta.Page.NextCursor
	for _, extra := range [][]string{{"--asc"}, {"--from", localReadPN}, {"--after", "1970-01-01"}, {"--before", "2030-01-01"}, {"--has-media"}, {"--type", "document"}, {"--forwarded"}, {"--starred"}, {"--chat", localReadLID}, {"--store", seedLocalReadStore(t)}} {
		args := append(append([]string{}, base...), "--cursor", token)
		args = append(args, extra...)
		assertCursorError(t, args, token)
	}
	assertCursorError(t, []string{"--store", dir, "--agent", "messages", "search", "other", "--sort", "time", "--chat", localReadPN, "--cursor", token}, token)
	assertCursorError(t, []string{"--store", dir, "--agent", "messages", "list", "--chat", localReadPN, "--cursor", token}, token)
	stdout, _, err = runAgentTest(t, "--store", dir, "--agent", "messages", "list", "--chat", localReadPN, "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	listToken := *decodeAgentTest(t, stdout).Meta.Page.NextCursor
	assertCursorError(t, append(append([]string{}, base...), "--cursor", listToken), listToken)
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

func TestSearchPaginationPreflightFlagsAndLiteral(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	base := []string{"--store", missing, "--agent", "messages", "search", "needle"}
	for _, extra := range [][]string{{"--sort", "rank"}, {"--asc"}, {"--asc=false"}, {"--cursor", "private-token"}, {"--sort", "relevance", "--cursor", "private-token"}, {"--sort", "time", "--limit", "201"}} {
		args := append(append([]string{}, base...), extra...)
		stdout, stderr, err := runAgentTest(t, args...)
		if err == nil || commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" || strings.Contains(stderr, "private-token") {
			t.Fatalf("preflight: %v %s %s", err, stdout, stderr)
		}
		if strings.Contains(strings.Join(extra, " "), "cursor") && !strings.Contains(stderr, "--sort time") {
			t.Fatal("missing recovery", stderr)
		}
	}
	for _, token := range []string{"", "private-token", strings.Repeat("x", store.MaxMessagesCursorBytes+1)} {
		assertCursorError(t, append(append([]string{}, base...), "--sort", "time", "--cursor", token), token)
	}
	for _, extra := range [][]string{{"--sort", "time"}, {"--sort", "relevance"}, {"--asc"}, {"--asc=false"}} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--store", missing, "messages", "search", "needle"}, extra...)...)
		if err == nil || stdout != "" || !strings.Contains(stderr, "require --agent") || strings.Contains(stderr, "schema_version") {
			t.Fatal(err, stdout, stderr)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preflight created archive", err)
	}
	dir := seedLocalReadStore(t)
	for _, literal := range []string{"--sort", "--asc", "--cursor", "--agent"} {
		stdout, stderr, err := runAgentTest(t, "--store", dir, "--json", "messages", "search", "--", literal)
		if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") {
			t.Fatal(err, stdout, stderr)
		}
		stdout, stderr, err = runAgentTest(t, "--store", dir, "--agent", "messages", "search", "--sort", "time", "--", literal)
		if err != nil || stderr != "" || decodeAgentTest(t, stdout).Meta.Page == nil {
			t.Fatal(err, stdout, stderr)
		}
	}
	// A known string flag consumes a flag-looking value without activating it.
	_, stderr, err := runAgentTest(t, "--store", missing, "messages", "search", "needle", "--sort", "--agent")
	if err == nil || strings.Contains(stderr, "schema_version") {
		t.Fatal(err, stderr)
	}
}
