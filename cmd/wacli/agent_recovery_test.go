package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestAgentTruncatedLabelRecovery(t *testing.T) {
	for _, n := range []int{320, 321} {
		dir := seedLocalReadStore(t)
		db, err := store.Open(filepath.Join(dir, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		label := "Fixture " + strings.Repeat("界", n-len("Fixture "))
		// Resolve reads public names from the synthetic session contact table.
		session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		if _, err := session.Exec("CREATE TABLE whatsmeow_contacts (their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT, redacted_phone TEXT)"); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Exec("INSERT INTO whatsmeow_contacts(their_jid,full_name) VALUES(?,?)", localReadPN, label); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertChat(localReadLID, "dm", label, time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertContact(localReadPN, "15550000001", label, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.SetAlias([]string{localReadPN}, label); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "label-media", SenderJID: localReadLID, Timestamp: time.Unix(101, 0), Text: "fixture message", MediaType: "document", Filename: label}); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		for _, detail := range []string{"compact", "full"} {
			for _, tc := range []struct {
				args []string
				hint string
			}{
				{[]string{"messages", "show", "--chat", localReadPN, "--id", "label-media"}, "messages show --chat"},
				{[]string{"messages", "list"}, "messages show --chat"},
				{[]string{"messages", "search", "fixture"}, "messages show --chat"},
				{[]string{"messages", "context", "--chat", localReadPN, "--id", "label-media"}, "messages show --chat"},
				{[]string{"chats", "show", "--jid", localReadLID}, "chats show --jid"},
				{[]string{"chats", "list"}, "chats show --jid"},
				{[]string{"contacts", "show", "--jid", localReadPN}, "contacts show --jid"},
				{[]string{"contacts", "list"}, "contacts show --jid"},
				{[]string{"contacts", "search", "Fixture"}, "contacts show --jid"},
				{[]string{"contacts", "resolve", localReadLID}, "contacts resolve INPUT"},
				{[]string{"history", "coverage", "--include-blocked"}, "history coverage --chat"},
				{[]string{"history", "coverage", "--include-blocked", "--evidence"}, "history coverage --chat"},
			} {
				args := append([]string{"--agent", "--store", dir, "--detail", detail}, tc.args...)
				stdout, stderr, err := runAgentTest(t, args...)
				if err != nil || stderr != "" {
					t.Fatal(args, err, stderr)
				}
				envelope := decodeAgentTest(t, stdout)
				truncated := detail == "compact" && n > 320
				if strings.Contains(stdout, `"fields_truncated"`) != truncated || strings.Contains(envelope.Meta.Recovery, tc.hint) != truncated {
					t.Fatalf("n=%d %v: incorrect truncation recovery: %s", n, args, stdout)
				}
				evidence := tc.args[len(tc.args)-1] == "--evidence"
				if evidence {
					if !strings.Contains(envelope.Meta.Recovery, "do not prove completeness") {
						t.Fatal("lost evidence warning", stdout)
					}
				} else if !truncated && envelope.Meta.Recovery != "" {
					t.Fatal("unnecessary query hint", stdout)
				}
			}
		}
	}
}

func TestAgentEmptyQueriesHaveNoRecovery(t *testing.T) {
	dir := seedLocalReadStore(t)
	for _, args := range [][]string{
		{"messages", "list", "--chat", "15559999999@s.whatsapp.net"},
		{"messages", "search", "no-synthetic-match"},
		{"chats", "list", "--query", "no-synthetic-match"},
		{"contacts", "search", "no-synthetic-match"},
		{"history", "coverage", "--kind", "broadcast"},
	} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir}, args...)...)
		if err != nil || stderr != "" {
			t.Fatal(args, err, stderr)
		}
		envelope := decodeAgentTest(t, stdout)
		if envelope.Meta.Recovery != "" || !strings.Contains(string(envelope.Data), "[]") {
			t.Fatalf("empty query suggested recovery: %v %s", args, stdout)
		}
	}
}
