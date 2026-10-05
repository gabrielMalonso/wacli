package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func draftAppFixture(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('15550000001:2@s.whatsapp.net','90001@lid'); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE); INSERT INTO whatsmeow_lid_map VALUES('90002','15550000002')`)
	return a
}
func draftAppRequest(t *testing.T, a *App, input DraftInput) DraftWriteRequest {
	t.Helper()
	id, err := store.NewDraftID()
	if err != nil {
		t.Fatal(err)
	}
	rid, err := store.NewDraftID()
	if err != nil {
		t.Fatal(err)
	}
	return DraftWriteRequest{Version: 1, Action: "create", DraftID: id, RevisionID: rid, StoreRef: a.StoreDir(), Input: &input}
}
func draftTextPointer(text string) *string { return &text }
func draftTestErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *store.DraftError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s: %v", code, err)
	}
}
func TestDraftOfflineIdentityMappingAndFrozenQuote(t *testing.T) {
	a := draftAppFixture(t)
	ctx := context.Background()
	for _, chat := range []string{"15550000002@s.whatsapp.net", "90002@lid"} {
		if err := a.DB().UpsertChat(chat, "dm", "fixture", time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "real-id", SenderJID: "90002@lid", Text: "real\nOlá", Timestamp: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	req := draftAppRequest(t, a, DraftInput{To: "90002@lid", Message: draftTextPointer("literal\\n\n👋"), ReplyTo: "real-id", Mentions: []string{"90002@lid"}})
	entry, err := a.WriteLocalDraft(ctx, req, os.Open)
	if err != nil {
		t.Fatal(err)
	}
	p := entry.Revision.Payload().Data()
	if a.wa != nil || p.Account.PN != "15550000001@s.whatsapp.net" || p.Account.LID != "90001@lid" || p.Recipient.PN != "15550000002@s.whatsapp.net" || p.Recipient.LID != "90002@lid" || p.Reply.Text != "real\nOlá" || p.Text.Text != "literal\\n\n👋" {
		t.Fatalf("not frozen/offline: %+v", p)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "90002@lid", MsgID: "real-id", SenderJID: "90002@lid", Text: "changed", Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	_, err = a.WriteLocalDraft(ctx, draftAppRequest(t, a, *req.Input), os.Open)
	if err == nil {
		t.Fatal("divergent alias quote chosen")
	}
	historyIdentityFixture(t, a, `UPDATE whatsmeow_lid_map SET pn='15550000003' WHERE lid='90002'`)
	frozen, err := a.DB().ReadDraft(ctx, req.DraftID, req.RevisionID)
	if err != nil || frozen.Revision.Payload().Hash() != entry.Revision.Payload().Hash() || frozen.Revision.Payload().Data().Reply.Text != "real\nOlá" {
		t.Fatal("frozen record changed", err)
	}
	unknown, err := a.WriteLocalDraft(ctx, draftAppRequest(t, a, DraftInput{To: "90099@lid", Message: draftTextPointer("unknown")}), os.Open)
	if err != nil || unknown.Revision.Payload().Data().Recipient.PN != "" {
		t.Fatal("invented unknown PN", err)
	}
	self := draftAppRequest(t, a, DraftInput{To: "90001@lid", Message: draftTextPointer("self")})
	if _, err := a.WriteLocalDraft(ctx, self, os.Open); err == nil {
		t.Fatal("allowed known self")
	}
}
func TestDraftIdentityRequiredOnlyForPreparation(t *testing.T) {
	ctx := context.Background()
	a := draftAppFixture(t)
	req := draftAppRequest(t, a, DraftInput{To: "+15550000002", Message: draftTextPointer("saved")})
	entry, err := a.WriteLocalDraft(ctx, req, os.Open)
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(a.StoreDir(), "session.db")
	if err := os.Rename(session, session+".fixture-hidden"); err != nil {
		t.Fatal(err)
	}
	update := req
	update.Action = "update"
	update.ExpectedRevision = req.RevisionID
	update.RevisionID, _ = store.NewDraftID()
	_, err = a.WriteLocalDraft(ctx, update, os.Open)
	draftTestErrorCode(t, err, "identity_unavailable")
	req.Action = "discard"
	req.Input = nil
	req.ExpectedRevision = req.RevisionID
	if _, err := a.WriteLocalDraft(ctx, req, os.Open); err != nil {
		t.Fatal("discard depended on session", err)
	}
	if _, err := a.DB().ReadDraft(ctx, entry.Record.ID, ""); err != nil {
		t.Fatal(err)
	}
	fresh := newTestApp(t)
	_, err = fresh.WriteLocalDraft(ctx, draftAppRequest(t, fresh, DraftInput{To: "+15550000002", Message: draftTextPointer("missing session")}), os.Open)
	draftTestErrorCode(t, err, "identity_unavailable")
	if _, err := os.Stat(filepath.Join(fresh.StoreDir(), "session.db")); !os.IsNotExist(err) {
		t.Fatal("created session", err)
	}
}
func TestDraftDocumentRetainedAfterDatabaseFailure(t *testing.T) {
	a := draftAppFixture(t)
	source := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(source, []byte("snapshot bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	req := draftAppRequest(t, a, DraftInput{To: "+15550000002", File: source})
	db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fixture_crash BEFORE INSERT ON draft_revisions BEGIN SELECT RAISE(ABORT,'fixture crash after publication');END`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WriteLocalDraft(context.Background(), req, os.Open); err == nil {
		t.Fatal("expected DB failure")
	}
	relative, _ := store.DraftSnapshotRelativePath(req.RevisionID)
	raw, err := os.ReadFile(filepath.Join(a.StoreDir(), relative))
	if err != nil || string(raw) != "snapshot bytes" {
		t.Fatal("published work removed", err)
	}
	if _, err := a.DB().ReadDraftRecord(context.Background(), req.DraftID); err == nil {
		t.Fatal("partial DB commit")
	}
}
func TestDraftRequestCanonicalBoundary(t *testing.T) {
	a := draftAppFixture(t)
	req := draftAppRequest(t, a, DraftInput{To: "+15550000002", Message: draftTextPointer("valid")})
	raw, _ := json.Marshal(req)
	var decoded DraftWriteRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(raw), `"to":`, `"unknown":0,"to":`, 1)), []byte(strings.Replace(string(raw), "valid", `\ud800`, 1)), append(raw, []byte("{}")...)} {
		if json.Unmarshal(bad, &decoded) == nil {
			t.Fatal("invalid typed input allowed")
		}
	}
}

func TestDraftLocalOwnNullableAndMappingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, sql, code string
		wantLID         string
	}{
		{"nullable fallback", `UPDATE whatsmeow_device SET lid=NULL;INSERT INTO whatsmeow_lid_map VALUES('90001','15550000001')`, "", "90001@lid"},
		{"ambiguous", `INSERT INTO whatsmeow_device VALUES('15550000003@s.whatsapp.net',NULL)`, "identity_unavailable", ""},
		{"invalid own", `UPDATE whatsmeow_device SET jid='100@s.whatsapp.net'`, "identity_unavailable", ""},
		{"map unavailable", `DROP TABLE whatsmeow_lid_map`, "identity_unavailable", ""},
		{"invalid map", `UPDATE whatsmeow_lid_map SET pn='15550000002@s.whatsapp.net'`, "identity_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := draftAppFixture(t)
			historyIdentityFixture(t, a, tc.sql)
			entry, err := a.WriteLocalDraft(context.Background(), draftAppRequest(t, a, DraftInput{To: "90002@lid", Message: draftTextPointer("fixture")}), os.Open)
			if tc.code != "" {
				draftTestErrorCode(t, err, tc.code)
			} else if err != nil || entry.Revision.Payload().Data().Account.LID != tc.wantLID {
				t.Fatal("nullable public fallback", err)
			}
			if a.wa != nil {
				t.Fatal("opened WA client")
			}
		})
	}
}
func TestDraftQuoteUnavailableAndUnknownSender(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params store.UpsertMessageParams
	}{
		{"revoked", store.UpsertMessageParams{Revoked: true, SenderJID: "90002@lid", Text: "retained"}},
		{"deleted", store.UpsertMessageParams{DeletedForMe: true, SenderJID: "90002@lid", Text: "retained"}},
		{"unknown sender", store.UpsertMessageParams{Text: "retained"}},
		{"missing text", store.UpsertMessageParams{SenderJID: "90002@lid"}},
		{"unsupported", store.UpsertMessageParams{Text: "caption", SenderJID: "90002@lid", MediaType: "document"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := draftAppFixture(t)
			chat := "15550000002@s.whatsapp.net"
			if err := a.DB().UpsertChat(chat, "dm", "fixture", time.Now()); err != nil {
				t.Fatal(err)
			}
			params := tc.params
			params.ChatJID = chat
			params.MsgID = "id"
			params.Timestamp = time.Now()
			if err := a.DB().UpsertMessage(params); err != nil {
				t.Fatal(err)
			}
			_, err := a.WriteLocalDraft(context.Background(), draftAppRequest(t, a, DraftInput{To: chat, Message: draftTextPointer("reply"), ReplyTo: "id"}), os.Open)
			if err == nil {
				t.Fatal("unavailable quote accepted")
			}
		})
	}
}

func TestDraftQuoteDiagnosticPrecedence(t *testing.T) {
	const peer = "15550000002@s.whatsapp.net"
	const alias = "90002@lid"
	for _, tc := range []struct {
		name  string
		quote store.UpsertMessageParams
		other *store.UpsertMessageParams
		field string
		purge bool
	}{
		{name: "PDF with caption and author", quote: store.UpsertMessageParams{MediaType: "document", MimeType: "application/pdf", Text: "caption", MediaCaption: "caption", SenderJID: alias}, field: "reply.unsupported"},
		{name: "PDF without text", quote: store.UpsertMessageParams{MediaType: "document", SenderJID: alias}, field: "reply.unsupported"},
		{name: "PDF without author", quote: store.UpsertMessageParams{MediaType: "document", Text: "caption"}, field: "reply.unsupported"},
		{name: "PDF with malformed author", quote: store.UpsertMessageParams{MediaType: "document", Text: "caption", SenderJID: "invalid@s.whatsapp.net"}, field: "reply.unsupported"},
		{name: "PDF with incompatible author", quote: store.UpsertMessageParams{MediaType: "document", Text: "caption", SenderJID: "15550000003@s.whatsapp.net"}, field: "reply.unsupported"},
		{name: "reaction", quote: store.UpsertMessageParams{ReactionToID: "target", Text: "reaction", SenderJID: alias}, field: "reply.unsupported"},
		{name: "buttons", quote: store.UpsertMessageParams{Buttons: []store.Button{{ID: "button", DisplayText: "label"}}, Text: "button text", SenderJID: alias}, field: "reply.unsupported"},
		{name: "revoked PDF", quote: store.UpsertMessageParams{Revoked: true, MediaType: "document", Text: "retained", SenderJID: alias}, field: "reply"},
		{name: "deleted PDF", quote: store.UpsertMessageParams{DeletedForMe: true, MediaType: "document", Text: "retained", SenderJID: alias}, field: "reply"},
		{name: "purged PDF", quote: store.UpsertMessageParams{Revoked: true, MediaType: "document", Text: "retained", SenderJID: alias}, field: "reply", purge: true},
		{name: "unavailable alias before unsupported", quote: store.UpsertMessageParams{Revoked: true, Text: "retained", SenderJID: alias}, other: &store.UpsertMessageParams{MediaType: "document", Text: "caption", SenderJID: alias}, field: "reply"},
		{name: "malformed author alias before unsupported", quote: store.UpsertMessageParams{Text: "text", SenderJID: "invalid@s.whatsapp.net"}, other: &store.UpsertMessageParams{MediaType: "document", Text: "caption", SenderJID: alias}, field: "reply.unsupported"},
		{name: "malformed author alias before unavailable", quote: store.UpsertMessageParams{Text: "text", SenderJID: "invalid@s.whatsapp.net"}, other: &store.UpsertMessageParams{DeletedForMe: true, Text: "retained", SenderJID: alias}, field: "reply"},
		{name: "text with malformed author", quote: store.UpsertMessageParams{Text: "text", SenderJID: "invalid@s.whatsapp.net"}, field: "recipient"},
	} {
		orders := []string{"PN first"}
		if tc.other != nil {
			orders = append(orders, "LID first")
		}
		for _, order := range orders {
			t.Run(tc.name+"/"+order, func(t *testing.T) {
				a := draftAppFixture(t)
				ctx := t.Context()
				seed, err := a.WriteLocalDraft(ctx, draftAppRequest(t, a, DraftInput{To: peer, Message: draftTextPointer("seed")}), os.Open)
				if err != nil {
					t.Fatal(err)
				}
				for i, chat := range []string{peer, alias} {
					if i == 1 && tc.other == nil {
						break
					}
					if err := a.DB().UpsertChat(chat, "dm", "fixture", time.Unix(1, 0)); err != nil {
						t.Fatal(err)
					}
					params := tc.quote
					if tc.other != nil && (i == 1) == (order == "PN first") {
						params = *tc.other
					}
					params.ChatJID, params.MsgID, params.Timestamp = chat, "quote", time.Unix(1, 0)
					if err := a.DB().UpsertMessage(params); err != nil {
						t.Fatal(err)
					}
				}
				if tc.purge {
					if err := a.DB().PurgeMessage(peer, "quote"); err != nil {
						t.Fatal(err)
					}
				}
				for _, action := range []string{"create", "update"} {
					req := draftAppRequest(t, a, DraftInput{To: peer, File: "UNOPENED_FIXTURE_DOCUMENT", ReplyTo: "quote"})
					if action == "update" {
						req.Action, req.DraftID, req.ExpectedRevision = action, seed.Record.ID, seed.Revision.ID()
					}
					opens := 0
					_, err := a.WriteLocalDraft(ctx, req, func(string) (*os.File, error) { opens++; return nil, errors.New("must not open") })
					var validation *store.DraftValidationError
					if !errors.As(err, &validation) || validation.Field != tc.field || opens != 0 {
						t.Fatalf("%s: wrong quote category or source opened: %v, opens=%d", action, err, opens)
					}
					if tc.field == "reply" && validation.Reason != "quote must have available text and known sender" {
						t.Fatal("unavailable refusal changed")
					}
				}
				page, err := a.DB().ListDrafts(ctx, a.StoreDir(), true, 20, "")
				if err != nil || len(page.Items) != 1 {
					t.Fatal("rejected quote committed a draft", err)
				}
				retained, err := a.DB().ReadDraft(ctx, seed.Record.ID, "")
				if err != nil || retained.Revision.ID() != seed.Revision.ID() || retained.Number != 1 || retained.Revision.Payload().Hash() != seed.Revision.Payload().Hash() {
					t.Fatal("rejected quote changed a revision", err)
				}
				if _, err := os.Stat(filepath.Join(a.StoreDir(), store.DraftMediaDirectory)); !os.IsNotExist(err) || a.wa != nil {
					t.Fatal("quote refusal opened media or WA", err)
				}
			})
		}
	}
}

func TestDraftRejectsIncompatibleDMQuoteBeforeSnapshotOrCommit(t *testing.T) {
	for _, sender := range []string{"15550000003@s.whatsapp.net", "15550000001@s.whatsapp.net", "90001@lid"} {
		t.Run(sender, func(t *testing.T) {
			a := draftAppFixture(t)
			chat := "15550000002@s.whatsapp.net"
			if err := a.DB().UpsertChat(chat, "dm", "fixture", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "real", SenderJID: sender, Text: "real text", Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
			req := draftAppRequest(t, a, DraftInput{To: chat, File: "unopened-fixture-document", ReplyTo: "real"})
			opens := 0
			_, err := a.WriteLocalDraft(context.Background(), req, func(string) (*os.File, error) { opens++; return nil, errors.New("must not open") })
			var validation *store.DraftValidationError
			if !errors.As(err, &validation) || validation.Field != "reply.sender" || opens != 0 {
				t.Fatal("quote not rejected before snapshot", err, opens)
			}
			if _, err := os.Stat(filepath.Join(a.StoreDir(), store.DraftMediaDirectory)); !os.IsNotExist(err) {
				t.Fatal("snapshot effects", err)
			}
			page, err := a.DB().ListDrafts(context.Background(), a.StoreDir(), true, 20, "")
			if err != nil || len(page.Items) != 0 {
				t.Fatal("draft committed", err)
			}
		})
	}
}

func TestDraftQuoteRawWhitespaceAndAliasDivergence(t *testing.T) {
	for _, scenario := range []string{"valid", "whitespace only", "edge divergence"} {
		t.Run(scenario, func(t *testing.T) {
			a := draftAppFixture(t)
			raw := "\t\u00a0quoted🙂\r\n"
			if scenario == "whitespace only" {
				raw = " \t\u00a0"
			}
			for _, chat := range []string{"15550000002@s.whatsapp.net", "90002@lid"} {
				if err := a.DB().UpsertChat(chat, "dm", "fixture", time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
				text := raw
				if scenario == "edge divergence" && chat == "90002@lid" {
					text = strings.TrimSpace(raw)
				}
				if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "quoted", SenderJID: "90002@lid", Text: text, Timestamp: time.Unix(1, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			req := draftAppRequest(t, a, DraftInput{To: "90002@lid", Message: draftTextPointer("reply"), ReplyTo: "quoted"})
			entry, err := a.WriteLocalDraft(t.Context(), req, os.Open)
			if scenario != "valid" {
				var validation *store.DraftValidationError
				if !errors.As(err, &validation) || validation.Field != "reply" {
					t.Fatal("unsupported empty/divergent quote accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal([]byte(raw), []byte(entry.Revision.Payload().Data().Reply.Text)) {
				t.Error("valid quote lost raw whitespace")
			}
			retained, err := a.DB().ReadDraft(t.Context(), req.DraftID, req.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(entry.Revision.Payload().CanonicalJSON(), retained.Revision.Payload().CanonicalJSON()) || entry.Revision.Payload().Hash() != retained.Revision.Payload().Hash() {
				t.Error("frozen quote payload/hash changed")
			}
		})
	}
}
