# Agent output contract

Read when: integrating a coding agent with bounded offline archive queries and stable errors.

`--agent` enables JSON contract **v1**. `--detail compact|full` chooses its public detail level; compact is the default. Flags work before or after the subcommand. `--agent --json` still returns v1. Existing `--json` envelopes, field names, list defaults, tables, and `--full` table behavior remain unchanged. `--detail` without `--agent` is an error; `--full` does not select full agent detail.

```bash
wacli --store /path/to/archive --agent messages list
wacli messages search "invoice" --account personal --agent --limit 10
wacli --account personal messages show --chat 123@s.whatsapp.net --id ABC --agent --detail full
wacli --store /path/to/archive --agent doctor
```

## Supported queries and data

| Command | `data` | Full additions |
| --- | --- | --- |
| `messages list/search` | `messages` array; search also reports `search_mode` (`fts5` or `like`) and effective `order` (`relevance`, `time_desc`, `time_asc`) | Each message's `full` content, caption, names, forwarding/star/download metadata, selected buttons |
| `messages show` | One message DTO | Same message additions |
| `messages context` | `messages` array and `selected_id` | Same message additions |
| `chats list` / `chats show` | `chats` array / one chat DTO | Archived, pinned, stored mute deadline |
| `contacts search` / `contacts show` | `contacts` array / one contact DTO | System name, tags, metadata update timestamp |
| `contacts resolve` | `resolutions` array, one per input | Untruncated names |
| `history coverage` | `coverage` array with local message counts, oldest/newest message dates and anchor status | Untruncated names |
| `auth status` | Local authentication observation, public linked JID/phone when known, `session_revoked`, `connected` | Same observation |
| `doctor` (offline) | Auth observation, lock state, FTS availability and heartbeat activity date | Store counts and `last_message_at` |

Other commands, including mutations, downloads, `history fill/backfill`, and `doctor --connect`, fail before their argument validators or store/network operations with `unsupported_command`. Help, root help without a command, and version retain their normal text semantics. Unknown commands/flags are usage errors. Shell completion is outside the agent contract. `--events` is rejected with `--agent` to keep one error envelope on stderr.

All archive queries use the existing read-only opener, validate the archive schema without migration, and work while the writer lock is held. They never initialize a missing archive. Authentication status reads the public session JID and local revocation observation, without loading credentials or opening a WhatsApp client; an existing directory without a session reports unauthenticated. Offline doctor requires a readable current-schema archive; store failures are error envelopes with exit 4. Inspect session state separately with `auth status --agent` even if `wacli.db` is unreadable, or run legacy `doctor --json` (without `--agent` or `--connect`) to retain its diagnostic report, including archive errors. Neither command connects.

## Envelope and identity

Success is a single JSON line on **stdout**. A failure is a single JSON line on **stderr**, with no success payload on stdout. Public fields use snake_case. Success contains `data` and no `error`; failure contains `error` and no `data`.

```json
{"schema_version":1,"success":true,"account":{"store_ref":"/path/to/archive"},"meta":{"source":"local","detail":"compact","completeness":"unknown","freshness":"unknown","limit":20,"excluded":["tombstones"],"page":{"returned":1,"has_more":false,"next_cursor":null},"recovery":"Use --detail full; retrieve one message with messages show --chat CHAT_JID --id ID --detail full."},"data":{"messages":[{"id":"ABC","chat_jid":"123@s.whatsapp.net","sender_jid":"456@lid","from_me":false,"timestamp":"2026-01-01T00:00:00Z","text":"Hello","text_truncated":false,"type":"text"}]}}
```

`account.store_ref` is the absolute, resolved selected store path, once per envelope. `account.name` is included when a named/default account supplied the selection. A name alone never identifies a manual store. This transparent reference does not create a UUID or write local state. When parsing/config resolution cannot establish the selected store, `store_ref` is null; a syntactically selected manual `--store` can still be identified on preflight failures. The reference is a path, not a promise of canonical inode identity through symlinks.

Message IDs and stored chat/sender/quote JIDs are preserved. Chat views keep stored JIDs even when lookup used a verified alternate PN/LID. Contacts retain the existing verified PN/LID view; an unmapped LID has no invented phone number. `contacts resolve` accepts identities, not names: it uses only the persisted local pair map, produces exactly one result per input, and has no limited candidate search or interactive disambiguation. `resolved=true` means a local PN/LID pair is known; it is not proof of remote existence, current freshness, or safe recipient selection. Unknown pairs explicitly return `resolved=false`, preserving the known input identity.

## Bounds and omissions

Agent lists default to **20** rows; explicit `--limit` must be **1–200** in both details. Context retains defaults of five messages before/after; values must be nonnegative and `before + after + 1` must be at most 200. Its metadata includes the requested `before`, `after`, and total `limit`. Resolve accepts at most 200 inputs and thus at most 200 total results; its metadata reports that cap. `messages list` and temporal `messages search --sort time` support local keyset pagination (below). Other lists have no cursors or inferred next pages. Reaching or falling below a limit does not prove complete WhatsApp history.

Compact message `text` is display text, capped at **320 Unicode code points**, preserving valid UTF-8. `text_truncated` explicitly records cutting; there is no appended ellipsis that changes the text. Other selected labels (names, aliases, filenames) have the same cap and report affected fields in `fields_truncated`. Identifiers are never truncated. `meta.recovery` documents full retrieval. Full exposes only selected public DTO fields: it does not serialize internal structs, keys, authenticated media URLs, session paths, raw protobufs or blobs. It does not expose local download paths or button URLs; `downloaded`/`downloaded_at` are the selected download observations. The selected `account.store_ref` is the intentional path exception.

The total encoded envelope, including newline, is capped at **1 MiB compact / 8 MiB full**, checked before writing any success bytes. `payload_too_large` is a typed exit-1 error with stdout entirely empty and guidance to narrow the query; full also suggests `--detail compact`. Full removes per-text truncation, not row/envelope bounds. These are encoded **output** bounds, not total memory bounds: database rows/DTOs and the JSON envelope are materialized before the size check.

List/search and context neighbors retain legacy exclusion of tombstones, explicitly reported as `meta.excluded=["tombstones"]`. Direct `show` and a context target can return a tombstone with `revoked`, `deleted_for_me`, `deleted_at`, `deletion_reason`, and `payload_purged_at` when present. `edited` remains visible in both details. Full may show retained tombstone content until an explicit purge has erased it. Compact includes essential media type/filename/MIME, quote identity, reaction target/emoji and forwarded state when present; optional full fields are intentionally selected by the documented detail level.

## Local message pagination

Every successful `--agent messages list` includes typed `meta.page` metadata:

```json
{"returned":20,"has_more":true,"next_cursor":"OPAQUE_TOKEN"}
```

`returned` counts this page's messages. `has_more` means an extra matching row was observed in **this local archive query**, using `limit + 1`. At the local end, `has_more=false` and `next_cursor=null`, including empty and exact-limit results. This does not prove WhatsApp history is complete or fresh: `meta.completeness` and `meta.freshness` remain `unknown`. No history download is started.

Pass `next_cursor` unchanged to `--cursor TOKEN`, retaining the store selection, filters and order:

```bash
wacli --store /path/to/archive --agent messages list --chat 123@s.whatsapp.net --limit 20
wacli --store /path/to/archive --agent messages list --chat 123@s.whatsapp.net --cursor "$cursor" --limit 50 --detail full
```

`--cursor` is accepted only with `--agent messages list` or `--agent messages search QUERY --sort time`; other contexts fail before store/network effects. Flags can precede or follow the command. Tokens are opaque, versioned, at most **512 bytes**, strictly decoded, and contain no credential or authorization. No server, secret, persistent token state or store UUID is needed. Tokens are not tamper-proof and must not be interpreted as access controls. Malformed, oversized, unsupported-version and mismatched tokens return `invalid_cursor` with exit 2; errors never reproduce the token or SQL.

The cursor binds the absolute selected `store_ref`, normalized requested chat, sorted effective chat JIDs (including locally verified PN/LID aliases), sender, exclusive before/after second bounds, from-me/them, forwarded/starred filters, order and cursor version. Absent time bounds differ from valid zero-second bounds. Agent chat/sender inputs normalize phone numbers to JIDs, trim surrounding whitespace and remove PN device components; sender remains a single exact stored-JID filter, without expanding sender aliases. Equivalent date spellings and JID spellings are accepted. Changing `--limit` within 1–200 or switching compact/full is safe and does not change the cursor scope. A different PN/LID alias set invalidates the cursor and requires restarting; changing the requested chat identity also requires restarting, even if its effective aliases overlap. The store reference remains a path, not inode/database identity: do not reuse cursors after replacing/restoring the archive at that path.

Pagination orders by the existing local `(ts, rowid)` key, descending by default or ascending with `--asc`, and resumes **strictly** beyond the last returned tuple. It uses no OFFSET and does not require the anchor row to remain present. Static archives return same-second messages without omissions or repeats, including split PN/LID chats. For list, existing timestamp indexes are reused; multiple chat JIDs each provide bounded candidates before the final sort.

Each page is a **live read**, not a snapshot spanning calls. Tombstones remain excluded. Deleting or tombstoning the anchor still permits continuation. Inserts/backfill on the unvisited side of the boundary can appear in subsequent pages; inserts on the already visited side are skipped until a restart. A newly inserted same-second row sorts by its new local rowid: it can appear in ascending continuation and is behind a descending boundary. Once a page reports the local end there is no continuation cursor; restart to observe later changes.

The current upsert keeps an existing rowid, and an ordinary content edit preserves its message timestamp, as covered by fixtures. Other ingestion/update paths may change timestamps or filter membership: a row moving across the boundary can be repeated or omitted, and edited content may differ across pages. Normal inserts use AUTOINCREMENT, so ordinary deletion does not reuse rowids; archive replacement/restore, explicit rowid insertion or sequence manipulation can invalidate that assumption. A reused rowid is only ordered by its current tuple and has no remembered identity in the cursor. There is no universal positional stability, exactly-once delivery or snapshot guarantee. Restart for a fresh traversal after such changes; no list of all IDs or hidden snapshot is retained.

## Temporal search pagination

```bash
wacli --store /path/to/archive --agent messages search "invoice" --sort time --limit 20
wacli --store /path/to/archive --agent messages search "invoice" --sort time --cursor "$cursor" --detail full
wacli --store /path/to/archive --agent messages search "invoice" --sort time --asc
```

Search defaults to `--sort relevance`: FTS5 keeps its existing `bm25` ranking with descending rowid ties; fallback LIKE keeps its existing newest-first order. Neither default search has page metadata or accepts a cursor. `data.search_mode` states `fts5` or `like`; `data.order` states the effective order, respectively `relevance` or `time_desc`. Relevance is never silently reordered to permit continuation: a cursor or `--asc` (including `--asc=false`) requires `--sort time`, otherwise exit 2 `invalid_arguments` points to that flag. Search `--sort` and `--asc` are agent-only; specifying either without `--agent` fails before opening an archive. Legacy search output, matching and ordering remain unchanged.

`--sort time` enables the same `meta.page`, limit+1, tuple boundary, error handling and envelope caps as list. `data.order` is `time_desc` by default or `time_asc` with `--asc`. The cursor is bound to the **search operation**, store, engine, effective query, requested chat and effective PN/LID aliases, sender (`--from`), exclusive time bounds, media/type, forwarded/starred filters and direction. List and search cursors cannot be exchanged. Page size and detail may change. Syntax is checked before opening the archive; scope is checked before running the matching query. The list v1 codec and scope remain compatible.

Query normalization follows the matching engine: FTS quotes each whitespace-delimited token, preserving case, token order and punctuation; equivalent whitespace spellings share a scope. LIKE preserves the exact query string, including whitespace, and escapes literal percent, underscore and backslash. No case folding or token rearrangement is imposed on the cursor. Type filters normalize case/whitespace. Temporal agent search normalizes chat and `--from` JIDs like list's sender filter, without expanding sender aliases. FTS/LIKE matching fields, sanitization and FTS snippets are shared with existing search.

SQLite returns at most limit+1 matching rows to Go; no complete match set or offset is materialized in Go. This does not bound SQLite's work: LIKE can scan candidates and FTS temporal ordering can inspect/sort matching rows on each page, depending on query/filter selectivity. No new index or persistent pagination state is introduced.

These are live local search pages with the same limits described above, without snapshot or exactly-once guarantees. Edits can change matching without changing timestamps; ingestion can move timestamps across the boundary. Chat/sender names also participate in matching, and PN/LID mapping changes invalidate the scope. Messages can be omitted or repeated across changing reads. A local end does not imply complete remote history; restart without a cursor after relevant changes.

## Evidence and errors

`meta.source` is always `local`; `completeness` and `freshness` are **unknown** in v1. Local message bounds, row counts and anchor status describe only the archive. Coverage `ready` means a local anchor exists, not complete history. Missing timestamps are null. `last_message_at` is a message date, never a synchronization date. `last_activity_at` is the heartbeat date (possibly stale); a lock or heartbeat does not prove connectivity. Offline `connected` is always **unknown**.

| Exit | Error code | Meaning |
| --- | --- | --- |
| 0 | — | Successful query |
| 2 | `invalid_arguments` | Invalid arguments, unknown commands/flags, invalid detail/limit/filter |
| 2 | `invalid_cursor` | Malformed/unsupported cursor or different archive, filters, identity scope or order |
| 2 | `unsupported_command` | Recognized command outside the initial agent surface |
| 3 | `not_found` | Requested local item is absent |
| 4 | `store_unavailable` | Selected configuration/store/session state is absent, unreadable or incompatible |
| 1 | `payload_too_large` | The encoded result exceeds the detail's envelope cap |
| 1 | `internal_error` | Unclassified failure |

Errors carry `code` and `message`; `recovery` appears only when an actionable step is known. Store, session and unclassified failures expose selected public messages rather than raw internal causes or paths; original causes remain available to typed exit handling. No network retry prediction is emitted. Exit codes outside agent mode keep their legacy behavior.

```json
{"schema_version":1,"success":false,"account":{"store_ref":"/path/to/archive"},"meta":{"source":"local","detail":"compact","completeness":"unknown","freshness":"unknown"},"error":{"code":"not_found","message":"Requested item was not found in the selected local archive."}}
```

Use `--` to terminate flags, for example `messages search -- "--agent"` searches that literal text in legacy mode. A string value such as `--query="text containing --agent"` does not activate agent mode. A known flag's separate value, including a literal `--agent`, is consumed as its value. Invalid boolean values for the agent flag produce a v1 usage error. Unknown flags never execute a mutator while formatting an error.
