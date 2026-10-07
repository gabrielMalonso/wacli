package app

import (
	"context"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The client-lifetime observer owns admission/drain. Persist only public receipt
// identities as received: PN/LID aliases are observations, not proven mappings.
func (a *App) recordReceiptChange(parent context.Context, v *events.Receipt) {
	if v == nil || v.Chat.IsEmpty() || v.Sender.IsEmpty() {
		return
	}
	var kind string
	switch v.Type {
	case types.ReceiptTypeDelivered:
		kind = "delivered"
	case types.ReceiptTypeRead:
		kind = "read"
	case types.ReceiptTypeReadSelf:
		kind = "read-self"
	case types.ReceiptTypePlayed:
		kind = "played"
	case types.ReceiptTypePlayedSelf:
		kind = "played-self"
	case types.ReceiptTypeSender:
		kind = "sender"
	default:
		return
	}
	ids := v.MessageIDs
	skipped := 0
	if len(ids) > 200 {
		skipped = len(ids) - 200
		ids = ids[:200]
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	jid := func(j types.JID) string {
		if j.IsEmpty() {
			return ""
		}
		return j.ToNonAD().String()
	}
	err := a.db.RecordChangeReceipts(ctx, jid(v.Chat), ids, v.IsFromMe, store.ChangeReceipt{Type: kind, ActorJID: jid(v.Sender), ActorDevice: v.Sender.Device, SenderAlt: jid(v.SenderAlt), RecipientAlt: jid(v.RecipientAlt), MessageSender: jid(v.MessageSender), EventAt: v.Timestamp})
	if err != nil || skipped > 0 {
		a.emitWarning("change_receipt_incomplete", "warning: some receipt observations could not be retained in the local change feed", map[string]any{"skipped": skipped, "persistence_failed": err != nil})
	}
}
