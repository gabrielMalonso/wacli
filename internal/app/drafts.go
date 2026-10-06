package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

// DraftImageInput selects image preparation without legacy document fields.
type DraftImageInput struct {
	Path string `json:"path"`
}

type DraftVoiceInput struct {
	Path string `json:"path"`
}

// DraftInput is a bounded local request, never a send operation. Message is a
// pointer so missing text and an explicitly empty text cannot select another kind.
type DraftInput struct {
	To       string              `json:"to"`
	Message  *string             `json:"message,omitempty"`
	Mentions []string            `json:"mentions,omitempty"`
	ReplyTo  string              `json:"reply_to,omitempty"`
	File     string              `json:"file,omitempty"`
	Filename string              `json:"filename,omitempty"`
	MIME     string              `json:"mime,omitempty"`
	Caption  string              `json:"caption,omitempty"`
	Contact  *store.DraftContact `json:"contact,omitempty"`
	Image    *DraftImageInput    `json:"image,omitempty"`
	Voice    *DraftVoiceInput    `json:"voice,omitempty"`
}

type DraftWriteRequest struct {
	Version          int         `json:"version"`
	Action           string      `json:"action"`
	DraftID          string      `json:"draft_id"`
	RevisionID       string      `json:"revision_id"`
	ExpectedRevision string      `json:"expected_revision,omitempty"`
	StoreRef         string      `json:"store_ref"`
	AccountName      string      `json:"account_name"`
	Input            *DraftInput `json:"input,omitempty"`
}

func (r DraftWriteRequest) Validate() error {
	image := r.Input != nil && r.Input.Image != nil
	voice := r.Input != nil && r.Input.Voice != nil
	if r.Version < 1 || r.Version > 3 || (r.Version == 2) != image || (r.Version == 3) != voice || (image || voice) && r.Action != "create" && r.Action != "update" {
		return &store.DraftValidationError{Field: "request.version", Reason: "unsupported"}
	}
	for _, id := range []string{r.DraftID, r.RevisionID} {
		if err := store.ValidateDraftID(id); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(r.StoreRef) || len(r.StoreRef) > 4096 || !utf8.ValidString(r.StoreRef) || len(r.AccountName) > store.MaxDraftFieldBytes || !utf8.ValidString(r.AccountName) {
		return &store.DraftValidationError{Field: "scope", Reason: "invalid selected archive"}
	}
	switch r.Action {
	case "create":
		if r.ExpectedRevision != "" || r.Input == nil {
			return &store.DraftValidationError{Field: "create", Reason: "complete input required"}
		}
	case "update":
		if r.Input == nil {
			return &store.DraftValidationError{Field: "update", Reason: "complete input required"}
		}
		if err := store.ValidateDraftID(r.ExpectedRevision); err != nil {
			return err
		}
	case "discard":
		if r.Input != nil || r.ExpectedRevision != r.RevisionID {
			return &store.DraftValidationError{Field: "discard", Reason: "matching revision required"}
		}
	default:
		return &store.DraftValidationError{Field: "action", Reason: "unsupported"}
	}
	if r.Input != nil {
		if err := r.Input.Validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil || len(raw) > store.MaxDraftPayloadBytes+16384 {
			return &store.DraftValidationError{Field: "request", Reason: "encoded request limit exceeded"}
		}
		return nil
	}
	return nil
}

func (in DraftInput) Validate() error {
	if _, err := store.NormalizeDraftTarget(in.To); err != nil {
		return err
	}
	if len(in.Mentions) > store.MaxDraftMentions {
		return &store.DraftValidationError{Field: "mentions", Reason: "at most 200 entries"}
	}
	for _, raw := range in.Mentions {
		jid, err := store.NormalizeDraftTarget(raw)
		if err != nil {
			return err
		}
		if strings.HasSuffix(jid, "@g.us") {
			return &store.DraftValidationError{Field: "mention", Reason: "users only"}
		}
	}
	count := 0
	if in.Message != nil {
		count++
	}
	if in.File != "" {
		count++
	}
	if in.Image != nil {
		count++
	}
	if in.Voice != nil {
		count++
	}
	if in.Contact != nil {
		count++
	}
	if count != 1 {
		return &store.DraftValidationError{Field: "input", Reason: "exactly one text, document, image, voice or contact variant is required"}
	}
	fields := []string{in.To, in.ReplyTo, in.Filename, in.MIME, in.Caption, in.File}
	if in.Image != nil {
		if in.Image.Path == "" || in.Filename != "" || in.MIME != "" {
			return &store.DraftValidationError{Field: "image", Reason: "image path required; document options are unsupported"}
		}
		fields = append(fields, in.Image.Path)
	}
	if in.Voice != nil {
		if in.Voice.Path == "" || in.Filename != "" || in.MIME != "" || in.Caption != "" || len(in.Mentions) > 0 {
			return &store.DraftValidationError{Field: "voice.invalid", Reason: "voice path required; caption, document options and mentions are unsupported"}
		}
		fields = append(fields, in.Voice.Path)
	}
	if in.Message != nil {
		fields = append(fields, *in.Message)
		if *in.Message == "" {
			return &store.DraftValidationError{Field: "text", Reason: "nonempty literal text required"}
		}
	}
	for _, value := range fields {
		if !utf8.ValidString(value) || len(value) > store.MaxDraftFieldBytes {
			return &store.DraftValidationError{Field: "input", Reason: "field UTF-8/64 KiB limit exceeded"}
		}
	}
	if in.Message == nil && len(in.Mentions) > 0 || in.File == "" && (in.Filename != "" || in.MIME != "" || in.Image == nil && in.Caption != "") || in.Contact != nil && in.ReplyTo != "" {
		return &store.DraftValidationError{Field: "input", Reason: "options do not match content variant"}
	}
	if in.Contact != nil {
		if _, err := store.NewDraftContact(in.Contact.DisplayName, in.Contact.Phone); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(in)
	if err != nil || len(encoded) > store.MaxDraftPayloadBytes {
		return &store.DraftValidationError{Field: "input", Reason: "encoded request exceeds 256 KiB"}
	}
	return nil
}

// WriteLocalDraft has no WAClient dependency and never opens the session for
// writing. The caller owns LOCK; IPC calls use the follow owner's existing lock.
func (a *App) WriteLocalDraft(ctx context.Context, request DraftWriteRequest, openSource func(string) (*os.File, error)) (store.DraftEntry, error) {
	if err := request.Validate(); err != nil {
		return store.DraftEntry{}, err
	}
	if a.opts.ReadOnly {
		return store.DraftEntry{}, store.DraftFailure("read_only", request.DraftID, request.RevisionID, "", nil)
	}
	if request.StoreRef != a.StoreDir() {
		return store.DraftEntry{}, store.DraftFailure("store_unavailable", request.DraftID, request.RevisionID, "", nil)
	}
	if request.Action == "discard" {
		return a.DB().DiscardDraft(ctx, request.DraftID, request.ExpectedRevision)
	}
	input := *request.Input
	target, err := store.NormalizeDraftTarget(input.To)
	if err != nil {
		return store.DraftEntry{}, err
	}
	if _, err = ParseHistoryJID(target); err != nil {
		return store.DraftEntry{}, err
	}
	initial, err := a.ReadDraftIdentities(ctx, []string{target})
	if err != nil {
		return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", err)
	}
	if len(initial) != 1 || initial[0].AccountJID == "" {
		return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", nil)
	}
	first := initial[0]
	ownPN, ownErr := store.NormalizeDraftTarget(first.AccountJID)
	if ownErr != nil || ownPN != first.AccountJID || !strings.HasSuffix(ownPN, "@s.whatsapp.net") {
		return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", ownErr)
	}
	if first.AccountAliasJID != "" {
		lid, err := store.NormalizeDraftTarget(first.AccountAliasJID)
		if err != nil || lid != first.AccountAliasJID || !strings.HasSuffix(lid, "@lid") {
			return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", err)
		}
	}

	if request.Action == "update" {
		current, err := a.DB().ReadDraftRecord(ctx, request.DraftID)
		if err != nil {
			return store.DraftEntry{}, err
		}
		if current.AccountID != first.AccountJID {
			return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", nil)
		}
		if current.State != "active" || current.HeadRevisionID != request.ExpectedRevision {
			return store.DraftEntry{}, store.DraftFailure("draft_conflict", request.DraftID, request.RevisionID, "", nil)
		}
	}
	inputs := []string{target}
	for _, raw := range input.Mentions {
		jid, _ := store.NormalizeDraftTarget(raw)
		inputs = append(inputs, jid)
	}
	var quoted []store.DraftQuoteRecord
	if input.ReplyTo != "" {
		quoted, err = a.DB().ReadDraftQuote(ctx, first.ChatJID, first.AliasJID, input.ReplyTo)
		if err != nil {
			return store.DraftEntry{}, err
		}
		if len(quoted) == 0 {
			return store.DraftEntry{}, store.DraftFailure("not_found", request.DraftID, request.RevisionID, "", nil)
		}
		// Unavailable content in either alias takes precedence over its type.
		unsupported := false
		for _, q := range quoted {
			if q.Unavailable {
				return store.DraftEntry{}, &store.DraftValidationError{Field: "reply", Reason: "quote must have available text and known sender"}
			}
			unsupported = unsupported || q.Unsupported
		}
		if unsupported {
			return store.DraftEntry{}, &store.DraftValidationError{Field: "reply.unsupported", Reason: "quoted content is not supported for draft replies"}
		}
		for _, q := range quoted {
			if strings.TrimSpace(q.Text) == "" || q.SenderJID == "" {
				return store.DraftEntry{}, &store.DraftValidationError{Field: "reply", Reason: "quote must have available text and known sender"}
			}
			jid, err := store.NormalizeDraftTarget(q.SenderJID)
			if err != nil {
				return store.DraftEntry{}, err
			}
			inputs = append(inputs, jid)
		}
	}
	identities, err := a.ReadDraftIdentities(ctx, inputs)
	if err != nil {
		return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", err)
	}
	byInput := make(map[string]HistoryIdentity, len(identities))
	for _, identity := range identities {
		if identity.AccountJID != first.AccountJID {
			return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", nil)
		}
		byInput[identity.InputJID] = identity
	}
	current := byInput[target]
	if current != first {
		return store.DraftEntry{}, store.DraftFailure("draft_conflict", request.DraftID, request.RevisionID, "", nil)
	}
	account := store.DraftIdentity{PN: first.AccountJID, LID: first.AccountAliasJID}
	recipient := store.DraftRecipientFromPublic(first.ChatJID, first.AliasJID)
	data := store.DraftPayloadData{Account: account, Recipient: recipient}
	if input.Message != nil {
		data.Kind = store.DraftTextKind
		data.Text = &store.DraftText{Text: *input.Message}
		for _, raw := range input.Mentions {
			jid, _ := store.NormalizeDraftTarget(raw)
			identity := byInput[jid]
			data.Text.Mentions = append(data.Text.Mentions, store.DraftRecipientFromPublic(identity.ChatJID, identity.AliasJID))
		}
	} else if input.Contact != nil {
		data.Kind = store.DraftContactKind
		data.Contact = input.Contact
	} else if input.Image != nil {
		data.Kind = store.DraftImageKind
		// Content metadata is filled from the captured image bytes below.
		data.Image = &store.DraftImage{MIME: "image/png", Caption: input.Caption}
	} else if input.Voice != nil {
		data.Kind = store.DraftVoiceKind
		data.Voice = &store.DraftVoice{}
	} else {
		name := input.Filename
		if name == "" {
			name = filepath.Base(input.File)
		}
		mimetype := input.MIME
		if mimetype == "" {
			mimetype = "application/octet-stream"
		}
		data.Kind = store.DraftDocumentKind
		data.Document = &store.DraftDocument{Filename: name, MIME: mimetype, Caption: input.Caption, Size: store.MaxDraftFileBytes, SHA256: strings.Repeat("0", sha256.Size*2)}
	}
	for _, q := range quoted {
		jid, _ := store.NormalizeDraftTarget(q.SenderJID)
		identity := byInput[jid]
		candidate := store.DraftReply{ChatJID: recipient.JID, ID: q.ID, Sender: store.DraftRecipientFromPublic(identity.ChatJID, identity.AliasJID), FromMe: q.FromMe, Text: q.Text}
		if data.Reply != nil && *data.Reply != candidate {
			return store.DraftEntry{}, &store.DraftValidationError{Field: "reply", Reason: "divergent quote records in verified aliases"}
		}
		data.Reply = &candidate
	}
	preflightData := data
	if data.Image != nil {
		preflightData.Kind, preflightData.Image = store.DraftTextKind, nil
		preflightData.Text = &store.DraftText{Text: "image preparation"}
	}
	if data.Voice != nil {
		preflightData.Kind, preflightData.Voice = store.DraftTextKind, nil
		preflightData.Text = &store.DraftText{Text: "voice preparation"}
	}
	if _, err := store.NewDraftPayload(preflightData); err != nil {
		return store.DraftEntry{}, err
	}
	name := ""
	if chat, err := a.DB().GetChat(recipient.JID); err == nil {
		name = chat.Name
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.DraftEntry{}, store.DraftFailure("store_unavailable", request.DraftID, request.RevisionID, "", err)
	}
	// Canonical contact metadata already merges aliases in PR9; this bounded
	// query is display-only and does not introduce nominal recipient routing.
	if !strings.HasSuffix(recipient.JID, "@g.us") {
		page, err := a.ReadContacts(ctx, ContactReadOptions{Operation: ContactSearch, Query: recipient.JID, Limit: 1, Paginate: true})
		if err != nil {
			return store.DraftEntry{}, store.DraftFailure("identity_unavailable", request.DraftID, request.RevisionID, "", err)
		}
		if len(page.Contacts) == 1 && page.Contacts[0].JID == recipient.JID {
			name = page.Contacts[0].Name
		}
	}
	review := store.DraftReviewSnapshot{RequestedRaw: input.To, RecipientName: name, AccountName: request.AccountName}
	if data.Kind.HasUpload() {
		review.SnapshotPath, _ = store.DraftSnapshotRelativePath(request.RevisionID)
		review.VerifiedAtCreate = time.Now().UTC()
		if data.Document != nil {
			preflight, _ := store.NewDraftPayload(data)
			if _, err := store.NewDraftRevision(request.DraftID, request.RevisionID, time.Now().UTC(), preflight, review); err != nil {
				return store.DraftEntry{}, err
			}
		}
		sourcePath := input.File
		if input.Image != nil {
			sourcePath = input.Image.Path
		}
		if input.Voice != nil {
			sourcePath = input.Voice.Path
		}
		options := DraftSnapshotOptions{StoreDir: a.StoreDir(), RevisionID: request.RevisionID, SourcePath: sourcePath, Image: input.Image != nil, Voice: input.Voice != nil, Filename: input.Filename, MIME: input.MIME, Caption: input.Caption, OpenSource: openSource}
		if input.Image != nil {
			options.validateImage = func(value store.DraftImage) error {
				candidate := data
				candidate.Image = &value
				payload, err := store.NewDraftPayload(candidate)
				if err != nil {
					return err
				}
				_, err = store.NewDraftRevision(request.DraftID, request.RevisionID, time.Now().UTC(), payload, review)
				return err
			}
		}
		if input.Voice != nil {
			options.validateVoice = func(value store.DraftVoice) error {
				candidate := data
				candidate.Voice = &value
				payload, err := store.NewDraftPayload(candidate)
				if err != nil {
					return err
				}
				_, err = store.NewDraftRevision(request.DraftID, request.RevisionID, time.Now().UTC(), payload, review)
				return err
			}
		}
		snapshot, err := CreateDraftSnapshot(ctx, options)
		if err != nil {
			// Typed voice refusals precede publication. Preserve their sanitized
			// category through the existing validation/IPC advice contract.
			if input.Voice != nil {
				if validation := voiceSnapshotValidation(err); validation != nil {
					return store.DraftEntry{}, validation
				}
			}
			code := "document_unavailable"
			if input.Voice != nil {
				code = "store_unavailable"
			}
			if input.Image != nil {
				code = "image_unavailable"
			}
			var snapshotErr *DraftSnapshotError
			if errors.As(err, &snapshotErr) && (snapshotErr.Publication == DraftPublicationUnknown || input.Voice != nil && snapshotErr.Publication == DraftPublished) {
				code = "local_write_uncertain"
			}
			return store.DraftEntry{}, store.DraftFailure(code, request.DraftID, request.RevisionID, "", err)
		}
		if snapshot.RevisionID() != request.RevisionID {
			return store.DraftEntry{}, store.DraftFailure("local_write_uncertain", request.DraftID, request.RevisionID, "", nil)
		}
		if input.Image != nil {
			data.Image = snapshot.Image()
		} else if input.Voice != nil {
			data.Voice = snapshot.Voice()
		} else {
			doc := snapshot.Document()
			data.Document = &doc
		}
		review.SnapshotPath = snapshot.RelativePath()
		review.VerifiedAtCreate = snapshot.VerifiedAtCreate()
	}
	payload, err := store.NewDraftPayload(data)
	if err != nil {
		return store.DraftEntry{}, err
	}
	revision, err := store.NewDraftRevision(request.DraftID, request.RevisionID, time.Now().UTC(), payload, review)
	if err != nil {
		return store.DraftEntry{}, err
	}
	entry, err := a.DB().WriteDraft(ctx, revision, request.ExpectedRevision)
	if err != nil {
		var failure *store.DraftError
		if !errors.As(err, &failure) {
			return store.DraftEntry{}, store.DraftFailure("store_unavailable", request.DraftID, request.RevisionID, payload.Hash(), err)
		}
	}
	return entry, err
}

// Only the inspector's confirmed pre-publication refusal can carry voice advice.
// Cancellation in copying, transport, publication or later effects stays a base
// failure; it must never be relabeled as an arguments refusal.
func voiceSnapshotValidation(err error) *store.DraftValidationError {
	var snapshot *DraftSnapshotError
	if !errors.As(err, &snapshot) || snapshot.Publication != DraftUnpublished || snapshot.Stage != "voice" {
		return nil
	}
	var voice *wa.OggOpusError
	if errors.As(err, &voice) {
		return &store.DraftValidationError{Field: "voice." + voice.Code, Reason: voice.Reason}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &store.DraftValidationError{Field: "voice.canceled", Reason: "structural voice validation canceled before publication"}
	}
	return nil
}

// UnmarshalJSON accepts only the versioned canonical internal request. It
// rejects unknown/duplicate fields and malformed UTF-8 before mutation.
func (r *DraftWriteRequest) UnmarshalJSON(raw []byte) error {
	type plain DraftWriteRequest
	if !utf8.Valid(raw) || len(raw) > store.MaxDraftPayloadBytes+16384 {
		return &store.DraftValidationError{Field: "request", Reason: "invalid encoding or size"}
	}
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return &store.DraftValidationError{Field: "request", Reason: "invalid typed input"}
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(raw, canonical) {
		return &store.DraftValidationError{Field: "request", Reason: "noncanonical typed input"}
	}
	next := DraftWriteRequest(value)
	if err := next.Validate(); err != nil {
		return err
	}
	*r = next
	return nil
}
