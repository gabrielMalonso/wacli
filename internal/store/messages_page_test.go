package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func pageIDs(msgs []Message) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.MsgID)
	}
	return ids
}

func collectPages(t *testing.T, db *DB, p ListMessagesPageParams) []Message {
	t.Helper()
	var all []Message
	for i := 0; i < 100; i++ {
		page, err := db.ListMessagesPage(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) > p.Limit || page.HasMore != (page.NextCursor != nil) {
			t.Fatalf("bad page: %+v", page)
		}
		all = append(all, page.Messages...)
		if !page.HasMore {
			return all
		}
		if len(page.Messages) != p.Limit || len(*page.NextCursor) > MaxMessagesCursorBytes {
			t.Fatalf("bad continued page: %+v", page)
		}
		p.Cursor = *page.NextCursor
	}
	t.Fatal("pagination never ended")
	return nil
}

func TestMessagesPageStaticTiesAndBoundaries(t *testing.T) {
	db := openTestDB(t)
	for _, chat := range []string{"pn", "lid", "chat"} {
		if err := db.UpsertChat(chat, "dm", "Fixture", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 13; i++ {
		chat := "pn"
		if i%2 == 0 {
			chat = "lid"
		}
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: fmt.Sprintf("m%02d", i), Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, chats := range [][]string{nil, {"pn"}, {"pn", "lid"}} {
		for _, asc := range []bool{false, true} {
			p := ListMessagesPageParams{StoreRef: "fixture", ListMessagesParams: ListMessagesParams{ChatJIDs: chats, Asc: asc}}
			expected, err := db.ListMessages(ListMessagesParams{ChatJIDs: chats, Asc: asc, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			for _, limit := range []int{1, 2, 3, len(expected), len(expected) + 1} {
				p.Limit = limit
				got := collectPages(t, db, p)
				if !reflect.DeepEqual(pageIDs(got), pageIDs(expected)) {
					t.Fatalf("chats=%v asc=%v limit=%d: %v vs %v", chats, asc, limit, pageIDs(got), pageIDs(expected))
				}
			}
			// Explicit expected row order at the same timestamp, independent of legacy.
			if len(chats) == 0 {
				for i, m := range expected {
					n := 12 - i
					if asc {
						n = i
					}
					if m.MsgID != fmt.Sprintf("m%02d", n) {
						t.Fatal(pageIDs(expected))
					}
				}
			}
		}
	}
	empty, err := db.ListMessagesPage(ListMessagesPageParams{ListMessagesParams: ListMessagesParams{ChatJID: "absent", Limit: 2}})
	if err != nil || len(empty.Messages) != 0 || empty.HasMore || empty.NextCursor != nil {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
}

func TestMessagesPageCombinedFiltersAndRawTimestamps(t *testing.T) {
	db := openTestDB(t)
	for _, chat := range []string{"pn", "lid", "chat"} {
		if err := db.UpsertChat(chat, "dm", "Fixture", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	yes := true
	after, before := time.Unix(99, 0), time.Unix(101, 0)
	for i := 0; i < 20; i++ {
		chat := "pn"
		if i%2 == 0 {
			chat = "lid"
		}
		ts := time.Unix(100, 0)
		if i == 0 {
			ts = time.Unix(-1, 0)
		}
		if i == 1 {
			ts = time.Unix(0, 0)
		}
		p := UpsertMessageParams{ChatJID: chat, MsgID: fmt.Sprint(i), Timestamp: ts, SenderJID: "sender", FromMe: i%3 != 0, IsForwarded: i%4 != 0, Revoked: i == 19}
		if err := db.UpsertMessage(p); err != nil {
			t.Fatal(err)
		}
		if i%5 != 0 {
			if err := db.SetStarred(SetStarredParams{ChatJID: chat, MsgID: p.MsgID, Starred: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, asc := range []bool{false, true} {
		params := ListMessagesParams{ChatJIDs: []string{"lid", "pn"}, SenderJID: "sender", FromMe: &yes, Forwarded: true, Starred: true, Before: &before, After: &after, Asc: asc, Limit: 2}
		p := ListMessagesPageParams{ListMessagesParams: params}
		got := pageIDs(collectPages(t, db, p))
		want := []string{"17", "14", "13", "11", "7", "2"}
		if asc {
			want = []string{"2", "7", "11", "13", "14", "17"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("asc=%v got=%v want=%v", asc, got, want)
		}
		// Raw SQLite dates are anchors even when the legacy DTO reports zero time.
		all := collectPages(t, db, ListMessagesPageParams{ListMessagesParams: ListMessagesParams{Limit: 1, Asc: asc}})
		if len(all) != 19 {
			t.Fatalf("raw date pagination: %v", pageIDs(all))
		}
	}
}

func TestMessagesPageScopeAndStrictCursor(t *testing.T) {
	db := openTestDB(t)
	for _, chat := range []string{"pn", "lid", "chat"} {
		if err := db.UpsertChat(chat, "dm", "Fixture", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "pn", MsgID: fmt.Sprint(i), Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	p := ListMessagesPageParams{StoreRef: "/fixture", ChatIdentity: "pn", ListMessagesParams: ListMessagesParams{ChatJIDs: []string{"pn", "lid"}, Limit: 1}}
	first, err := db.ListMessagesPage(p)
	if err != nil {
		t.Fatal(err)
	}
	token := *first.NextCursor
	p.Cursor = token
	p.Limit = 2
	p.ChatJIDs = []string{" lid ", "pn", "pn"}
	if page, err := db.ListMessagesPage(p); err != nil || page.HasMore || len(page.Messages) != 2 {
		t.Fatalf("normalization/limit: %+v %v", page, err)
	}
	zero := time.Unix(0, 0)
	yes, no := true, false
	for _, change := range []func(*ListMessagesPageParams){
		func(p *ListMessagesPageParams) { p.StoreRef = "/other" }, func(p *ListMessagesPageParams) { p.ChatIdentity = "lid" }, func(p *ListMessagesPageParams) { p.ChatJIDs = []string{"pn"} }, func(p *ListMessagesPageParams) { p.SenderJID = "other" },
		func(p *ListMessagesPageParams) { p.Before = &zero }, func(p *ListMessagesPageParams) { p.After = &zero }, func(p *ListMessagesPageParams) { p.FromMe = &yes }, func(p *ListMessagesPageParams) { p.FromMe = &no }, func(p *ListMessagesPageParams) { p.Asc = true }, func(p *ListMessagesPageParams) { p.Forwarded = true }, func(p *ListMessagesPageParams) { p.Starred = true },
	} {
		changed := p
		change(&changed)
		_, err := db.ListMessagesPage(changed)
		var typed *MessagesCursorError
		if !errors.As(err, &typed) || !typed.Mismatch {
			t.Fatalf("scope accepted: %+v %v", changed, err)
		}
	}
	cursor, _ := decodeMessagesCursor(token)
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	badJSON := []string{
		`{}`, strings.Replace(string(raw), `"v":1`, `"v":2`, 1), strings.Replace(string(raw), `"rowid":3`, `"rowid":0`, 1), strings.Replace(string(raw), `"ts":100,`, "", 1), strings.Replace(string(raw), `"ts":100`, `"ts":null`, 1), strings.Replace(string(raw), `"ts":100`, `"ts":"100"`, 1), strings.Replace(string(raw), `"v":1`, `"v":1,"v":1`, 1), string(raw) + `{}`, string(raw) + ` `, strings.Replace(string(raw), `"v":1`, `"v":1,"x":0`, 1),
	}
	for _, raw := range badJSON {
		if err := ValidateMessagesCursor(base64.RawURLEncoding.EncodeToString([]byte(raw))); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	cursor.RowID = -1
	negative, _ := json.Marshal(cursor)
	for _, bad := range []string{"", "secret-token-fragment", strings.Repeat("A", 513), token + "=", token + "\n", base64.RawURLEncoding.EncodeToString(negative)} {
		err := ValidateMessagesCursor(bad)
		var typed *MessagesCursorError
		if !errors.As(err, &typed) || strings.Contains(err.Error(), "secret-token-fragment") {
			t.Fatalf("bad token error: %v", err)
		}
	}
	// A mismatch must be rejected without trying SQL, even on a closed DB.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p.StoreRef = "other"
	_, err = db.ListMessagesPage(p)
	var typed *MessagesCursorError
	if !errors.As(err, &typed) || !typed.Mismatch {
		t.Fatalf("SQL before scope validation: %v", err)
	}
}

func TestMessagesPageLiveDeletionInsertionAndUpsert(t *testing.T) {
	for _, asc := range []bool{false, true} {
		t.Run(fmt.Sprint(asc), func(t *testing.T) {
			db := openTestDB(t)
			for _, chat := range []string{"pn", "lid", "chat"} {
				if err := db.UpsertChat(chat, "dm", "Fixture", time.Unix(100, 0)); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= 5; i++ {
				if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "chat", MsgID: fmt.Sprint(i), Timestamp: time.Unix(int64(i)*100, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			p := ListMessagesPageParams{ListMessagesParams: ListMessagesParams{ChatJID: "chat", Limit: 2, Asc: asc}}
			page, err := db.ListMessagesPage(p)
			if err != nil {
				t.Fatal(err)
			}
			anchor := page.Messages[1]
			p.Cursor = *page.NextCursor
			if _, err := db.sql.Exec("DELETE FROM messages WHERE rowid = ?", anchor.rowID); err != nil {
				t.Fatal(err)
			}
			// Newly inserted rows behind the boundary are skipped; ahead are visible.
			for _, ts := range []int64{150, 450} {
				if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "chat", MsgID: fmt.Sprint(ts), Timestamp: time.Unix(ts, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			original, err := db.GetMessage("chat", "3")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "chat", MsgID: "3", Timestamp: time.Unix(900, 0), Text: "edited", Edited: true}); err != nil {
				t.Fatal(err)
			}
			edited, err := db.GetMessage("chat", "3")
			if err != nil || edited.rowID != original.rowID || !edited.Timestamp.Equal(original.Timestamp) {
				t.Fatalf("upsert moved row: %v", err)
			}
			if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "chat", MsgID: "same-second", Timestamp: anchor.Timestamp}); err != nil {
				t.Fatal(err)
			}
			got := pageIDs(collectPages(t, db, p))
			want := []string{"3", "2", "150", "1"}
			if asc {
				want = []string{"same-second", "3", "4", "450", "5"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("live got=%v want=%v", got, want)
			}
		})
	}
}

func TestMessagesPageQueryPlanUsesExistingIndexes(t *testing.T) {
	db := openTestDB(t)
	for _, chats := range [][]string{nil, {"pn"}, {"pn", "lid"}} {
		for _, asc := range []bool{false, true} {
			query, args := listMessagesQuery(ListMessagesParams{ChatJIDs: chats, Limit: 3, Asc: asc}, &messageCursor{TS: 100, RowID: 50})
			rows, err := db.sql.Query("EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			var plans []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			plan := strings.Join(plans, "\n")
			index := "idx_messages_ts"
			if len(chats) > 0 {
				index = "idx_messages_chat_ts"
			}
			if !strings.Contains(plan, index) || strings.Contains(plan, "SCAN m") {
				t.Fatalf("unbounded message scan chats=%v asc=%v:\n%s", chats, asc, plan)
			}
			t.Logf("chats=%v asc=%v:\n%s", chats, asc, plan)
		}
	}
}
