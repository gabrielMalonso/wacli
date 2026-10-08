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
	"go.mau.fi/whatsmeow/types"
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
func (a *App) explicitHistoryAnchor(ctx context.Context, chatJID, id string) (store.MessageInfo, HistoryIdentity, error) {
	if _, err := a.db.GetChat(chatJID); err != nil {
		return store.MessageInfo{}, HistoryIdentity{}, err
	}
	m, err := a.db.GetHistoryAnchorMessage(chatJID, id)
	if err != nil {
		return store.MessageInfo{}, HistoryIdentity{}, err
	}
	invalid := func() (store.MessageInfo, HistoryIdentity, error) {
		return store.MessageInfo{}, HistoryIdentity{}, store.ErrInvalidHistoryAnchor
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

	identity, err := a.explicitHistoryScope(ctx, chat)
	if err != nil {
		return store.MessageInfo{}, HistoryIdentity{}, err
	}
	ownPN, _ := types.ParseJID(identity.AccountJID)
	ownLID, _ := types.ParseJID(identity.AccountAliasJID)
	if m.FromMe {
		match, err := a.historyAuthorsMatch(ctx, sender, ownPN)
		if err != nil {
			return store.MessageInfo{}, HistoryIdentity{}, err
		}
		if !match {
			return invalid()
		}
	} else {
		if sender == ownPN || sender == ownLID {
			return invalid()
		}
		if sender.Server != ownPN.Server {
			pair, err := a.wa.CheckPublicPair(ctx, sender, ownPN)
			if err != nil {
				return store.MessageInfo{}, HistoryIdentity{}, err
			}
			if pair == wa.PublicPairVerified {
				return invalid()
			}
		}
		if chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer {
			match, err := a.historyAuthorsMatch(ctx, sender, chat)
			if err != nil {
				return store.MessageInfo{}, HistoryIdentity{}, err
			}
			if !match {
				return invalid()
			}
		}
	}
	return store.MessageInfo{ChatJID: m.ChatJID, MsgID: m.MsgID, SenderJID: m.SenderJID, Timestamp: m.Timestamp, FromMe: m.FromMe}, identity, nil
}

// Explicit recovery observes only public local facts. Unlike the legacy
// resolvers this path cannot discover recipients or treat read errors as absence.
func (a *App) explicitHistoryScope(ctx context.Context, chat types.JID) (HistoryIdentity, error) {
	account := a.historySenderAccount(ctx)
	if account.err != nil {
		return HistoryIdentity{}, account.err
	}
	if account.pn.IsEmpty() {
		return HistoryIdentity{}, store.ErrInvalidHistoryAnchor
	}
	identity := HistoryIdentity{InputJID: chat.String(), ChatJID: chat.String(), AccountJID: account.pn.String()}
	if !account.lid.IsEmpty() {
		if account.pair != wa.PublicPairVerified {
			return HistoryIdentity{}, fmt.Errorf("explicit history account pair is not coherently verified")
		}
		identity.AccountAliasJID = account.lid.String()
	}
	if chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer {
		return identity, nil
	}
	alias, err := a.wa.LookupLocalAlias(ctx, chat)
	if err != nil {
		return HistoryIdentity{}, err
	}
	if alias.IsEmpty() {
		return identity, nil
	}
	normalized, err := historyUserJID(alias.String())
	if err != nil {
		return HistoryIdentity{}, err
	}
	if _, err := store.NormalizeDraftTarget(normalized.String()); err != nil {
		return HistoryIdentity{}, err
	}
	pair, err := a.wa.CheckPublicPair(ctx, chat, normalized)
	if err != nil {
		return HistoryIdentity{}, err
	}
	if pair != wa.PublicPairVerified {
		return HistoryIdentity{}, fmt.Errorf("explicit history identity pair is not coherently verified")
	}
	if chat.Server == types.DefaultUserServer {
		identity.AliasJID = normalized.String()
	} else {
		identity.ChatJID, identity.AliasJID = normalized.String(), chat.String()
	}
	return identity, nil
}
