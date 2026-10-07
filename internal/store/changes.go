package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Change is a retained notification, not a historical message snapshot.
// EventID is stable across rereads; message references use exact stored keys.
type Change struct {
	EventID         string         `json:"event_id"`
	Kind            string         `json:"kind"`
	RecordedAt      time.Time      `json:"recorded_at"`
	ChatJID         string         `json:"chat_jid"`
	ID              string         `json:"id"`
	PreviousChatJID string         `json:"previous_chat_jid,omitempty"`
	SenderJID       string         `json:"sender_jid,omitempty"`
	FromMe          *bool          `json:"from_me,omitempty"`
	Tombstone       *bool          `json:"tombstone,omitempty"`
	Receipt         *ChangeReceipt `json:"receipt,omitempty"`
}

type ChangeReceipt struct {
	Type          string    `json:"type"`
	ActorJID      string    `json:"actor_jid"`
	ActorDevice   uint16    `json:"actor_device"`
	SenderAlt     string    `json:"sender_alt,omitempty"`
	RecipientAlt  string    `json:"recipient_alt,omitempty"`
	MessageSender string    `json:"message_sender,omitempty"`
	EventAt       time.Time `json:"event_at"`
}

type ChangesPage struct {
	Changes      []Change  `json:"changes"`
	HasMore      bool      `json:"has_more"`
	NextCursor   string    `json:"next_cursor"`
	IntroducedAt time.Time `json:"introduced_at"`
}

type ChangesCursorError struct{ Expired bool }

func (e *ChangesCursorError) Error() string {
	if e.Expired {
		return "Change cursor continuity is unavailable; archive may have been truncated or restored."
	}
	return "Invalid change cursor or cursor belongs to another archive or selection."
}

type changeCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	Epoch   string `json:"epoch"`
	Seq     int64  `json:"seq"`
	Anchor  string `json:"anchor"`
}

func ValidateChangesCursor(token string) error { _, err := decodeChangeCursor(token); return err }
func decodeChangeCursor(token string) (changeCursor, error) {
	var c changeCursor
	bad := &ChangesCursorError{}
	if len(token) == 0 || len(token) > 512 {
		return c, bad
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return c, bad
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, bad
	}
	canonical, _ := json.Marshal(c)
	validHex := func(s string, n int) bool {
		b, e := hex.DecodeString(s)
		return e == nil && len(b) == n && hex.EncodeToString(b) == s
	}
	if base64.RawURLEncoding.EncodeToString(canonical) != token || c.Version != 1 || c.Seq < 0 || !validHex(c.Scope, 32) || !validHex(c.Epoch, 16) || (c.Seq == 0 && c.Anchor != "") || (c.Seq > 0 && !validHex(c.Anchor, 16)) {
		return c, bad
	}
	return c, nil
}

// ListChanges reads one WAL snapshot. A terminal/empty page still returns a
// checkpoint so a restarted consumer can see later commits without timestamps.
// There are no filters: the cursor covers the whole selected archive.
func (d *DB) ListChanges(ctx context.Context, storeRef string, limit int, token string) (ChangesPage, error) {
	page := ChangesPage{Changes: []Change{}}
	if limit < 1 || limit > 200 {
		return page, fmt.Errorf("change limit must be between 1 and 200")
	}
	scope := sha256.Sum256([]byte("changes list\x00" + storeRef))
	c := changeCursor{Version: 1, Scope: hex.EncodeToString(scope[:])}
	if token != "" {
		var err error
		c, err = decodeChangeCursor(token)
		if err != nil {
			return page, err
		}
		if c.Scope != hex.EncodeToString(scope[:]) {
			return page, &ChangesCursorError{}
		}
	}
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if err = validateChangeSchema(ctx, tx); err != nil {
		return page, err
	}
	var epoch string
	var introduced int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch,introduced_at FROM change_feed_identity WHERE slot=1`).Scan(&epoch, &introduced); err != nil {
		return page, err
	}
	if token != "" && c.Epoch != epoch {
		return page, &ChangesCursorError{}
	}
	c.Epoch = epoch
	page.IntroducedAt = time.Unix(introduced, 0).UTC()
	if c.Seq > 0 {
		var anchor string
		err = tx.QueryRowContext(ctx, `SELECT event_id FROM archive_changes WHERE seq=?`, c.Seq).Scan(&anchor)
		if errors.Is(err, sql.ErrNoRows) || err == nil && anchor != c.Anchor {
			return page, &ChangesCursorError{Expired: true}
		}
		if err != nil {
			return page, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq,event_id,kind,recorded_at,chat_jid,msg_id,previous_chat_jid,sender_jid,from_me,tombstone,receipt_type,actor_jid,actor_device,sender_alt,recipient_alt,message_sender,event_ts
 FROM archive_changes WHERE seq>? ORDER BY seq LIMIT ?`, c.Seq, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var change Change
		var receipt ChangeReceipt
		var seq, recorded, eventTS int64
		var fromMe, tombstone bool
		if err = rows.Scan(&seq, &change.EventID, &change.Kind, &recorded, &change.ChatJID, &change.ID, &change.PreviousChatJID, &change.SenderJID, &fromMe, &tombstone, &receipt.Type, &receipt.ActorJID, &receipt.ActorDevice, &receipt.SenderAlt, &receipt.RecipientAlt, &receipt.MessageSender, &eventTS); err != nil {
			rows.Close()
			return page, err
		}
		if len(page.Changes) == limit {
			page.HasMore = true
			break
		}
		change.RecordedAt = time.Unix(recorded, 0).UTC()
		if change.Kind != "identity_mapping" {
			change.FromMe = &fromMe
		}
		if change.Kind != "identity_mapping" && change.Kind != "receipt" {
			change.Tombstone = &tombstone
		}
		if change.Kind == "receipt" {
			receipt.EventAt = time.Unix(eventTS, 0).UTC()
			change.Receipt = &receipt
		}
		page.Changes = append(page.Changes, change)
		c.Seq, c.Anchor = seq, change.EventID
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if err = tx.Commit(); err != nil {
		return page, err
	}
	raw, _ := json.Marshal(c)
	page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	return page, nil
}

// RecordChangeReceipts retains a bounded SDK observation batch atomically. No
// message lookup, alias discovery or recipient-delivery inference is performed.
func (d *DB) RecordChangeReceipts(ctx context.Context, chat string, ids []string, fromMe bool, receipt ChangeReceipt) error {
	if len(ids) > 200 || chat == "" || receipt.ActorJID == "" {
		return fmt.Errorf("invalid receipt batch")
	}
	switch receipt.Type {
	case "delivered", "read", "read-self", "played", "played-self", "sender":
	default:
		return fmt.Errorf("unsupported receipt observation")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if id == "" {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_changes(kind,chat_jid,msg_id,from_me,receipt_type,actor_jid,actor_device,sender_alt,recipient_alt,message_sender,event_ts)
 VALUES('receipt',?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat_jid,msg_id,receipt_type,actor_jid,actor_device,from_me,sender_alt,recipient_alt,message_sender,event_ts) WHERE kind='receipt' DO NOTHING`, chat, id, fromMe, receipt.Type, receipt.ActorJID, receipt.ActorDevice, receipt.SenderAlt, receipt.RecipientAlt, receipt.MessageSender, receipt.EventAt.Unix())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
