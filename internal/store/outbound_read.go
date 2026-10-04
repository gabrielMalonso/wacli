package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

type OutboundObservationPage struct {
	Items      []OutboundObservation
	HasMore    bool
	NextCursor *string
}
type OutboundEntry struct {
	Operation    OutboundOperation
	Evidence     OutboundEvidence
	Observations OutboundObservationPage
}
type OutboundListItem struct {
	Operation OutboundOperation
	Evidence  OutboundEvidence
}
type OutboundPage struct {
	Items      []OutboundListItem
	HasMore    bool
	NextCursor *string
}

type outboundCursor struct {
	Version     int    `json:"v"`
	Domain      string `json:"domain"`
	Store       string `json:"store"`
	Account     string `json:"account"`
	Operation   string `json:"operation"`
	Created     int64  `json:"created"`
	ID          string `json:"id"`
	Observation int64  `json:"observation"`
}

func ValidateOutboundCursor(token string) error { _, err := decodeOutboundCursor(token); return err }
func decodeOutboundCursor(token string) (outboundCursor, error) {
	var c outboundCursor
	bad := func() (outboundCursor, error) { return c, outboundError("invalid_cursor", nil) }
	if len(token) == 0 || len(token) > 16384 {
		return bad()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return bad()
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return bad()
	}
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != token || c.Version != 1 || c.Store == "" || len(c.Store) > 4096 || c.Account != "" && ValidateOutboundAccount(c.Account) != nil {
		return bad()
	}
	switch c.Domain {
	case "operations":
		if ValidateDraftID(c.ID) != nil || c.Created < 1 || c.Operation != "" || c.Observation != 0 {
			return bad()
		}
	case "observations":
		if ValidateDraftID(c.Operation) != nil || c.Observation < 1 || c.ID != "" || c.Created != 0 || c.Account != "" {
			return bad()
		}
	default:
		return bad()
	}
	return c, nil
}
func encodeOutboundCursor(c outboundCursor) (*string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, outboundError("store_error", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err = ValidateOutboundCursor(token); err != nil {
		return nil, outboundError("store_error", err)
	}
	return &token, nil
}

// Read selects by ID OR key+frozen own PN. No current session identity is read.
// Operation, referenced payload validation, facts and pagination share one snapshot.
func (d *DB) readOutbound(ctx context.Context, id, key, account string, limit int, cursor string) (OutboundEntry, error) {
	var empty OutboundEntry
	if limit < 1 || limit > 200 {
		return empty, invalidOutbound("limit")
	}
	byID := id != ""
	if byID {
		if ValidateDraftID(id) != nil || key != "" || account != "" {
			return empty, invalidOutbound("selector")
		}
	} else if ValidateOutboundKey(key) != nil || ValidateOutboundAccount(account) != nil {
		return empty, invalidOutbound("selector")
	}
	var continuation outboundCursor
	if cursor != "" {
		var err error
		continuation, err = decodeOutboundCursor(cursor)
		if err != nil {
			return empty, err
		}
		if continuation.Domain != "observations" || continuation.Store != d.path {
			return empty, outboundError("invalid_cursor", nil)
		}
	}
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, outboundError("store_error", err)
	}
	defer tx.Rollback()
	where := "WHERE o.id=?"
	args := []any{id}
	if !byID {
		where = "WHERE o.account_jid=? AND o.idempotency_key=?"
		args = []any{account, key}
	}
	o, err := scanOutbound(tx.QueryRowContext(ctx, "SELECT "+outboundColumns+outboundFrom+where, args...))
	if err != nil {
		return empty, err
	}
	if cursor != "" && continuation.Operation != o.ID {
		return empty, outboundError("invalid_cursor", nil)
	}
	revision, err := readDraft(ctx, tx, o.DraftID, o.RevisionID)
	if err != nil {
		return empty, outboundError("store_error", err)
	}
	p := revision.Revision.Payload().Data()
	if revision.Revision.Payload().Hash() != o.Hash || p.Account != o.Account || p.Recipient != o.Recipient || p.Kind != o.Kind {
		return empty, outboundError("store_error", fmt.Errorf("outbound scope differs from frozen payload"))
	}
	e, items, more, err := outboundEvidence(ctx, tx, o, continuation.Observation, limit)
	if err != nil {
		return empty, err
	}
	page := OutboundObservationPage{Items: make([]OutboundObservation, 0, len(items)), HasMore: more}
	for _, f := range items {
		page.Items = append(page.Items, f.OutboundObservation)
	}
	if more {
		page.NextCursor, err = encodeOutboundCursor(outboundCursor{Version: 1, Domain: "observations", Store: d.path, Operation: o.ID, Observation: items[len(items)-1].ID})
		if err != nil {
			return empty, err
		}
	}
	if err = tx.Commit(); err != nil {
		return empty, outboundError("store_error", err)
	}
	return OutboundEntry{o, e, page}, nil
}

// List loads only immutable revision summaries, never full payload or media.
// Facts are derived within the same snapshot as the operation page.
func (d *DB) listOutbound(ctx context.Context, storeRef, account string, limit int, cursor string) (OutboundPage, error) {
	empty := OutboundPage{}
	if limit < 1 || limit > 200 || account != "" && ValidateOutboundAccount(account) != nil {
		return empty, invalidOutbound("list filters")
	}
	query := "SELECT " + outboundColumns + outboundFrom + "WHERE 1=1"
	args := []any{}
	if account != "" {
		query += " AND o.account_jid=?"
		args = append(args, account)
	}
	if cursor != "" {
		c, err := decodeOutboundCursor(cursor)
		if err != nil {
			return empty, err
		}
		if c.Domain != "operations" || c.Store != storeRef || c.Account != account {
			return empty, outboundError("invalid_cursor", nil)
		}
		query += " AND (o.created_at>? OR (o.created_at=? AND o.id>?))"
		args = append(args, c.Created, c.Created, c.ID)
	}
	query += " ORDER BY o.created_at,o.id LIMIT ?"
	args = append(args, limit+1)
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, outboundError("store_error", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return empty, outboundError("store_error", err)
	}
	page := OutboundPage{Items: make([]OutboundListItem, 0, limit+1)}
	for rows.Next() {
		o, err := scanOutbound(rows)
		if err != nil {
			rows.Close()
			return empty, err
		}
		page.Items = append(page.Items, OutboundListItem{Operation: o})
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return empty, outboundError("store_error", errors.Join(err, closeErr))
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
		last := page.Items[limit-1].Operation
		page.NextCursor, err = encodeOutboundCursor(outboundCursor{Version: 1, Domain: "operations", Store: storeRef, Account: account, Created: last.CreatedAt.UnixNano(), ID: last.ID})
		if err != nil {
			return empty, err
		}
	}
	for i := range page.Items {
		e, _, _, err := outboundEvidence(ctx, tx, page.Items[i].Operation, 0, 1)
		if err != nil {
			return empty, err
		}
		page.Items[i].Evidence = e
	}
	if err = tx.Commit(); err != nil {
		return empty, outboundError("store_error", err)
	}
	return page, nil
}
