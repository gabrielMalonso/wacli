# companion integrations

Read when: building a local analytics, search, CRM, or agent-side companion tool on top of synced `wacli` data.

`wacli` is intentionally useful from scripts without becoming a plugin host. Companion tools should prefer stable CLI output first, then use read-only SQLite access when they need low-latency local queries or their own derived database.

## Integration surfaces

- Use `--json` for one-shot command output from `chats`, `contacts`, `groups`, `messages`, `calls`, and `doctor`.
- Use `--events` for line-delimited lifecycle events from long-running `auth`, `sync`, and `history backfill` commands.
- Use `sync --webhook` for live-message delivery to another process or service.
- Use a read-only SQLite connection to `<store>/wacli.db` for local analytics that need joins, cursors, or incremental scans.

Prefer the CLI or webhook when possible. Direct SQLite reads are powerful, but the schema can evolve between releases.

## Store paths

The default store is:

- Linux: `~/.local/state/wacli`, with legacy `~/.wacli` reused when present.
- macOS and other platforms: `~/.wacli`.

Override with `--store DIR` or `WACLI_STORE_DIR`. Named accounts live in `config.yaml` and resolve with `--account NAME`; each account points at a normal isolated store directory.

The store contains two SQLite databases:

- `session.db`: owned by `whatsmeow`; contains linked-device identity and keys.
- `wacli.db`: owned by `wacli`; contains chats, contacts, groups, messages, status broadcasts, call events, media metadata, and local state.

Companion tools should not read or write `session.db` unless they are explicitly working on WhatsApp session internals. Never write to `wacli.db` from a companion tool.

For multi-account tools, iterate configured accounts explicitly and annotate derived rows with the account name in the companion tool's own database. Do not merge account data into `wacli.db`.

## Read-only SQLite

Open the database in SQLite read-only mode:

```bash
sqlite3 "file:$HOME/.wacli/wacli.db?mode=ro" \
  "SELECT chat_jid, msg_id, datetime(ts, 'unixepoch') AS at, display_text
   FROM messages
   WHERE deleted_at IS NULL
   ORDER BY ts DESC
   LIMIT 20"
```

In Python:

```python
from pathlib import Path
import sqlite3

db = Path.home() / ".wacli" / "wacli.db"
conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
conn.row_factory = sqlite3.Row

rows = conn.execute("""
    SELECT chat_jid, msg_id, sender_jid, sender_name, ts, display_text
    FROM messages
    WHERE deleted_at IS NULL
    ORDER BY ts DESC
    LIMIT ?
""", (50,)).fetchall()
```

Use normal readonly SQLite when `wacli sync --follow` may write concurrently or start later; absent sidecars do not establish immutability. SQLite may create or update WAL/SHM bookkeeping and fail when required bookkeeping is unavailable. Wacli's readers use `mode=ro`/`query_only` without an immutable fallback; they do not create missing databases/directories/sessions, migrate, chmod, acquire writer LOCK or open WhatsApp. Reads do not share a global snapshot. See [store readonly access](store.md#local-reads-by-default).

## Common queries

Recent human-visible messages:

```sql
SELECT
  m.chat_jid,
  COALESCE(m.chat_name, c.name, '') AS chat_name,
  m.msg_id,
  m.sender_jid,
  COALESCE(m.sender_name, '') AS sender_name,
  m.ts,
  COALESCE(m.display_text, m.text, '') AS text
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid
WHERE m.deleted_at IS NULL
ORDER BY m.ts DESC
LIMIT 100;
```

Incremental scan cursor:

```sql
SELECT rowid, chat_jid, msg_id, sender_jid, ts, display_text
FROM messages
WHERE rowid > ? AND deleted_at IS NULL
ORDER BY rowid ASC
LIMIT 1000;
```

The `rowid` cursor discovers inserts only. Deletion is an in-place update, so this cursor must not be used as a deletion feed. A companion that retains payloads must periodically reconcile the complete tombstone and purge-key sets by `(chat_jid, msg_id)`:

```sql
SELECT chat_jid, msg_id, deleted_at, deletion_reason, payload_purged_at
FROM messages
WHERE deleted_at IS NOT NULL;

SELECT chat_jid, msg_id, purged_at, deleted_at, deletion_reason
FROM message_payload_purges;
```

The purge ledger intentionally survives chat cleanup cascades. Missing rows in any scan are never evidence of deletion.

Recent WhatsApp call events:

```sql
SELECT chat_jid, call_id, event_type, direction, media, outcome, duration_secs, ts
FROM call_events
ORDER BY ts DESC
LIMIT 100;
```

Recent WhatsApp status broadcasts:

```sql
SELECT msg_id, sender_jid, sender_name, ts, text, media_type, media_caption
FROM status_messages
ORDER BY ts DESC
LIMIT 100;
```

Known chats by newest activity, including local unread badge counts:

```sql
SELECT jid, kind, name, last_message_ts, archived, pinned, muted_until,
       unread != 0 AS unread,
       unread_count
FROM chats
ORDER BY COALESCE(last_message_ts, 0) DESC
LIMIT 100;
```

`chats.unread` stores the boolean unread marker/state used by `chats list --unread` and `--no-unread`. `chats.unread_count` stores the numeric unread-message count separately; marker-only unread state from manual mark-unread and history sync leaves `unread_count` at zero.

Community subgroups:

```sql
SELECT jid, name, linked_parent_jid
FROM groups
WHERE linked_parent_jid IS NOT NULL
ORDER BY name;
```

## Privacy and safety

- Store derived data in your own database, not in `wacli.db`.
- Treat JIDs, display names, message text, media filenames, and local media paths as sensitive.
- Hash JIDs with a tool-local salt if you only need stable identity buckets.
- Provide a delete or opt-out path if the companion tool tracks people.
- Do not copy `session.db`, media keys, or WhatsApp device keys into unrelated systems.
- Use `WACLI_READONLY=1` when shelling out to `wacli` to reject intentional WhatsApp mutations and archive writes by wacli. Requested [message export files](messages.md#export), [media download output](media.md#download), [explicit adapter execution](media.md#explicit-local-transcription), and [SQLite WAL/SHM bookkeeping](store.md#local-reads-by-default) remain permitted. Downloads may use the network; the chosen adapter is not sandboxed and may make network requests or file writes.

## Speaker-tracking pattern

A speaker tracker can stay small and non-invasive:

1. Run `wacli sync --follow` separately to keep the store warm.
2. Keep a cursor using the largest processed `messages.rowid`.
3. Read only new rows from `messages` in read-only mode.
4. Skip `from_me` rows if you only want contacts.
5. Hash `sender_jid` before writing to the tool database.
6. Store counts, first/last seen timestamps, and opt-out state in the tool database.

This pattern keeps `wacli` responsible for WhatsApp sync and keeps the companion tool responsible only for its derived local state.
