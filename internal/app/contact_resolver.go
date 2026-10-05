package app

import (
	"context"
	"database/sql"
	"errors"

	"go.mau.fi/whatsmeow/types"
)

// ContactResolver distinguishes unknown public pairs from failed identity reads.
// Other LocalResolver consumers keep their existing best-effort interface.
// The optional display-name lookup retains its legacy fallback behavior.
type ContactResolver interface {
	ResolveLIDToPN(context.Context, types.JID) (types.JID, error)
	ResolvePNToLID(context.Context, types.JID) (types.JID, error)
	ResolveChatName(context.Context, types.JID, string) string
}

type contactSessionResolver struct {
	*readOnlySessionResolver
	sources contactIdentitySources
}

// ReadOnlyContactResolver shares the session lifetime with App, without opening
// a WhatsApp client or migrating the session. Missing tables and pre-LID devices
// are valid sources of unknown pairs, just as in the streaming contact reader.
func (a *App) ReadOnlyContactResolver(ctx context.Context) (ContactResolver, error) {
	resolver, err := a.readOnlySessionResolver()
	if err != nil {
		return nil, err
	}
	tx, err := resolver.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sources, err := inspectContactIdentitySourcesInSchema(ctx, tx, "main")
	if err != nil {
		return nil, err
	}
	return &contactSessionResolver{readOnlySessionResolver: resolver, sources: sources}, nil
}

func (r *contactSessionResolver) ResolveLIDToPN(ctx context.Context, jid types.JID) (types.JID, error) {
	if jid.Server != types.HiddenUserServer {
		return jid, nil
	}
	if r.sources.Own {
		own, err := r.resolveOwnPNForLID(ctx, jid)
		if err != nil {
			return types.EmptyJID, err
		}
		if !own.IsEmpty() {
			return own, nil
		}
	}
	if !r.sources.Map {
		return jid, nil
	}
	var user sql.NullString
	err := r.db.QueryRowContext(ctx, "SELECT pn FROM whatsmeow_lid_map WHERE lid=?", jid.User).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return jid, nil
	}
	if err != nil {
		return types.EmptyJID, err
	}
	if !user.Valid || user.String == "" {
		return jid, nil
	}
	return types.JID{User: user.String, Device: jid.Device, Server: types.DefaultUserServer}, nil
}

func (r *contactSessionResolver) ResolvePNToLID(ctx context.Context, jid types.JID) (types.JID, error) {
	if jid.Server != types.DefaultUserServer {
		return jid, nil
	}
	if r.sources.Own {
		own, err := r.resolveOwnLIDForPN(ctx, jid)
		if err != nil {
			return types.EmptyJID, err
		}
		if !own.IsEmpty() {
			return own, nil
		}
	}
	if !r.sources.Map {
		return jid, nil
	}
	var user sql.NullString
	err := r.db.QueryRowContext(ctx, "SELECT lid FROM whatsmeow_lid_map WHERE pn=?", jid.User).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return jid, nil
	}
	if err != nil {
		return types.EmptyJID, err
	}
	if !user.Valid || user.String == "" {
		return jid, nil
	}
	return types.JID{User: user.String, Device: jid.Device, Server: types.HiddenUserServer}, nil
}
