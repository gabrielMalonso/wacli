package store

import (
	"fmt"
	"strings"
)

// CountConversationMessages counts distinct message IDs across a chat and an optional
// verified alias. Moving or merging duplicate alias rows does not change it.
// The caller owns identity resolution; this does not infer aliases.
func (d *DB) CountConversationMessages(chatJID, aliasJID string) (int64, error) {
	chatJID = strings.TrimSpace(chatJID)
	aliasJID = strings.TrimSpace(aliasJID)
	if chatJID == "" {
		return 0, fmt.Errorf("chat JID is required")
	}
	var count int64
	if aliasJID == "" || aliasJID == chatJID {
		return d.CountChatMessages(chatJID)
	}
	err := d.sql.QueryRowContext(storeCtx(), `SELECT COUNT(*) FROM (
		SELECT msg_id FROM messages WHERE chat_jid = ?
		UNION SELECT msg_id FROM messages WHERE chat_jid = ?
	)`, chatJID, aliasJID).Scan(&count)
	return count, err
}
