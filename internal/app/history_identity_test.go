package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func historyIdentityFixture(t *testing.T, a *App, query string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func TestReadHistoryIdentitiesStrictOfflineScope(t *testing.T) {
	a := newTestApp(t)
	inputs := []string{"200:2@s.whatsapp.net", "300@lid", "400@g.us", "400@g.us"}
	got, err := a.ReadHistoryIdentities(context.Background(), inputs)
	if err != nil || len(got) != 3 || got[0].InputJID != "200@s.whatsapp.net" || got[0].AccountJID != "" || got[0].AliasJID != "" {
		t.Fatalf("missing session: %+v %v", got, err)
	}
	historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('100:2@s.whatsapp.net','500@lid'); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE); INSERT INTO whatsmeow_lid_map VALUES('300','200')`)
	got, err = a.ReadHistoryIdentities(context.Background(), inputs)
	if err != nil || got[0].AliasJID != "300@lid" || got[1].ChatJID != "200@s.whatsapp.net" || got[1].AliasJID != "300@lid" || got[2].AliasJID != "" || got[2].AccountJID != "100@s.whatsapp.net" {
		t.Fatalf("strict pair: %+v %v", got, err)
	}
	own, ownErr := a.ReadHistoryIdentities(context.Background(), []string{"100@s.whatsapp.net", "500@lid"})
	if ownErr != nil || own[0].AliasJID != "500@lid" || own[1].ChatJID != "100@s.whatsapp.net" {
		t.Fatalf("own pair: %+v %v", own, ownErr)
	}
	historyIdentityFixture(t, a, `DROP TABLE whatsmeow_lid_map`)
	if _, err = a.ReadHistoryIdentities(context.Background(), []string{"200@s.whatsapp.net"}); err == nil {
		t.Fatal("identity query failure became absent alias")
	}
}
func TestReadHistoryIdentitiesRejectsAmbiguousAccountAndBounds(t *testing.T) {
	a := newTestApp(t)
	historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('100@s.whatsapp.net',NULL),('101@s.whatsapp.net',NULL); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE)`)
	got, err := a.ReadHistoryIdentities(context.Background(), []string{"200@s.whatsapp.net"})
	if err != nil || got[0].AccountJID != "" {
		t.Fatalf("ambiguous account: %+v %v", got, err)
	}
	if _, err = a.ReadHistoryIdentities(context.Background(), make([]string, 201)); err == nil {
		t.Fatal("unbounded input")
	}
}

func TestReadHistoryIdentitiesNullableOwnLIDWithMappedPair(t *testing.T) {
	a := newTestApp(t)
	historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('100@s.whatsapp.net',NULL); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE); INSERT INTO whatsmeow_lid_map VALUES('500','100'),('300','200')`)
	got, err := a.ReadHistoryIdentities(context.Background(), []string{"100@s.whatsapp.net", "500@lid", "200@s.whatsapp.net"})
	if err != nil || got[0].AliasJID != "500@lid" || got[1].ChatJID != "100@s.whatsapp.net" || got[2].AliasJID != "300@lid" || got[0].AccountJID != "100@s.whatsapp.net" {
		t.Fatalf("nullable own LID rejected map: %+v %v", got, err)
	}
	// Multiple device rows cannot establish the selected public account, but a
	// null own pair must not poison a separately verified map pair.
	historyIdentityFixture(t, a, `INSERT INTO whatsmeow_device VALUES('101@s.whatsapp.net','501@lid')`)
	got, err = a.ReadHistoryIdentities(context.Background(), []string{"200@s.whatsapp.net"})
	if err != nil || got[0].AliasJID != "300@lid" || got[0].AccountJID != "" {
		t.Fatalf("mixed own facts: %+v %v", got, err)
	}
	historyIdentityFixture(t, a, `DROP TABLE whatsmeow_device`)
	if _, err = a.ReadHistoryIdentities(context.Background(), []string{"200@s.whatsapp.net"}); err == nil {
		t.Fatal("SQL failure became unknown account")
	}
}

func TestHistoryInputAndMappedIdentitySyntax(t *testing.T) {
	for _, input := range []string{"@lid", "123@", "123@g.us@extra", "123 @g.us", "123\n@g.us", "g.us"} {
		if _, err := ParseHistoryJID(input); err == nil {
			t.Fatalf("invalid input accepted: %q", input)
		}
		if _, err := PrepareBackfillOptions(BackfillOptions{ChatJID: input}); err == nil {
			t.Fatalf("invalid recovery input: %q", input)
		}
	}
	for _, pair := range []struct{ lid, pn, input string }{
		{"300@lid", "200", "200@s.whatsapp.net"},
		{"300", "200@s.whatsapp.net", "300@lid"},
	} {
		a := newTestApp(t)
		historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('100@s.whatsapp.net',NULL); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE)`)
		db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "session.db"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO whatsmeow_lid_map VALUES(?,?)`, pair.lid, pair.pn)
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.ReadHistoryIdentities(context.Background(), []string{pair.input}); err == nil {
			t.Fatal("corrupt public pair became verified scope")
		}
	}
}
