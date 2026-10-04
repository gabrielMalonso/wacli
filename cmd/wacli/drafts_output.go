package main

import (
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
	Contact         *store.DraftContact  `json:"contact,omitempty"`
	Reply           *store.DraftReply    `json:"reply,omitempty"`
	TruncatedFields []string             `json:"truncated_fields,omitempty"`
	Recovery        string               `json:"recovery,omitempty"`
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
	if len(d.TruncatedFields) > 0 || d.Document != nil && !full {
		d.Recovery = fmt.Sprintf("Use draft show %s --revision %s --agent --detail full; document bytes require separate inspection. No approval is recorded.", r.ID, revision.ID())
	}
	return d
}

func classifyDraftError(err error) *out.AgentError {
	var existing *out.AgentError
	if errors.As(err, &existing) {
		return existing
	}
	var validation *store.DraftValidationError
	if errors.As(err, &validation) {
		code := "invalid_arguments"
		if validation.Field == "cursor" {
			code = "invalid_cursor"
		}
		return &out.AgentError{Code: code, Message: validation.Error(), Recovery: "Use explicit JIDs/phones and complete bounded input; discover recipients with contacts search/resolve or chats list.", ExitCode: 2, Cause: err}
	}
	var failure *store.DraftError
	if !errors.As(err, &failure) {
		code := "store_unavailable"
		if lock.IsLocked(err) {
			code = "store_locked"
		}
		var snapshot *app.DraftSnapshotError
		if errors.As(err, &snapshot) {
			code = "document_unavailable"
		}
		return &out.AgentError{Code: code, Message: "Selected local draft archive, identity or document is unavailable.", Recovery: "Inspect the selected local store and media roots without connecting; retained snapshots may remain after preparation failures.", ExitCode: 4, Cause: err}
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
		message = "Selected local draft archive is unavailable; retained snapshots may remain."
		recovery = "Inspect doctor --agent against the selected local store without --connect."
	case "identity_unavailable":
		exit = 4
		message = "An unambiguous local own PN identity in this store is required for create/update; no session is created or connected."
		recovery = "Inspect auth status --agent in the selected store; restore the intended session explicitly outside draft preparation."
	case "invalid_arguments":
		exit = 2
		message = "Invalid complete local draft input."
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
		meta.Recovery = d.Recovery
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
