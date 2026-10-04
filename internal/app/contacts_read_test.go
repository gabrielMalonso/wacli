package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/store"
)

func newContactPageFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := db.UpsertContact(fmt.Sprintf("1555000%d@s.whatsapp.net", i), "", "", "Tie", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY,pn TEXT UNIQUE); INSERT INTO whatsmeow_lid_map VALUES ('9000','15550000'); CREATE TABLE whatsmeow_device (jid TEXT,lid TEXT,noise_key BLOB); INSERT INTO whatsmeow_device VALUES ('15550005:3@s.whatsapp.net','9005@lid',X'50524956415445');`)
	session.Close()
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, dir
}
func fixtureSQL(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func TestContactCursorMappingScopeAndLiveChanges(t *testing.T) {
	a, dir := newContactPageFixture(t)
	ctx := context.Background()
	options := ContactReadOptions{Operation: ContactList, Limit: 1, Paginate: true}
	first, err := a.ReadContacts(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	options.Cursor = *first.NextCursor
	// A changed map for another identity is live data, not a global cursor stamp.
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `INSERT INTO contacts (jid,full_name,updated_at) VALUES ('9001@lid','Tie',1)`)
	fixtureSQL(t, filepath.Join(dir, "session.db"), `INSERT INTO whatsmeow_lid_map VALUES ('9001','15550001')`)
	second, err := a.ReadContacts(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if second.Contacts[0].JID != "15550001@s.whatsapp.net" {
		t.Fatalf("live merge: %+v", second)
	}
	// Move an unvisited LID group behind the anchor: omission is permitted.
	fixtureSQL(t, filepath.Join(dir, "session.db"), `DELETE FROM whatsmeow_lid_map WHERE lid='9000'; UPDATE whatsmeow_lid_map SET pn='15550000' WHERE lid='9001'`)
	options.Cursor = *second.NextCursor
	third, err := a.ReadContacts(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if third.Contacts[0].JID != "15550002@s.whatsapp.net" {
		t.Fatalf("live boundary: %+v", third)
	}
	// Availability changes alter the semantic scope and fail before main matching.
	fixtureSQL(t, filepath.Join(dir, "session.db"), `DROP TABLE whatsmeow_lid_map`)
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `ALTER TABLE contacts RENAME TO intentionally_unqueryable_contacts`)
	_, err = a.ReadContacts(ctx, options)
	var cursor *ContactsCursorError
	if !errors.As(err, &cursor) || !cursor.Mismatch {
		t.Fatalf("scope before matching: %v", err)
	}
}
func TestContactCursorQueryPairChangePrecedesMatching(t *testing.T) {
	a, dir := newContactPageFixture(t)
	ctx := context.Background()
	// One original matching row plus an exact query-resolved canonical group.
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `UPDATE contacts SET full_name='9000@lid' WHERE jid='15550004@s.whatsapp.net'`)
	p := ContactReadOptions{Operation: ContactSearch, Query: "9000@lid", Limit: 1, Paginate: true}
	page, err := a.ReadContacts(ctx, p)
	if err != nil || page.NextCursor == nil {
		t.Fatalf("%v %+v", err, page)
	}
	p.Cursor = *page.NextCursor
	fixtureSQL(t, filepath.Join(dir, "session.db"), `UPDATE whatsmeow_lid_map SET pn='15550003' WHERE lid='9000'`)
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `ALTER TABLE contacts RENAME TO intentionally_unqueryable_contacts`)
	_, err = a.ReadContacts(ctx, p)
	var cursor *ContactsCursorError
	if !errors.As(err, &cursor) || !cursor.Mismatch {
		t.Fatalf("query pair scope before matching: %v", err)
	}
}
func TestContactReadConnectionIsPublicOnlyReadonlyAndClosed(t *testing.T) {
	a, dir := newContactPageFixture(t)
	ctx := context.Background()
	conn, owner, err := a.DB().OpenContactReadConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer conn.Close()
	if err := registerContactParsers(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS identity", readOnlySessionURI(filepath.Join(dir, "session.db"))); err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(dc any) error {
		sqlite := dc.(*sqlite3.SQLiteConn)
		sqlite.RegisterAuthorizer(func(op int, table, column, database string) int {
			if op == sqlite3.SQLITE_READ && database == "identity" {
				if table == "whatsmeow_device" && column != "jid" && column != "lid" {
					return sqlite3.SQLITE_DENY
				}
				if table == "whatsmeow_lid_map" && column != "lid" && column != "pn" {
					return sqlite3.SQLITE_DENY
				}
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sources, err := inspectContactIdentitySources(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := contactQueryIdentity(ctx, tx, sources, "9005@lid"); err != nil {
		t.Fatal(err)
	}
	q, args := contactRowsQuery(sources, ContactReadOptions{Operation: ContactSearch, Query: "Tie"})
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 6 {
		t.Fatalf("public projection count=%d error=%v", count, err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE identity.whatsmeow_lid_map SET pn='fake'"); err == nil {
		t.Fatal("identity source was writable")
	}
	if _, err := tx.QueryContext(ctx, "SELECT noise_key FROM identity.whatsmeow_device"); err == nil {
		t.Fatal("secret read authorizer did not guard fixture")
	}
	tx.Rollback()
	conn.Close()
	owner.Close()
	drivers := sql.Drivers()
	for i := 0; i < 4; i++ {
		if _, err := a.ReadContacts(ctx, ContactReadOptions{Operation: ContactList, Limit: 20, Paginate: true}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(drivers, sql.Drivers()) {
		t.Fatal("per-call driver registry growth")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.ReadContacts(canceled, ContactReadOptions{Operation: ContactList, Limit: 20, Paginate: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	// No transaction survives a call/cancellation and blocks subsequent writes.
	fixtureSQL(t, filepath.Join(dir, "session.db"), `UPDATE whatsmeow_lid_map SET pn='15550100' WHERE lid='9000'`)
	assertNoAppSQLiteSidecars(t, filepath.Join(dir, "session.db"))
}
func TestContactGroupManyVariantsHasDeterministicMerge(t *testing.T) {
	a, dir := newContactPageFixture(t)
	for i := 1; i <= 200; i++ {
		fixtureSQL(t, filepath.Join(dir, "wacli.db"), `INSERT INTO contacts (jid,system_name,full_name,updated_at) VALUES (?,?,?,?)`, fmt.Sprintf("15550000:%d@s.whatsapp.net", i), "Split system", fmt.Sprintf("Tie %03d", i), i)
	}
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `INSERT INTO contact_aliases (jid,alias,updated_at) VALUES ('15550000:200@s.whatsapp.net','Nonchosen alias',1)`)
	p := ContactReadOptions{Operation: ContactSearch, Query: "Nonchosen alias", Limit: 20, Paginate: true}
	page, err := a.ReadContacts(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Contacts) != 1 || page.Contacts[0].JID != "15550000@s.whatsapp.net" || page.Contacts[0].Alias != "Nonchosen alias" || page.Contacts[0].SystemName != "Split system" || cap(page.Contacts) > 21 {
		t.Fatalf("group %+v", page)
	}
}
func TestContactsCursorStoredInvalidUTF8(t *testing.T) {
	_, err := contactsPageFromHeap(contactHeap{{JID: "A" + string([]byte{0xff}), Name: "A"}, {JID: "B", Name: "B"}}, ContactReadOptions{Operation: ContactList, Limit: 1, Paginate: true}, (contactScope{}).hash())
	if err == nil {
		t.Fatal("invalid UTF8 silently changed stored continuation key")
	}
}

func BenchmarkReadContacts(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			dir := seedContactBenchmark(b, n)
			a, err := New(Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				b.Fatal(err)
			}
			defer a.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err := a.ReadContacts(context.Background(), ContactReadOptions{Operation: ContactList, Limit: 20, Paginate: true})
				if err != nil {
					b.Fatal(err)
				}
				if len(page.Contacts) != 20 || !page.HasMore {
					b.Fatalf("page %+v", page)
				}
				b.ReportMetric(float64(cap(page.Contacts)), "retained_slots")
			}
		})
	}
}
func seedContactBenchmark(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		tb.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		tb.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("ATTACH DATABASE ? AS identity", filepath.Join(dir, "session.db")); err != nil {
		tb.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE identity.whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE NOT NULL); CREATE TABLE identity.whatsmeow_device(jid TEXT,lid TEXT,noise_key BLOB); INSERT INTO identity.whatsmeow_device VALUES ('15550000000:4@s.whatsapp.net','90000000000@lid',X'50524956415445')`); err != nil {
		tb.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	contact, err := tx.Prepare(`INSERT INTO contacts(jid,phone,full_name,push_name,updated_at) VALUES (?,?,?,?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer contact.Close()
	mapping, err := tx.Prepare(`INSERT INTO identity.whatsmeow_lid_map VALUES (?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer mapping.Close()
	alias, err := tx.Prepare(`INSERT INTO contact_aliases(jid,alias,updated_at) VALUES (?,?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer alias.Close()
	for i := 0; i < n; i++ {
		pn := fmt.Sprint(15550000000 + int64(i))
		lid := fmt.Sprint(90000000000 + int64(i))
		name := fmt.Sprintf("Fixture Ω %03d", i%307)
		if _, err := contact.Exec(pn+"@s.whatsapp.net", pn, name, "Original push name", i+1); err != nil {
			tb.Fatal(err)
		}
		if i%2 == 0 {
			if _, err := contact.Exec(lid+"@lid", lid, "LID hidden name", "LID push", i+2); err != nil {
				tb.Fatal(err)
			}
			if _, err := mapping.Exec(lid, pn); err != nil {
				tb.Fatal(err)
			}
		}
		if i%7 == 0 {
			if _, err := alias.Exec(pn+"@s.whatsapp.net", "PN alias "+name, 1); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return dir
}

// Opt-in query plan evidence, kept out of normal test output.
func TestContactReadQueryPlan(t *testing.T) {
	if os.Getenv("WACLI_CONTACT_QUERY_PLAN") != "1" {
		t.Skip("query plan evidence is opt-in")
	}
	dir := seedContactBenchmark(t, 10000)
	a, err := New(Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	conn, owner, err := a.DB().OpenContactReadConn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer conn.Close()
	if err := registerContactParsers(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "ATTACH DATABASE ? AS identity", readOnlySessionURI(filepath.Join(dir, "session.db"))); err != nil {
		t.Fatal(err)
	}
	for _, op := range []ContactOperation{ContactList, ContactSearch} {
		q, args := contactRowsQuery(contactIdentitySources{Map: true, Own: true}, ContactReadOptions{Operation: op, Query: "Fixture"})
		rows, err := conn.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s", op, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestContactContinuationDoesNotNeedSurvivingAnchor(t *testing.T) {
	a, dir := newContactPageFixture(t)
	ctx := context.Background()
	p := ContactReadOptions{Operation: ContactList, Limit: 2, Paginate: true}
	first, err := a.ReadContacts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Cursor = *first.NextCursor
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `DELETE FROM contacts WHERE jid=?`, first.Contacts[1].JID)
	next, err := a.ReadContacts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if next.Contacts[0].JID != "15550002@s.whatsapp.net" {
		t.Fatalf("deleted anchor %+v", next)
	}
	// Name changes are live; a visited row moved beyond the boundary can repeat.
	fixtureSQL(t, filepath.Join(dir, "wacli.db"), `UPDATE contacts SET full_name='Z moved' WHERE jid=?`, first.Contacts[0].JID)
	p.Limit = 200
	last, err := a.ReadContacts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if last.Contacts[len(last.Contacts)-1].JID != first.Contacts[0].JID {
		t.Fatalf("live name %+v", last)
	}
}
