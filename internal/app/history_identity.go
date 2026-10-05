package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// HistoryIdentity is a local, strict observation, not remote recipient validation.
// Missing session/account facts remain unknown; query errors never imply no alias.
type HistoryIdentity struct {
	InputJID        string
	ChatJID         string
	AliasJID        string
	AccountJID      string
	AccountAliasJID string
}

// ParseHistoryJID validates a complete input before syntactic AD normalization.
// The upstream parser accepts server-only and extra-at inputs; recovery keys must not.
func ParseHistoryJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if strings.Count(raw, "@") != 1 || strings.ContainsFunc(raw, unicode.IsSpace) || strings.ContainsFunc(raw, unicode.IsControl) {
		return types.JID{}, fmt.Errorf("invalid history chat JID")
	}
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" || jid.Server == "" {
		return types.JID{}, fmt.Errorf("invalid history chat JID")
	}
	return jid.ToNonAD(), nil
}

func historyMappedJID(user, server string) (types.JID, error) {
	jid, err := ParseHistoryJID(user + "@" + server)
	if err != nil || jid.User != user || jid.Server != server {
		return types.JID{}, fmt.Errorf("invalid local history identity mapping")
	}
	return jid, nil
}

func publicHistoryAccount(raw string) string {
	jid, err := ParseHistoryJID(raw)
	if err != nil || jid.User == "" || jid.Server != types.DefaultUserServer {
		return ""
	}
	return jid.ToNonAD().String()
}

func (a *App) ReadHistoryIdentities(ctx context.Context, inputs []string) ([]HistoryIdentity, error) {
	return a.readLocalIdentities(ctx, inputs, 200)
}

// Draft preparation may include 200 mentions, a target and two quote senders.
// History's public 200-input contract remains unchanged.
func (a *App) ReadDraftIdentities(ctx context.Context, inputs []string) ([]HistoryIdentity, error) {
	result, err := a.readLocalIdentities(ctx, inputs, 203)
	if err != nil || len(result) == 0 || result[0].AccountJID == "" || result[0].AccountAliasJID != "" {
		return result, err
	}
	// A nullable device LID permits the same verified public-map fallback used
	// by history/contact identity reads; no LID digits become a phone.
	own, err := a.readLocalIdentities(ctx, []string{result[0].AccountJID}, 1)
	if err != nil {
		return nil, err
	}
	if len(own) != 1 || own[0].AccountJID != result[0].AccountJID {
		return nil, fmt.Errorf("local draft account changed during observation")
	}
	for i := range result {
		result[i].AccountAliasJID = own[0].AliasJID
	}
	return result, nil
}

func (a *App) readLocalIdentities(ctx context.Context, inputs []string, maxInputs int) ([]HistoryIdentity, error) {
	return a.readLocalIdentitiesPolicy(ctx, inputs, maxInputs, false)
}

// The chat-state policy rejects malformed public device facts and checks the
// requested own identity against the map. Existing readers retain their policy.
func (a *App) readLocalIdentitiesPolicy(ctx context.Context, inputs []string, maxInputs int, strictChatState bool) ([]HistoryIdentity, error) {
	if len(inputs) > maxInputs {
		return nil, fmt.Errorf("history evidence accepts at most 200 inputs")
	}
	result := make([]HistoryIdentity, 0, len(inputs))
	seen := map[string]bool{}
	for _, raw := range inputs {
		jid, err := ParseHistoryJID(raw)
		if err != nil || jid.User == "" {
			return nil, fmt.Errorf("invalid history identity")
		}
		key := jid.ToNonAD().String()
		if !seen[key] {
			seen[key] = true
			result = append(result, HistoryIdentity{InputJID: key, ChatJID: key})
		}
	}
	if len(result) == 0 {
		return result, nil
	}
	path := filepath.Join(a.StoreDir(), "session.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return nil, err
	}
	r, err := openReadOnlySessionResolver(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// Read only public account identity, in a single transaction with the mapping.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT jid, COALESCE(lid,'') FROM whatsmeow_device`)
	if err != nil {
		return nil, err
	}
	var account, ownAlias string
	count := 0
	for rows.Next() {
		var raw, lidText string
		if err = rows.Scan(&raw, &lidText); err != nil {
			_ = rows.Close()
			return nil, err
		}
		count++
		account = publicHistoryAccount(raw)
		if strictChatState && account == "" {
			_ = rows.Close()
			return nil, fmt.Errorf("invalid public account identity")
		}
		lid, parseErr := ParseHistoryJID(lidText)
		if parseErr == nil && lid.Server == types.HiddenUserServer && lid.User != "" {
			ownAlias = lid.ToNonAD().String()
		} else {
			ownAlias = ""
			if strictChatState && lidText != "" {
				_ = rows.Close()
				return nil, fmt.Errorf("invalid public account alias")
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		account = ""
		ownAlias = ""
	}
	for i := range result {
		result[i].AccountJID = account
		result[i].AccountAliasJID = ownAlias
		jid, _ := types.ParseJID(result[i].InputJID)
		if account != "" && ownAlias != "" && (jid.String() == account || jid.String() == ownAlias) {
			result[i].ChatJID = account
			result[i].AliasJID = ownAlias
			if !strictChatState {
				continue
			}
		}
		var other string
		switch jid.Server {
		case types.DefaultUserServer:
			err = tx.QueryRowContext(ctx, `SELECT lid FROM whatsmeow_lid_map WHERE pn=?`, jid.User).Scan(&other)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			alias, err := historyMappedJID(other, types.HiddenUserServer)
			if err != nil {
				return nil, err
			}
			var reverse string
			if err = tx.QueryRowContext(ctx, `SELECT pn FROM whatsmeow_lid_map WHERE lid=?`, other).Scan(&reverse); err != nil {
				return nil, err
			}
			if reverse != jid.User {
				return nil, fmt.Errorf("inconsistent local history identity mapping")
			}
			result[i].AliasJID = alias.String()
		case types.HiddenUserServer:
			err = tx.QueryRowContext(ctx, `SELECT pn FROM whatsmeow_lid_map WHERE lid=?`, jid.User).Scan(&other)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var reverse string
			if err = tx.QueryRowContext(ctx, `SELECT lid FROM whatsmeow_lid_map WHERE pn=?`, other).Scan(&reverse); err != nil {
				return nil, err
			}
			if reverse != jid.User || other == "" {
				return nil, fmt.Errorf("inconsistent local history identity mapping")
			}
			pn, err := historyMappedJID(other, types.DefaultUserServer)
			if err != nil {
				return nil, err
			}
			result[i].ChatJID = pn.String()
			result[i].AliasJID = jid.String()
		}
		if strictChatState && account != "" && ownAlias != "" {
			item := result[i]
			if (item.ChatJID == account || item.AliasJID == ownAlias) && (item.ChatJID != account || item.AliasJID != ownAlias) {
				return nil, fmt.Errorf("public device and requested identity map contradict")
			}
		}
	}
	return result, tx.Commit()
}

// Relation compares only public account and the observed identity set, at this
// read. It says nothing about intervening remapping, restored archives or freshness.
func HistoryIdentityRelation(a store.HistoryAttempt, current HistoryIdentity) string {
	if a.AccountJID == "" || current.AccountJID == "" || a.WindowChatJID == "" {
		return "unknown"
	}
	if a.AccountJID != current.AccountJID {
		return "changed"
	}
	historical := []string{a.WindowChatJID}
	if a.WindowAliasJID != "" && a.WindowAliasJID != a.WindowChatJID {
		historical = append(historical, a.WindowAliasJID)
	}
	present := []string{current.ChatJID}
	if current.AliasJID != "" && current.AliasJID != current.ChatJID {
		present = append(present, current.AliasJID)
	}
	slices.Sort(historical)
	slices.Sort(present)
	if slices.Equal(historical, present) {
		return "matching_snapshot"
	}
	// An unavailable pair is not positive evidence of a different pair.
	if len(present) == 1 && len(historical) == 2 {
		return "unknown"
	}
	return "changed"
}
