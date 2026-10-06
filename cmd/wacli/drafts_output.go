package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

type draftDocumentDTO struct {
	Filename         string    `json:"filename"`
	MIME             string    `json:"mime"`
	Caption          string    `json:"caption"`
	Size             int64     `json:"size"`
	SHA256           string    `json:"sha256"`
	VerifiedAtCreate time.Time `json:"verified_at_create"`
	SnapshotPath     string    `json:"snapshot_path,omitempty"`
}
type draftImageDTO struct {
	MIME             string    `json:"mime"`
	Caption          string    `json:"caption"`
	Size             int64     `json:"size"`
	SHA256           string    `json:"sha256"`
	Width            uint32    `json:"width"`
	Height           uint32    `json:"height"`
	ThumbnailBytes   int       `json:"thumbnail_bytes"`
	ThumbnailSHA256  string    `json:"thumbnail_sha256"`
	VerifiedAtCreate time.Time `json:"verified_at_create"`
	SnapshotPath     string    `json:"snapshot_path,omitempty"`
}

type draftDTO struct {
	ID              string               `json:"id"`
	RevisionID      string               `json:"revision_id"`
	Revision        int                  `json:"revision"`
	Hash            string               `json:"hash"`
	PayloadVersion  int                  `json:"payload_version"`
	State           string               `json:"state"`
	HeadRevisionID  string               `json:"head_revision_id"`
	AccountIdentity store.DraftIdentity  `json:"account_identity"`
	AccountName     string               `json:"account_name_at_create"`
	CreatedAt       time.Time            `json:"created_at"`
	RevisionAt      time.Time            `json:"revision_at"`
	DiscardedAt     *time.Time           `json:"discarded_at,omitempty"`
	RequestedRaw    string               `json:"requested_raw"`
	RequestedJID    string               `json:"requested_jid"`
	Recipient       store.DraftRecipient `json:"recipient"`
	RecipientName   string               `json:"recipient_name"`
	Kind            store.DraftKind      `json:"kind"`
	Defaults        store.DraftDefaults  `json:"defaults"`
	Text            *store.DraftText     `json:"text,omitempty"`
	Document        *draftDocumentDTO    `json:"document,omitempty"`
	Image           *draftImageDTO       `json:"image,omitempty"`
	Voice           *draftVoiceDTO       `json:"voice,omitempty"`
	Contact         *store.DraftContact  `json:"contact,omitempty"`
	Reply           *store.DraftReply    `json:"reply,omitempty"`
	TruncatedFields []string             `json:"truncated_fields,omitempty"`
	Recovery        string               `json:"recovery,omitempty"`
}

type draftVoiceDTO struct {
	store.DraftVoice
	Basis            string    `json:"basis"`
	PTT              bool      `json:"ptt"`
	SecondsPresent   bool      `json:"seconds_present"`
	WaveformPresent  bool      `json:"waveform_present"`
	VerifiedAtCreate time.Time `json:"verified_at_create"`
	SnapshotPath     string    `json:"snapshot_path,omitempty"`
}

func projectDraft(entry store.DraftEntry, storeRef string, full bool) draftDTO {
	r, revision := entry.Record, entry.Revision
	p := revision.Payload().Data()
	review := revision.Review()
	d := draftDTO{ID: r.ID, RevisionID: revision.ID(), Revision: entry.Number, Hash: revision.Payload().Hash(), PayloadVersion: p.Version, State: r.State, HeadRevisionID: r.HeadRevisionID, AccountIdentity: p.Account, AccountName: review.AccountName, CreatedAt: r.CreatedAt, RevisionAt: revision.CreatedAt(), DiscardedAt: r.DiscardedAt, RequestedRaw: review.RequestedRaw, RequestedJID: review.RequestedJID, Recipient: p.Recipient, RecipientName: review.RecipientName, Kind: p.Kind, Defaults: p.Defaults, Text: p.Text, Contact: p.Contact, Reply: p.Reply}
	cut := func(field, value string) string {
		if full {
			return value
		}
		runes := []rune(value)
		if len(runes) > 512 {
			d.TruncatedFields = append(d.TruncatedFields, field)
			return string(runes[:512])
		}
		return value
	}
	d.RecipientName = cut("recipient_name", d.RecipientName)
	d.AccountName = cut("account_name_at_create", d.AccountName)
	if d.Text != nil {
		d.Text.Text = cut("text.text", d.Text.Text)
	}
	if d.Reply != nil {
		d.Reply.Text = cut("reply.text", d.Reply.Text)
	}
	if d.Contact != nil {
		d.Contact.DisplayName = cut("contact.display_name", d.Contact.DisplayName)
		d.Contact.VCard = cut("contact.vcard", d.Contact.VCard)
	}
	if p.Document != nil {
		d.Document = &draftDocumentDTO{Filename: cut("document.filename", p.Document.Filename), MIME: p.Document.MIME, Caption: cut("document.caption", p.Document.Caption), Size: p.Document.Size, SHA256: p.Document.SHA256, VerifiedAtCreate: review.VerifiedAtCreate}
		if full {
			relative, err := store.DraftSnapshotRelativePath(revision.ID())
			if err == nil {
				d.Document.SnapshotPath = filepath.Join(storeRef, filepath.FromSlash(relative))
			}
		}
		d.Recovery = "Inspect document bytes appropriately. Snapshot metadata describes creation, not current availability/integrity or approval."
	}
	if p.Image != nil {
		value := p.Image
		thumbnailDigest := sha256.Sum256(value.JPEGThumbnail)
		d.Image = &draftImageDTO{MIME: value.MIME, Caption: cut("image.caption", value.Caption), Size: value.Size, SHA256: value.SHA256, Width: value.Width, Height: value.Height, ThumbnailBytes: len(value.JPEGThumbnail), ThumbnailSHA256: hex.EncodeToString(thumbnailDigest[:]), VerifiedAtCreate: review.VerifiedAtCreate}
		if full {
			relative, err := store.DraftSnapshotRelativePath(revision.ID())
			if err == nil {
				d.Image.SnapshotPath = filepath.Join(storeRef, filepath.FromSlash(relative))
			}
		}
		d.Recovery = "Inspect image bytes visually with appropriate local tools. Metadata describes creation, not current availability/integrity or human approval."
	}
	if p.Voice != nil {
		d.Voice = &draftVoiceDTO{DraftVoice: *p.Voice, Basis: "declared_structure", PTT: true, VerifiedAtCreate: review.VerifiedAtCreate}
		if full {
			relative, err := store.DraftSnapshotRelativePath(revision.ID())
			if err == nil {
				d.Voice.SnapshotPath = filepath.Join(storeRef, filepath.FromSlash(relative))
			}
		}
		d.Recovery = "Inspect voice bytes separately with appropriate local tools. Declared structure is not decoded duration, speech quality or human approval; creation metadata does not establish current snapshot availability/integrity."
		if !full {
			d.Recovery = fmt.Sprintf("Use draft show %s --revision %s --agent --detail full; inspect voice bytes separately. Declared structure does not establish decoded duration, speech quality, current availability/integrity or human approval.", r.ID, revision.ID())
		}
	}
	if d.Image != nil && !full {
		d.Recovery = fmt.Sprintf("Use draft show %s --revision %s --agent --detail full; inspect image bytes separately. Metadata describes creation only, not current availability/integrity. No approval is recorded.", r.ID, revision.ID())
	} else if d.Document != nil && !full {
		d.Recovery = fmt.Sprintf("Use draft show %s --revision %s --agent --detail full; inspect document bytes separately. Metadata describes creation only, not current availability/integrity. No approval is recorded.", r.ID, revision.ID())
	} else if len(d.TruncatedFields) > 0 {
		d.Recovery = fmt.Sprintf("Use draft show %s --revision %s --agent --detail full; No approval is recorded.", r.ID, revision.ID())
	}
	return d
}

func classifyDraftError(err error) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) {
		return existing
	}
	var failure *store.DraftError
	if !errors.As(err, &failure) {
		var validation *store.DraftValidationError
		if errors.As(err, &validation) {
			switch validation.Field {
			case "voice.invalid":
				return &out.AgentError{Code: "invalid_arguments", Message: "Voice Ogg/Opus structure is invalid or incomplete.", Recovery: "Inspect the complete local Ogg/Opus bytes; no conversion or fallback is performed.", ExitCode: 2, Cause: err}
			case "voice.unsupported_profile":
				return &out.AgentError{Code: "invalid_arguments", Message: "Voice format is outside the supported Ogg/Opus PTT profile; a broader format may still be valid.", Recovery: "Inspect the documented version 1, mapping 0, single-stream, zero-origin profile before preparing another revision.", ExitCode: 2, Cause: err}
			case "voice.quota":
				return &out.AgentError{Code: "invalid_arguments", Message: "Voice exceeds a local preparation quota; these quotas are not WhatsApp limits.", Recovery: "Inspect the documented byte, packet, page and one-hour encoded timeline quotas.", ExitCode: 2, Cause: err}
			case "voice.canceled":
				return &out.AgentError{Code: "invalid_arguments", Message: "Voice structural validation was canceled before snapshot publication or revision persistence.", Recovery: "Inspect the current local draft state before explicitly preparing another revision.", ExitCode: 2, Cause: err}
			case "reply.sender":
				return &out.AgentError{Code: "invalid_arguments", Message: "Quoted sender identity is unavailable or incompatible with the observed local account or recipient.", Recovery: "Inspect messages show --chat CHAT_JID --id MESSAGE_ID --agent --detail full locally; select a compatible quote or explicitly recreate without --reply-to.", ExitCode: 2, Cause: err}
			case "reply.unsupported":
				return &out.AgentError{Code: "invalid_arguments", Message: "Quoted message content is unsupported for draft replies.", Recovery: "Inspect messages show --chat CHAT_JID --id MESSAGE_ID --agent --detail full locally; select a supported text quote or explicitly prepare complete create/update input without --reply-to.", ExitCode: 2, Cause: err}
			}
			code := "invalid_arguments"
			if validation.Field == "cursor" {
				code = "invalid_cursor"
			}
			return &out.AgentError{Code: code, Message: validation.Error(), Recovery: "Use explicit JIDs/phones and complete bounded input; discover recipients with contacts search/resolve or chats list.", ExitCode: 2, Cause: err}
		}
		code := "store_unavailable"
		if lock.IsLocked(err) {
			code = "store_locked"
		}
		var snapshot *app.DraftSnapshotError
		if errors.As(err, &snapshot) {
			code = "document_unavailable"
		}
		message := "Selected local draft archive, identity or document is unavailable."
		recovery := "Inspect the selected local store and media roots without connecting; retained snapshots may remain after preparation failures."
		if code == "store_locked" {
			message = "Selected local store is locked by a writer."
			recovery = "Wait for the current writer to finish; inspect drafts and the intended result before explicitly requesting the action again."
		}
		return &out.AgentError{Code: code, Message: message, Recovery: recovery, ExitCode: 4, Cause: err}
	}
	exit := 1
	message := "Local draft write has an uncertain result; do not replay automatically."
	recovery := "Inspect draft show ID --revision REV in the selected store; a hash is not permission to send."
	switch failure.Code {
	case "not_found":
		exit = 3
		message = "Requested draft, revision or quoted text was not found locally."
	case "store_unavailable":
		exit = 4
		message = "Selected local draft archive is unreadable or contains incompatible/corrupt records; retained snapshots may remain."
		recovery = "Inspect doctor --agent against the selected local store without --connect."
	case "identity_unavailable":
		exit = 4
		message = "An unambiguous local own PN identity in this store is required for create/update; no session is created or connected."
		recovery = "Inspect auth status --agent in the selected store; restore the intended session explicitly outside draft preparation."
	case "invalid_arguments":
		exit = 2
		message = "Invalid complete local draft input."
		recovery = "Check complete input; for --reply-to, inspect messages show --chat CHAT_JID --id MESSAGE_ID --agent --detail full locally. Use a compatible quote or explicitly recreate without --reply-to."
	case "image_unavailable":
		exit = 4
		message = "Image preparation failed locally; published or uncertain artifacts are retained."
	case "document_unavailable":
		exit = 4
		message = "Document preparation failed locally; published or uncertain artifacts are retained."
	case "read_only":
		exit = 2
		message = "Read-only policy rejects local draft writes."
	case "local_write_not_dispatched":
		message = "Local draft request expired in the owner queue; no draft write was started."
		recovery = "Inspect current draft state before explicitly requesting another local write."
	case "draft_conflict":
		message = "Draft revision/state changed; inspect the current draft before editing or discarding."
		recovery = "Use draft show ID --agent to obtain the current revision before supplying a new --if-revision."
	}
	correlation := &out.AgentDraftError{DraftID: failure.DraftID, RevisionID: failure.RevisionID, Hash: failure.Hash}
	return &out.AgentError{Code: failure.Code, Message: message, Recovery: recovery, ExitCode: exit, Cause: err, Draft: correlation}
}

// Capture failures that existing output helpers intentionally treat as a closed
// read pipe. After a draft write, even EPIPE must expose an uncertain result.
type draftOutputWriter struct {
	writer  io.Writer
	failure error
}

func (w *draftOutputWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.failure = err
	}
	return n, err
}

func writeDraftEntry(flags *rootFlags, entry store.DraftEntry, wrote bool) error {
	return writeDraftEntryTo(os.Stdout, flags, entry, wrote)
}
func writeDraftEntryTo(writer io.Writer, flags *rootFlags, entry store.DraftEntry, wrote bool) error {
	d := projectDraft(entry, *flags.agentAccount.StoreRef, flags.agent && flags.detail == "full" || !flags.agent && flags.fullOutput)
	w := &draftOutputWriter{writer: writer}
	var err error
	if flags.agent {
		meta := agentMeta(flags)
		if d.Recovery != "" {
			meta.Recovery = "See data.recovery."
		}
		err = out.WriteAgentJSON(w, flags.agentAccount, meta, d)
	} else if flags.asJSON {
		err = out.WriteJSON(w, struct {
			Account out.AgentAccount `json:"account"`
			Draft   draftDTO         `json:"draft"`
		}{flags.agentAccount, d})
	} else {
		raw, encodeErr := json.MarshalIndent(d, "", "  ")
		err = encodeErr
		if err == nil {
			_, err = fmt.Fprintln(w, string(raw))
		}
	}
	if wrote && w.failure != nil {
		err = w.failure
	}
	if err != nil && wrote {
		return classifyDraftError(store.DraftFailure("local_write_uncertain", entry.Record.ID, entry.Revision.ID(), entry.Revision.Payload().Hash(), err))
	}
	return err
}

type draftListDTO struct {
	ID         string             `json:"id"`
	State      string             `json:"state"`
	RevisionID string             `json:"revision_id"`
	Hash       string             `json:"hash"`
	Revision   int                `json:"revision"`
	CreatedAt  time.Time          `json:"created_at"`
	Summary    store.DraftSummary `json:"summary"`
}

func writeDraftList(flags *rootFlags, page store.DraftPage) error {
	items := make([]draftListDTO, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, draftListDTO{item.Record.ID, item.Record.State, item.RevisionID, item.Hash, item.Number, item.Record.CreatedAt, item.Summary})
	}
	if flags.agent {
		meta := agentMeta(flags)
		meta.Page = &out.AgentPage{Returned: len(items), HasMore: page.HasMore, NextCursor: page.NextCursor}
		meta.Recovery = "List contains stored summaries only; retrieve draft show ID --revision REV to inspect a revision."
		return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, items)
	}
	return out.WriteJSON(os.Stdout, struct {
		Account    out.AgentAccount `json:"account"`
		Drafts     []draftListDTO   `json:"drafts"`
		HasMore    bool             `json:"has_more"`
		NextCursor *string          `json:"next_cursor"`
	}{flags.agentAccount, items, page.HasMore, page.NextCursor})
}
