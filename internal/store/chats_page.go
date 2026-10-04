package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Chat keys include the complete stored JID; allow text identities but bound decoding.
const MaxChatsCursorBytes = 16384

// ChatsCursorError never includes token contents, identities or SQL.
type ChatsCursorError struct{ Mismatch bool }

func (e *ChatsCursorError) Error() string {
	if e.Mismatch {
		return "Cursor does not match the selected local archive or chat filters; restart without --cursor."
	}
	return "Invalid or unsupported chats cursor; restart without --cursor."
}

type ListChatsPageParams struct {
	ChatListFilter
	StoreRef string
	Cursor   string
}

type ChatsPage struct {
	Chats      []Chat
	HasMore    bool
	NextCursor *string
}

type chatKey struct {
	Pin int    `json:"pin"`
	TS  int64  `json:"ts"`
	JID string `json:"jid"`
}

type chatCursor struct {
	Version   int     `json:"v"`
	Operation string  `json:"op"`
	Scope     string  `json:"scope"`
	Key       chatKey `json:"key"`
}

// ValidateChatsCursor checks syntax/version before opening an archive.
func ValidateChatsCursor(token string) error {
	_, err := decodeChatsCursor(token)
	return err
}

func decodeChatsCursor(token string) (*chatCursor, error) {
	invalid := &ChatsCursorError{}
	if len(token) == 0 || len(token) > MaxChatsCursorBytes {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var cursor chatCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, invalid
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != token || cursor.Version != 1 || cursor.Operation != "chats list" || (cursor.Key.Pin != 0 && cursor.Key.Pin != 1) || cursor.Key.JID == "" {
		return nil, invalid
	}
	scope, err := base64.RawURLEncoding.Strict().DecodeString(cursor.Scope)
	if err != nil || len(scope) != sha256.Size || base64.RawURLEncoding.EncodeToString(scope) != cursor.Scope {
		return nil, invalid
	}
	return &cursor, nil
}

// Whitespace-only queries do not filter; otherwise LIKE preserves the exact input.
func effectiveChatQuery(query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	return query
}

func (p ListChatsPageParams) cursorScope() string {
	raw, _ := json.Marshal(struct {
		Operation, Store, Query         string
		Archived, Pinned, Muted, Unread *bool
	}{"chats list v1", p.StoreRef, effectiveChatQuery(p.Query), p.Archived, p.Pinned, p.Muted, p.Unread})
	hash := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// Share these exact expressions between ORDER BY and the keyset predicate.
const chatPinKeySQL = `CASE WHEN COALESCE(pinned,0) != 0 THEN 1 ELSE 0 END`
const chatTSKeySQL = `COALESCE(last_message_ts,0)`

func chatsPageQuery(f ChatListFilter, anchor *chatKey, now int64) (string, []any) {
	q := `SELECT jid, kind, COALESCE(name,''), ` + chatTSKeySQL + `, COALESCE(archived,0), ` + chatPinKeySQL + `, COALESCE(muted_until,0), COALESCE(unread,0), COALESCE(unread_count,0) FROM chats WHERE 1=1`
	var args []any
	if query := effectiveChatQuery(f.Query); query != "" {
		q += ` AND (LOWER(name) LIKE LOWER(?) ESCAPE '\' OR LOWER(jid) LIKE LOWER(?) ESCAPE '\')`
		needle := likeContains(query)
		args = append(args, needle, needle)
	}
	if f.Archived != nil {
		q += ` AND (COALESCE(archived,0) != 0) = ?`
		args = append(args, boolToInt(*f.Archived))
	}
	if f.Pinned != nil {
		q += ` AND (` + chatPinKeySQL + `) = ?`
		args = append(args, boolToInt(*f.Pinned))
	}
	if f.Muted != nil {
		q += ` AND (COALESCE(muted_until,0) = -1 OR COALESCE(muted_until,0) > ?) = ?`
		args = append(args, now, boolToInt(*f.Muted))
	}
	if f.Unread != nil {
		q += ` AND (COALESCE(unread,0) != 0) = ?`
		args = append(args, boolToInt(*f.Unread))
	}
	if anchor != nil {
		q += ` AND ((` + chatPinKeySQL + `) < ? OR ((` + chatPinKeySQL + `) = ? AND (` + chatTSKeySQL + ` < ? OR (` + chatTSKeySQL + ` = ? AND jid COLLATE BINARY > ?))))`
		args = append(args, anchor.Pin, anchor.Pin, anchor.TS, anchor.TS, anchor.JID)
	}
	q += ` ORDER BY (` + chatPinKeySQL + `) DESC, ` + chatTSKeySQL + ` DESC, jid COLLATE BINARY ASC LIMIT ?`
	args = append(args, f.Limit)
	return q, args
}

// ListChatsPage reads raw stored identities with at most limit+1 rows in Go.
// It needs no surviving anchor or cross-page snapshot. SQLite may scan/sort.
func (d *DB) ListChatsPage(p ListChatsPageParams) (ChatsPage, error) {
	if p.Limit < 1 || p.Limit > 200 {
		return ChatsPage{}, fmt.Errorf("page limit must be between 1 and 200")
	}
	scope := p.cursorScope()
	var anchor *chatKey
	if p.Cursor != "" {
		cursor, err := decodeChatsCursor(p.Cursor)
		if err != nil {
			return ChatsPage{}, err
		}
		if cursor.Scope != scope {
			return ChatsPage{}, &ChatsCursorError{Mismatch: true}
		}
		anchor = &cursor.Key
	}
	f := p.ChatListFilter
	f.Limit++
	query, args := chatsPageQuery(f, anchor, nowUTC().Unix())
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return ChatsPage{}, err
	}
	defer rows.Close()
	page := ChatsPage{Chats: make([]Chat, 0, p.Limit)}
	var last chatKey
	for rows.Next() {
		var c Chat
		var ts int64
		var archived, pinned, unread, unreadCount int
		if err := rows.Scan(&c.JID, &c.Kind, &c.Name, &ts, &archived, &pinned, &c.MutedUntil, &unread, &unreadCount); err != nil {
			return ChatsPage{}, err
		}
		if len(page.Chats) == p.Limit {
			page.HasMore = true
			continue
		}
		c.LastMessageTS = fromUnix(ts)
		c.Archived, c.Pinned = archived != 0, pinned != 0
		applyChatUnread(&c, unread, unreadCount)
		page.Chats = append(page.Chats, c)
		// Preserve raw ts: fromUnix intentionally hides nonpositive timestamps in DTOs.
		last = chatKey{Pin: pinned, TS: ts, JID: c.JID}
	}
	if err := rows.Err(); err != nil {
		return ChatsPage{}, err
	}
	if page.HasMore {
		raw, err := json.Marshal(chatCursor{Version: 1, Operation: "chats list", Scope: scope, Key: last})
		if err != nil {
			return ChatsPage{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		if err := ValidateChatsCursor(token); err != nil {
			// This key came from stored data, not from a caller's cursor.
			return ChatsPage{}, fmt.Errorf("stored chat key cannot produce a supported continuation")
		}
		page.NextCursor = &token
	}
	return page, nil
}
