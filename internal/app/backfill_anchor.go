package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

func validateBeforeID(id string) error {
	if !utf8.ValidString(id) || len(id) > 256 || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return fmt.Errorf("--before-id requires an exact message ID of at most 256 bytes")
	}
	return nil
}

func anchorFailureCode(err error) string {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrInvalidHistoryAnchor) {
		return "no_local_anchor"
	}
	return "store_state"
}

// Resolve only in this writer's archive and exact chat. Validation never repairs
// metadata, infers a phone number from a LID or substitutes another anchor.
func (a *App) explicitHistoryAnchor(ctx context.Context, chatJID, id string) (store.MessageInfo, string, error) {
	if _, err := a.db.GetChat(chatJID); err != nil {
		return store.MessageInfo{}, "", err
	}
	m, err := a.db.GetHistoryAnchorMessage(chatJID, id)
	if err != nil {
		return store.MessageInfo{}, "", err
	}
	invalid := func() (store.MessageInfo, string, error) {
		return store.MessageInfo{}, "", store.ErrInvalidHistoryAnchor
	}
	chat, err := ParseHistoryJID(m.ChatJID)
	if err != nil || chat.String() != chatJID || m.MsgID != id {
		return invalid()
	}
	if strings.ContainsFunc(m.SenderJID, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return invalid()
	}
	sender, err := historyUserJID(m.SenderJID)
	if err != nil {
		return invalid()
	}
	if _, err := store.NormalizeDraftTarget(sender.String()); err != nil {
		return invalid()
	}
	account := a.historySenderAccount(ctx)
	if account.err != nil {
		return store.MessageInfo{}, "", account.err
	}
	if account.pn.IsEmpty() {
		return invalid()
	}
	// Use the same strict author/account rules as history ingestion, including
	// verified PN/LID pairs and contradictions in from_me or incoming authors.
	author, err := a.historyMessageSender(ctx, account, wa.ParsedMessage{Chat: chat, SenderJID: sender.String(), FromMe: m.FromMe}, []string{sender.String()})
	if err != nil {
		return store.MessageInfo{}, "", fmt.Errorf("%w: %v", store.ErrInvalidHistoryAnchor, err)
	}
	if author == "" {
		return invalid()
	}
	return store.MessageInfo{ChatJID: m.ChatJID, MsgID: m.MsgID, SenderJID: m.SenderJID, Timestamp: m.Timestamp, FromMe: m.FromMe}, account.pn.String(), nil
}
