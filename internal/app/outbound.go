package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

// LookupOutboundSend resolves an existing binding without session, current
// identity, active/head/media checks. Not-found alone permits new preparation.
func LookupOutboundSend(ctx context.Context, repository store.OutboundArchive, r OutboundSendRequest) (OutboundSendResult, bool, error) {
	if err := r.Validate(); err != nil {
		return OutboundSendResult{}, false, outboundFailure(r, "invalid_arguments", err)
	}
	e, err := repository.Read(ctx, "", r.Key, r.OwnPN, 1, "")
	if err != nil {
		var typed *store.OutboundError
		if errors.As(err, &typed) && typed.Code == "not_found" {
			return OutboundSendResult{}, false, nil
		}
		return OutboundSendResult{}, false, outboundFailure(r, "store_error", err)
	}
	o := e.Operation
	if o.Version != r.Version || o.Account.PN != r.OwnPN || o.DraftID != r.DraftID || o.RevisionID != r.RevisionID || o.Hash != r.Hash {
		return OutboundSendResult{}, false, outboundFailure(r, "idempotency_conflict", nil)
	}
	return OutboundSendResult{Entry: e, Duplicate: true, Persistence: "confirmed", KnownResult: o.Result}, true, nil
}

type outboundRunner struct {
	repository  store.OutboundArchive
	readDraft   func(context.Context, string, string) (store.DraftEntry, error)
	identities  func(context.Context, []string) ([]HistoryIdentity, error)
	open        func() (wa.OutboundClient, error)
	connect     func(context.Context) error
	document    func(context.Context, store.DraftRevision) ([]byte, error)
	history     func(store.DraftPayloadData, store.OutboundOperation, whatsmeow.SendResponse, *whatsmeow.UploadResponse) error
	finalBudget time.Duration
}

// SendOutbound owns synchronous SDK calls until return and bounded independent
// finalization. The caller retains the existing LOCK/IPC slot throughout.
func (a *App) SendOutbound(ctx context.Context, r OutboundSendRequest, beforeNew func(context.Context) error) (OutboundSendResult, error) {
	if a.ReadOnly() {
		return OutboundSendResult{}, outboundFailure(r, "read_only", nil)
	}
	if r.StoreRef != a.StoreDir() {
		return OutboundSendResult{}, outboundFailure(r, "scope_conflict", nil)
	}
	runner := outboundRunner{repository: a.DB().Outbound(), readDraft: a.DB().ReadDraft, identities: a.ReadDraftIdentities, finalBudget: 2 * time.Second,
		open: func() (wa.OutboundClient, error) {
			if err := a.OpenWA(); err != nil {
				return nil, err
			}
			client, ok := a.WA().(wa.OutboundClient)
			if !ok {
				return nil, fmt.Errorf("outbound adapter unavailable")
			}
			return client, nil
		},
		connect: func(ctx context.Context) error { return a.Connect(ctx, false, nil) },
		document: func(ctx context.Context, revision store.DraftRevision) ([]byte, error) {
			return readOutboundDocument(ctx, a.StoreDir(), revision)
		},
		history: a.persistOutboundHistory,
	}
	return runner.send(ctx, r, beforeNew)
}

func (x outboundRunner) send(ctx context.Context, r OutboundSendRequest, beforeNew func(context.Context) error) (OutboundSendResult, error) {
	if old, found, err := LookupOutboundSend(ctx, x.repository, r); found || err != nil {
		return old, err
	}
	entry, err := x.readDraft(ctx, r.DraftID, r.RevisionID)
	if err != nil {
		code := "store_error"
		var missing *store.DraftError
		if errors.As(err, &missing) && missing.Code == "not_found" {
			code = "not_found"
		}
		return OutboundSendResult{}, outboundFailure(r, code, err)
	}
	p := entry.Revision.Payload().Data()
	if p.Account.PN != r.OwnPN {
		return OutboundSendResult{}, outboundFailure(r, "scope_conflict", nil)
	}
	if entry.Revision.Payload().Hash() != r.Hash {
		return OutboundSendResult{}, outboundFailure(r, "hash_conflict", nil)
	}
	if entry.Record.State != "active" {
		return OutboundSendResult{}, outboundFailure(r, "draft_conflict", nil)
	}
	revalidate := func() error {
		identities, err := x.identities(ctx, outboundIdentityInputs(p))
		if err != nil {
			return err
		}
		return validateOutboundIdentities(p, identities)
	}
	if err = revalidate(); err != nil {
		return OutboundSendResult{}, outboundFailure(r, "revision_required", err)
	}
	to, _, err := prepareOutboundPayload(p, nil)
	if err != nil {
		return OutboundSendResult{}, outboundFailure(r, "revision_required", err)
	}
	if err = ctx.Err(); err != nil {
		return OutboundSendResult{}, outboundFailure(r, "not_dispatched", err)
	}
	client, err := x.open()
	if err != nil {
		return OutboundSendResult{}, outboundFailure(r, "identity_unavailable", err)
	}
	if client.LinkedJID() != p.Account.PN || client.LinkedLID() != p.Account.LID {
		return OutboundSendResult{}, outboundFailure(r, "revision_required", nil)
	}
	messageID, err := client.GenerateOutboundMessageID()
	if err != nil || store.ValidateOutboundMessageID(messageID) != nil {
		return OutboundSendResult{}, outboundFailure(r, "preparation_failed", err)
	}
	id, err := store.NewDraftID()
	if err != nil {
		return OutboundSendResult{}, outboundFailure(r, "preparation_failed", err)
	}
	o, err := x.repository.Reserve(ctx, store.OutboundReservation{Version: r.Version, ID: id, DraftID: r.DraftID, RevisionID: r.RevisionID, Hash: r.Hash, Key: r.Key, MessageID: messageID, Account: p.Account, CreatedAt: time.Now().UTC()})
	if err != nil {
		code := "persistence_unconfirmed"
		var typed *store.OutboundError
		if errors.As(err, &typed) && (typed.Code == "idempotency_conflict" || typed.Code == "draft_conflict" || typed.Code == "hash_conflict" || typed.Code == "identity_unavailable") {
			code = typed.Code
		}
		failure := outboundFailure(r, code, err)
		failure.OperationID, failure.MessageID = id, messageID
		return OutboundSendResult{}, failure
	}
	if o.ID != id {
		return x.retained(ctx, r, o, true)
	}
	finish := func(result store.OutboundResult, code string, ack *store.OutboundObservation, cause error) (OutboundSendResult, error) {
		return x.finalize(r, o, result, code, ack, cause)
	}
	step := func(phase store.OutboundPhase) error {
		next, err := x.repository.Checkpoint(ctx, store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: store.OutboundPending, At: outboundNow(o)})
		if err == nil {
			o = next
		}
		return err
	}
	if beforeNew != nil {
		if err = beforeNew(ctx); err != nil {
			return finish(store.OutboundNotDispatched, "deadline", nil, err)
		}
	}
	if err = step(store.OutboundPreparing); err != nil {
		return finish(store.OutboundNotDispatched, "store_error", nil, err)
	}
	var buffer []byte
	if p.Kind.HasUpload() {
		buffer, err = x.document(ctx, entry.Revision)
		if err != nil {
			return finish(store.OutboundNotDispatched, "preparation_failed", nil, err)
		}
	}
	if err = x.connect(ctx); err != nil {
		return finish(store.OutboundNotDispatched, outboundContextCode(err, "transport_error"), nil, err)
	}
	if err = revalidate(); err != nil {
		return finish(store.OutboundNotDispatched, "identity_changed", nil, err)
	}
	var uploaded *whatsmeow.UploadResponse
	if p.Kind.HasUpload() {
		if err = step(store.OutboundUploadPossible); err != nil {
			return finish(store.OutboundNotDispatched, "store_error", nil, err)
		}
		if err = ctx.Err(); err != nil {
			return finish(store.OutboundNotDispatched, outboundContextCode(err, "upload_error"), nil, err)
		}
		digest := sha256.Sum256(buffer)
		length := uint64(len(buffer))
		uploadType := whatsmeow.MediaDocument
		if p.Kind == store.DraftImageKind {
			uploadType = whatsmeow.MediaImage
		} else if p.Kind == store.DraftVoiceKind {
			uploadType = whatsmeow.MediaAudio
		}
		response, uploadErr := client.Upload(ctx, buffer, uploadType)
		if uploadErr != nil {
			return finish(store.OutboundNotDispatched, outboundContextCode(uploadErr, "upload_error"), nil, uploadErr)
		}
		if response.FileLength != length || !bytes.Equal(response.FileSHA256, digest[:]) || len(response.MediaKey) != 32 || len(response.FileEncSHA256) != 32 || response.URL == "" || response.DirectPath == "" {
			return finish(store.OutboundNotDispatched, "upload_error", nil, fmt.Errorf("inconsistent upload response"))
		}
		uploaded = &response
		if err = step(store.OutboundUploadReturned); err != nil {
			return finish(store.OutboundNotDispatched, "store_error", nil, err)
		}
		if err = revalidate(); err != nil {
			return finish(store.OutboundNotDispatched, "identity_changed", nil, err)
		}
	}
	_, msg, err := prepareOutboundPayload(p, uploaded)
	if err != nil {
		return finish(store.OutboundNotDispatched, "preparation_failed", nil, err)
	}
	if client.LinkedJID() != p.Account.PN || client.LinkedLID() != p.Account.LID {
		return finish(store.OutboundNotDispatched, "identity_changed", nil, nil)
	}
	if err = step(store.OutboundDispatchPossible); err != nil {
		return finish(store.OutboundNotDispatched, "store_error", nil, err)
	}
	if err = ctx.Err(); err != nil {
		return finish(store.OutboundUncertain, outboundContextCode(err, "transport_error"), nil, err)
	}
	response, sendErr := client.SendOutbound(ctx, to, o.MessageID, msg)
	coherent := response.ID == o.MessageID && response.Chat.ToNonAD() == to && (response.Sender.ToNonAD().String() == p.Account.PN || p.Account.LID != "" && response.Sender.ToNonAD().String() == p.Account.LID)
	if sendErr != nil {
		if coherent && errors.Is(sendErr, whatsmeow.ErrServerReturnedError) {
			return finish(store.OutboundRejected, "server_error", nil, sendErr)
		}
		return finish(store.OutboundUncertain, outboundContextCode(sendErr, "transport_error"), nil, sendErr)
	}
	if !coherent || response.Timestamp.IsZero() || response.Timestamp.UnixNano() <= 0 {
		return finish(store.OutboundUncertain, "transport_error", nil, fmt.Errorf("inconsistent SDK acknowledgement"))
	}
	ack := &store.OutboundObservation{Fact: store.OutboundAck, Source: store.OutboundSendResponse, ChatJID: o.Recipient.JID, ActorJID: response.Sender.ToNonAD().String(), EventAt: &response.Timestamp, ObservedAt: outboundNow(o)}
	result, finalErr := finish(store.OutboundAccepted, "", ack, nil)
	if x.history != nil && x.history(p, o, response, uploaded) != nil {
		result.HistoryWarning = true
	}
	if finalErr != nil {
		var failure *OutboundSendError
		if errors.As(finalErr, &failure) {
			failure.Result = &result
		}
	}
	return result, finalErr
}

func outboundNow(o store.OutboundOperation) time.Time { return maxTime(time.Now().UTC(), o.UpdatedAt) }
func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
func outboundContextCode(err error, fallback string) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return fallback
}

func (x outboundRunner) retained(ctx context.Context, r OutboundSendRequest, o store.OutboundOperation, duplicate bool) (OutboundSendResult, error) {
	e, err := x.repository.Read(ctx, o.ID, "", "", 1, "")
	if err != nil {
		return OutboundSendResult{}, outboundFailure(r, "persistence_unconfirmed", err)
	}
	return OutboundSendResult{Entry: e, Duplicate: duplicate, Persistence: "confirmed", KnownResult: o.Result}, nil
}

func (x outboundRunner) finalize(r OutboundSendRequest, o store.OutboundOperation, result store.OutboundResult, code string, ack *store.OutboundObservation, cause error) (OutboundSendResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), x.finalBudget)
	defer cancel()
	known := OutboundSendResult{Entry: store.OutboundEntry{Operation: o, Evidence: store.OutboundEvidence{Accepted: "unknown", Delivered: "unknown", Read: "unknown", Scope: "recipient"}}, Persistence: "unconfirmed", KnownResult: result, KnownACK: ack}
	if strings.HasSuffix(o.Recipient.JID, "@g.us") {
		delivered, read := 0, 0
		known.Entry.Evidence.Scope = "participants"
		known.Entry.Evidence.DeliveredParticipants = &delivered
		known.Entry.Evidence.ReadParticipants = &read
	}
	for range 8 {
		e, err := x.repository.Read(ctx, o.ID, "", "", 1, "")
		if err != nil {
			cause = errors.Join(cause, err)
			break
		}
		o = e.Operation
		known.Entry = e
		if result == store.OutboundNotDispatched && o.DispatchPossibleAt != nil {
			result = store.OutboundUncertain
			known.KnownResult = result
		}
		if o.Phase == store.OutboundFinalized {
			if o.Result != result {
				break
			}
			known.Persistence = "confirmed"
			break
		}
		at := outboundNow(o)
		if ack != nil {
			at = maxTime(at, ack.ObservedAt)
		}
		_, err = x.repository.Checkpoint(ctx, store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: store.OutboundFinalized, Result: result, At: at, ErrorCode: code, Ack: ack})
		if err == nil {
			continue
		} // reread the committed result and facts
		// A lost commit response can still have retained the ACK. Re-read before
		// another CAS; retries here affect persistence only, never the SDK call.
		cause = errors.Join(cause, err)
	}
	if known.Persistence != "confirmed" {
		failure := outboundFailure(r, "persistence_unconfirmed", cause)
		failure.Result = &known
		return known, failure
	}
	if result != store.OutboundAccepted {
		failure := outboundFailure(r, string(result), cause)
		failure.Result = &known
		return known, failure
	}
	return known, nil
}

func (a *App) persistOutboundHistory(p store.DraftPayloadData, o store.OutboundOperation, response whatsmeow.SendResponse, uploaded *whatsmeow.UploadResponse) error {
	u := store.UpsertMessageParams{ChatJID: o.Recipient.JID, MsgID: o.MessageID, SenderJID: response.Sender.ToNonAD().String(), SenderName: "me", Timestamp: response.Timestamp, FromMe: true}
	if p.Text != nil {
		u.Text = p.Text.Text
	}
	if p.Contact != nil {
		u.Text = wa.ContactDisplayText(&waE2E.ContactMessage{DisplayName: &p.Contact.DisplayName, Vcard: &p.Contact.VCard})
	}
	if p.Document != nil {
		u.Text = p.Document.Caption
		u.MediaType = "document"
		u.MediaCaption = p.Document.Caption
		u.Filename = p.Document.Filename
		u.MimeType = p.Document.MIME
		u.FileLength = uint64(p.Document.Size)
		if uploaded != nil {
			u.DirectPath = uploaded.DirectPath
			u.MediaKey = uploaded.MediaKey
			u.FileSHA256 = uploaded.FileSHA256
			u.FileEncSHA256 = uploaded.FileEncSHA256
		}
	}
	if p.Image != nil {
		u.Text, u.MediaCaption = p.Image.Caption, p.Image.Caption
		u.MediaType, u.MimeType, u.FileLength = "image", p.Image.MIME, uint64(p.Image.Size)
		if uploaded != nil {
			u.DirectPath, u.MediaKey = uploaded.DirectPath, uploaded.MediaKey
			u.FileSHA256, u.FileEncSHA256 = uploaded.FileSHA256, uploaded.FileEncSHA256
		}
	}
	if p.Voice != nil {
		u.MediaType, u.MimeType, u.FileLength = "audio", p.Voice.MIME, uint64(p.Voice.Size)
		if uploaded != nil {
			u.DirectPath, u.MediaKey = uploaded.DirectPath, uploaded.MediaKey
			u.FileSHA256, u.FileEncSHA256 = uploaded.FileSHA256, uploaded.FileEncSHA256
		}
	}
	if p.Reply != nil {
		u.QuotedMsgID = p.Reply.ID
		u.QuotedSenderJID = p.Reply.Sender.JID
	}
	kind := "dm"
	if strings.HasSuffix(o.Recipient.JID, "@g.us") {
		kind = "group"
	}
	chatErr := a.DB().UpsertChat(o.Recipient.JID, kind, "", response.Timestamp)
	return errors.Join(chatErr, a.DB().UpsertMessage(u))
}
