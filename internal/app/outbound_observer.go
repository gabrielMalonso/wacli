package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const outboundEventIDs = 200
const outboundEventBudget = 2 * time.Second

// One account snapshot belongs to one client lifetime. Admissions and WaitGroup
// additions share the closing gate; no lock is held during SQL or handler removal.
type outboundObserver struct {
	mu         sync.Mutex
	closed     bool
	conflicted bool
	account    store.DraftIdentity
	workers    sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	app        *App
	client     WAClient
	archive    store.OutboundArchive
}

func newOutboundObserver(a *App, client WAClient) *outboundObserver {
	ctx, cancel := context.WithCancel(context.Background())
	return &outboundObserver{ctx: ctx, cancel: cancel, app: a, client: client, archive: a.db.Outbound(), account: outboundPublicAccount(client.LinkedJID(), client.LinkedLID())}
}

func outboundPublicAccount(pn, lid string) store.DraftIdentity {
	if store.ValidateOutboundAccount(pn) != nil {
		return store.DraftIdentity{}
	}
	if lid != "" {
		jid, err := types.ParseJID(lid)
		if err != nil || !outboundUser(jid) || jid.Server != types.HiddenUserServer || jid.ToNonAD().String() != lid {
			return store.DraftIdentity{}
		}
	}
	return store.DraftIdentity{PN: pn, LID: lid}
}

func (o *outboundObserver) enter(parent context.Context) (context.Context, func(), bool) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, nil, false
	}
	o.workers.Add(1)
	o.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(o.ctx, cancel)
	return ctx, func() { stop(); cancel(); o.workers.Done() }, true
}

func (o *outboundObserver) closeAdmissions() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.cancel()
}

func (o *outboundObserver) event(state *sessionObservation, evt any) {
	ctx, done, ok := o.enter(context.Background())
	if !ok {
		return
	}
	defer done()
	var batch *outboundEvidenceBatch
	switch evt.(type) {
	case *events.Receipt, *events.Message:
		batch = o.batch(ctx)
		defer batch.finish()
	}
	o.app.observeSessionState(state, evt)
	if v, ok := evt.(*events.PairSuccess); ok && v != nil {
		account := outboundPublicAccount(v.ID.ToNonAD().String(), v.LID.ToNonAD().String())
		o.mu.Lock()
		if !o.conflicted && o.account.PN == "" {
			o.account = account
		} else if account != o.account {
			o.conflicted = true
		}
		o.mu.Unlock()
		return
	}
	switch v := evt.(type) {
	case *events.Receipt:
		batch.receipt(v)
		o.app.recordReceiptChange(ctx, v)
	case *events.Message:
		batch.liveEcho(v)
	}
}

type outboundEvidenceBatch struct {
	observer           *outboundObserver
	account            store.DraftIdentity
	ctx                context.Context
	cancel             context.CancelFunc
	observedAt         time.Time
	examined, retained int
	skipped, conflicts int
	failures           int
	source             store.OutboundSource
}

func (o *outboundObserver) batch(parent context.Context) *outboundEvidenceBatch {
	// Capture observation time and identity once, before lookups/persistence.
	at := nowUTC()
	o.mu.Lock()
	account := o.account
	if o.conflicted {
		account = store.DraftIdentity{}
	}
	o.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, outboundEventBudget)
	return &outboundEvidenceBatch{observer: o, account: account, ctx: ctx, cancel: cancel, observedAt: at}
}

func (b *outboundEvidenceBatch) finish() {
	b.cancel()
	if b.skipped+b.conflicts+b.failures == 0 {
		return
	}
	b.observer.app.emitWarning("outbound_evidence_incomplete", "warning: some outbound event evidence could not be retained; absence remains unknown, do not resend to resolve it", map[string]any{
		"source": b.source, "examined": b.examined, "retained": b.retained, "skipped": b.skipped, "scope_conflicts": b.conflicts, "persistence_failures": b.failures,
	})
}

func (b *outboundEvidenceBatch) lookup(id string) (store.OutboundOperation, bool) {
	if b.account.PN == "" || b.examined >= outboundEventIDs || b.ctx.Err() != nil {
		b.skipped++
		return store.OutboundOperation{}, false
	}
	b.examined++
	if store.ValidateOutboundMessageID(id) != nil {
		b.conflicts++
		return store.OutboundOperation{}, false
	}
	o, err := b.observer.archive.ByMessage(b.ctx, b.account.PN, id)
	if err != nil {
		var failure *store.OutboundError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			b.failures++
		}
		return o, false
	}
	if o.Account != b.account {
		b.conflicts++
		return o, false
	}
	return o, o.DispatchPossibleAt != nil
}

func outboundUser(jid types.JID) bool {
	normalized, err := store.NormalizeDraftTarget(jid.ToNonAD().String())
	return err == nil && normalized == jid.ToNonAD().String() && len(normalized) <= 128 && jid.User != "" && jid.Integrator == 0 && (jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer)
}

func outboundOwn(account store.DraftIdentity, jid types.JID) bool {
	s := jid.ToNonAD().String()
	return outboundUser(jid) && (s == account.PN || account.LID != "" && s == account.LID)
}

func outboundPeer(o store.OutboundOperation, jid types.JID) bool {
	s := jid.ToNonAD().String()
	return outboundUser(jid) && (s == o.Recipient.JID || o.Recipient.PN != "" && s == o.Recipient.PN || o.Recipient.LID != "" && s == o.Recipient.LID)
}

func outboundChat(o store.OutboundOperation, chat types.JID) bool {
	if chat.Server == types.GroupServer {
		return chat.Device == 0 && chat.RawAgent == 0 && chat.Integrator == 0 && chat.String() == o.Recipient.JID
	}
	return outboundPeer(o, chat)
}

func (b *outboundEvidenceBatch) observe(o store.OutboundOperation, f store.OutboundObservation) {
	f.ObservedAt = b.observedAt
	if err := store.ValidateOutboundObservation(o, f); err != nil {
		b.conflicts++
		return
	}
	if _, err := b.observer.archive.Observe(b.ctx, o.ID, o.Account, o.MessageID, f); err != nil {
		b.failures++
	} else {
		b.retained++
	}
}

func outboundEventTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

func (b *outboundEvidenceBatch) receipt(v *events.Receipt) {
	b.source = store.OutboundLiveReceipt
	if v == nil {
		return
	}
	var fact store.OutboundFact
	switch v.Type {
	case types.ReceiptTypeDelivered:
		fact = store.OutboundDelivered
	case types.ReceiptTypeRead:
		fact = store.OutboundRead
	case types.ReceiptTypeServerError:
		fact = store.OutboundServerError
	default:
		return
	}
	group := v.Chat.Server == types.GroupServer
	// Grouped receipts inherit IsFromMe/SenderAlt from a partial source. Always
	// validate the actual participant, even if the inherited flag says otherwise.
	if !outboundUser(v.Sender) || outboundOwn(b.account, v.Sender) || !group && v.IsFromMe || v.IsGroup != group {
		return
	}
	alias := v.SenderAlt
	aliasChecked := false
	seen := make(map[string]struct{}, min(len(v.MessageIDs), outboundEventIDs))
	for i, id := range v.MessageIDs {
		if i >= outboundEventIDs {
			b.skipped += len(v.MessageIDs) - i
			break
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		o, ok := b.lookup(id)
		if !ok {
			continue
		}
		if !outboundChat(o, v.Chat) || group && v.Sender.Server == types.HiddenUserServer && b.account.LID == "" || !v.MessageSender.IsEmpty() && !outboundOwn(b.account, v.MessageSender) || !group && (!outboundPeer(o, v.Sender) || !alias.IsEmpty() && !outboundPeer(o, alias) || !v.RecipientAlt.IsEmpty()) {
			b.conflicts++
			continue
		}
		if group && !alias.IsEmpty() && !aliasChecked {
			aliasChecked = true
			if !outboundUser(alias) || alias.Server == v.Sender.Server || alias.Device != 0 && alias.Device != v.Sender.Device {
				b.conflicts++
				return
			}
			pair, err := b.observer.client.CheckPublicPair(b.ctx, v.Sender, alias)
			if err != nil {
				b.failures++
				return
			}
			if pair == wa.PublicPairContradictory {
				b.conflicts++
				return
			}
			if pair != wa.PublicPairVerified {
				// Uncorroborated inherited aliases never merge participants.
				alias = types.JID{}
			}
		}
		f := store.OutboundObservation{Fact: fact, Source: store.OutboundLiveReceipt, ChatJID: v.Chat.ToNonAD().String(), ActorJID: v.Sender.ToNonAD().String(), Device: v.Sender.Device, EventAt: outboundEventTime(v.Timestamp)}
		if !alias.IsEmpty() {
			f.ActorAlias = alias.ToNonAD().String()
		}
		if fact == store.OutboundServerError {
			f.ErrorCode = "server_error"
		}
		b.observe(o, f)
	}
}

// Only original supported content can be an echo. Mutation parsers may replace
// an edit/reaction's ID with its target's ID, which is not the original content.
func outboundEchoKind(msg *waE2E.Message) store.DraftKind {
	if msg == nil {
		return ""
	}
	v := (&events.Message{RawMessage: msg}).UnwrapRaw()
	m := v.Message
	if v.IsEdit || m.GetProtocolMessage() != nil || m.GetReactionMessage() != nil || m.GetEncReactionMessage() != nil || m.GetSecretEncryptedMessage() != nil {
		return ""
	}
	switch {
	case m.GetConversation() != "" || m.GetExtendedTextMessage().GetText() != "":
		return store.DraftTextKind
	case m.GetContactMessage() != nil:
		return store.DraftContactKind
	case m.GetImageMessage() != nil:
		return store.DraftImageKind
	case m.GetAudioMessage().GetPTT():
		return store.DraftVoiceKind
	case m.GetDocumentMessage() != nil:
		return store.DraftDocumentKind
	default:
		return ""
	}
}

func (b *outboundEvidenceBatch) liveEcho(v *events.Message) {
	b.source = store.OutboundLiveEcho
	if v == nil || !v.Info.IsFromMe || v.IsEdit || v.Info.Edit != types.EditAttributeEmpty {
		return
	}
	if v.SourceWebMsg != nil {
		b.historyEcho(v.Info.Chat.String(), v.SourceWebMsg)
		return
	}
	raw := v.RawMessage
	if raw == nil {
		raw = v.Message
	}
	kind := outboundEchoKind(raw)
	if kind == "" {
		return
	}
	o, ok := b.lookup(v.Info.ID)
	if !ok {
		return
	}
	if kind != o.Kind || !outboundChat(o, v.Info.Chat) || !outboundOwn(b.account, v.Info.Sender) || !v.Info.SenderAlt.IsEmpty() && !outboundOwn(b.account, v.Info.SenderAlt) || !v.Info.RecipientAlt.IsEmpty() && !outboundPeer(o, v.Info.RecipientAlt) {
		b.conflicts++
		return
	}
	if meta := v.Info.DeviceSentMeta; meta != nil && meta.DestinationJID != "" {
		destination, err := types.ParseJID(meta.DestinationJID)
		if err != nil || !outboundChat(o, destination) {
			b.conflicts++
			return
		}
	}
	f := store.OutboundObservation{Fact: store.OutboundOwnEcho, Source: store.OutboundLiveEcho, ChatJID: v.Info.Chat.ToNonAD().String(), ActorJID: v.Info.Sender.ToNonAD().String(), Device: v.Info.Sender.Device, EventAt: outboundEventTime(v.Info.Timestamp)}
	if !v.Info.SenderAlt.IsEmpty() {
		f.ActorAlias = v.Info.SenderAlt.ToNonAD().String()
	}
	b.observe(o, f)
}

func (b *outboundEvidenceBatch) historyEcho(chatID string, v *waWeb.WebMessageInfo) {
	b.source = store.OutboundHistoryEcho
	if v == nil || v.GetKey() == nil || v.GetKey().FromMe == nil || !v.GetKey().GetFromMe() {
		return
	}
	kind := outboundEchoKind(v.GetMessage())
	if kind == "" {
		return
	}
	o, ok := b.lookup(v.GetKey().GetID())
	if !ok {
		return
	}
	chat, err := types.ParseJID(chatID)
	if err != nil || !outboundChat(o, chat) || kind != o.Kind {
		b.conflicts++
		return
	}
	if remote := v.GetKey().GetRemoteJID(); remote != "" {
		remoteJID, err := types.ParseJID(remote)
		if err != nil || remoteJID.ToNonAD() != chat.ToNonAD() {
			b.conflicts++
			return
		}
	}
	actor, _ := types.ParseJID(b.account.PN)
	if author := v.GetOriginalSelfAuthorUserJIDString(); author != "" {
		actor, err = types.ParseJID(author)
		if err != nil || !outboundOwn(b.account, actor) {
			b.conflicts++
			return
		}
	}
	var at *time.Time
	if v.MessageTimestamp != nil {
		at = outboundEventTime(time.Unix(int64(v.GetMessageTimestamp()), 0))
	}
	b.observe(o, store.OutboundObservation{Fact: store.OutboundOwnEcho, Source: store.OutboundHistoryEcho, ChatJID: chat.ToNonAD().String(), ActorJID: actor.ToNonAD().String(), Device: actor.Device, EventAt: at})
}

func (a *App) outboundHistoryBatch(ctx context.Context) (*outboundEvidenceBatch, func(), bool) {
	a.waMu.Lock()
	o := a.outboundEvents
	a.waMu.Unlock()
	if o == nil {
		return nil, func() {}, true
	}
	ctx, done, ok := o.enter(ctx)
	if !ok {
		return nil, nil, false
	}
	b := o.batch(ctx)
	return b, func() { b.finish(); done() }, true
}
