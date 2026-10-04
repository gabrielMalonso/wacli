package store

import (
	"testing"
	"time"
)

func TestCountConversationMessagesDeduplicatesVerifiedAlias(t *testing.T) {
	db := openTestDB(t)
	pn, lid := "15550000001@s.whatsapp.net", "100000000001@lid"
	for _, row := range []struct{ chat, id string }{
		{pn, "shared"}, {lid, "shared"}, {pn, "phone-only"}, {lid, "lid-only"},
		{"unrelated@g.us", "unrelated"},
	} {
		if err := db.UpsertChat(row.chat, "dm", "fixture", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: row.chat, MsgID: row.id, Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		chat, alias string
		want        int64
	}{
		{pn, lid, 3}, {lid, pn, 3}, {pn, "", 2}, {pn, pn, 2}, {"missing@g.us", "", 0},
	} {
		got, err := db.CountConversationMessages(tc.chat, tc.alias)
		if err != nil || got != tc.want {
			t.Fatalf("CountConversationMessages(%q, %q) = %d, %v; want %d", tc.chat, tc.alias, got, err, tc.want)
		}
	}
	if err := db.MigrateLIDToPN(lid, pn); err != nil {
		t.Fatal(err)
	}
	if got, err := db.CountConversationMessages(pn, lid); err != nil || got != 3 {
		t.Fatalf("after merge count = %d, %v; want 3", got, err)
	}
	if _, err := db.CountConversationMessages(" ", lid); err == nil {
		t.Fatal("empty chat must fail")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"", lid} {
		if _, err := db.CountConversationMessages(pn, alias); err == nil {
			t.Fatal("closed database must fail")
		}
	}
}
