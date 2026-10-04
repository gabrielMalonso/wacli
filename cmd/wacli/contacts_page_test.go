package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

func agentContactsPage(t *testing.T, dir string, command []string, cursor string, limit int, detail string) (agentContacts, *string) {
	t.Helper()
	args := append([]string{"--store", dir, "--read-only", "--agent", "--limit", fmt.Sprint(limit), "--detail", detail}, command...)
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	stdout, stderr, err := runAgentTest(t, args...)
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	env := decodeAgentTest(t, stdout)
	var data agentContacts
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	p := env.Meta.Page
	if p == nil || p.Returned != len(data.Contacts) || p.HasMore != (p.NextCursor != nil) || env.Meta.Limit != limit || env.Meta.Completeness != "unknown" || env.Meta.Freshness != "unknown" {
		t.Fatalf("metadata: %s", stdout)
	}
	for _, c := range data.Contacts {
		if detail == "full" && c.Full == nil {
			t.Fatalf("full detail missing: %s", stdout)
		}
	}
	return data, p.NextCursor
}

func TestAgentContactsPagesCanonicalTiesWALAndLegacy(t *testing.T) {
	dir := seedContactReadStore(t, true)
	db := openSystemImportStore(t, dir)
	defer db.Close()
	session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 57; i++ {
		pn := fmt.Sprintf("1555100%04d@s.whatsapp.net", i)
		lid := fmt.Sprintf("9500%04d@lid", i)
		for _, jid := range []string{pn, lid} {
			if err := db.UpsertContact(jid, "not-a-phone", "", "Tie Ω %_\\ --agent", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := session.Exec("INSERT INTO whatsmeow_lid_map VALUES (?,?)", strings.TrimSuffix(lid, "@lid"), strings.TrimSuffix(pn, "@s.whatsapp.net")); err != nil {
			t.Fatal(err)
		}
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	before := snapshotLocalStore(t, dir)
	for _, command := range [][]string{{"contacts", "list"}, {"contacts", "search", "Tie Ω %_\\ --agent"}} {
		var got []string
		cursor := ""
		for page := 0; page < 40; page++ {
			limit, detail := 3, "compact"
			if page > 0 {
				limit, detail = 4, "full"
			}
			data, next := agentContactsPage(t, dir, command, cursor, limit, detail)
			for _, c := range data.Contacts {
				got = append(got, c.JID)
				if strings.HasSuffix(c.JID, "@lid") {
					t.Fatal("mapped LID survived dedup")
				}
			}
			if next == nil {
				break
			}
			cursor = *next
		}
		legacyArgs := append([]string{"--store", dir, "--json", "--limit", "100"}, command...)
		stdout, stderr, err := runAgentTest(t, legacyArgs...)
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		var legacy struct{ Data []store.Contact }
		if err := json.Unmarshal([]byte(stdout), &legacy); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, c := range legacy.Data {
			want = append(want, c.JID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("paged=%v legacy=%v", got, want)
		}
		if command[1] == "search" && len(got) != 57 {
			t.Fatalf("distinct count %d", len(got))
		}
	}
	after := snapshotLocalStore(t, dir)
	for _, files := range []map[string]localFileSnapshot{before, after} {
		for path := range files {
			if strings.HasSuffix(path, "-shm") {
				delete(files, path)
			}
		}
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read changed fixture/permissions/WAL/lock")
	}
	// The legacy default, explicit large limit and JSON list envelope survive.
	stdout, _, err := runAgentTest(t, "--store", dir, "--json", "contacts", "search", "Tie")
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct{ Data []store.Contact }
	json.Unmarshal([]byte(stdout), &legacy)
	if len(legacy.Data) != 50 {
		t.Fatalf("legacy default: %s", stdout)
	}
	stdout, _, err = runAgentTest(t, "--store", dir, "--agent", "contacts", "list")
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeAgentTest(t, stdout); env.Meta.Limit != 20 || env.Meta.Page.Returned != 20 {
		t.Fatalf("agent default %s", stdout)
	}
}

// The existing show reader supplies the old canonical view. Only its matching
// predicate is reproduced here, rather than duplicating identity/merge logic.
func referenceContactSearch(t *testing.T, a *app.App, query string) []store.Contact {
	t.Helper()
	ctx := context.Background()
	resolver, err := contactReadResolver(a)
	if err != nil {
		t.Fatal(err)
	}
	display, err := contactsForDisplay(ctx, a, resolver)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.DB().SearchContacts(query, math.MaxInt)
	if err != nil {
		t.Fatal(err)
	}
	matched := map[string]bool{}
	for _, c := range raw {
		matched[c.JID] = true
	}
	needle := strings.ToLower(query)
	qjid := resolveContactReadJID(ctx, resolver, query)
	result := []store.Contact{}
	for _, d := range display {
		match := d.contact.JID == qjid || strings.Contains(strings.ToLower(d.contact.JID), needle) || strings.Contains(strings.ToLower(d.contact.Phone), needle)
		for _, source := range d.sources {
			match = match || matched[source]
		}
		for _, alias := range d.aliases {
			match = match || strings.Contains(strings.ToLower(alias), needle)
		}
		if match {
			result = append(result, d.contact)
		}
	}
	return result
}

func TestContactsStreamingDifferentialIdentityAndMetadata(t *testing.T) {
	dir := seedContactReadStore(t, true)
	db := openSystemImportStore(t, dir)
	for _, row := range []struct{ jid, system, full, push string }{
		{contactPN, "PN system", "Hidden PN full", "hidden PN push"},
		{contactLID, "LID system", "Hidden LID full", "hidden LID push"},
		{"15550001001:7@s.whatsapp.net", "Variant system", "Variant source", ""},
		{"15550001001.0:3@s.whatsapp.net", "", "AD Source", ""},
		{"95550000009@lid", "Own metadata", "Own device source", ""},
		{"15550000009:3@s.whatsapp.net", "Own PN system", "Own phone source", ""},
		{"textual Ω raw", "", "Textual name", ""},
		{"broken.XX@s.whatsapp.net", "", "Broken name", ""},
		{"96660000000:2@lid", "", "Unmapped AD", ""},
		{"invalid@lid@extra", "", "Odd parseable source", ""},
		{"\u00a0raw whitespace\u00a0", "", " Unicode raw name ", ""},
	} {
		if err := db.UpsertContact(row.jid, "stored-phone", row.push, row.full, "first name", "business name"); err != nil {
			t.Fatal(err)
		}
		if row.system != "" {
			if err := db.SetSystemName(row.jid, row.system); err != nil {
				t.Fatal(err)
			}
		}
	}
	for jid, alias := range map[string]string{contactPN: "PN alias", contactLID: "LID alias", "15550001001:7@s.whatsapp.net": "Variant alias", "15550001003@s.whatsapp.net": "Ghost PN alias", "15550000009@s.whatsapp.net": "Own canonical alias", "95550000009@lid": "Own LID alias", "textual Ω raw": "Unicode Ω alias", "96660000000:2@lid": "Unknown alias"} {
		if err := db.SetAlias([]string{jid}, alias); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec(`CREATE TABLE whatsmeow_device (jid TEXT,lid TEXT,noise_key BLOB); INSERT INTO whatsmeow_device VALUES (' 15550000009:5@s.whatsapp.net ',' 95550000009@lid ',X'50524956415445'); INSERT INTO whatsmeow_lid_map VALUES ('95550000009','15550009999');`); err != nil {
		t.Fatal(err)
	}
	session.Close()
	a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	resolver, err := contactReadResolver(a)
	if err != nil {
		t.Fatal(err)
	}
	display, err := contactsForDisplay(ctx, a, resolver)
	if err != nil {
		t.Fatal(err)
	}
	page, err := a.ReadContacts(ctx, app.ContactReadOptions{Operation: app.ContactList, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Contact{}
	for _, d := range display {
		want = append(want, d.contact)
	}
	if !reflect.DeepEqual(page.Contacts, want) {
		t.Fatalf("stream=%+v\nlegacy=%+v", page.Contacts, want)
	}
	for _, query := range []string{"Hidden PN full", "hidden PN push", "Hidden LID full", "hidden LID push", "LID system", "Variant source", "Variant alias", "AD Source", "LID alias", "Ghost PN alias", "0001003", contactLID, contactPN, "95550000009@lid", "Own LID alias", "Own device source", "15550000009@s.whatsapp.net", "stored-phone", "Broken name", "96660000000:2@lid", "Ω alias", "ω alias", "Ω raw", "%_\\", "first name", "business name", "Odd parseable source", " Unicode raw name ", "missing fixture"} {
		t.Run(query, func(t *testing.T) {
			got, err := a.ReadContacts(ctx, app.ContactReadOptions{Operation: app.ContactSearch, Query: query, Limit: 100, Paginate: true})
			if err != nil {
				t.Fatal(err)
			}
			want := referenceContactSearch(t, a, query)
			if !reflect.DeepEqual(got.Contacts, want) {
				t.Fatalf("query %q\ngot=%+v\nwant=%+v", query, got.Contacts, want)
			}
		})
	}
}

func TestAgentContactsCursorPreflightScopeAndLimits(t *testing.T) {
	dir := seedContactReadStore(t, true)
	_, next := agentContactsPage(t, dir, []string{"contacts", "list"}, "", 1, "compact")
	token := *next
	_, next = agentContactsPage(t, dir, []string{"contacts", "search", "Alex"}, "", 1, "compact")
	searchToken := *next
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	malformed := []string{"", "PRIVATE_CONTACT_CURSOR", strings.Repeat("x", app.MaxContactsCursorBytes+1), token + "=", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"v":1`, `"v":9`, 1))), base64.RawURLEncoding.EncodeToString(append(raw, ' '))}
	missing := filepath.Join(dir, "missing")
	for _, bad := range malformed {
		assertCursorError(t, []string{"--agent", "--store", missing, "contacts", "list", "--cursor", bad}, bad)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("preflight created store")
	}
	for _, args := range [][]string{{"contacts", "search", "Alex", "--cursor", token}, {"contacts", "list", "--cursor", searchToken}, {"contacts", "search", "alex", "--cursor", searchToken}, {"contacts", "search", "Alex ", "--cursor", searchToken}, {"messages", "list", "--cursor", token}, {"chats", "list", "--cursor", token}} {
		assertCursorError(t, append([]string{"--agent", "--store", dir}, args...), "")
	}
	other := seedContactReadStore(t, true)
	assertCursorError(t, []string{"--agent", "--store", other, "contacts", "list", "--cursor", token}, token)
	for _, limit := range []string{"0", "-1", "201"} {
		stdout, stderr, err := runAgentTest(t, "--agent", "--store", missing, "contacts", "list", "--limit", limit)
		if stdout != "" || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatalf("%v %s", err, stderr)
		}
	}
	for _, args := range [][]string{{"contacts", "list", "--cursor", token}, {"--agent", "contacts", "show", "--jid", contactPN, "--cursor", token}, {"--agent", "contacts", "list", "extra"}, {"--agent", "contacts", "list", "--detail", "invalid"}} {
		stdout, _, err := runAgentTest(t, append([]string{"--store", missing}, args...)...)
		if err == nil || stdout != "" {
			t.Fatalf("guard: %v %s", err, stdout)
		}
	}
	// Literal query/flag values stay literal, including termination with --.
	for _, args := range [][]string{{"--json", "contacts", "search", "--", "--agent"}, {"--agent", "contacts", "search", "--", "--cursor"}, {"--agent", "contacts", "search", "--", "--agent"}} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir}, args...)...)
		if err != nil || stderr != "" {
			t.Fatalf("literal: %v %s", err, stderr)
		}
		if strings.Contains(stdout, "schema_version") != (args[0] == "--agent") {
			t.Fatalf("literal intent %s", stdout)
		}
	}
}

func TestAgentContactsUnknownMissingAndOversizedKeys(t *testing.T) {
	dir := seedContactReadStore(t, false)
	_, next := agentContactsPage(t, dir, []string{"contacts", "list"}, "", 2, "compact")
	if next == nil {
		t.Fatal("need continuation")
	}
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "contacts", "search", "Alex", "--limit", "200")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	var data agentContacts
	json.Unmarshal(decodeAgentTest(t, stdout).Data, &data)
	if len(data.Contacts) != 3 {
		t.Fatal("unknown pair merged")
	}
	for _, c := range data.Contacts {
		if c.JID == contactLID && c.Phone != "" {
			t.Fatal("unknown LID invented a phone")
		}
	}
	for _, query := range []string{"missing", "Alex"} {
		stdout, stderr, err = runAgentTest(t, "--store", dir, "--agent", "contacts", "search", query, "--limit", "3")
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		env := decodeAgentTest(t, stdout)
		if env.Meta.Page.HasMore || env.Meta.Page.NextCursor != nil {
			t.Fatalf("exact/empty end %s", stdout)
		}
	}
	// Textual/Unicode IDs are sorted using full keys, never compact DTOs.
	largeDir := t.TempDir()
	db := openSystemImportStore(t, largeDir)
	name := strings.Repeat("Ω", 400)
	for _, jid := range []string{"raw-A", "raw-B", "é raw", "Ω raw"} {
		if err := db.UpsertContact(jid, "", "", name, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	var got []string
	cursor := ""
	for n := 0; n < 5; n++ {
		data, next := agentContactsPage(t, largeDir, []string{"contacts", "list"}, cursor, 1, "compact")
		for _, c := range data.Contacts {
			got = append(got, c.JID)
			if len(c.FieldsTruncated) == 0 {
				t.Fatal("expected truncated name")
			}
		}
		if next == nil {
			break
		}
		cursor = *next
	}
	want := []string{"raw-A", "raw-B", "é raw", "Ω raw"}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	db = openSystemImportStore(t, largeDir)
	for _, jid := range want {
		if err := db.SetSystemName(jid, strings.Repeat("SECRET_STORED_KEY", 2000)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	stdout, stderr, err = runAgentTest(t, "--agent", "--store", largeDir, "contacts", "list", "--limit", "1")
	if stdout != "" || commandExitCode(err) != 1 || decodeAgentTest(t, stderr).Error.Code != "internal_error" || strings.Contains(stderr, "SECRET_STORED_KEY") {
		t.Fatalf("oversized key %v %s", err, stderr)
	}
}

func TestAgentContactsPublicIdentityCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, sql, content string
		unavailable        bool
	}{
		{name: "absent public tables", sql: `CREATE TABLE unrelated_fixture(value TEXT)`},
		{name: "pre-LID device schema", sql: `CREATE TABLE whatsmeow_device(jid TEXT,noise_key BLOB); INSERT INTO whatsmeow_device VALUES ('15550009999:2@s.whatsapp.net',X'50524956415445')`},
		{name: "incompatible map", sql: `CREATE TABLE whatsmeow_lid_map(lid TEXT,wrong_pn TEXT)`, unavailable: true},
		{name: "incompatible device", sql: `CREATE TABLE whatsmeow_device(wrong_jid TEXT,lid TEXT)`, unavailable: true},
		{name: "corrupt file", content: "PRIVATE_CORRUPT_IDENTITY_FIXTURE", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedContactReadStore(t, false)
			path := filepath.Join(dir, "session.db")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				session, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = session.Exec(tc.sql)
				session.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotLocalStore(t, dir)
			stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "contacts", "list")
			if tc.unavailable {
				if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" || strings.Contains(stderr, "PRIVATE") || strings.Contains(stderr, "whatsmeow") {
					t.Fatalf("%v %s", err, stderr)
				}
			} else {
				if err != nil || stderr != "" {
					t.Fatalf("%v %s", err, stderr)
				}
				var data agentContacts
				json.Unmarshal(decodeAgentTest(t, stdout).Data, &data)
				if len(data.Contacts) != 4 {
					t.Fatalf("unknown identities %s", stdout)
				}
			}
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("compatibility read modified fixture")
			}
		})
	}
}

func TestLegacyContactSearchEmptyJSONRemainsNull(t *testing.T) {
	dir := seedContactReadStore(t, false)
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--json", "contacts", "search", "missing fixture")
	if err != nil || stderr != "" || !strings.Contains(stdout, `"data":null`) {
		t.Fatalf("legacy empty %v %s %s", err, stdout, stderr)
	}
}

func TestAgentContactsWideRawKeysAndEnvelopeCaps(t *testing.T) {
	dir := t.TempDir()
	db := openSystemImportStore(t, dir)
	prefix := strings.Repeat("Ω raw ", 450)
	for _, suffix := range []string{"A", "B", "C"} {
		if err := db.UpsertContact(prefix+suffix, "", "", strings.Repeat("é", 600), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	first, next := agentContactsPage(t, dir, []string{"contacts", "list"}, "", 1, "compact")
	if next == nil || len(*next) < 4096 || len(*next) > app.MaxContactsCursorBytes || first.Contacts[0].JID != prefix+"A" {
		t.Fatal("complete wide raw key did not produce a bounded cursor")
	}
	second, next := agentContactsPage(t, dir, []string{"contacts", "list"}, *next, 1, "full")
	if next == nil || second.Contacts[0].JID != prefix+"B" {
		t.Fatal("wide key continuation failed")
	}
	last, next := agentContactsPage(t, dir, []string{"contacts", "list"}, *next, 1, "compact")
	if next != nil || last.Contacts[0].JID != prefix+"C" {
		t.Fatal("wide key end failed")
	}
	// Caps remain envelope-wide, including full names and untruncated identities.
	for _, tc := range []struct{ detail, jid, name string }{{"compact", strings.Repeat("raw-id", 180000), "Name"}, {"full", "raw", strings.Repeat("full-name", 1000000)}} {
		dir := t.TempDir()
		db := openSystemImportStore(t, dir)
		if err := db.UpsertContact(tc.jid, "", "", tc.name, "", ""); err != nil {
			t.Fatal(err)
		}
		db.Close()
		stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "--detail", tc.detail, "contacts", "list")
		if stdout != "" || commandExitCode(err) != 1 || decodeAgentTest(t, stderr).Error.Code != "payload_too_large" {
			t.Fatalf("cap %v %s", err, stderr)
		}
	}
}

// Run explicitly against both plain and FTS builds; every process uses only
// this synthetic --store, without auth/connect/send or user configuration.
func TestContactsBinaryFixturePages(t *testing.T) {
	binary := os.Getenv("WACLI_CONTACT_E2E_BIN")
	if binary == "" {
		t.Skip("binary fixture evidence is opt-in")
	}
	dir := seedContactReadStore(t, true)
	db := openSystemImportStore(t, dir)
	for i := 0; i < 45; i++ {
		if err := db.UpsertContact(fmt.Sprintf("1555999%04d@s.whatsapp.net", i), "", "", "E2E same name", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before := snapshotLocalStore(t, dir)
	for _, command := range [][]string{{"contacts", "list"}, {"contacts", "search", "E2E same name"}, {"contacts", "search", "0001003"}} {
		var got []string
		cursor := ""
		ended := false
		for n := 0; n < 20; n++ {
			args := append([]string{"--store", dir, "--read-only", "--agent", "--limit", "7"}, command...)
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			output, err := exec.Command(binary, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("binary %v %s", err, output)
			}
			env := decodeAgentTest(t, string(output))
			var data agentContacts
			if err := json.Unmarshal(env.Data, &data); err != nil {
				t.Fatal(err)
			}
			if env.Meta.Page == nil || env.Meta.Page.Returned != len(data.Contacts) {
				t.Fatalf("binary page %s", output)
			}
			for _, c := range data.Contacts {
				got = append(got, c.JID)
			}
			if env.Meta.Page.NextCursor == nil {
				ended = true
				break
			}
			cursor = *env.Meta.Page.NextCursor
		}
		if !ended {
			t.Fatal("binary traversal did not terminate")
		}
		a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		if command[1] == "search" {
			for _, c := range referenceContactSearch(t, a, command[2]) {
				want = append(want, c.JID)
			}
		} else {
			resolver, err := contactReadResolver(a)
			if err != nil {
				t.Fatal(err)
			}
			view, err := contactsForDisplay(context.Background(), a, resolver)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range view {
				want = append(want, d.contact.JID)
			}
		}
		a.Close()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("binary got=%v want=%v", got, want)
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Fatal("binary changed fixture")
	}
}
