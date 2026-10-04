package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func collectSearchPages(t *testing.T, db *DB, p SearchMessagesPageParams) []Message {
	t.Helper()
	var all []Message
	for n := 0; n < 100; n++ {
		page, err := db.SearchMessagesPage(p)
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
		p.Cursor = *page.NextCursor
	}
	t.Fatal("pagination did not end")
	return nil
}

func TestSearchPageMatchingFiltersSnippetsAndBoundaries(t *testing.T) {
	db := openTestDB(t)
	for _, chat := range []string{"pn", "lid", "other"} {
		if err := db.UpsertChat(chat, "dm", "Fixture needle", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 19; i++ {
		chat := "pn"
		if i%2 == 0 {
			chat = "lid"
		}
		if i == 18 {
			chat = "other"
		}
		p := UpsertMessageParams{ChatJID: chat, MsgID: fmt.Sprintf("m%02d", i), SenderJID: "sender", Timestamp: time.Unix(100, 0), Text: "needle report 100% some_thing", DisplayText: "needle display", MediaCaption: "needle caption", Filename: "needle.txt", IsForwarded: i%3 != 0, Revoked: i == 17}
		if i%2 == 0 {
			p.MediaType = "document"
		}
		if i == 16 {
			p.SenderJID = "other"
		}
		if i == 0 {
			p.Timestamp = time.Unix(-1, 0)
		}
		if i == 1 {
			p.Timestamp = time.Unix(0, 0)
		}
		if err := db.UpsertMessage(p); err != nil {
			t.Fatal(err)
		}
		if i%4 != 0 {
			if err := db.SetStarred(SetStarredParams{ChatJID: chat, MsgID: p.MsgID, Starred: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, before := time.Unix(99, 0), time.Unix(101, 0)
	filters := []SearchMessagesParams{
		{Query: "needle"}, {Query: "needle report"}, {Query: "100%"}, {Query: "some_thing"}, {Query: "needle", ChatJIDs: []string{"lid", "pn"}, From: "sender", After: &after, Before: &before, HasMedia: true, Type: " DOCUMENT ", Forwarded: true, Starred: true},
		{Query: "needle", Type: "text"}, {Query: "needle", ChatJID: "other"}, {Query: "absent"},
	}
	for _, params := range filters {
		params.Limit = 200
		legacy, err := db.SearchMessages(params)
		if err != nil {
			t.Fatal(err)
		}
		for _, asc := range []bool{false, true} {
			expected := append([]Message(nil), legacy...)
			sort.Slice(expected, func(i, j int) bool {
				a, b := expected[i], expected[j]
				if asc {
					return a.rowTS < b.rowTS || a.rowTS == b.rowTS && a.rowID < b.rowID
				}
				return a.rowTS > b.rowTS || a.rowTS == b.rowTS && a.rowID > b.rowID
			})
			for _, limit := range []int{1, 2, 3, max(1, len(expected)), len(expected) + 1, 200} {
				p := SearchMessagesPageParams{SearchMessagesParams: params, StoreRef: "fixture", Asc: asc}
				p.Limit = limit
				got := collectSearchPages(t, db, p)
				if !reflect.DeepEqual(pageIDs(got), pageIDs(expected)) {
					t.Fatalf("%+v asc=%v limit=%d: %v vs %v", params, asc, limit, pageIDs(got), pageIDs(expected))
				}
				for i, m := range got {
					if m.Text != expected[i].Text || m.Snippet != expected[i].Snippet {
						t.Fatal("legacy text/snippet changed")
					}
					if m.MsgID == "m17" {
						t.Fatal("tombstone leaked")
					}
				}
			}
		}
	}
	for _, limit := range []int{0, -1, 201} {
		if _, err := db.SearchMessagesPage(SearchMessagesPageParams{SearchMessagesParams: SearchMessagesParams{Query: "needle", Limit: limit}}); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}

func TestSearchPageScopeEngineOperationAndLiveAnchor(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertChat("pn", "dm", "Fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: "pn", MsgID: fmt.Sprint(i), Timestamp: time.Unix(100, 0), Text: "needle report"}); err != nil {
			t.Fatal(err)
		}
	}
	p := SearchMessagesPageParams{StoreRef: "fixture", ChatIdentity: "pn", SearchMessagesParams: SearchMessagesParams{Query: "needle", ChatJIDs: []string{"pn", "lid"}, Limit: 1}}
	first, err := db.SearchMessagesPage(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Cursor = *first.NextCursor
	mismatch := func(q SearchMessagesPageParams) {
		t.Helper()
		_, err := db.SearchMessagesPage(q)
		var ce *MessagesCursorError
		if !errors.As(err, &ce) || !ce.Mismatch {
			t.Fatalf("expected scope mismatch: %v", err)
		}
	}
	before, after := time.Unix(200, 0), time.Unix(1, 0)
	for _, change := range []func(*SearchMessagesPageParams){
		func(q *SearchMessagesPageParams) { q.Query = "report" }, func(q *SearchMessagesPageParams) { q.StoreRef = "other" }, func(q *SearchMessagesPageParams) { q.ChatIdentity = "lid" }, func(q *SearchMessagesPageParams) { q.ChatJIDs = []string{"pn"} }, func(q *SearchMessagesPageParams) { q.From = "sender" }, func(q *SearchMessagesPageParams) { q.Before = &before }, func(q *SearchMessagesPageParams) { q.After = &after }, func(q *SearchMessagesPageParams) { q.Asc = true }, func(q *SearchMessagesPageParams) { q.Forwarded = true }, func(q *SearchMessagesPageParams) { q.Starred = true }, func(q *SearchMessagesPageParams) { q.Type = "image" }, func(q *SearchMessagesPageParams) { q.HasMedia = true },
	} {
		q := p
		change(&q)
		mismatch(q)
	}
	// Engine scope rejection happens before SQL, even if the current engine could
	// not execute its matching query. No FTS table is required for this proof.
	enabled := db.ftsEnabled
	db.ftsEnabled = !enabled
	mismatch(p)
	db.ftsEnabled = enabled
	list, err := db.ListMessagesPage(ListMessagesPageParams{StoreRef: "fixture", ChatIdentity: "pn", ListMessagesParams: ListMessagesParams{ChatJIDs: []string{"pn", "lid"}, Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	q := p
	q.Cursor = *list.NextCursor
	mismatch(q)
	_, err = db.ListMessagesPage(ListMessagesPageParams{StoreRef: "fixture", ChatIdentity: "pn", Cursor: p.Cursor, ListMessagesParams: ListMessagesParams{ChatJIDs: p.ChatJIDs, Limit: 1}})
	var ce *MessagesCursorError
	if !errors.As(err, &ce) || !ce.Mismatch {
		t.Fatal(err)
	}
	q = p
	q.ChatJIDs = []string{"lid", "pn", "pn"}
	q.Limit = 2
	if _, err := db.SearchMessagesPage(q); err != nil {
		t.Fatal("equivalent filters rejected", err)
	}
	q = p
	q.Query = " needle "
	_, err = db.SearchMessagesPage(q)
	if enabled && err != nil {
		t.Fatal("FTS whitespace normalization", err)
	}
	if !enabled && err == nil {
		t.Fatal("LIKE whitespace changed matching scope")
	}
	q = p
	q.From = " sender "
	if q.cursorScope(enabled) == func() string { r := q; r.From = "sender"; return r.cursorScope(enabled) }() {
		t.Fatal("raw LIKE/FTS sender SQL argument lost in scope")
	}
	for _, token := range []string{"bad", ""} {
		if token == "" {
			continue
		}
		q = p
		q.Cursor = token
		_, err := db.SearchMessagesPage(q)
		if !errors.As(err, &ce) || ce.Mismatch {
			t.Fatal(err)
		}
	}
	if err := db.MarkMessageRevoked("pn", first.Messages[0].MsgID); err != nil {
		t.Fatal(err)
	}
	got := collectSearchPages(t, db, p)
	if !reflect.DeepEqual(pageIDs(got), []string{"3", "2", "1", "0"}) {
		t.Fatal(pageIDs(got))
	}
	// Matching can change between pages; the cursor contains no retained set.
	if err := db.UpdateMessageText("pn", "2", "absent"); err != nil {
		t.Fatal(err)
	}
	got = collectSearchPages(t, db, p)
	if !reflect.DeepEqual(pageIDs(got), []string{"3", "1", "0"}) {
		t.Fatal(pageIDs(got))
	}
}

func TestSearchPagePreservesDefaultRankVersusTime(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertChat("chat", "dm", "Fixture", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []UpsertMessageParams{
		{ChatJID: "chat", MsgID: "older-relevant", Timestamp: time.Unix(100, 0), Text: "needle needle needle needle"},
		{ChatJID: "chat", MsgID: "newer", Timestamp: time.Unix(200, 0), Text: "needle less relevant message with many other words filler padding text"},
	} {
		if err := db.UpsertMessage(p); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := db.SearchMessages(SearchMessagesParams{Query: "needle", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := "newer"
	if db.HasFTS() {
		first = "older-relevant"
	}
	if legacy[0].MsgID != first {
		t.Fatalf("default search lost rank/order: %v", pageIDs(legacy))
	}
	page, err := db.SearchMessagesPage(SearchMessagesPageParams{SearchMessagesParams: SearchMessagesParams{Query: "needle", Limit: 2}})
	if err != nil || !reflect.DeepEqual(pageIDs(page.Messages), []string{"newer", "older-relevant"}) {
		t.Fatal(page, err)
	}
}
