package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

// Build frozen revisions without opening an archive, session, socket or media.
func draftOutputFixture(t *testing.T, kind store.DraftKind, text string, reply *store.DraftReply) store.DraftEntry {
	t.Helper()
	data := store.DraftPayloadData{
		Account:   store.DraftIdentity{PN: "15550000009@s.whatsapp.net", LID: "90009@lid"},
		Recipient: store.DraftRecipient{JID: "15550000002@s.whatsapp.net", PN: "15550000002@s.whatsapp.net", LID: "90002@lid"},
		Kind:      kind, Reply: reply,
	}
	switch kind {
	case store.DraftTextKind:
		data.Text = &store.DraftText{Text: text, Mentions: []store.DraftRecipient{{JID: "15550000003@s.whatsapp.net"}}}
	case store.DraftContactKind:
		contact, err := store.NewDraftContact(text, "+15550000003")
		if err != nil {
			t.Fatal(err)
		}
		data.Contact = &contact
	case store.DraftDocumentKind:
		data.Document = &store.DraftDocument{Filename: "fixture.txt", MIME: "text/plain", Caption: text, Size: 13, SHA256: strings.Repeat("c", 64)}
	}
	payload, err := store.NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	id, rid := strings.Repeat("a", 32), strings.Repeat("b", 32)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	review := store.DraftReviewSnapshot{RequestedRaw: data.Recipient.JID, RecipientName: "Fixture Peer", AccountName: "fixture"}
	if kind == store.DraftDocumentKind {
		review.SnapshotPath = "draft-media/" + rid + ".blob"
		review.VerifiedAtCreate = at
	}
	revision, err := store.NewDraftRevision(id, rid, at, payload, review)
	if err != nil {
		t.Fatal(err)
	}
	return store.DraftEntry{Record: store.DraftRecord{ID: id, AccountID: data.Account.PN, State: "active", HeadRevisionID: rid, CreatedAt: at, UpdatedAt: at}, Revision: revision, Number: 1}
}

func TestDraftOutputRecoveryByPayload(t *testing.T) {
	quote := &store.DraftReply{ChatJID: "15550000002@s.whatsapp.net", ID: "quote-fixture", Sender: store.DraftRecipient{JID: "90002@lid"}, Text: strings.Repeat("Q", 513)}
	cases := []struct {
		name      string
		kind      store.DraftKind
		text      string
		reply     *store.DraftReply
		truncated []string
	}{
		{"text_ASCII512", store.DraftTextKind, strings.Repeat("A", 512), nil, nil},
		{"text_ASCII513", store.DraftTextKind, strings.Repeat("A", 513), nil, []string{"text.text"}},
		{"text_ASCII541", store.DraftTextKind, strings.Repeat("A", 541), nil, []string{"text.text"}},
		{"text_Unicode512", store.DraftTextKind, strings.Repeat("👋", 512), nil, nil},
		{"text_Unicode513", store.DraftTextKind, strings.Repeat("👋", 513), nil, []string{"text.text"}},
		{"text_valid_quote513", store.DraftTextKind, "reply", quote, []string{"reply.text"}},
		{"contact_short", store.DraftContactKind, "Fixture", nil, nil},
		{"contact_ASCII541", store.DraftContactKind, strings.Repeat("A", 541), nil, []string{"contact.display_name", "contact.vcard"}},
		{"contact_Unicode512", store.DraftContactKind, strings.Repeat("👋", 512), nil, []string{"contact.vcard"}},
		{"contact_Unicode513", store.DraftContactKind, strings.Repeat("👋", 513), nil, []string{"contact.display_name", "contact.vcard"}},
		{"document_short", store.DraftDocumentKind, "Fixture", nil, nil},
		{"document_ASCII541", store.DraftDocumentKind, strings.Repeat("A", 541), nil, []string{"document.caption"}},
		{"document_Unicode512", store.DraftDocumentKind, strings.Repeat("👋", 512), nil, nil},
		{"document_Unicode513", store.DraftDocumentKind, strings.Repeat("👋", 513), nil, []string{"document.caption"}},
	}
	storeRef := "/fixture/store"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := draftOutputFixture(t, tc.kind, tc.text, tc.reply)
			payload := entry.Revision.Payload()
			canonical := payload.CanonicalJSON()
			for _, detail := range []string{"compact", "full"} {
				flags := &rootFlags{agent: true, detail: detail, agentAccount: out.AgentAccount{StoreRef: &storeRef}}
				var encoded bytes.Buffer
				if err := writeDraftEntryTo(&encoded, flags, entry, false); err != nil {
					t.Fatal(err)
				}
				envelope := decodeAgentTest(t, encoded.String())
				var dto draftDTO
				if err := json.Unmarshal(envelope.Data, &dto); err != nil {
					t.Fatal(err)
				}
				if envelope.Meta.Source != "local" || envelope.Meta.Completeness != "unknown" || envelope.Meta.Freshness != "unknown" {
					t.Fatal("source or certainty changed")
				}
				if dto.ID != entry.Record.ID || dto.RevisionID != entry.Revision.ID() || dto.HeadRevisionID != entry.Record.HeadRevisionID || dto.Revision != entry.Number || dto.Hash != payload.Hash() || dto.AccountIdentity != payload.Data().Account || dto.Recipient != payload.Data().Recipient || dto.Defaults != payload.Data().Defaults {
					t.Fatal("frozen correlation, identity or defaults changed")
				}
				wantTruncated := tc.truncated
				if detail == "full" {
					wantTruncated = nil
				}
				if !reflect.DeepEqual(dto.TruncatedFields, wantTruncated) {
					t.Fatalf("%s truncation: got %v want %v", detail, dto.TruncatedFields, wantTruncated)
				}
				wantRecovery := tc.kind == store.DraftDocumentKind || len(wantTruncated) > 0
				if (dto.Recovery != "") != wantRecovery {
					t.Fatal("recovery does not match review needs")
				}
				if wantRecovery {
					if envelope.Meta.Recovery != "See data.recovery." || strings.Count(encoded.String(), dto.Recovery) != 1 {
						t.Fatal("complete guidance missing or duplicated")
					}
					if !strings.Contains(dto.Recovery, "approval") {
						t.Fatal("approval warning missing")
					}
					if detail == "compact" && (!strings.Contains(dto.Recovery, entry.Record.ID) || !strings.Contains(dto.Recovery, "--revision "+entry.Revision.ID()) || !strings.Contains(dto.Recovery, "--detail full")) {
						t.Fatal("recovery does not identify the full frozen revision")
					}
				} else if envelope.Meta.Recovery != "" || bytes.Contains(envelope.Data, []byte(`"recovery"`)) {
					t.Fatal("unneeded recovery emitted")
				}
				if (strings.Contains(dto.Recovery, "document bytes")) != (tc.kind == store.DraftDocumentKind) {
					t.Fatal("document guidance leaked to another payload")
				}
				if dto.Document != nil {
					if !strings.Contains(dto.Recovery, "creation") || !strings.Contains(dto.Recovery, "availability/integrity") {
						t.Fatal("document certainty warnings missing")
					}
					if dto.Document.Size != 13 || dto.Document.SHA256 != strings.Repeat("c", 64) || dto.Document.VerifiedAtCreate != entry.Revision.CreatedAt() {
						t.Fatal("document verification metadata changed")
					}
					wantPath := ""
					if detail == "full" {
						wantPath = filepath.Join(storeRef, "draft-media", entry.Revision.ID()+".blob")
					}
					if dto.Document.SnapshotPath != wantPath {
						t.Fatal("document path detail changed")
					}
				}
				if dto.Text != nil {
					wantText := tc.text
					if detail == "compact" && utf8.RuneCountInString(wantText) > 512 {
						wantText = string([]rune(wantText)[:512])
					}
					if dto.Text.Text != wantText || !utf8.ValidString(dto.Text.Text) || !reflect.DeepEqual(dto.Text.Mentions, payload.Data().Text.Mentions) {
						t.Fatal("text, Unicode boundary or mentions changed")
					}
				}
				if detail == "full" && (!reflect.DeepEqual(dto.Text, payload.Data().Text) || !reflect.DeepEqual(dto.Contact, payload.Data().Contact) || !reflect.DeepEqual(dto.Reply, payload.Data().Reply)) {
					t.Fatal("full frozen content changed")
				}
				if !bytes.Equal(canonical, payload.CanonicalJSON()) {
					t.Fatal("projection mutated payload")
				}
				t.Logf("%s %s bytes=%d", tc.name, detail, encoded.Len())
			}
			page := store.DraftPage{Items: []store.DraftListItem{{Record: entry.Record, RevisionID: entry.Revision.ID(), Hash: payload.Hash(), Number: entry.Number, Summary: store.DraftRevisionSummary(entry.Revision)}}}
			for _, detail := range []string{"compact", "full"} {
				flags := &rootFlags{agent: true, detail: detail, agentAccount: out.AgentAccount{StoreRef: &storeRef}}
				var err error
				raw := captureRootStdout(t, func() { err = writeDraftList(flags, page) })
				if err != nil {
					t.Fatal(err)
				}
				envelope := decodeAgentTest(t, raw)
				var items []draftListDTO
				if err := json.Unmarshal(envelope.Data, &items); err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Hash != payload.Hash() || !reflect.DeepEqual(items[0].Summary, page.Items[0].Summary) || !strings.Contains(envelope.Meta.Recovery, "stored summaries only") {
					t.Fatal("list summary contract changed")
				}
				t.Logf("%s list/%s bytes=%d", tc.name, detail, len(raw))
			}
		})
	}
}

func TestDraftOutputLegacyRecovery(t *testing.T) {
	storeRef := "/fixture/store"
	for _, kind := range []store.DraftKind{store.DraftTextKind, store.DraftDocumentKind} {
		entry := draftOutputFixture(t, kind, strings.Repeat("A", 541), nil)
		for _, full := range []bool{false, true} {
			flags := &rootFlags{asJSON: true, fullOutput: full, agentAccount: out.AgentAccount{StoreRef: &storeRef}}
			var encoded bytes.Buffer
			if err := writeDraftEntryTo(&encoded, flags, entry, false); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Success bool `json:"success"`
				Data    struct {
					Account out.AgentAccount `json:"account"`
					Draft   draftDTO         `json:"draft"`
				} `json:"data"`
			}
			if err := json.Unmarshal(encoded.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.Success || envelope.Data.Account.StoreRef == nil || *envelope.Data.Account.StoreRef != storeRef || envelope.Data.Draft.Hash != entry.Revision.Payload().Hash() {
				t.Fatal("legacy scope or hash changed")
			}
			if full && len(envelope.Data.Draft.TruncatedFields) != 0 {
				t.Fatal("legacy full truncated")
			}
			if (!full || kind == store.DraftDocumentKind) && (envelope.Data.Draft.Recovery == "" || strings.Contains(envelope.Data.Draft.Recovery, "See data.recovery.")) {
				t.Fatal("legacy complete guidance missing")
			}
			t.Logf("legacy %s full=%v bytes=%d", kind, full, encoded.Len())
			flags.asJSON = false
			encoded.Reset()
			if err := writeDraftEntryTo(&encoded, flags, entry, false); err != nil {
				t.Fatal(err)
			}
			var human draftDTO
			if err := json.Unmarshal(encoded.Bytes(), &human); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(human, envelope.Data.Draft) {
				t.Fatal("human and legacy projections differ")
			}
		}
	}
}

func TestDraftReplySenderGuidanceIsFixed(t *testing.T) {
	for _, reason := range []string{"incoming DM quote does not match the observed peer", "incoming quote contradicts the observed local account", "outgoing quote does not match the local account", "a known user sender is required", "UNTRUSTED_REASON /private/path\n"} {
		cause := &store.DraftValidationError{Field: "reply.sender", Reason: reason}
		failure := classifyDraftError(fmt.Errorf("wrapped: %w", cause))
		if failure.Code != "invalid_arguments" || failure.ExitCode != 2 || !errors.Is(failure, cause) {
			t.Fatal("typed validation handling changed")
		}
		if failure.Message != "Quoted sender identity is unavailable or incompatible with the observed local account or recipient." || failure.Recovery != "Inspect messages show --chat CHAT_JID --id MESSAGE_ID --agent --detail full locally; select a compatible quote or explicitly recreate without --reply-to." {
			t.Fatal("sender guidance is not fixed")
		}
	}
	if failure := classifyDraftError(&store.DraftValidationError{Field: "cursor", Reason: "invalid"}); failure.Code != "invalid_cursor" || failure.ExitCode != 2 {
		t.Fatal("cursor classification changed")
	}
}

func TestDraftGenericOwnerInputGuidance(t *testing.T) {
	message := "reply"
	req := sendDelegateRequest{Kind: draftWriteKind, Draft: &app.DraftWriteRequest{Version: 1, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/fixture/store", Input: &app.DraftInput{To: "15550000002@s.whatsapp.net", Message: &message, ReplyTo: "quote"}}}
	// Missing categorical detail must remain compatible with an older owner.
	response := sendDelegateResponse{DraftFailure: store.DraftFailure("invalid_arguments", req.Draft.DraftID, req.Draft.RevisionID, "", nil), DraftRequestHash: draftRequestHash(*req.Draft)}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sendDelegateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	failure := classifyDraftError(draftIPCFailure(req, decoded))
	if failure.Code != "invalid_arguments" || failure.ExitCode != 2 || failure.Draft == nil || failure.Draft.DraftID != req.Draft.DraftID || failure.Draft.RevisionID != req.Draft.RevisionID {
		t.Fatal("owner refusal correlation changed")
	}
	if strings.Contains(failure.Recovery, "draft show") || strings.Contains(failure.Recovery, "replay") || !strings.Contains(failure.Recovery, "for --reply-to") || !strings.Contains(failure.Recovery, "messages show") {
		t.Fatal("generic refusal assumes a draft or loses conditional quote guidance")
	}
}

func TestDraftOutputUncertaintyWithoutStore(t *testing.T) {
	entry := draftOutputFixture(t, store.DraftTextKind, "saved", nil)
	storeRef := "/fixture/store"
	flags := &rootFlags{agent: true, detail: "compact", agentAccount: out.AgentAccount{StoreRef: &storeRef}}
	if err := writeDraftEntryTo(draftBrokenWriter{syscall.EPIPE}, flags, entry, false); err != nil {
		t.Fatal("read pipe semantics changed")
	}
	for _, cause := range []error{syscall.EPIPE, io.ErrShortWrite} {
		failure := classifyDraftError(writeDraftEntryTo(draftBrokenWriter{cause}, flags, entry, true))
		if failure.Code != "local_write_uncertain" || failure.ExitCode != 1 || failure.Draft == nil || failure.Draft.DraftID != entry.Record.ID || failure.Draft.RevisionID != entry.Revision.ID() || failure.Draft.Hash != entry.Revision.Payload().Hash() {
			t.Fatal("output uncertainty or correlation changed")
		}
		if !strings.Contains(failure.Message, "do not replay automatically") || !strings.Contains(failure.Recovery, "not permission to send") {
			t.Fatal("write uncertainty warnings lost")
		}
	}
}
