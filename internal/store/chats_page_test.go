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

func chatIDs(chats []Chat) []string {
	ids := make([]string, len(chats))
	for i, c := range chats {
		ids[i] = c.JID
	}
	return ids
}

func collectChatPages(t *testing.T, db *DB, p ListChatsPageParams) []Chat {
	t.Helper()
	var chats []Chat
	for n := 0; n < 100; n++ {
		page, err := db.ListChatsPage(p)
		if err != nil {
			t.Fatal(err)
		}
		if page.HasMore != (page.NextCursor != nil) || len(page.Chats) > p.Limit {
			t.Fatalf("invalid page: %+v", page)
		}
		chats = append(chats, page.Chats...)
		if !page.HasMore {
			return chats
		}
		p.Cursor = *page.NextCursor
	}
	t.Fatal("unbounded traversal")
	return nil
}

func insertChatPageFixture(t *testing.T, db *DB, jid, name string, ts, pin, archive, mute, unread any) {
	t.Helper()
	_, err := db.sql.Exec(`INSERT INTO chats(jid,kind,name,last_message_ts,pinned,archived,muted_until,unread) VALUES(?,'dm',?,?,?,?,?,?)`, jid, name, ts, pin, archive, mute, unread)
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatsPageStableKeysAndRawTimestamps(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 11; i++ {
		insertChatPageFixture(t, db, fmt.Sprintf("tie%02d", i), "", 100, 1, 0, 0, 0)
	}
	insertChatPageFixture(t, db, "pin-nonboolean", "", 101, -2, 0, 0, 0)
	insertChatPageFixture(t, db, "null", "", nil, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "zero", "", 0, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "negative-a", "", -1, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "negative-b", "", -2, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "real", "", 200, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "A textual/%_\\ JID", "", 0, 0, 0, 0, 0)
	p := ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 2}, StoreRef: "/synthetic"}
	got := collectChatPages(t, db, p)
	want := []string{"pin-nonboolean"}
	for i := 0; i < 11; i++ {
		want = append(want, fmt.Sprintf("tie%02d", i))
	}
	want = append(want, "real", "A textual/%_\\ JID", "null", "zero", "negative-a", "negative-b")
	if !reflect.DeepEqual(chatIDs(got), want) {
		t.Fatalf("got=%v want=%v", chatIDs(got), want)
	}
	// Limit 1 forces an anchor for null/zero/negative; all public nonpositive dates stay unknown.
	p.Limit = 1
	if !reflect.DeepEqual(chatIDs(collectChatPages(t, db, p)), want) {
		t.Fatal("limit changed traversal")
	}
	for _, c := range got[13:] {
		if !c.LastMessageTS.IsZero() {
			t.Fatalf("nonpositive DTO timestamp: %+v", c)
		}
	}
	p.Limit = len(want)
	exact, err := db.ListChatsPage(p)
	if err != nil || exact.HasMore || exact.NextCursor != nil || len(exact.Chats) != len(want) {
		t.Fatalf("exact: %+v %v", exact, err)
	}
	p.Query = "absent"
	empty, err := db.ListChatsPage(p)
	if err != nil || empty.HasMore || len(empty.Chats) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	for _, limit := range []int{-1, 0, 201} {
		p.Limit = limit
		if _, err := db.ListChatsPage(p); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
}

func TestChatsPageAllTriStateFiltersAndLiteralQuery(t *testing.T) {
	db := openTestDB(t)
	fixed := time.Unix(1000, 0)
	old := nowUTC
	nowUTC = func() time.Time { return fixed }
	t.Cleanup(func() { nowUTC = old })
	for mask := 0; mask < 16; mask++ {
		mute := 0
		if mask&4 != 0 {
			mute = -1
		}
		insertChatPageFixture(t, db, fmt.Sprintf("j%02d", mask), `literal %_\ name`, 100, mask&1, (mask>>1)&1, mute, (mask>>3)&1)
	}
	states := []*bool{nil, new(bool), boolPtr(true)}
	for _, pin := range states {
		for _, archive := range states {
			for _, mute := range states {
				for _, unread := range states {
					f := ChatListFilter{Limit: 2, Query: `%_\`, Pinned: pin, Archived: archive, Muted: mute, Unread: unread}
					got := chatIDs(collectChatPages(t, db, ListChatsPageParams{ChatListFilter: f}))
					var want []string
					for _, pinned := range []bool{true, false} {
						for mask := 0; mask < 16; mask++ {
							if (mask&1 != 0) != pinned {
								continue
							}
							values := []bool{mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0}
							match := true
							for i, filter := range []*bool{pin, archive, mute, unread} {
								if filter != nil && *filter != values[i] {
									match = false
								}
							}
							if match {
								want = append(want, fmt.Sprintf("j%02d", mask))
							}
						}
					}
					if len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
						t.Fatalf("filters %+v: got=%v want=%v", f, got, want)
					}
				}
			}
		}
	}
	for _, query := range []string{"literal", `%`, `_`, `\`, `literal %_\ name`, "", " \t"} {
		got := collectChatPages(t, db, ListChatsPageParams{ChatListFilter: ChatListFilter{Query: query, Limit: 3}})
		if len(got) != 16 {
			t.Fatalf("literal query %q returned %d", query, len(got))
		}
	}
	insertChatPageFixture(t, db, "wildcard-decoy", "literal xxx name", 100, 0, 0, 0, 0)
	if got := collectChatPages(t, db, ListChatsPageParams{ChatListFilter: ChatListFilter{Query: `%_\`, Limit: 3}}); len(got) != 16 {
		t.Fatalf("wildcards were not literal: %d", len(got))
	}
}

func boolPtr(value bool) *bool { return &value }

func TestChatsPageCursorStrictnessAndScopeBeforeQuery(t *testing.T) {
	db := openTestDB(t)
	for _, jid := range []string{"a", "b", "c"} {
		insertChatPageFixture(t, db, jid, "fixture", 100, 0, 0, 0, 0)
	}
	p := ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 1}, StoreRef: "/fixture"}
	first, err := db.ListChatsPage(p)
	if err != nil {
		t.Fatal(err)
	}
	token := *first.NextCursor
	cursor, err := decodeChatsCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(c chatCursor) string { raw, _ := json.Marshal(c); return base64.RawURLEncoding.EncodeToString(raw) }
	malformed := []string{"", "PRIVATE_TOKEN_SQL", strings.Repeat("x", MaxChatsCursorBytes+1), token + "=", token + "\n"}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	for _, s := range []string{string(raw) + " ", strings.Replace(string(raw), `"v":1`, `"v":1,"v":1`, 1), strings.Replace(string(raw), `"v":1`, `"v":1,"extra":1`, 1), strings.Replace(string(raw), `"v":1`, `"v":1.0`, 1), strings.Replace(string(raw), `"v":1,`, "", 1)} {
		malformed = append(malformed, base64.RawURLEncoding.EncodeToString([]byte(s)))
	}
	for _, mutate := range []func(*chatCursor){func(c *chatCursor) { c.Version = 2 }, func(c *chatCursor) { c.Operation = "messages list" }, func(c *chatCursor) { c.Key.Pin = 2 }, func(c *chatCursor) { c.Key.JID = "" }, func(c *chatCursor) { c.Scope = "bad" }} {
		c := *cursor
		mutate(&c)
		malformed = append(malformed, encode(c))
	}
	// Existing list/search shape cannot enter the chat domain.
	raw, _ = json.Marshal(messageCursor{Version: 1, Scope: cursor.Scope, TS: 100, RowID: 1})
	malformed = append(malformed, base64.RawURLEncoding.EncodeToString(raw))
	for _, bad := range malformed {
		err := ValidateChatsCursor(bad)
		var typed *ChatsCursorError
		if !errors.As(err, &typed) || typed.Mismatch {
			t.Fatalf("accepted malformed token length=%d", len(bad))
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Fatal("token leaked")
		}
	}
	if ValidateMessagesCursor(token) == nil {
		t.Fatal("message decoder accepted chat token")
	}
	p.Cursor = token
	p.Limit = 2
	if got := chatIDs(collectChatPages(t, db, p)); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("limit change: %v", got)
	}
	// Even with an unavailable query table, a mismatch is classified before SQL.
	if _, err := db.sql.Exec(`ALTER TABLE chats RENAME TO fixture_hidden_chats`); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ListChatsPageParams){
		func(p *ListChatsPageParams) { p.StoreRef = "/other" }, func(p *ListChatsPageParams) { p.Query = "fixture" },
		func(p *ListChatsPageParams) { p.Pinned = boolPtr(false) }, func(p *ListChatsPageParams) { p.Archived = boolPtr(false) }, func(p *ListChatsPageParams) { p.Muted = boolPtr(false) }, func(p *ListChatsPageParams) { p.Unread = boolPtr(false) },
		func(p *ListChatsPageParams) { p.Pinned = boolPtr(true) }, func(p *ListChatsPageParams) { p.Archived = boolPtr(true) }, func(p *ListChatsPageParams) { p.Muted = boolPtr(true) }, func(p *ListChatsPageParams) { p.Unread = boolPtr(true) },
	} {
		changed := p
		mutate(&changed)
		_, err := db.ListChatsPage(changed)
		var typed *ChatsCursorError
		if !errors.As(err, &typed) || !typed.Mismatch {
			t.Fatalf("scope not checked pre-query: %v", err)
		}
	}
	equivalent := p
	equivalent.Query = " \t"
	if equivalent.cursorScope() != p.cursorScope() {
		t.Fatal("blank query scope differs")
	}
	exact := p
	exact.Query = " fixture "
	other := p
	other.Query = "fixture"
	if exact.cursorScope() == other.cursorScope() {
		t.Fatal("literal query whitespace lost")
	}
}

func TestChatsPageLiveMutationAndMuteExpiry(t *testing.T) {
	db := openTestDB(t)
	fixed := time.Unix(1000, 0)
	old := nowUTC
	calls := 0
	nowUTC = func() time.Time { calls++; return fixed }
	t.Cleanup(func() { nowUTC = old })
	// Equality is expired; -1 is forever; zero and other negatives are unmuted.
	for i, mute := range []any{-1, 1001, 1000, 999, 0, 0, -2} {
		insertChatPageFixture(t, db, fmt.Sprintf("m%d", i), "", 100, 0, 0, mute, 0)
	}
	p := ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 1, Muted: boolPtr(true)}, StoreRef: "/fixture"}
	first, err := db.ListChatsPage(p)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(chatIDs(first.Chats), []string{"m0"}) || !first.HasMore {
		t.Fatalf("incoherent clock: calls=%d page=%+v", calls, first)
	}
	p.Cursor = *first.NextCursor
	fixed = time.Unix(1001, 0)
	end, err := db.ListChatsPage(p)
	if err != nil || len(end.Chats) != 0 || end.HasMore {
		t.Fatalf("expiry should remove next row: %+v %v", end, err)
	}
	if got := collectChatPages(t, db, ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 20, Muted: boolPtr(false)}}); len(got) != 6 {
		t.Fatalf("unmuted normalization: %v", chatIDs(got))
	}
	// Anchor deletion and moves across a boundary are live, not exactly-once.
	for _, jid := range []string{"a", "b", "c", "d"} {
		insertChatPageFixture(t, db, jid, "selected", 100, 0, 0, 0, 0)
	}
	p = ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 2, Query: "selected"}}
	first, err = db.ListChatsPage(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Cursor = *first.NextCursor
	for _, stmt := range []string{`DELETE FROM chats WHERE jid='b'`, `UPDATE chats SET last_message_ts=99 WHERE jid='a'`, `UPDATE chats SET pinned=1 WHERE jid='c'`, `INSERT INTO chats(jid,kind,name,last_message_ts) VALUES('e','dm','selected',98)`} {
		if _, err := db.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if got := chatIDs(collectChatPages(t, db, p)); !reflect.DeepEqual(got, []string{"d", "a", "e"}) {
		t.Fatalf("live got=%v", got)
	}
	for _, column := range []string{"name='removed'", "unread=0", "archived=1"} {
		// Changes in matching may remove still-unvisited rows without changing the key.
		p = ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 1, Query: "selected", Unread: boolPtr(true), Archived: boolPtr(false)}}
		if _, err := db.sql.Exec(`UPDATE chats SET name='selected',unread=1,archived=0,pinned=0,last_message_ts=100 WHERE jid IN ('a','d','e')`); err != nil {
			t.Fatal(err)
		}
		first, err = db.ListChatsPage(p)
		if err != nil {
			t.Fatal(err)
		}
		p.Cursor = *first.NextCursor
		if _, err := db.sql.Exec(`UPDATE chats SET ` + column + ` WHERE jid='d'`); err != nil {
			t.Fatal(err)
		}
		if got := chatIDs(collectChatPages(t, db, p)); !reflect.DeepEqual(got, []string{"e"}) {
			t.Fatalf("membership %s got=%v", column, got)
		}
	}
}

func TestChatsPageQueryPlanAndCost(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.sql.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO chats(jid,kind,name,last_message_ts,pinned) SELECT printf('j%05d',x),'dm','fixture',x%100,x%2 FROM n`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []ChatListFilter{{Limit: 21}, {Limit: 21, Query: "fixture", Muted: boolPtr(false), Pinned: boolPtr(true)}} {
		q, args := chatsPageQuery(f, &chatKey{Pin: 1, TS: 99, JID: "j00001"}, 1000)
		if strings.Contains(q, "OFFSET") {
			t.Fatal("offset pagination")
		}
		rows, err := db.sql.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		t.Logf("10000 chats plan: %s", strings.Join(plan, "; "))
		start := time.Now()
		p := ListChatsPageParams{ChatListFilter: f}
		p.Limit = 20
		for i := 0; i < 20; i++ {
			page, err := db.ListChatsPage(p)
			if err != nil || len(page.Chats) != 20 {
				t.Fatalf("cost probe: %v", err)
			}
			p.Cursor = *page.NextCursor
		}
		t.Logf("10000 chats, 20 pages of 20, query=%q: %s (SQLite scan/sort remains unbounded)", f.Query, time.Since(start))
	}
}

func TestChatsPagePinNullNormalization(t *testing.T) {
	db := openTestDB(t)
	// Current schema forbids NULL flags; test defensive normalization without schema changes.
	for _, pin := range []any{nil, 0, 1, -1, 2} {
		var got int
		if err := db.sql.QueryRow(`SELECT `+chatPinKeySQL+` FROM (SELECT ? AS pinned)`, pin).Scan(&got); err != nil {
			t.Fatal(err)
		}
		want := 1
		if pin == nil || pin == 0 {
			want = 0
		}
		if got != want {
			t.Fatalf("pin=%v key=%d", pin, got)
		}
	}
}

func TestChatsPageLivePinRepeatActivitySkip(t *testing.T) {
	db := openTestDB(t)
	for _, jid := range []string{"a", "b", "c", "d"} {
		insertChatPageFixture(t, db, jid, "", 100, 1, 0, 0, 0)
	}
	p := ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 2}}
	first, err := db.ListChatsPage(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Cursor = *first.NextCursor
	// A visited row unpinned moves to the unvisited side; an unvisited row with
	// newer activity moves to the visited side. Both obey the same tuple predicate.
	if _, err := db.sql.Exec(`UPDATE chats SET pinned=0 WHERE jid='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE chats SET last_message_ts=101 WHERE jid='c'`); err != nil {
		t.Fatal(err)
	}
	if got := chatIDs(collectChatPages(t, db, p)); !reflect.DeepEqual(got, []string{"d", "a"}) {
		t.Fatalf("live pin/activity: %v", got)
	}
}

func TestChatsPageGeneratedCursorSizeAndTextIdentity(t *testing.T) {
	db := openTestDB(t)
	jid := `PRIVATE textual JID "<>&` + "🙂\\\x00"
	insertChatPageFixture(t, db, jid, "", 200, 0, 0, 0, 0)
	insertChatPageFixture(t, db, "next", "", 100, 0, 0, 0, 0)
	p := ListChatsPageParams{ChatListFilter: ChatListFilter{Limit: 1}}
	page, err := db.ListChatsPage(p)
	if err != nil {
		t.Fatal(err)
	}
	token := *page.NextCursor
	if len(token) >= 512 {
		t.Fatalf("ordinary token should be compact: %d", len(token))
	}
	cursor, err := decodeChatsCursor(token)
	if err != nil || cursor.Key.JID != jid {
		t.Fatalf("text identity lost: %v", err)
	}
	p.Cursor = token
	if got := chatIDs(collectChatPages(t, db, p)); !reflect.DeepEqual(got, []string{"next"}) {
		t.Fatalf("text key continuation: %v", got)
	}
	if _, err := db.sql.Exec(`UPDATE chats SET jid=? WHERE jid=?`, strings.Repeat("x", MaxChatsCursorBytes), jid); err != nil {
		t.Fatal(err)
	}
	p.Cursor = ""
	page, err = db.ListChatsPage(p)
	var callerError *ChatsCursorError
	if err == nil || errors.As(err, &callerError) || page.NextCursor != nil || len(page.Chats) != 0 {
		t.Fatalf("bad stored-data failure: page=%+v err=%v", page, err)
	}
}
