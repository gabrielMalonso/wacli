package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/types"
)

func TestContactResolverLegacySourcesAndUnknownPairs(t *testing.T) {
	for _, tc := range []struct {
		name, schema        string
		mapped, unavailable bool
	}{
		{name: "no public tables"},
		{name: "pre-LID device", schema: `CREATE TABLE whatsmeow_device(jid TEXT); INSERT INTO whatsmeow_device VALUES ('15550000:3@s.whatsapp.net')`},
		{name: "pre-LID with map", schema: `CREATE TABLE whatsmeow_device(jid TEXT); CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES ('9000','15550000')`, mapped: true},
		{name: "nullable pair", schema: `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES (NULL,NULL),('15550000:3@s.whatsapp.net',NULL); CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES ('9000',NULL),(NULL,'15550000')`},
		{name: "bad map", schema: `CREATE TABLE whatsmeow_lid_map(lid TEXT,wrong_pn TEXT)`, unavailable: true},
		{name: "bad device", schema: `CREATE TABLE whatsmeow_device(wrong_jid TEXT,lid TEXT)`, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dir := newContactPageFixture(t)
			path := filepath.Join(dir, "session.db")
			fixtureSQL(t, path, `DROP TABLE whatsmeow_lid_map; DROP TABLE whatsmeow_device;`+tc.schema)
			resolver, err := a.ReadOnlyContactResolver(context.Background())
			if tc.unavailable {
				if err == nil {
					t.Fatal("incompatible schema became unknown")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pn := types.JID{User: "15550000", Device: 3, Server: types.DefaultUserServer}
			lid := types.JID{User: "9000", Device: 3, Server: types.HiddenUserServer}
			gotPN, err := resolver.ResolveLIDToPN(context.Background(), lid)
			wantPN := lid
			if tc.mapped {
				wantPN = pn
			}
			if err != nil || gotPN != wantPN {
				t.Fatalf("LID => %s %v want %s", gotPN, err, wantPN)
			}
			gotLID, err := resolver.ResolvePNToLID(context.Background(), pn)
			wantLID := pn
			if tc.mapped {
				wantLID = lid
			}
			if err != nil || gotLID != wantLID {
				t.Fatalf("PN => %s %v want %s", gotLID, err, wantLID)
			}
			unknown := types.NewJID("9999", types.HiddenUserServer)
			got, err := resolver.ResolveLIDToPN(context.Background(), unknown)
			if err != nil || got != unknown {
				t.Fatalf("unknown => %s %v", got, err)
			}
			assertNoAppSQLiteSidecars(t, path)
		})
	}
}

func TestContactResolverOwnNullableDoesNotHideLaterPair(t *testing.T) {
	a, dir := newContactPageFixture(t)
	fixtureSQL(t, filepath.Join(dir, "session.db"), `DELETE FROM whatsmeow_device; INSERT INTO whatsmeow_device(jid,lid) VALUES (NULL,'9005@lid'),('15550000:1@s.whatsapp.net',NULL),('15550005:3@s.whatsapp.net','9005@lid'); INSERT INTO whatsmeow_lid_map VALUES ('9005','15550999')`)
	// Both contact error-aware and existing best-effort consumers keep own priority.
	best, err := a.ReadOnlyResolver()
	if err != nil {
		t.Fatal(err)
	}
	strict, err := a.ReadOnlyContactResolver(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pn := types.JID{User: "15550005", Device: 4, Server: types.DefaultUserServer}
	lid := types.JID{User: "9005", Device: 4, Server: types.HiddenUserServer}
	gotPN, e1 := strict.ResolveLIDToPN(context.Background(), lid)
	gotLID, e2 := strict.ResolvePNToLID(context.Background(), pn)
	if e1 != nil || e2 != nil || gotPN != pn || gotLID != lid || best.ResolveLIDToPN(context.Background(), lid) != pn || best.ResolvePNToLID(context.Background(), pn) != lid {
		t.Fatalf("own priority PN=%s/%v LID=%s/%v", gotPN, e1, gotLID, e2)
	}
}

func TestContactResolverQueryFailurePreservesBestEffortInterface(t *testing.T) {
	a, _ := newContactPageFixture(t)
	resolver, err := a.ReadOnlyContactResolver(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	best, err := a.readOnlySessionResolver()
	if err != nil {
		t.Fatal(err)
	}
	best.db.SetMaxOpenConns(1)
	conn, err := best.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(dc any) error {
		dc.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, database string) int {
			if op == sqlite3.SQLITE_READ && (table == "whatsmeow_lid_map" || table == "whatsmeow_device") {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	pn := types.NewJID("15550000", types.DefaultUserServer)
	lid := types.NewJID("9000", types.HiddenUserServer)
	if _, err := resolver.ResolveLIDToPN(context.Background(), lid); err == nil {
		t.Fatal("own query denial became unknown")
	}
	if _, err := resolver.ResolvePNToLID(context.Background(), pn); err == nil {
		t.Fatal("own query denial became unknown")
	}
	if best.ResolveLIDToPN(context.Background(), lid) != lid || best.ResolvePNToLID(context.Background(), pn) != pn {
		t.Fatal("best-effort interface changed")
	}
}

func TestContactResolverSQLStepErrorsAreNotUnknown(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			// A connection-local failing function injects SQLite stepping errors. The
			// test projection stands in for a source failing after schema inspection.
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "session.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("synthetic public identity read failure")
			if err := conn.Raw(func(dc any) error {
				return dc.(*sqlite3.SQLiteConn).RegisterFunc("r5_fail", func(string) (string, error) { return "", failure }, false)
			}); err != nil {
				t.Fatal(err)
			}
			schema := `CREATE VIEW whatsmeow_lid_map AS SELECT '9000' AS lid,r5_fail('map') AS pn`
			if own {
				schema = `CREATE VIEW whatsmeow_device AS SELECT '15550111@s.whatsapp.net' AS jid,'9011@lid' AS lid UNION ALL SELECT r5_fail('own'),'9000@lid'`
			}
			if _, err := conn.ExecContext(context.Background(), schema); err != nil {
				t.Fatal(err)
			}
			conn.Close()
			resolver := &contactSessionResolver{readOnlySessionResolver: &readOnlySessionResolver{db: db}, sources: contactIdentitySources{Map: !own, Own: own}}
			pn := types.NewJID("15550000", types.DefaultUserServer)
			lid := types.NewJID("9000", types.HiddenUserServer)
			if _, err := resolver.ResolveLIDToPN(context.Background(), lid); err == nil {
				t.Fatal("SQL step/Scan failure became unknown")
			}
			if _, err := resolver.ResolvePNToLID(context.Background(), pn); err == nil {
				t.Fatal("SQL step/Scan failure became unknown")
			}
		})
	}
}
