package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types"
)

const (
	DraftPayloadVersion  = 1
	MaxDraftFieldBytes   = 64 << 10
	MaxDraftPayloadBytes = 256 << 10
	MaxDraftMentions     = 200
	MaxDraftFileBytes    = 100 << 20
	draftHashPrefix      = "wacli-draft-payload-v1\x00"
)

type DraftKind string

const (
	DraftTextKind     DraftKind = "text"
	DraftDocumentKind DraftKind = "document"
	DraftContactKind  DraftKind = "contact"
)

// DraftValidationError contains field names, never caller content or paths.
type DraftValidationError struct{ Field, Reason string }

func (e *DraftValidationError) Error() string { return "invalid draft " + e.Field + ": " + e.Reason }

func invalidDraft(field, reason string) error { return &DraftValidationError{field, reason} }

// DraftIdentity records public facts supplied by a local identity reader.
// An absent LID means unknown, not that the account has no LID remotely.
type DraftIdentity struct {
	PN  string `json:"pn"`
	LID string `json:"lid"`
}

type DraftRecipient struct {
	JID string `json:"jid"`
	PN  string `json:"pn"`
	LID string `json:"lid"`
}

type DraftText struct {
	Text     string           `json:"text"`
	Mentions []DraftRecipient `json:"mentions"`
}

type DraftDocument struct {
	Filename string `json:"filename"`
	MIME     string `json:"mime"`
	Caption  string `json:"caption"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type DraftContact struct {
	DisplayName string `json:"display_name"`
	Phone       string `json:"phone"`
	VCard       string `json:"vcard"`
}

// DraftReply is frozen text, not a request to reload message content at send time.
// The local lookup must additionally reject unavailable/deleted records and
// divergent records with the same message ID in verified chat aliases.
type DraftReply struct {
	ChatJID string         `json:"chat_jid"`
	ID      string         `json:"id"`
	Sender  DraftRecipient `json:"sender"`
	FromMe  bool           `json:"from_me"`
	Text    string         `json:"text"`
}

type DraftDefaults struct {
	LinkPreview bool   `json:"link_preview"`
	Ephemeral   bool   `json:"ephemeral"`
	Expiration  uint32 `json:"expiration"`
	AllowSelf   bool   `json:"allow_self"`
}

// DraftPayloadData is the serialization boundary. NewDraftPayload validates
// the union and freezes a copy; callers cannot mutate a constructed payload.
type DraftPayloadData struct {
	Version   int            `json:"version"`
	Account   DraftIdentity  `json:"account"`
	Recipient DraftRecipient `json:"recipient"`
	Kind      DraftKind      `json:"kind"`
	Defaults  DraftDefaults  `json:"defaults"`
	Text      *DraftText     `json:"text,omitempty"`
	Document  *DraftDocument `json:"document,omitempty"`
	Contact   *DraftContact  `json:"contact,omitempty"`
	Reply     *DraftReply    `json:"reply,omitempty"`
}

type DraftPayload struct {
	canonical string
	hash      string
}

func (p DraftPayload) CanonicalJSON() []byte { return []byte(p.canonical) }
func (p DraftPayload) Hash() string          { return p.hash }

// Data returns independent pointers and slices. The zero value is not valid.
func (p DraftPayload) Data() DraftPayloadData {
	var data DraftPayloadData
	_ = json.Unmarshal([]byte(p.canonical), &data)
	return data
}

func NewDraftPayload(data DraftPayloadData) (DraftPayload, error) {
	if data.Version != 0 && data.Version != DraftPayloadVersion {
		return DraftPayload{}, invalidDraft("version", "unsupported version")
	}
	data.Version = DraftPayloadVersion
	if data.Defaults != (DraftDefaults{}) {
		return DraftPayload{}, invalidDraft("defaults", "only explicit disabled defaults are supported")
	}
	account, err := normalizeDraftAccount(data.Account)
	if err != nil {
		return DraftPayload{}, err
	}
	data.Account = account
	data.Recipient, err = normalizeDraftRecipient(data.Recipient, false)
	if err != nil {
		return DraftPayload{}, err
	}
	if draftRecipientIsSelf(data.Recipient, data.Account) {
		return DraftPayload{}, invalidDraft("recipient", "matches the observed local account")
	}
	count := 0
	for _, present := range []bool{data.Text != nil, data.Document != nil, data.Contact != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return DraftPayload{}, invalidDraft("payload", "exactly one content variant is required")
	}
	switch data.Kind {
	case DraftTextKind:
		if data.Text == nil {
			return DraftPayload{}, invalidDraft("kind", "does not match content")
		}
		text := *data.Text
		if err := checkDraftString("text", text.Text, true); err != nil {
			return DraftPayload{}, err
		}
		text.Mentions, err = normalizeDraftMentions(text.Mentions)
		if err != nil {
			return DraftPayload{}, err
		}
		data.Text = &text
	case DraftDocumentKind:
		if data.Document == nil {
			return DraftPayload{}, invalidDraft("kind", "does not match content")
		}
		document, err := NewDraftDocument(*data.Document)
		if err != nil {
			return DraftPayload{}, err
		}
		data.Document = &document
	case DraftContactKind:
		if data.Contact == nil || data.Reply != nil {
			return DraftPayload{}, invalidDraft("kind", "contact content is required and contact replies are unsupported")
		}
		contact := *data.Contact
		card, err := NewDraftContact(contact.DisplayName, contact.Phone)
		if err != nil {
			return DraftPayload{}, err
		}
		if contact.VCard != "" && contact.VCard != card.VCard {
			return DraftPayload{}, invalidDraft("vcard", "does not match explicit contact fields")
		}
		data.Contact = &card
	default:
		return DraftPayload{}, invalidDraft("kind", "unsupported content")
	}
	if data.Reply != nil {
		reply := *data.Reply
		if err := normalizeDraftReply(&reply, data.Recipient, data.Account); err != nil {
			return DraftPayload{}, err
		}
		data.Reply = &reply
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return DraftPayload{}, err
	}
	if len(raw) > MaxDraftPayloadBytes {
		return DraftPayload{}, invalidDraft("payload", "canonical JSON exceeds 256 KiB")
	}
	sum := sha256.Sum256(append([]byte(draftHashPrefix), raw...))
	return DraftPayload{canonical: string(raw), hash: hex.EncodeToString(sum[:])}, nil
}

// DecodeDraftPayload accepts only canonical, versioned internal serialization.
// Re-encoding also rejects duplicate/missing fields, case aliases, lossy UTF-8
// or surrogate escapes, alternate defaults and trailing JSON.
func DecodeDraftPayload(raw []byte) (DraftPayload, error) {
	if len(raw) > MaxDraftPayloadBytes || !utf8.Valid(raw) {
		return DraftPayload{}, invalidDraft("payload", "invalid encoding or size")
	}
	var data DraftPayloadData
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return DraftPayload{}, invalidDraft("payload", "invalid internal JSON")
	}
	payload, err := NewDraftPayload(data)
	if err != nil {
		return DraftPayload{}, err
	}
	if !bytes.Equal(payload.CanonicalJSON(), raw) {
		return DraftPayload{}, invalidDraft("payload", "noncanonical internal JSON")
	}
	return payload, nil
}

// NormalizeDraftTarget accepts only an explicit phone number or DM/group JID.
// It never searches contacts or resolves identities over the network.
func NormalizeDraftTarget(raw string) (string, error) {
	if !utf8.ValidString(raw) || len(raw) > 256 {
		return "", invalidDraft("recipient", "invalid encoding or size")
	}
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "@") {
		phone, err := normalizeDraftPhone(raw)
		if err != nil {
			return "", err
		}
		return phone + "@" + types.DefaultUserServer, nil
	}
	if strings.Count(raw, "@") != 1 {
		return "", invalidDraft("recipient", "expected an explicit phone or DM/group JID")
	}
	user, server, _ := strings.Cut(raw, "@")
	switch server {
	case types.DefaultUserServer, types.HiddenUserServer:
		if base, device, found := strings.Cut(user, ":"); found {
			if !draftDigits(device) {
				return "", invalidDraft("recipient", "invalid device suffix")
			}
			if _, err := strconv.ParseUint(device, 10, 16); err != nil {
				return "", invalidDraft("recipient", "invalid device suffix")
			}
			user = base
		}
		if !draftDigits(user) || (server == types.DefaultUserServer && (len(user) < 7 || len(user) > 15)) {
			return "", invalidDraft("recipient", "invalid numeric user")
		}
	case types.GroupServer:
		parts := strings.Split(user, "-")
		if len(parts) > 2 || !draftDigits(parts[0]) || (len(parts) == 2 && !draftDigits(parts[1])) {
			return "", invalidDraft("recipient", "invalid group user")
		}
	default:
		return "", invalidDraft("recipient", "only explicit DM/group targets are supported; discover JIDs with contacts search/resolve or chats list")
	}
	return user + "@" + server, nil
}

func normalizeDraftAccount(account DraftIdentity) (DraftIdentity, error) {
	if account.PN == "" {
		return DraftIdentity{}, invalidDraft("account", "an unambiguous local PN identity is required")
	}
	pn, err := NormalizeDraftTarget(account.PN)
	if err != nil || !strings.HasSuffix(pn, "@"+types.DefaultUserServer) {
		return DraftIdentity{}, invalidDraft("account", "invalid own PN")
	}
	account.PN = pn
	if account.LID != "" {
		lid, err := NormalizeDraftTarget(account.LID)
		if err != nil || !strings.HasSuffix(lid, "@"+types.HiddenUserServer) {
			return DraftIdentity{}, invalidDraft("account", "invalid own LID")
		}
		account.LID = lid
	}
	return account, nil
}

func normalizeDraftRecipient(recipient DraftRecipient, userOnly bool) (DraftRecipient, error) {
	jid, err := NormalizeDraftTarget(recipient.JID)
	if err != nil {
		return DraftRecipient{}, err
	}
	if strings.HasSuffix(jid, "@"+types.GroupServer) {
		if userOnly || recipient.PN != "" || recipient.LID != "" {
			return DraftRecipient{}, invalidDraft("recipient", "user identity required or invalid group mapping")
		}
		return DraftRecipient{JID: jid}, nil
	}
	if recipient.PN != "" {
		recipient.PN, err = NormalizeDraftTarget(recipient.PN)
		if err != nil || !strings.HasSuffix(recipient.PN, "@"+types.DefaultUserServer) {
			return DraftRecipient{}, invalidDraft("mapping", "invalid PN")
		}
	}
	if recipient.LID != "" {
		recipient.LID, err = NormalizeDraftTarget(recipient.LID)
		if err != nil || !strings.HasSuffix(recipient.LID, "@"+types.HiddenUserServer) {
			return DraftRecipient{}, invalidDraft("mapping", "invalid LID")
		}
	}
	if strings.HasSuffix(jid, "@"+types.DefaultUserServer) {
		if recipient.PN != "" && recipient.PN != jid {
			return DraftRecipient{}, invalidDraft("mapping", "PN differs from requested JID")
		}
		recipient.PN = jid
	} else {
		if recipient.LID != "" && recipient.LID != jid {
			return DraftRecipient{}, invalidDraft("mapping", "LID differs from requested JID")
		}
		recipient.LID = jid
	}
	recipient.JID = jid
	if recipient.PN != "" {
		recipient.JID = recipient.PN
	}
	return recipient, nil
}

func draftRecipientIsSelf(recipient DraftRecipient, account DraftIdentity) bool {
	return recipient.PN == account.PN || (account.LID != "" && recipient.LID == account.LID)
}

func normalizeDraftMentions(mentions []DraftRecipient) ([]DraftRecipient, error) {
	if len(mentions) > MaxDraftMentions {
		return nil, invalidDraft("mentions", "at most 200 entries are supported")
	}
	normalized := make([]DraftRecipient, 0, len(mentions))
	for _, mention := range mentions {
		value, err := normalizeDraftRecipient(mention, true)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, value)
	}
	slices.SortFunc(normalized, func(a, b DraftRecipient) int { return strings.Compare(a.JID, b.JID) })
	result := make([]DraftRecipient, 0, len(normalized))
	for _, mention := range normalized {
		if len(result) > 0 && result[len(result)-1].JID == mention.JID {
			if result[len(result)-1] != mention {
				return nil, invalidDraft("mentions", "conflicting identity observations")
			}
			continue
		}
		result = append(result, mention)
	}
	return result, nil
}

func normalizeDraftReply(reply *DraftReply, target DraftRecipient, account DraftIdentity) error {
	chat, err := NormalizeDraftTarget(reply.ChatJID)
	if err != nil || (chat != target.JID && chat != target.PN && chat != target.LID) {
		return invalidDraft("reply", "chat does not match the frozen recipient or verified alias")
	}
	reply.ChatJID = target.JID
	if err := checkDraftString("reply.id", reply.ID, true); err != nil {
		return err
	}
	if err := checkDraftString("reply.text", reply.Text, true); err != nil {
		return err
	}
	reply.Sender, err = normalizeDraftRecipient(reply.Sender, true)
	if err != nil {
		return invalidDraft("reply.sender", "a known user sender is required")
	}
	own := DraftRecipient{JID: account.PN, PN: account.PN, LID: account.LID}
	if reply.FromMe {
		if !draftRecipientsMatch(reply.Sender, own) {
			return invalidDraft("reply.sender", "outgoing quote does not match the local account")
		}
	} else {
		if draftRecipientIsSelf(reply.Sender, account) {
			return invalidDraft("reply.sender", "incoming quote contradicts the observed local account")
		}
		if !strings.HasSuffix(target.JID, "@"+types.GroupServer) && !draftRecipientsMatch(reply.Sender, target) {
			return invalidDraft("reply.sender", "incoming DM quote does not match the observed peer")
		}
	}
	return nil
}

// Match only shared observed identities, rejecting contradictory pairs. An
// unmapped LID cannot establish equality with a PN by guessing a phone.
func draftRecipientsMatch(a, b DraftRecipient) bool {
	if a.PN != "" && b.PN != "" && a.PN != b.PN || a.LID != "" && b.LID != "" && a.LID != b.LID {
		return false
	}
	return a.JID == b.JID || a.PN != "" && a.PN == b.PN || a.LID != "" && a.LID == b.LID
}

// NewDraftDocument validates and canonicalizes metadata without reading files.
func NewDraftDocument(document DraftDocument) (DraftDocument, error) {
	for _, field := range []struct {
		name, value string
		required    bool
	}{{"filename", document.Filename, true}, {"mime", document.MIME, true}, {"caption", document.Caption, false}} {
		if err := checkDraftString(field.name, field.value, field.required); err != nil {
			return DraftDocument{}, err
		}
	}
	typ, params, err := mime.ParseMediaType(document.MIME)
	if err != nil || strings.Count(typ, "/") != 1 || strings.Contains(typ, "*") {
		return DraftDocument{}, invalidDraft("mime", "invalid MIME type")
	}
	document.MIME = mime.FormatMediaType(typ, params)
	if err := checkDraftString("mime", document.MIME, true); err != nil {
		return DraftDocument{}, err
	}
	if document.Size < 0 || document.Size > MaxDraftFileBytes || !draftHex(document.SHA256, sha256.Size*2) {
		return DraftDocument{}, invalidDraft("document", "invalid size or SHA-256")
	}
	encoded, err := json.Marshal(document)
	if err != nil || len(encoded) > MaxDraftPayloadBytes {
		return DraftDocument{}, invalidDraft("document", "encoded metadata exceeds the payload budget")
	}
	return document, nil
}

func checkDraftString(field, value string, required bool) error {
	if !utf8.ValidString(value) || len(value) > MaxDraftFieldBytes || (required && value == "") {
		return invalidDraft(field, "expected UTF-8 within 64 KiB with required content present")
	}
	return nil
}

func draftDigits(value string) bool {
	return value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func normalizeDraftPhone(raw string) (string, error) {
	var phone strings.Builder
	for i, r := range raw {
		switch {
		case r == '+' && i == 0:
		case r >= '0' && r <= '9':
			phone.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", invalidDraft("phone", "use an explicit PN phone, or discover a JID with contacts search/resolve or chats list")
		}
	}
	value := phone.String()
	if len(value) < 7 || len(value) > 15 {
		return "", invalidDraft("phone", "expected 7 to 15 digits")
	}
	return value, nil
}

func draftHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func NewDraftID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate draft ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func ValidateDraftID(id string) error {
	if !draftHex(id, 32) {
		return invalidDraft("id", "expected 32 lowercase hexadecimal characters")
	}
	return nil
}
