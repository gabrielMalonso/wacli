package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MaxMessagesCursorBytes = 512

// MessagesCursorError deliberately never includes token contents or SQL.
type MessagesCursorError struct{ Mismatch bool }

func (e *MessagesCursorError) Error() string {
	if e.Mismatch {
		return "Cursor does not match the selected local archive, filters, order, or identity mapping; restart without --cursor."
	}
	return "Invalid or unsupported messages cursor; restart without --cursor."
}

// ListMessagesPageParams binds a live archive read to the selected store and
// normalized requested chat, as well as the effective filters in ListMessagesParams.
// Limit is deliberately excluded from the cursor scope.
type ListMessagesPageParams struct {
	ListMessagesParams
	StoreRef     string
	ChatIdentity string
	Cursor       string
}

type MessagesPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor *string
}

type messageCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	TS      int64  `json:"ts"`
	RowID   int64  `json:"rowid"`
}

// ValidateMessagesCursor checks syntax/version before a CLI opens an archive.
func ValidateMessagesCursor(token string) error {
	_, err := decodeMessagesCursor(token)
	return err
}

func decodeMessagesCursor(token string) (*messageCursor, error) {
	invalid := &MessagesCursorError{}
	if len(token) == 0 || len(token) > MaxMessagesCursorBytes {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var cursor messageCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, invalid
	}
	// Canonical re-encoding rejects unknown/duplicate/missing fields, whitespace,
	// trailing data, alternate numeric spellings and noncanonical base64.
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != token || cursor.Version != 1 || cursor.RowID <= 0 {
		return nil, invalid
	}
	scope, err := base64.RawURLEncoding.Strict().DecodeString(cursor.Scope)
	if err != nil || len(scope) != sha256.Size || base64.RawURLEncoding.EncodeToString(scope) != cursor.Scope {
		return nil, invalid
	}
	return &cursor, nil
}

func (p ListMessagesPageParams) cursorScope() string {
	chats := uniqueNonEmptyStrings(append([]string{p.ChatJID}, p.ChatJIDs...))
	sort.Strings(chats)
	seconds := func(t *time.Time) *int64 {
		if t == nil {
			return nil
		}
		sec := unix(*t)
		return &sec
	}
	// A fixed typed representation makes equivalent SQL filters share a scope.
	scope := struct {
		Store, Chat             string
		Chats                   []string
		Sender                  string
		Before, After           *int64
		FromMe                  *bool
		Asc, Forwarded, Starred bool
	}{p.StoreRef, strings.TrimSpace(p.ChatIdentity), chats, strings.TrimSpace(p.SenderJID), seconds(p.Before), seconds(p.After), p.FromMe, p.Asc, p.Forwarded, p.Starred}
	raw, _ := json.Marshal(scope)
	hash := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// ListMessagesPage uses (ts,rowid) keyset ordering, independent of whether the
// anchor still exists. Each call observes live data, not a cross-page snapshot.
func (d *DB) ListMessagesPage(p ListMessagesPageParams) (MessagesPage, error) {
	if p.Limit < 1 || p.Limit > 200 {
		return MessagesPage{}, fmt.Errorf("page limit must be between 1 and 200")
	}
	scope := p.cursorScope()
	var anchor *messageCursor
	if p.Cursor != "" {
		var err error
		anchor, err = decodeMessagesCursor(p.Cursor)
		if err != nil {
			return MessagesPage{}, err
		}
		if anchor.Scope != scope {
			return MessagesPage{}, &MessagesCursorError{Mismatch: true}
		}
	}
	queryParams := p.ListMessagesParams
	queryParams.Limit++
	query, args := listMessagesQuery(queryParams, anchor)
	msgs, err := d.scanMessages(query, args...)
	if err != nil {
		return MessagesPage{}, err
	}
	return messagesPage(msgs, p.Limit, scope)
}

func messagesPage(msgs []Message, limit int, scope string) (MessagesPage, error) {
	page := MessagesPage{Messages: msgs, HasMore: len(msgs) > limit}
	if page.HasMore {
		page.Messages = msgs[:limit]
		last := page.Messages[len(page.Messages)-1]
		raw, err := json.Marshal(messageCursor{Version: 1, Scope: scope, TS: last.rowTS, RowID: last.rowID})
		if err != nil {
			return MessagesPage{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		page.NextCursor = &token
	}
	return page, nil
}
