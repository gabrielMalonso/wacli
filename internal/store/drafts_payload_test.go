package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func draftTextFixture(text string) DraftPayloadData {
	return DraftPayloadData{
		Account:   DraftIdentity{PN: "15550000001@s.whatsapp.net"},
		Recipient: DraftRecipient{JID: "15550000002@s.whatsapp.net"},
		Kind:      DraftTextKind, Text: &DraftText{Text: text},
	}
}

func mustDraftPayload(t *testing.T, data DraftPayloadData) DraftPayload {
	t.Helper()
	payload, err := NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestDraftPayloadGoldenAndExactText(t *testing.T) {
	payload := mustDraftPayload(t, draftTextFixture("Olá 👋\n\\n"))
	want := `{"version":1,"account":{"pn":"15550000001@s.whatsapp.net","lid":""},"recipient":{"jid":"15550000002@s.whatsapp.net","pn":"15550000002@s.whatsapp.net","lid":""},"kind":"text","defaults":{"link_preview":false,"ephemeral":false,"expiration":0,"allow_self":false},"text":{"text":"Olá 👋\n\\n","mentions":[]}}`
	if string(payload.CanonicalJSON()) != want {
		t.Fatalf("canonical JSON = %s", payload.CanonicalJSON())
	}
	const wantHash = "ad56e5033072dfbbe555ba978d7e6afe8c62df15c21ba47137641cfb3c4ef8c7"
	if payload.Hash() != wantHash {
		t.Fatalf("hash = %s, want %s", payload.Hash(), wantHash)
	}
	decoded, err := DecodeDraftPayload(payload.CanonicalJSON())
	if err != nil || decoded.Hash() != payload.Hash() || decoded.Data().Text.Text != "Olá 👋\n\\n" {
		t.Fatalf("roundtrip: %v, %s", err, decoded.Hash())
	}
	for _, text := range []string{" \t\r\n", "\\n", "é", "e\u0301"} {
		if got := mustDraftPayload(t, draftTextFixture(text)).Data().Text.Text; got != text {
			t.Fatalf("literal text changed: %q => %q", text, got)
		}
	}
	if mustDraftPayload(t, draftTextFixture("é")).Hash() == mustDraftPayload(t, draftTextFixture("e\u0301")).Hash() {
		t.Fatal("distinct Unicode bytes share a hash")
	}
}

func TestDraftPayloadFreezesCopies(t *testing.T) {
	data := draftTextFixture("original")
	data.Text.Mentions = []DraftRecipient{{JID: "15550000003@s.whatsapp.net"}}
	data.Reply = &DraftReply{ChatJID: data.Recipient.JID, ID: "real-id", Sender: data.Recipient, Text: "original quote"}
	payload := mustDraftPayload(t, data)
	original := payload.CanonicalJSON()
	data.Text.Text = "modified"
	data.Text.Mentions[0].JID = "15550000004@s.whatsapp.net"
	data.Reply.Text = "modified quote"
	copy := payload.Data()
	copy.Text.Text = "modified again"
	copy.Text.Mentions[0].JID = "15550000005@s.whatsapp.net"
	copy.Reply.Text = "modified again"
	exported := payload.CanonicalJSON()
	exported[0] = 'x'
	if !bytes.Equal(original, payload.CanonicalJSON()) || payload.Data().Reply.Text != "original quote" {
		t.Fatal("caller mutated frozen payload")
	}
}

func TestDraftPayloadRejectsInvalidUnionAndDefaults(t *testing.T) {
	cases := map[string]func(*DraftPayloadData){
		"empty variant":      func(p *DraftPayloadData) { p.Text = nil },
		"ambiguous union":    func(p *DraftPayloadData) { p.Contact = &DraftContact{DisplayName: "Fixture", Phone: "15550000003"} },
		"kind mismatch":      func(p *DraftPayloadData) { p.Kind = DraftDocumentKind },
		"unknown kind":       func(p *DraftPayloadData) { p.Kind = "video" },
		"unknown version":    func(p *DraftPayloadData) { p.Version = 2 },
		"empty text":         func(p *DraftPayloadData) { p.Text.Text = "" },
		"invalid UTF8":       func(p *DraftPayloadData) { p.Text.Text = string([]byte{0xff}) },
		"field limit":        func(p *DraftPayloadData) { p.Text.Text = strings.Repeat("x", MaxDraftFieldBytes+1) },
		"encoded limit":      func(p *DraftPayloadData) { p.Text.Text = strings.Repeat("<", MaxDraftFieldBytes) },
		"self PN":            func(p *DraftPayloadData) { p.Recipient.JID = p.Account.PN },
		"self LID":           func(p *DraftPayloadData) { p.Account.LID = "12345@lid"; p.Recipient.JID = "12345:7@lid" },
		"no own PN":          func(p *DraftPayloadData) { p.Account.PN = "" },
		"LID own PN":         func(p *DraftPayloadData) { p.Account.PN = "12345@lid" },
		"default preview":    func(p *DraftPayloadData) { p.Defaults.LinkPreview = true },
		"default ephemeral":  func(p *DraftPayloadData) { p.Defaults.Ephemeral = true },
		"default expiration": func(p *DraftPayloadData) { p.Defaults.Expiration = 86400 },
		"default self":       func(p *DraftPayloadData) { p.Defaults.AllowSelf = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			data := draftTextFixture("fixture")
			mutate(&data)
			_, err := NewDraftPayload(data)
			var typed *DraftValidationError
			if !errors.As(err, &typed) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
	_ = mustDraftPayload(t, draftTextFixture(strings.Repeat("x", MaxDraftFieldBytes)))
}

func TestDraftPayloadInternalJSONRejectsLossAndExtraFields(t *testing.T) {
	raw := string(mustDraftPayload(t, draftTextFixture("fixture")).CanonicalJSON())
	for name, input := range map[string]string{
		"unknown":            strings.Replace(raw, `"version":1`, `"unknown":1,"version":1`, 1),
		"duplicate":          strings.Replace(raw, `"version":1`, `"version":1,"version":1`, 1),
		"missing version":    strings.Replace(raw, `"version":1,`, "", 1),
		"case alias":         strings.Replace(raw, `"version"`, `"Version"`, 1),
		"missing default":    strings.Replace(raw, `"allow_self":false`, `"ignored":false`, 1),
		"nested unknown":     strings.Replace(raw, `"text":"fixture"`, `"text":"fixture","other":null`, 1),
		"NaN":                strings.Replace(raw, `"expiration":0`, `"expiration":NaN`, 1),
		"unpaired surrogate": strings.Replace(raw, "fixture", `\ud800`, 1),
		"raw UTF8":           strings.Replace(raw, "fixture", string([]byte{0xff}), 1),
		"trailing":           raw + "{}",
		"oversize":           strings.Repeat(" ", MaxDraftPayloadBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDraftPayload([]byte(input)); err == nil {
				t.Fatal("accepted invalid internal JSON")
			}
		})
	}
}

func TestDraftTargetsAreExplicitAndStrict(t *testing.T) {
	for input, want := range map[string]string{
		"+1 (555) 000-0002":            "15550000002@s.whatsapp.net",
		"15550000002:7@s.whatsapp.net": "15550000002@s.whatsapp.net",
		"12345:65535@lid":              "12345@lid",
		"123456789012345@g.us":         "123456789012345@g.us",
		"15550000002-1234567@g.us":     "15550000002-1234567@g.us",
	} {
		got, err := NormalizeDraftTarget(input)
		if err != nil || got != want {
			t.Fatalf("target %q => %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"Fixture Name", "", "not-a-number@lid", "12345@@lid", "12345:65536@lid", "12345:-1@lid", "12345.256:0@lid", "status@broadcast", "12345@broadcast", "12345@newsletter", "12345@unknown", "@g.us", "-123@g.us", "1-2-3@g.us", "123:1@g.us", "s.whatsapp.net", "123@s.whatsapp.net"} {
		if _, err := NormalizeDraftTarget(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	data := draftTextFixture("unknown is not self")
	data.Recipient = DraftRecipient{JID: "12345@lid"}
	got := mustDraftPayload(t, data).Data().Recipient
	if got.JID != "12345@lid" || got.PN != "" || got.LID != "12345@lid" {
		t.Fatalf("invented PN: %+v", got)
	}
}

func TestDraftMappingsMentionsAndHash(t *testing.T) {
	data := draftTextFixture("fixture")
	base := mustDraftPayload(t, data)
	data.Recipient = DraftRecipient{JID: "12345:7@lid", PN: "15550000002@s.whatsapp.net"}
	mapped := mustDraftPayload(t, data)
	if mapped.Data().Recipient.JID != base.Data().Recipient.JID || mapped.Hash() == base.Hash() {
		t.Fatal("mapping must canonicalize routing and change effective hash")
	}
	data.Text.Mentions = []DraftRecipient{{JID: "15550000004@s.whatsapp.net"}, {JID: "15550000003@s.whatsapp.net"}, {JID: "15550000004:9@s.whatsapp.net"}}
	first := mustDraftPayload(t, data)
	data.Text.Mentions = []DraftRecipient{{JID: "15550000003@s.whatsapp.net"}, {JID: "15550000004@s.whatsapp.net"}}
	second := mustDraftPayload(t, data)
	if first.Hash() != second.Hash() || len(first.Data().Text.Mentions) != 2 {
		t.Fatal("mention ordering/dedup changed effective payload")
	}
	data.Text.Mentions = []DraftRecipient{{JID: "123@g.us"}}
	if _, err := NewDraftPayload(data); err == nil {
		t.Fatal("accepted group mention")
	}
	data.Text.Mentions = make([]DraftRecipient, MaxDraftMentions+1)
	if _, err := NewDraftPayload(data); err == nil {
		t.Fatal("accepted too many mention inputs")
	}
	data.Text.Mentions = []DraftRecipient{{JID: "15550000003@s.whatsapp.net"}, {JID: "15550000003@s.whatsapp.net", LID: "12345@lid"}}
	if _, err := NewDraftPayload(data); err == nil {
		t.Fatal("accepted conflicting mention observations")
	}
	data.Text.Mentions = nil
	data.Recipient = DraftRecipient{JID: "15550000002@s.whatsapp.net", PN: "15550000003@s.whatsapp.net"}
	if _, err := NewDraftPayload(data); err == nil {
		t.Fatal("accepted conflicting requested PN")
	}
}

func TestDraftReplyFrozenIdentityAndFields(t *testing.T) {
	data := draftTextFixture("reply")
	data.Recipient = DraftRecipient{JID: "15550000002@s.whatsapp.net", LID: "12345@lid"}
	data.Reply = &DraftReply{ChatJID: "12345@lid", ID: "real-ID", Sender: DraftRecipient{JID: "12345@lid", PN: "15550000002@s.whatsapp.net"}, Text: "real\ntext"}
	payload := mustDraftPayload(t, data)
	if got := payload.Data().Reply; got.ChatJID != data.Recipient.JID || got.Sender.JID != data.Recipient.JID || got.ID != "real-ID" || got.Text != "real\ntext" {
		t.Fatalf("quote = %+v", got)
	}
	for name, mutate := range map[string]func(*DraftReply){
		"other chat":            func(q *DraftReply) { q.ChatJID = "15550000003@s.whatsapp.net" },
		"empty ID":              func(q *DraftReply) { q.ID = "" },
		"unavailable text":      func(q *DraftReply) { q.Text = "" },
		"unknown sender":        func(q *DraftReply) { q.Sender = DraftRecipient{} },
		"group sender":          func(q *DraftReply) { q.Sender.JID = "123@g.us" },
		"wrong outgoing sender": func(q *DraftReply) { q.FromMe = true },
		"oversize quote":        func(q *DraftReply) { q.Text = strings.Repeat("x", MaxDraftFieldBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := payload.Data()
			mutate(copy.Reply)
			if _, err := NewDraftPayload(copy); err == nil {
				t.Fatal("accepted invalid quote")
			}
		})
	}
	data.Reply.FromMe = true
	data.Reply.Sender = DraftRecipient{JID: data.Account.PN}
	_ = mustDraftPayload(t, data)
}

func TestDraftDocumentMetadataAndContact(t *testing.T) {
	document := DraftDocument{Filename: "fixture.pdf", MIME: "Application/PDF; Z=last; A=first", Size: MaxDraftFileBytes, SHA256: strings.Repeat("a", 64)}
	canonical, err := NewDraftDocument(document)
	if err != nil || canonical.MIME != "application/pdf; a=first; z=last" {
		t.Fatalf("MIME: %+v %v", canonical, err)
	}
	for _, invalid := range []DraftDocument{
		{Filename: "fixture", MIME: "invalid", SHA256: strings.Repeat("a", 64)},
		{Filename: "fixture", MIME: "application/pdf", SHA256: strings.Repeat("A", 64)},
		{Filename: "fixture", MIME: "application/pdf", Size: MaxDraftFileBytes + 1, SHA256: strings.Repeat("a", 64)},
		{Filename: "fixture", MIME: "application/pdf", Size: -1, SHA256: strings.Repeat("a", 64)},
		{Filename: strings.Repeat("<", MaxDraftFieldBytes), MIME: "application/pdf", SHA256: strings.Repeat("a", 64)},
	} {
		if _, err := NewDraftDocument(invalid); err == nil {
			t.Fatal("accepted invalid document")
		}
	}
	data := draftTextFixture("")
	data.Kind, data.Text, data.Document = DraftDocumentKind, nil, &document
	_ = mustDraftPayload(t, data)
	data.Kind, data.Document, data.Contact = DraftContactKind, nil, &DraftContact{DisplayName: "Fixture", Phone: "+1 (555) 000-0003"}
	card := mustDraftPayload(t, data).Data().Contact
	if card.Phone != "15550000003" || !strings.Contains(card.VCard, "TEL;TYPE=CELL;waid=15550000003:+15550000003\r\n") {
		t.Fatalf("card = %+v", card)
	}
	data.Contact.VCard = "unrelated"
	if _, err := NewDraftPayload(data); err == nil {
		t.Fatal("accepted supplied divergent vCard")
	}
}

func TestDraftContactEscapesInjectionAndPostEscapeLimit(t *testing.T) {
	name := "A\\B;C,D\r\nTEL:+999\rFN:injected\n尾"
	card, err := NewDraftContact(name, "15550000003")
	if err != nil {
		t.Fatal(err)
	}
	if card.DisplayName != name {
		t.Fatal("display name changed")
	}
	lines := strings.Split(card.VCard, "\r\n")
	if len(lines) != 7 || lines[3] != `FN:A\\B\;C\,D\nTEL:+999\nFN:injected\n尾` {
		t.Fatalf("property injection or bad escape: %q", lines)
	}
	for _, phone := range []string{"12345@lid", "15550000003\r\nFN:injected", "١٥٥٥٠٠٠٠٠٠٣", "123", ""} {
		if _, err := NewDraftContact("Fixture", phone); err == nil {
			t.Fatalf("accepted phone %q", phone)
		}
	}
	if _, err := NewDraftContact(strings.Repeat("\\", 17000), "15550000003"); err == nil {
		t.Fatal("vCard exceeds post-escaping limit")
	}
	if _, err := NewDraftContact("Fixture\x00Name", "15550000003"); err == nil {
		t.Fatal("accepted invalid vCard control character")
	}
}

func TestDraftRevisionMetadataDoesNotChangePayloadHash(t *testing.T) {
	payload := mustDraftPayload(t, draftTextFixture("fixture"))
	draftID, err := NewDraftID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID, err := NewDraftID()
	if err != nil || revisionID == draftID {
		t.Fatalf("IDs: %v", err)
	}
	created := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("fixture", 3600))
	review := DraftReviewSnapshot{RequestedRaw: "+1 (555) 000-0002", RecipientName: "Fixture"}
	revision, err := NewDraftRevision(draftID, revisionID, created, payload, review)
	if err != nil {
		t.Fatal(err)
	}
	review.RecipientName = "Changed name"
	copy := revision.Review()
	copy.RequestedRaw = "different"
	if revision.DraftID() != draftID || revision.ID() != revisionID || revision.CreatedAt().Location() != time.UTC || revision.Review().RecipientName != "Fixture" || revision.Payload().Hash() != payload.Hash() {
		t.Fatal("revision changed")
	}
	other, err := NewDraftRevision(draftID, strings.Repeat("a", 32), created.Add(time.Minute), payload, review)
	if err != nil || other.Payload().Hash() != revision.Payload().Hash() {
		t.Fatalf("metadata affected hash: %v", err)
	}
	for _, id := range []string{"", "../escape", strings.Repeat("A", 32), strings.Repeat("a", 31)} {
		if err := ValidateDraftID(id); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
		if path, err := DraftSnapshotRelativePath(id); err == nil || path != "" {
			t.Fatalf("constructed path from invalid ID: %q", path)
		}
	}
	if _, err := NewDraftRevision(draftID, revisionID, created, DraftPayload{}, review); err == nil {
		t.Fatal("accepted zero payload")
	}
	review.RequestedRaw = "15550000004"
	if _, err := NewDraftRevision(draftID, revisionID, created, payload, review); err == nil {
		t.Fatal("accepted unrelated review target")
	}
}
