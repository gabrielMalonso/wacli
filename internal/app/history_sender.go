package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

// These are observations for one history batch, not identity repair evidence
// for records already in the archive. Crypto events keep their SDK sender.
type historySenderAccount struct {
	pn, lid types.JID
	pair    wa.PublicPairResult
	err     error
}

func historyUserJID(raw string) (types.JID, error) {
	jid, err := ParseHistoryJID(raw)
	if err != nil || jid.Integrator != 0 || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return types.JID{}, fmt.Errorf("invalid history author")
	}
	return jid, nil
}

func (a *App) historySenderAccount(ctx context.Context) historySenderAccount {
	var account historySenderAccount
	for _, field := range []struct {
		raw, server string
		jid         *types.JID
	}{
		{a.wa.LinkedJID(), types.DefaultUserServer, &account.pn},
		{a.wa.LinkedLID(), types.HiddenUserServer, &account.lid},
	} {
		if strings.TrimSpace(field.raw) == "" {
			continue
		}
		jid, err := historyUserJID(field.raw)
		_, normalizedErr := store.NormalizeDraftTarget(jid.String())
		if err != nil || jid.Server != field.server || normalizedErr != nil {
			account.err = fmt.Errorf("invalid public history account")
			return account
		}
		*field.jid = jid
	}
	if !account.pn.IsEmpty() && !account.lid.IsEmpty() {
		pair, err := a.wa.CheckPublicPair(ctx, account.pn, account.lid)
		account.pair = pair
		if err != nil {
			account.err = err
		} else if pair == wa.PublicPairContradictory {
			account.err = fmt.Errorf("contradictory public history account")
		}
	}
	return account
}

func (a *App) historyAuthorsMatch(ctx context.Context, left, right types.JID) (bool, error) {
	if left == right {
		return true, nil
	}
	if left.Server == right.Server {
		return false, fmt.Errorf("contradictory history authors")
	}
	pair, err := a.wa.CheckPublicPair(ctx, left, right)
	if err != nil {
		return false, err
	}
	if pair == wa.PublicPairContradictory {
		return false, fmt.Errorf("contradictory history author aliases")
	}
	return pair == wa.PublicPairVerified, nil
}

func (a *App) historyMessageSender(ctx context.Context, account historySenderAccount, pm wa.ParsedMessage, assertions []string) (string, error) {
	if account.err != nil {
		return "", account.err
	}
	var sender types.JID
	if pm.FromMe {
		// A public PN is a direct account fact. An absent PN is not supplied by
		// a remote JID, originalSelfAuthor, or a best-effort LID lookup.
		sender = account.pn
	} else if pm.SenderJID != "" {
		var err error
		sender, err = historyUserJID(pm.SenderJID)
		if err != nil {
			return "", err
		}
	}
	for i, raw := range assertions {
		if slices.Contains(assertions[:i], raw) {
			continue
		}
		author, err := historyUserJID(raw)
		if err != nil {
			return "", err
		}
		if sender.IsEmpty() {
			continue
		}
		match := sender == account.pn && author == account.lid && account.pair == wa.PublicPairVerified
		if !match {
			match, err = a.historyAuthorsMatch(ctx, sender, author)
		}
		if err != nil || !match {
			return "", err
		}
	}
	if sender.IsEmpty() {
		return "", nil
	}
	if !pm.FromMe {
		if sender == account.pn || sender == account.lid {
			return "", fmt.Errorf("incoming history author contradicts local account")
		}
		if pm.Chat.Server == types.DefaultUserServer || pm.Chat.Server == types.HiddenUserServer {
			match, err := a.historyAuthorsMatch(ctx, sender, pm.Chat.ToNonAD())
			if err != nil || !match {
				return "", err
			}
		}
	}
	if sender.Server == types.HiddenUserServer {
		// The resolver only suggests a candidate. A conversion needs the strict
		// two-way observation; failures never certify an alias. Without a
		// candidate, retain the explicit LID author, not an invented PN.
		pn := a.wa.ResolveLIDToPN(ctx, sender).ToNonAD()
		if pn != sender {
			if pn.Server != types.DefaultUserServer {
				return "", fmt.Errorf("invalid history author alias")
			}
			match, err := a.historyAuthorsMatch(ctx, sender, pn)
			if err != nil || !match {
				return "", err
			}
			sender = pn
		}
	}
	return sender.String(), nil
}

// Refuse an unknown-author replay of a retained known-sender row, rather than
// certifying its old sender or changing the shared upsert's content protections.
func (a *App) retainHistoryMessageSender(ctx context.Context, pm wa.ParsedMessage) error {
	if pm.SenderJID != "" {
		return nil
	}
	chat := canonicalJIDString(a.canonicalStoreJID(ctx, pm.Chat))
	var oldSender string
	var err error
	if pm.Chat == types.StatusBroadcastJID {
		old, readErr := a.db.GetStatusMessage(pm.ID)
		oldSender, err = old.SenderJID, readErr
	} else {
		old, readErr := a.db.GetMessage(chat, pm.ID)
		oldSender, err = old.SenderJID, readErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if oldSender != "" {
		return fmt.Errorf("history author unavailable; retained message was not replaced")
	}
	return nil
}
