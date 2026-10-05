# store

Read when: inspecting local SQLite size/counts or pruning old local chat/group rows.

`wacli store` manages the selected account's local `wacli.db` mirror. Cleanup commands only delete local wacli cache/history rows; they do not delete WhatsApp chats, leave groups, or remove messages from WhatsApp servers.

## Local reads by default

Local queries open existing databases read-only, without acquiring the writer
`LOCK`, creating the store/session, migrating schemas, or intentionally changing
stored data or file permissions. They work alongside a writer such as
`sync --follow`. JSON envelopes and payload fields remain unchanged.

| Local query | Source |
| --- | --- |
| `messages list/search/starred/show/context/export` | Local message archive |
| `chats list/show` (including channel chats) | Local chat archive |
| `contacts search/show/resolve` | Local contacts, aliases, and persisted PN/LID mapping |
| `groups list`, `groups participants list` | Local group snapshots |
| `polls list`, `poll show`, `calls list` | Local poll/call archive |
| `history coverage`, `history fill --dry-run` | Local coverage/plan |
| `store stats` | Local counts |
| `chats cleanup`, `groups prune`, `store cleanup`, `messages purge` with `--dry-run` | Local maintenance candidates/retained payload |
| `contacts import-system --dry-run` | Local contacts plus the requested input/source |
| `auth status`, `doctor` without `--connect` | Read-only session/status and diagnostics |

Persisted phone/JID/LID mappings resolve identities for display and lookup without
opening a writable WhatsApp client or rewriting historical rows. Missing mappings
leave the original identity unresolved. A missing session also leaves a message
chat filter literal, but session stat errors other than absence and readonly
opening failures stop the query instead of returning an empty success. Optional
best-effort display-name decoration is unchanged. No refresh happens during a
local query.

A missing `wacli.db` produces an actionable error without creating its directory
or database. `auth status` instead reports unauthenticated when its session is
absent; legacy offline `doctor` reports archive or auth-source failures in
`store_error` and retains its diagnostic JSON shape and exit 0. If the auth source
fails, its existing `authenticated=false` is a placeholder, not proof of
logout or lost authentication; no `linked_jid` is inferred. Known auth/JID observations remain
available if only the archive fails. Agent doctor instead returns its sanitized
error with exit 4 and no auth data. Read-only archive access requires the current schema
version: older or unversioned stores need an **explicit writable upgrade** (for
example, `auth` or `sync`); a newer schema needs a compatible newer wacli binary.
Local queries never perform that upgrade automatically. These writable commands
may connect to WhatsApp; there is currently no dedicated offline upgrade command.
Read-only validation also requires the complete supported migration-version set.
A synthetic/anomalous ledger that replaces an expected version with zero or a
negative value is refused even if its maximum version and row count still match;
this check does not diagnose general database corruption.

Archive and public-session readers (identity resolution, `auth status` and
offline `doctor`) use normal SQLite `mode=ro` with
`query_only`, including when no sidecars exist yet. SQLite may create or update
WAL/`-shm` bookkeeping even for a clean database in WAL journal mode. This permits
subsequent queries to observe commits from a writer that opens later; absence of
sidecars is not evidence of an immutable database. Missing database/session files
or directories are never created, and readers do not migrate, chmod, acquire the
writer LOCK or open a WhatsApp client. Required bookkeeping that cannot be
performed because of permissions produces an error, without an immutable fallback.
For example, a clean WAL file in a directory without write permission can fail,
while a clean DELETE-journal file needs no WAL bookkeeping. This does not promise
a common snapshot across statements, pages or the archive/session files, or
remote freshness/completeness.

`--read-only` and `WACLI_READONLY=1` remain barriers to explicit mutations.
Remote refresh/inspection commands retain their documented writable behavior,
including `contacts refresh/check`, `groups refresh/info`, `channels list/info`,
live profile queries, and `doctor --connect`. Media download retains its requested
file-writing behavior; `messages export --output` also writes the requested file
while reading the archive read-only. Maintenance commands with `--dry-run` use
this same read-only archive boundary; their execution paths still require the
writer lock and a writable store.

## Commands

```bash
wacli store stats
wacli store cleanup [--days N] [--dry-run] [--confirm]
```

Related cleanup commands:

```bash
wacli chats cleanup [--days N] [--jid JID] [--dry-run] [--confirm]
wacli groups prune [--days N] [--left-only=false|--include-active] [--dry-run] [--confirm]
```

## Notes

- `store stats` reads local counts for chats, groups, left groups, and normal chat messages.
- Status broadcasts are persisted separately in `status_messages`; they are not chat rows and are not included in normal chat/message cleanup paths.
- Location pins are persisted in `message_locations`. Coordinates follow the same lifecycle as the rest of a message's local payload: `messages purge` removes them with the retained payload, `store cleanup` and `chats cleanup` remove them with their chat, and a LID-to-phone identity migration moves them with the message rather than leaving them under the old identity.
- `store cleanup` removes chats whose known local activity is older than `--days` and deletes their messages through the SQLite chat/message cascade.
- `chats cleanup --jid JID` removes one local chat row and its local messages.
- `groups prune` removes local group metadata plus the matching local chat/messages for pruned group JIDs.
- `groups prune` defaults to groups you have left. `--days N` limits that to groups left more than `N` days ago.
- `groups prune --include-active --days N` also prunes active groups whose last known local message is older than `N` days. Groups with no known local activity timestamp are skipped.
- Destructive cleanup commands require confirmation unless `--confirm` is passed.
- If a row cannot be deleted, bulk cleanup continues with the other targets, then exits nonzero with the underlying errors and the number successfully deleted. In `--json` mode, failures use the error envelope on stderr and do not emit a success result on stdout. Successfully deleted rows stay deleted; failed deletions are rolled back individually.
- Use `--dry-run` first; it reads the existing current-schema store without taking the writer lock, deleting media, migrating schemas, or changing data/permissions. It works alongside sync, subject to the SQLite WAL bookkeeping caveat above.
- `store cleanup --dry-run` counts each selected chat once, including retained tombstones and chats with zero messages, and reuses those counts in text output. A counting error stops the preview before emitting its result. Selection and individual counts remain separate reads, not a globally atomic snapshot.
- `--read-only` and `WACLI_READONLY=1` allow `--dry-run` previews and reject cleanup/purge execution before opening the store for writes. Defaults, target selection, and confirmation requirements are unchanged.
- Use `--account NAME` to target a named account store. Use `--store DIR` for manual stores or migration debugging; it cannot be combined with `--account`.

## Examples

```bash
wacli store stats
wacli store cleanup --days 365 --dry-run
wacli chats cleanup --jid 1234567890@s.whatsapp.net --dry-run
wacli groups prune --dry-run
wacli groups prune --days 180 --dry-run
wacli groups prune --include-active --days 365 --dry-run
```
