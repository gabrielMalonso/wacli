package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// SearchMessagesPageParams always selects temporal ordering. Relevance search
// remains available through SearchMessages and does not accept a cursor.
type SearchMessagesPageParams struct {
	SearchMessagesParams
	StoreRef     string
	ChatIdentity string
	Asc          bool
	Cursor       string
}

func (p SearchMessagesPageParams) cursorScope(fts bool) string {
	filters := ListMessagesPageParams{
		StoreRef: p.StoreRef, ChatIdentity: p.ChatIdentity,
		ListMessagesParams: ListMessagesParams{ChatJID: p.ChatJID, ChatJIDs: p.ChatJIDs,
			Before: p.Before, After: p.After, Asc: p.Asc,
			Forwarded: p.Forwarded, Starred: p.Starred},
	}
	sender := p.From
	if strings.TrimSpace(sender) == "" {
		sender = ""
	}
	query, engine := p.Query, "like"
	if fts {
		query, engine = sanitizeFTSQuery(p.Query), "fts5"
	}
	// LIKE uses the exact input, including whitespace; FTS uses quoted fields.
	// A distinct operation keeps list v1 cursors compatible and noninterchangeable.
	raw, _ := json.Marshal(struct {
		Operation, Filters, Query, Engine, Type, Sender string
		HasMedia                                        bool
	}{"messages search", filters.cursorScope(), query, engine, normalizedMessageType(p.Type), sender, p.HasMedia})
	hash := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// SearchMessagesPage observes live matching rows; no snapshot or match set is
// retained between calls. Validate scope before executing the matching query.
func (d *DB) SearchMessagesPage(p SearchMessagesPageParams) (MessagesPage, error) {
	if p.Limit < 1 || p.Limit > 200 {
		return MessagesPage{}, fmt.Errorf("page limit must be between 1 and 200")
	}
	if err := validateSearchMessages(p.SearchMessagesParams); err != nil {
		return MessagesPage{}, err
	}
	scope := p.cursorScope(d.ftsEnabled)
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
	params := p.SearchMessagesParams
	params.Limit++
	query, args := searchMessagesQuery(params, d.ftsEnabled, true, p.Asc, anchor)
	msgs, err := d.scanMessages(query, args...)
	if err != nil {
		return MessagesPage{}, err
	}
	return messagesPage(msgs, p.Limit, scope)
}
