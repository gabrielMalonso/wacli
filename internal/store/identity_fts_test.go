//go:build sqlite_fts5

package store

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestMessageIdentityUpdatesDoNotRewriteFTS(t *testing.T) {
	db := openTestDB(t)
	db.sql.SetMaxOpenConns(1)
	if err := db.UpsertChat("100@g.us", "group", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{
		ChatJID: "100@g.us", MsgID: "identity", SenderJID: "123@lid",
		Timestamp: time.Now(), Text: "searchable needle",
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := feedPage(t, db, "", 20).NextCursor
	var ftsWrites atomic.Int64
	conn, err := db.sql.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Observe actual FTS shadow-table writes on the single fixture connection.
	// Archive notifications are legitimate writes outside the search index.
	err = conn.Raw(func(driver any) error {
		sqlite, ok := driver.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected fixture driver %T", driver)
		}
		sqlite.RegisterUpdateHook(func(_ int, database, table string, _ int64) {
			if database == "main" && strings.HasPrefix(table, "messages_fts_") {
				ftsWrites.Add(1)
			}
		})
		return nil
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE messages SET sender_jid = ?, quoted_sender_jid = ? WHERE msg_id = ?`,
		"15550000001@s.whatsapp.net", "15550000002@s.whatsapp.net", "identity"); err != nil {
		t.Fatal(err)
	}
	if writes := ftsWrites.Load(); writes != 0 {
		t.Fatalf("identity-only update wrote %d FTS shadow rows, want none", writes)
	}
	msgs, err := db.SearchMessages(SearchMessagesParams{Query: "needle", Limit: 10})
	if err != nil || len(msgs) != 1 || msgs[0].SenderJID != "15550000001@s.whatsapp.net" {
		t.Fatalf("search after identity update: %+v, %v", msgs, err)
	}
	message, err := db.GetMessage("100@g.us", "identity")
	if err != nil || message.QuotedSenderJID != "15550000002@s.whatsapp.net" {
		t.Fatalf("quoted identity after update: %+v, %v", message, err)
	}
	page := feedPage(t, db, checkpoint, 20)
	if len(page.Changes) != 1 || page.Changes[0].Kind != "message_update" || page.Changes[0].ChatJID != "100@g.us" || page.Changes[0].ID != "identity" || page.Changes[0].SenderJID != message.SenderJID {
		t.Fatalf("identity change notification: %+v", page)
	}
	// A real indexed-field update proves the observer detects FTS rewrites.
	if _, err := db.sql.Exec(`UPDATE messages SET text = 'changed needle' WHERE msg_id = 'identity'`); err != nil {
		t.Fatal(err)
	}
	if ftsWrites.Load() == 0 {
		t.Fatal("indexed text update did not produce observable FTS writes")
	}
}

func TestSelectiveFTSUpdatesPreserveSearchLifecycle(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertChat("100@g.us", "group", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{
		ChatJID: "100@g.us", MsgID: "lifecycle", Timestamp: time.Now(), Text: "original",
	}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"text", "media_caption", "filename", "chat_name", "sender_name", "display_text"} {
		t.Run(column, func(t *testing.T) {
			if _, err := db.sql.Exec(`UPDATE messages SET ` + column + ` = 'replacement'`); err != nil {
				t.Fatal(err)
			}
			msgs, err := db.SearchMessages(SearchMessagesParams{Query: "replacement", Limit: 10})
			if err != nil || len(msgs) != 1 {
				t.Fatalf("search updated %s: %+v, %v", column, msgs, err)
			}
			if _, err := db.sql.Exec(`UPDATE messages SET ` + column + ` = NULL`); err != nil {
				t.Fatal(err)
			}
			msgs, err = db.SearchMessages(SearchMessagesParams{Query: "replacement", Limit: 10})
			if err != nil || len(msgs) != 0 {
				t.Fatalf("search cleared %s: %+v, %v", column, msgs, err)
			}
		})
	}
	for _, step := range []struct {
		query string
		want  int
	}{
		{`UPDATE messages SET text = 'retained'`, 1},
		{`UPDATE messages SET rowid = rowid + 100`, 1},
		{`UPDATE messages SET deleted_at = 1`, 0},
		{`UPDATE messages SET deleted_at = NULL`, 1},
		{`DELETE FROM messages`, 0},
	} {
		if _, err := db.sql.Exec(step.query); err != nil {
			t.Fatal(err)
		}
		msgs, err := db.SearchMessages(SearchMessagesParams{Query: "retained", Limit: 10})
		if err != nil || len(msgs) != step.want {
			t.Fatalf("%s: search = %+v, %v", step.query, msgs, err)
		}
	}
}
