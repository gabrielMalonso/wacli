package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"go.mau.fi/whatsmeow/types"
)

func contactIdentityFixtureSQL(t *testing.T, dir, query string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestContactsRejectIncompatibleIdentityBeforeMetadataWrites(t *testing.T) {
	for _, schema := range []string{
		`ALTER TABLE whatsmeow_lid_map RENAME COLUMN pn TO wrong_pn`,
		`CREATE TABLE whatsmeow_device(wrong_jid TEXT,lid TEXT)`,
	} {
		t.Run(schema, func(t *testing.T) {
			dir := seedContactReadStore(t, true)
			contactIdentityFixtureSQL(t, dir, schema)
			before := snapshotLocalStore(t, dir)
			commands := [][]string{{"contacts", "show", "--jid", contactLID}, {"contacts", "show", "--jid", contactPN}, {"contacts", "resolve", contactLID, contactPN}, {"contacts", "list"}, {"contacts", "search", contactLID}}
			for _, command := range commands {
				stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir, "--read-only", "--agent"}, command...)...)
				if stdout != "" || commandExitCode(err) != 4 {
					t.Fatalf("%v stdout=%s stderr=%s err=%v", command, stdout, stderr, err)
				}
				env := decodeAgentTest(t, stderr)
				if env.Error.Code != "store_unavailable" || env.Error.Message != "Selected local identity state cannot be read." {
					t.Fatalf("identity error %s", stderr)
				}
				stdout, stderr, err = runAgentTest(t, append([]string{"--store", dir, "--read-only", "--json"}, command...)...)
				if err == nil || stdout != "" || !strings.Contains(stderr, "incompatible public") {
					t.Fatalf("legacy command=%v %s %s %v", command, stdout, stderr, err)
				}
			}
			a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, jid := range []string{contactPN, contactLID} {
				if _, err := contactMetadataJIDs(context.Background(), a, jid); err == nil {
					t.Fatal("incompatible schema allowed metadata selection")
				}
			}
			a.Close()
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("failed readonly identity reads modified fixture")
			}
			// The write command must stop before changing either half of local metadata.
			for _, tc := range []struct {
				kind string
				args []string
			}{
				{"alias", []string{"set", "--jid", contactLID, "--alias", "must not be written"}},
				{"tags", []string{"add", "--jid", contactPN, "--tag", "must not be written"}},
			} {
				flags := &rootFlags{storeDir: dir}
				cmd := newContactsAliasCmd(flags)
				if tc.kind == "tags" {
					cmd = newContactsTagsCmd(flags)
				}
				cmd.SetArgs(tc.args)
				if err := cmd.Execute(); err == nil {
					t.Fatal("metadata write ignored incompatible identity schema")
				}
			}
			db := openSystemImportStore(t, dir)
			defer db.Close()
			for _, jid := range []string{contactPN, contactLID} {
				c, err := db.GetContact(jid)
				if err != nil || c.Alias != "" || len(c.Tags) != 0 {
					t.Fatalf("metadata changed %+v %v", c, err)
				}
			}
		})
	}
}

func TestContactsLegacyIdentitySourcesRemainUnknownOrMapped(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		mapped       bool
	}{
		{name: "missing session"},
		{name: "missing public tables", schema: `DROP TABLE whatsmeow_lid_map`},
		{name: "pre-LID device", schema: `DROP TABLE whatsmeow_lid_map;CREATE TABLE whatsmeow_device(jid TEXT);INSERT INTO whatsmeow_device VALUES('15550001001:3@s.whatsapp.net')`},
		{name: "pre-LID with map", schema: `CREATE TABLE whatsmeow_device(jid TEXT);INSERT INTO whatsmeow_device VALUES('15550001001:3@s.whatsapp.net')`, mapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedContactReadStore(t, tc.schema != "")
			if tc.schema != "" {
				contactIdentityFixtureSQL(t, dir, tc.schema)
			}
			before := snapshotLocalStore(t, dir)
			for _, jid := range []string{contactPN, contactLID} {
				shown := runContactsShow(t, dir, jid)
				want := jid
				if tc.mapped {
					want = contactPN
				}
				if shown.JID != want || (!tc.mapped && jid == contactLID && shown.Phone != "") {
					t.Fatalf("show %+v want %s", shown, want)
				}
			}
			stdout, stderr, err := runAgentTest(t, "--store", dir, "--read-only", "--agent", "contacts", "resolve", contactPN, contactLID, "9999999@lid")
			if err != nil || stderr != "" {
				t.Fatalf("resolve %s %s %v", stdout, stderr, err)
			}
			var data struct{ Resolutions []contactResolution }
			if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Resolutions) != 3 || data.Resolutions[0].Resolved != tc.mapped || data.Resolutions[1].Resolved != tc.mapped || data.Resolutions[2].Resolved {
				t.Fatalf("resolve %+v", data)
			}
			a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, jid := range []string{contactPN, contactLID} {
				selected, err := contactMetadataJIDs(context.Background(), a, jid)
				want := []string{jid}
				if tc.mapped {
					want = []string{contactPN, contactLID}
				}
				if err != nil || !reflect.DeepEqual(selected, want) {
					t.Fatalf("metadata %v %v want %v", selected, err, want)
				}
			}
			a.Close()
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("legacy identity reads modified fixture")
			}
		})
	}
}

type failingContactResolver struct{ failure error }

func (f failingContactResolver) ResolveLIDToPN(_ context.Context, _ types.JID) (types.JID, error) {
	return types.EmptyJID, f.failure
}
func (f failingContactResolver) ResolvePNToLID(_ context.Context, _ types.JID) (types.JID, error) {
	return types.EmptyJID, f.failure
}
func (f failingContactResolver) ResolveChatName(context.Context, types.JID, string) string { return "" }

func TestContactsPropagateIdentityLookupFailure(t *testing.T) {
	failure := errors.New("synthetic public identity SQL failure")
	resolver := failingContactResolver{failure: failure}
	for _, jid := range []string{contactPN, contactLID} {
		if _, err := resolveContactIdentity(context.Background(), resolver, jid); !errors.Is(err, failure) {
			t.Fatalf("resolve error %v", err)
		}
		if _, err := contactIdentityJIDs(context.Background(), resolver, jid); !errors.Is(err, failure) {
			t.Fatalf("metadata error %v", err)
		}
	}
	a, err := app.New(app.Options{StoreDir: seedContactReadStore(t, true), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := contactsForDisplay(context.Background(), a, resolver); !errors.Is(err, failure) {
		t.Fatalf("display error %v", err)
	}
}
