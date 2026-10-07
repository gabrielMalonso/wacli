# companion integrations

Read when: building a local analytics, search, CRM, or agent-side companion tool on top of synced `wacli` data.

`wacli` is intentionally useful from scripts without becoming a plugin host. Companion tools should prefer stable CLI output first, then use read-only SQLite access when they need low-latency local queries or their own derived database.

## Integration surfaces

- Use `--json` for one-shot command output from `chats`, `contacts`, `groups`, `messages`, `calls`, and `doctor`.
- For supported agent queries/actions, use the [versioned `--agent` contract](agent.md) with typed errors, compact/full projections and local cursors. Accounts still use legacy `--json`; auth/sync events keep their separate stream. An unsupported agent action must not silently fall back to a mutation in another mode.
- Use `--events` for line-delimited lifecycle events from long-running `auth`, `sync`, and `history backfill` commands.
- Use `sync --webhook` for live-message delivery to another process or service.
- Use a read-only SQLite connection to `<store>/wacli.db` for local analytics that need joins, cursors, or incremental scans.

Prefer the CLI or webhook when possible. Direct SQLite reads are powerful, but the schema can evolve between releases.

## Store paths

Platform defaults are described in [accounts](accounts.md). Linux uses an absolute `XDG_STATE_HOME` when set, otherwise the XDG state directory with legacy fallback; other platforms use `~/.wacli`. Configured accounts can point elsewhere. Obtain the actual selected path rather than constructing one from an account name:

```bash
wacli --read-only accounts list --json
read -r -p 'Account name from data.accounts: ' wacli_account
: "${wacli_account:?Select an existing account}"
wacli --read-only accounts show "$wacli_account" --json
```

`accounts list` reports `data.config_path` and each account's `store_dir`; `accounts show` returns `data.store_dir`. Keep `--account NAME` explicit for subsequent archive commands. A manual `--store DIR` cannot be combined with `--account`; `WACLI_STORE_DIR` overrides unselected/default stores, not an explicit account. Agent envelopes report the resolved selected `account.store_ref`.

The store contains two SQLite databases:

- `session.db`: owned by `whatsmeow`; contains linked-device identity and keys.
- `wacli.db`: owned by `wacli`; contains chats, contacts, groups, messages, status broadcasts, call events, media metadata, and local state.

Companion tools should not read or write `session.db` unless they are explicitly working on WhatsApp session internals. Never write to `wacli.db` from a companion tool.

For multi-account tools, iterate configured accounts explicitly and annotate derived rows with the account name in the companion tool's own database. Do not merge account data into `wacli.db`.

## Read-only SQLite

The following Bash example resolves the selected account through the CLI, checks its legacy success envelope, and percent-encodes the complete absolute database path with Python's `Path.as_uri()`. Spaces, Unicode and literal percent signs remain part of the path, not URI syntax. Python 3 and the SQLite CLI are needed only for these direct database examples; normal wacli queries do not require them.

```bash
wacli_account_json="$(wacli --read-only accounts show "$wacli_account" --json)" || exit "$?"
wacli_db_uri="$(printf '%s\n' "$wacli_account_json" | python3 -c '
import json, sys
from pathlib import Path
account = json.load(sys.stdin)
if account.get("success") is not True:
    raise SystemExit("Account selection failed; do not guess a store path.")
db = (Path(account["data"]["store_dir"]) / "wacli.db").resolve()
print(db.as_uri() + "?mode=ro")
')" || exit "$?"
sqlite3 -readonly "$wacli_db_uri" \
  "SELECT chat_jid, msg_id, datetime(ts, 'unixepoch') AS at, display_text
   FROM messages
   WHERE deleted_at IS NULL
   ORDER BY ts DESC
   LIMIT 20"
```

For a Python companion, resolve the same explicit account and close the readonly connection after the query. Pass the selected account name as `sys.argv[1]` (for example, `python3 companion.py "$wacli_account"` if you save this example as `companion.py`):

```python
from contextlib import closing
from pathlib import Path
import json
import sqlite3
import subprocess
import sys

selection = subprocess.run(
    ["wacli", "--read-only", "accounts", "show", sys.argv[1], "--json"],
    check=True, capture_output=True, text=True,
)
account = json.loads(selection.stdout)
if account.get("success") is not True:
    raise SystemExit("Account selection failed; do not guess a store path.")
db = (Path(account["data"]["store_dir"]) / "wacli.db").resolve()
with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as conn:
    conn.row_factory = sqlite3.Row
    rows = conn.execute("""
        SELECT chat_jid, msg_id, sender_jid, sender_name, ts, display_text
        FROM messages
        WHERE deleted_at IS NULL
        ORDER BY ts DESC
        LIMIT ?
    """, (50,)).fetchall()
    print(json.dumps([dict(row) for row in rows], ensure_ascii=False))
```

An absent database fails instead of creating one. Account/store selection failures must stop the query; do not substitute another account or initialize an archive. Direct schema queries need a compatible archive and SQLite build; schema compatibility is checked by wacli's archive readers, not by these snippets.

Use normal readonly SQLite when `wacli sync --follow` may write concurrently or start later; absent sidecars do not establish immutability. SQLite may create or update WAL/SHM bookkeeping and fail when required bookkeeping is unavailable. Wacli's readers use `mode=ro`/`query_only` without an immutable fallback; they do not create missing databases/directories/sessions, migrate, chmod, acquire writer LOCK or open WhatsApp. Reads do not share a global snapshot. See [store readonly access](store.md#local-reads-by-default).

## Local health and optional FTS checks

Run these diagnostics only on the selected readonly `wacli.db` connection. `PRAGMA integrity_check` checks SQLite structure, not WhatsApp completeness, freshness or remote consistency. Visible-message counts use `deleted_at IS NULL`, matching current list/search and FTS trigger filtering; `revoked=0 AND deleted_for_me=0` is not an equivalent tombstone filter.

```sql
PRAGMA integrity_check;

SELECT COUNT(*) AS visible_messages
FROM messages
WHERE deleted_at IS NULL;

SELECT COUNT(*) AS duplicate_keys
FROM (
    SELECT chat_jid, msg_id
    FROM messages
    GROUP BY chat_jid, msg_id
    HAVING COUNT(*) > 1
);

SELECT COUNT(*) AS orphan_messages
FROM messages m
LEFT JOIN chats c ON c.jid = m.chat_jid
WHERE c.jid IS NULL;

SELECT EXISTS(
    SELECT 1 FROM sqlite_master
    WHERE type = 'table' AND name = 'messages_fts'
) AS fts_table_present;
```

The supported schema's unique message key normally makes `duplicate_keys` zero. An orphan count is a local relationship observation, not evidence of remote deletion.

FTS is optional: a plain build/archive can have no `messages_fts`, while wacli search falls back to LIKE. Run this separate query only when `fts_table_present=1` **and** the SQLite reader supports FTS5. If it cannot read the table, report that failure rather than treating it as a zero count:

```sql
SELECT COUNT(*) AS indexed_messages FROM messages_fts;
```

Compare it with `visible_messages` in the same read transaction when a concurrent writer could change counts. Equal counts alone do not prove equal indexed content or complete history. Do not create/rebuild the index or write either database from a companion.

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
