# Agent output contract

Read when: integrating a coding agent with bounded archive queries, explicit history recovery, local drafts, and stable errors.

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
| `contacts list/search` / `contacts show` | `contacts` array / one contact DTO | System name, tags, metadata update timestamp |
| `contacts resolve` | `resolutions` array, one per input | Untruncated names |
| `history coverage` | `coverage` array with local counts/dates/anchor status; opt-in `--evidence` adds independent `recovery_evidence` | Untruncated names; evidence options, anchors and checkpoint measurements |
| `history backfill --chat JID` | Explicit live action: chat, attempt ID, requests/responses, net growth, stop reason and persisted `evidence` | Evidence options, anchors and scoped global sync counter |
| `draft show/create/update/discard` | Frozen local revision, identity, payload hash and state | Untruncated content and document expected snapshot path |
| `draft list` | Stored summary array with page metadata | Same bounded summaries |
| `auth status` | Local authentication observation, public linked JID/phone when known, `session_revoked`, `connected` | Same observation |
| `doctor` (offline) | Auth observation, lock state, FTS availability and heartbeat activity date | Store counts and `last_message_at` |

Other commands, including mutations outside these explicit actions, downloads, `history fill`, and `doctor --connect`, fail before their argument validators or store/network operations with `unsupported_command`. Help, root help without a command, and version retain their normal text semantics. Unknown commands/flags are usage errors. Shell completion is outside the agent contract. `--events` is rejected with `--agent` to keep one error envelope on stderr.

All archive queries use the existing read-only opener, validate the archive schema without migration, and work while the writer lock is held. They never initialize a missing archive. Authentication status reads the public session JID and local revocation observation, without loading credentials or opening a WhatsApp client; an existing directory without a session reports unauthenticated. Offline doctor requires a readable current-schema archive; store failures are error envelopes with exit 4. Inspect session state separately with `auth status --agent` even if `wacli.db` is unreadable, or run legacy `doctor --json` (without `--agent` or `--connect`) to retain its diagnostic report, including archive errors. Neither command connects.

## History recovery

`--agent history backfill --chat JID` explicitly requests recovery using the existing standalone runner or connected sync owner. It does not authorize any other mutation, connection command, or download. The central command capability remains an explicit allowlist: unsupported, local read, local draft write, or history recovery. `--read-only` and `WACLI_READONLY=1` reject this action with `read_only` (exit 2); malformed options and `--events` fail with exit 2 before opening/delegating. Bounds remain count 1–500, up to 100 batches, wait/idle up to five minutes (legacy nonpositive defaults preserved). There is no new executor, replay, journal or automatic retry policy.

`meta.source="live"` describes the action's capability, including parsing, policy and pre-dispatch failures; it does not assert a request reached the primary. Coverage remains `source="local"`. Both freshness and completeness stay `unknown`. Success exposes only the final successful operation observation; partial results on errors are available through retained evidence when persistence succeeded.

Recovery failures may include optional typed `error.history = {attempt_id, phase, outcome, correlation_confirmed}`. Other commands omit it. Phases are `preparing`, `observing`, `dispatch_possible`, `finalizing`; outcomes are `not_dispatched` or `uncertain`. A generated 32-hex attempt ID is carried unchanged through IPC, persistence and replies. It is internal correlation, not an idempotency/replay key or public flag. A missing/mismatched reply or old owner cannot confirm retention: correlation is false, and the outcome is uncertain after possible dispatch. An ID can already have been replaced in the latest slot. Do not parse `recovery` prose to recover correlation.

`no_local_anchor` (exit 3) and `store_state` (exit 4) describe proven pre-dispatch conditions only. Operational refusal is exit 1. Once dispatch is possible, errors/cancellation/lost output prioritize `backfill_outcome_uncertain` (exit 1), with safe phase/correlation and no SQL/path/anchor causes. `not_dispatched` does not mean the archive was untouched: standalone connection or ordinary owner sync may have persisted messages. A delivered success followed by output failure also never proves rollback. Inspect evidence and local coverage before deciding whether another explicit attempt is appropriate.

`history coverage --evidence` retains legacy `data.coverage` and adds top-level `data.recovery_evidence`, independent of chat rows. Explicit `--chat` inputs and their currently verified aliases are consulted even without a chat/anchor or when coverage excludes blocked chats; without `--chat`, only returned coverage scopes are consulted. At most 200 inputs / 400 deduplicated keys are fetched by primary key; no entire evidence catalogue scan. Each requested key has nullable `latest` and `last_success`: null means **no retained record**, not never executed. Two slots per input bound retained attempts for that input; new inputs grow the table. Reads never migrate, repair or write evidence.

Compact evidence contains state, ID, operation times, immutable scope, useful counters/growth/stop reason and callback observation times. Full adds options/anchors/checkpoint counts and the explicitly scoped legacy global counter. `unfinalized` is neither a running nor a crashed assertion. Local raw counts/dates are separate observations, including removals/edits/revocations; absent chat rows do not prove no orphan messages. `primary_no_more_messages` is a response observed during that window, not a completeness certificate for global/future history. There is no network correlation ID: late replies within an open window can be observed, while callbacks after closure are ignored. Identity relation compares account and verified pair facts only; account changes invalidate matching, absent facts remain unknown, and `matching_snapshot` promises no continuity after restoration or mapping changes. No age-based freshness or file/catalog generation is inferred.

```bash
wacli --account personal --agent history coverage --chat 123@s.whatsapp.net --evidence
wacli --account personal --agent history backfill --chat 123@s.whatsapp.net --requests 1
```

## Envelope and identity

Success is a single JSON line on **stdout**. A failure is a single JSON line on **stderr**, with no success payload on stdout. Public fields use snake_case. Success contains `data` and no `error`; failure contains `error` and no `data`.

```json
{"schema_version":1,"success":true,"account":{"store_ref":"/path/to/archive"},"meta":{"source":"local","detail":"compact","completeness":"unknown","freshness":"unknown","limit":20,"excluded":["tombstones"],"page":{"returned":1,"has_more":false,"next_cursor":null},"recovery":"Use --detail full; retrieve one message with messages show --chat CHAT_JID --id ID --detail full."},"data":{"messages":[{"id":"ABC","chat_jid":"123@s.whatsapp.net","sender_jid":"456@lid","from_me":false,"timestamp":"2026-01-01T00:00:00Z","text":"Hello","text_truncated":false,"type":"text"}]}}
```

`account.store_ref` is the absolute, resolved selected store path, once per envelope. `account.name` is included when a named/default account supplied the selection. A name alone never identifies a manual store. This transparent reference does not create a UUID or write local state. When parsing/config resolution cannot establish the selected store, `store_ref` is null; a syntactically selected manual `--store` can still be identified on preflight failures. The reference is a path, not a promise of canonical inode identity through symlinks.

Message IDs and stored chat/sender/quote JIDs are preserved. Chat views keep stored JIDs even when lookup used a verified alternate PN/LID. Contacts retain the existing verified PN/LID view; an unmapped LID has no invented phone number. `contacts resolve` accepts identities, not names: it uses only the persisted local pair map, produces exactly one result per input, and has no limited candidate search or interactive disambiguation. `resolved=true` means a local PN/LID pair is known; it is not proof of remote existence, current freshness, or safe recipient selection. Unknown pairs explicitly return `resolved=false`, preserving the known input identity.

## Bounds and omissions

Agent lists default to **20** rows; explicit `--limit` must be **1–200** in both details. Context retains defaults of five messages before/after; values must be nonnegative and `before + after + 1` must be at most 200. Its metadata includes the requested `before`, `after`, and total `limit`. Resolve accepts at most 200 inputs and thus at most 200 total results; its metadata reports that cap. `contacts list/search`, `chats list`, `messages list` and temporal `messages search --sort time` support local keyset pagination (below). Other lists have no cursors or inferred next pages. Reaching or falling below a limit does not prove complete WhatsApp history.

Compact message `text` is display text, capped at **320 Unicode code points**, preserving valid UTF-8. `text_truncated` explicitly records cutting; there is no appended ellipsis that changes the text. Other selected labels (names, aliases, filenames) have the same cap and report affected fields in `fields_truncated`. Identifiers are never truncated. `meta.recovery` documents full retrieval. Full exposes only selected public DTO fields: it does not serialize internal structs, keys, authenticated media URLs, session paths, raw protobufs or blobs. It does not expose local download paths or button URLs; `downloaded`/`downloaded_at` are the selected download observations. The selected `account.store_ref` is an intentional path exception. The new [draft contract](drafts.md) also deliberately exposes a derived document `snapshot_path` only in full detail, without opening/statting bytes or certifying current integrity.

The total encoded envelope, including newline, is capped at **1 MiB compact / 8 MiB full**, checked before writing any success bytes. `payload_too_large` is a typed exit-1 error with stdout entirely empty and guidance to narrow the query; full also suggests `--detail compact`. Full removes per-text truncation, not row/envelope bounds. These are encoded **output** bounds, not total memory bounds: database rows/DTOs and the JSON envelope are materialized before the size check.

List/search and context neighbors retain legacy exclusion of tombstones, explicitly reported as `meta.excluded=["tombstones"]`. Direct `show` and a context target can return a tombstone with `revoked`, `deleted_for_me`, `deleted_at`, `deletion_reason`, and `payload_purged_at` when present. `edited` remains visible in both details. Full may show retained tombstone content until an explicit purge has erased it. Compact includes essential media type/filename/MIME, quote identity, reaction target/emoji and forwarded state when present; optional full fields are intentionally selected by the documented detail level.

## Contact pagination

`--agent contacts list` and `--agent contacts search QUERY` return the canonical contact view with `meta.page = {returned, has_more, next_cursor}`. Defaults are 20, limits 1–200 in either detail. Identity grouping and matching happen before limiting. An extra matching identity sets `has_more=true`; empty/exact-limit local ends return false/null. Arrays retain their existing compact/full public fields; `show` retains its tag union and `resolve` its explicit unknown results. Legacy search keeps its matching, default 50, explicit limits, tables and JSON fields.

```bash
wacli --store /path/to/archive --agent contacts list
wacli --store /path/to/archive --agent contacts search Alice --limit 2
wacli --store /path/to/archive --agent contacts search Alice --cursor TOKEN --limit 10 --detail full
```

Results use `(Name fallback JID, JID)` in complete, case-sensitive Go string order (UTF-8 byte order), independent of DTO truncation. Identity sources fold in their original display-name/JID-fallback order, with complete stored JID as the deterministic tie-breaker for previously undefined equal source keys. Verified PN rows retain display priority, aliases and system names retain their current precedence, and metadata from either half remains searchable even without a counterpart contact row. Unknown LIDs never become phone numbers; generic textual identities stay generic. Search preserves the original SQLite LOWER/escaped-LIKE matching of stored fields plus the existing Go Unicode case-insensitive matching on canonical IDs/phones and aliases.

Contacts have a separate strict canonical codec, version 1 and **16 KiB** ceiling for the two complete textual keys. Contact list/search operations, message tokens and chat tokens cannot be interchanged. Syntax/version is checked before opening the archive; semantic scope is checked before the matching stream. A stored key too large (or invalid UTF-8) to encode a supported continuation fails with `internal_error` (exit 1), without key echo or success bytes. Invalid caller tokens use `invalid_cursor` (exit 2), without token/JID/SQL echo. Cursors are opaque continuation data, not credentials or tamper-proof authorization.

Scope binds operation, selected resolved store, literal search query, view version, availability of public mapping sources, and the public pair applicable to a JID query. Limit and detail may change. Available local identity data consists only of the session's public `lid/pn` map and own-device `jid/lid` columns. A nullable own-device `lid` means no own pair, allowing fallback to the public map. Missing public tables and old device schemas without a LID mean unknown pairs; incompatible map columns, unreadable/corrupt databases and query failures are errors, never an invented empty mapping. No client, migration, login or key material is loaded. Reads use the normal validated readonly archive opener, no writer lock, and support active WAL writers.

One leased SQLite connection/read transaction covers source inspection, query pair/scope and the stream for a call; it is closed on success, error or cancellation. There is no cross-page snapshot and no promise of universally atomic snapshots across the two files. Normal unrelated session writes do not invalidate scope. A name or another contact's mapping can move/merge identities across the boundary and omit/repeat them. Only applicable query-pair or source-availability changes mismatch scope; **there is no universal mapping-change detection**, filesystem stamp, full mapping/catalog digest or persisted pagination state. Restart to re-observe live changes. Local exhaustion leaves completeness/freshness `unknown`.

Go retains at most `limit+1` candidate contacts and one fixed-field group accumulator, without catalogue/source/alias maps. Retained memory is O(page size × field width + largest current row), while cumulative allocations grow with scanned rows. SQLite scans raw contacts each page, uses existing map/alias indexes, and materializes/sorts a temporary canonical projection which can spill to temporary files. Work includes O(N log N) sorting and mapping/metadata lookups, with own-device projection costs dependent on its cardinality/query plan; roundtrips are constant per call, not per contact. Page metadata does not claim bounded database work or constant native RSS.

## Local chat pagination

Every successful `--agent chats list` includes `meta.page` with `returned`, `has_more` and `next_cursor`, using the same limit+1 semantics as message pages. Defaults are 20 rows, limits 1–200; empty and exact-limit results return `has_more=false`, `next_cursor=null`. Change limit or detail between pages freely:

```bash
wacli --store /path/to/archive --agent chats list --unread --query family --limit 20
wacli --store /path/to/archive --agent chats list --unread --query family --cursor "$cursor" --limit 50 --detail full
```

Agent chat pages read **raw stored rows**: PN and LID identities remain separate, even with a verified mapping. JIDs and unread counts are neither canonicalized nor consolidated in memory. Legacy tables/JSON retain their existing display resolution, filters, ordering and default limit of 50.

Ordering uses `(pin DESC, activity DESC, jid BINARY ASC)`. Pin is 0 for NULL/zero and 1 for any nonzero value. Activity is the stored `last_message_ts`: NULL and zero share key 0, negative integers retain their exact value and sort below zero. The identical normalized expressions are used for ordering and the strict continuation predicate. Public `last_message_at` stays null for nonpositive/unknown activity; the cursor preserves the raw key separately. A textual JID is allowed and compared in SQLite BINARY order. Deleting the anchor does not prevent continuation.

The chat codec has its own `chats list` domain and version, with canonical strict decoding and an exceptional **16 KiB** ceiling to accommodate complete textual JIDs; ordinary tokens are compact, not padded to this ceiling. A stored identity that cannot encode a supported bounded continuation is an internal-data failure (`internal_error`, exit 1), with no JID echo, success payload or invalid next page. Message list/search tokens and chat tokens are not interchangeable. Syntax/version is checked before opening the store; scope before the chat query. Invalid caller tokens never echo tokens, JIDs or SQL and use typed `invalid_cursor` exit 2. Tokens are opaque continuation data, not authorization or tamper-proof credentials.

Scope binds the absolute selected `store_ref`, exact effective query and all four tri-state filters (`archived`, `pinned`, `muted`, `unread`): unset differs from false. Whitespace-only queries disable matching and share the empty-query scope; all other queries preserve whitespace and case. Matching is case-insensitive literal containment on name/JID, escaping percent, underscore and backslash. Limit/detail are excluded from scope. Store references identify paths, not inode/database identities; restart after replacing/restoring the database at that path.

Agent boolean filters treat NULL/zero archived, pinned and unread as false and nonzero as true (current schema requires non-NULL state flags). Muted is true for `muted_until=-1` or a deadline strictly greater than one coherent current Unix-second value per query. NULL/zero, other negative values and deadlines equal to or earlier than now are unmuted. The clock is re-evaluated for each page and is not in the scope: expiration can remove/add matching rows **without any new database write**.

Each page is a live read. Pin, activity, unread/archive/mute or name changes may move or remove rows; moving across the boundary can repeat or omit rows. Sync can fuse or alter stored PN/LID rows between pages; pages reflect those current stored rows without compensating in memory. Once a local end is reported, restart to observe changes. No snapshot or exactly-once guarantee exists, and local exhaustion proves neither WhatsApp completeness nor freshness; both remain `unknown`.

SQL returns at most limit+1 rows to Go, with no OFFSET or complete chat set materialized in Go. With no new schema/index, SQLite may scan and sort candidates on every page; this is not a bound on database work.

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

`--cursor` is accepted only with `--agent chats list`, `--agent messages list` or `--agent messages search QUERY --sort time`; other contexts fail before store/network effects. Flags can precede or follow the command. Message tokens are opaque, versioned, at most **512 bytes**, strictly decoded, and contain no credential or authorization. No server, secret, persistent token state or store UUID is needed. Tokens are not tamper-proof and must not be interpreted as access controls. Malformed, oversized, unsupported-version and mismatched tokens return `invalid_cursor` with exit 2; errors never reproduce the token or SQL.

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

## Local draft capability

Only `draft create/update/discard` gain `local_draft_write`; `draft show/list` use `local_read`. Every draft envelope, including policy/parsing failures, remains v1 `source=local`. Other mutation capabilities remain blocked. See [local drafts](drafts.md) for payload/field/file limits, compact review, identity freezing, expected snapshot paths, live pagination and growing retention.

Draft policy/usage/cursor errors use exit 2, not-found exit 3, store/identity/document failures exit 4, and CAS/uncertainty exit 1. Optional `error.draft={draft_id,revision_id,hash}` is confined to this operation. `local_write_uncertain` includes output failure after commit or unconfirmed IPC correlation; check exact IDs without automatic replay. Hashes never authorize sending. Show/list require no current session and never inspect document bytes.
