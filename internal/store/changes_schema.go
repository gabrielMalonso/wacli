package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// References only: the log must not retain message bodies or media credentials.
const changesSchema = `
CREATE TABLE IF NOT EXISTS change_feed_identity (
 slot INTEGER PRIMARY KEY CHECK(slot=1), epoch TEXT NOT NULL,
 introduced_at INTEGER NOT NULL
);
INSERT OR IGNORE INTO change_feed_identity VALUES(1,lower(hex(randomblob(16))),unixepoch());
CREATE TABLE IF NOT EXISTS archive_changes (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 event_id TEXT NOT NULL UNIQUE DEFAULT (lower(hex(randomblob(16)))),
 recorded_at INTEGER NOT NULL DEFAULT (unixepoch()),
 kind TEXT NOT NULL,
 chat_jid TEXT NOT NULL,
 msg_id TEXT NOT NULL,
 previous_chat_jid TEXT NOT NULL DEFAULT '',
 sender_jid TEXT NOT NULL DEFAULT '',
 from_me INTEGER NOT NULL DEFAULT 0,
 tombstone INTEGER NOT NULL DEFAULT 0,
 receipt_type TEXT NOT NULL DEFAULT '',
 actor_jid TEXT NOT NULL DEFAULT '',
 actor_device INTEGER NOT NULL DEFAULT 0,
 sender_alt TEXT NOT NULL DEFAULT '',
 recipient_alt TEXT NOT NULL DEFAULT '',
 message_sender TEXT NOT NULL DEFAULT '',
 event_ts INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_change_receipt ON archive_changes(
 chat_jid,msg_id,receipt_type,actor_jid,actor_device,from_me,sender_alt,recipient_alt,message_sender,event_ts
) WHERE kind='receipt';
`

func migrateChanges(d *DB) error {
	// Finish existing legacy column repairs before freezing the compared columns.
	if err := d.ensureCurrentSchema(); err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(changesSchema); err != nil {
		return err
	}
	statements, err := messageChangeTriggers(tx)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Compare every persisted column, including metadata/identity/media enrichment.
// NULL-safe comparisons suppress SDK/history replays that leave the row unchanged.
func messageChangeTriggers(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return nil, err
	}
	var comparisons []string
	for rows.Next() {
		var cid, required, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &required, &def, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		if name != "rowid" {
			quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
			comparisons = append(comparisons, "OLD."+quoted+" IS NOT NEW."+quoted)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Legacy partial archives cannot expose a feed.
	if len(comparisons) == 0 {
		return nil, nil
	}
	return []string{
		`CREATE TRIGGER IF NOT EXISTS changes_message_insert AFTER INSERT ON messages BEGIN
 INSERT INTO archive_changes(kind,chat_jid,msg_id,sender_jid,from_me,tombstone)
 VALUES(CASE WHEN NEW.deleted_at IS NOT NULL THEN 'message_tombstone' ELSE 'message_insert' END,
 NEW.chat_jid,NEW.msg_id,COALESCE(NEW.sender_jid,''),NEW.from_me,NEW.deleted_at IS NOT NULL); END`,
		`CREATE TRIGGER IF NOT EXISTS changes_message_update AFTER UPDATE ON messages WHEN ` + strings.Join(comparisons, " OR ") + ` BEGIN
 INSERT INTO archive_changes(kind,chat_jid,msg_id,previous_chat_jid,sender_jid,from_me,tombstone)
 VALUES(CASE WHEN NEW.deleted_at IS NOT NULL THEN 'message_tombstone' ELSE 'message_update' END,
 NEW.chat_jid,NEW.msg_id,CASE WHEN OLD.chat_jid IS NOT NEW.chat_jid THEN OLD.chat_jid ELSE '' END,
 COALESCE(NEW.sender_jid,''),NEW.from_me,NEW.deleted_at IS NOT NULL); END`,
		`CREATE TRIGGER IF NOT EXISTS changes_message_delete AFTER DELETE ON messages BEGIN
 INSERT INTO archive_changes(kind,chat_jid,msg_id,sender_jid,from_me,tombstone)
 VALUES('message_delete',OLD.chat_jid,OLD.msg_id,COALESCE(OLD.sender_jid,''),OLD.from_me,OLD.deleted_at IS NOT NULL); END`,
	}, nil
}

type changeSchemaReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// A migration ledger alone cannot certify feed coverage when required objects
// are missing. This check is readonly and never repairs an incompatible store.
func validateChangeSchema(ctx context.Context, reader changeSchemaReader) error {
	var count int
	err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE
	(type='trigger' AND tbl_name='messages' AND name IN ('changes_message_insert','changes_message_update','changes_message_delete'))
	OR (type='table' AND name IN ('messages','archive_changes','change_feed_identity'))
	OR (type='index' AND name='idx_change_receipt')`).Scan(&count)
	if err != nil {
		return err
	}
	if count != 7 {
		return fmt.Errorf("change feed schema unavailable")
	}
	var messages int
	return reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT chat_jid,msg_id,sender_jid,from_me,deleted_at FROM messages LIMIT 0)`).Scan(&messages)
}
