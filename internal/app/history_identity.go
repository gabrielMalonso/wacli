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

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// HistoryIdentity is a local, strict observation, not remote recipient validation.
// Missing session/account facts remain unknown; query errors never imply no alias.
type HistoryIdentity struct {
	InputJID   string
	ChatJID    string
	AliasJID   string
	AccountJID string
}

func publicHistoryAccount(raw string) string {
	jid, err := types.ParseJID(strings.TrimSpace(raw))
	if err != nil || jid.User == "" || jid.Server != types.DefaultUserServer {
		return ""
	}
	return jid.ToNonAD().String()
}

func (a *App) ReadHistoryIdentities(ctx context.Context, inputs []string) ([]HistoryIdentity, error) {
	if len(inputs) > 200 {
		return nil, fmt.Errorf("history evidence accepts at most 200 inputs")
	}
	result := make([]HistoryIdentity, 0, len(inputs))
	seen := map[string]bool{}
	for _, raw := range inputs {
		jid, err := types.ParseJID(strings.TrimSpace(raw))
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
		lid, parseErr := types.ParseJID(lidText)
		if parseErr == nil && lid.Server == types.HiddenUserServer && lid.User != "" {
			ownAlias = lid.ToNonAD().String()
		} else {
			ownAlias = ""
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
		jid, _ := types.ParseJID(result[i].InputJID)
		if account != "" && ownAlias != "" && (jid.String() == account || jid.String() == ownAlias) {
			result[i].ChatJID = account
			result[i].AliasJID = ownAlias
			continue
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
			alias := types.NewJID(other, types.HiddenUserServer)
			if alias.User == "" {
				return nil, fmt.Errorf("invalid local history identity mapping")
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
			result[i].ChatJID = types.NewJID(other, types.DefaultUserServer).String()
			result[i].AliasJID = jid.String()
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
