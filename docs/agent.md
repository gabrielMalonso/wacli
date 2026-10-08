# Agent output contract

Read when: integrating a coding agent with bounded archive queries, explicit history recovery, local drafts, durable outbound dispatch, explicit read/unread/archive actions, exact media recovery, explicit local transcription, and stable errors.

`--agent` enables JSON contract **v1**. `--detail compact|full` chooses its public detail level; compact is the default. Flags work before or after the subcommand. `--agent --json` still returns v1. Existing `--json` envelopes, field names, list defaults, tables, and `--full` table behavior remain unchanged. `--detail` without `--agent` is an error; `--full` does not select full agent detail.

Discover this binary's capabilities first, then use legacy account discovery and bind the exact requested/listed name explicitly. The name below is illustrative, not a default; automated callers bind it from validated task input and the response without an interactive prompt:

```bash
wacli --read-only capabilities --agent || exit "$?"
wacli --read-only accounts list --json || exit "$?"
wacli_account='example-account' # Replace with the reviewed name from data.accounts.
wacli --account "$wacli_account" --read-only --agent auth status
wacli --account "$wacli_account" --read-only --agent messages list --limit 20
wacli --account "$wacli_account" --read-only --agent messages search "invoice" --sort time --limit 10
wacli --account "$wacli_account" --read-only --agent doctor
```

Accounts use legacy JSON; help/version remain text. For a manual archive use `--store DIR` instead of `--account`, never both. The [quickstart](quickstart.md#4-search-and-read) shows selection of returned chat/message IDs and full recovery. For an authorized write, read [drafts](drafts.md) before [outbound](outbound.md); use [media](media.md) only for the file task at hand. Local exhaustion never certifies remote freshness/completeness, and an uncertain effect calls for inspection rather than automatic replay. Readonly rejects intentional archive/WhatsApp mutations; requested output, network download, explicit adapter and WAL bookkeeping exceptions remain, as described in [integrations](integrations.md#optional-mode-only-wrapper-example).

## Static capability discovery

```bash
wacli capabilities --agent
wacli capabilities --json
wacli capabilities
```

Discovery reads the compiled command tree and the same support classification used by agent preflight. It does not read the account registry, select an archive, open SQLite/session/LOCK/IPC, connect, migrate or inspect credentials. Missing, old, corrupt or locked stores and malformed default account configuration do not affect it. Executing discovery rejects explicit `--account`, `--store` and `-a`/`--for-account`; run account discovery separately. Textual help/version do not execute discovery: `capabilities --help` and `help capabilities` may accept and ignore legacy `--account`/`--store` selectors without resolving selection or accessing config/store. Strict `-a`/`--for-account` refusal still applies to discovery help. Environment/default selection is ignored. In agent mode the envelope has `account={"store_ref":null}`, `meta.source="local"`, and unknown freshness/completeness. Compact/full return the same catalog; ordinary `--json` uses the legacy envelope and no flag prints a command table. `--agent --events` remains an error.

`data` contains:

- `cli_version`: this binary's effective version, also reported by `version`.
- `account_availability="unknown"`: no selected account was inspected. Static support does not certify authentication, recipient support, network/media availability, a compatible owner, permission to act, or successful execution.
- `agent_contract`: envelope `schema_version`, `versioning="additive_within_version"`, single-line JSON encoding, stdout success/stderr error streams, supported detail levels, maximum bounded query results (200), compact/full encoded envelope caps (1/8 MiB), and the usual exit categories (0 success, 1 operational, 2 usage/policy, 3 missing local record, 4 store). Action-specific codes remain documented on their pages.
- `commands`: sorted canonical runnable command paths from this binary. Containers and hidden completion-protocol commands are omitted. `agent_mode` is `supported`, `unsupported`, or `text` for help/version. Unsupported means refused under `--agent`, even if the legacy command can run; it does not describe that command's legacy effects or authorize fallback. Help flags on all commands and root help remain text.

Supported entries add a `capability` category, the same `source` as their agent envelopes, whether `read_only` policy permits the command, and static `requirements`/important `constraints` where applicable. `requirements` name an existing compatible archive or writable archive, writer LOCK or compatible owner, exact media reference/output, readable local auth state, or an explicitly supplied input/adapter. They are prerequisites, not observations that they are satisfied. `constraints` summarize mode restrictions; command help and the command-specific documentation remain authoritative for flags, argument validation, defaults and narrower limits. No attempt is made to serialize every validator as a schema.

For example, `doctor` supports agent mode only offline; `media retry` requires a standalone writer LOCK and never delegates; `outbound send` requires an exact draft/revision/hash/key and supports text/contact/document/image/voice, while `send text` remains unsupported. A live capability may finish from a local retained duplicate or cached bytes without connecting. `read_only=true` for media download/transcription permits explicit output/adapter effects and is not a sandbox or a claim of no network. Historical observations, `synced`, local rows, cursor exhaustion and `source` never certify current liveness or remote completeness.

The CLI release version, agent envelope version, SQLite migration version, diagnostic observation version and IPC/payload versions are independent. Within agent v1, fields and command/capability entries may be added without removing or changing existing meanings; incompatible envelope semantics require a new contract version. Consumers should tolerate additional object fields and catalog entries, check the contract version and exact requested command's mode, and refuse unknown modes/capabilities they cannot handle. Unknown `error.code` or uncertain effects must never cause an automatic retry or a switch to legacy mutation. Discovery is not an approval record; retained outbound idempotency and operation-specific inspection rules remain unchanged.

## Supported queries and data

| Command | `data` | Full additions |
| --- | --- | --- |
| `capabilities` (unbound) | Static command/capability catalog and agent contract limits; account availability unknown | Same catalog |
| `messages list/search` | `messages` array; search also reports `search_mode` (`fts5` or `like`) and effective `order` (`relevance`, `time_desc`, `time_asc`) | Each message's `full` content, caption, names, forwarding/star/download metadata, selected buttons |
| `messages show` | One message DTO | Same message additions |
| `changes list` | `changes` reference array and `introduced_at`; every page has `meta.page.next_cursor`, including empty/final pages | Same reference DTO |
| `messages context` | `messages` array and `selected_id` | Same message additions |
| `media status --chat JID --id ID` | One exact media reference, local file observations and dated retained unavailability; opt-in `--verify` and optional output observation | Untruncated filename |
| `media download --chat JID --id ID --output PATH` | Verified explicit output, `cached` / `existing` / `downloaded`, bytes/hash/checks and `recorded=false` | Untruncated filename |
| `media retry --chat JID --id ID --output PATH` | Explicit live recovery of one exact reference, dated phone/CDN observation, independent file publication and archive persistence | Untruncated filename |
| `media transcribe --file PATH --adapter /absolute/executable` | Explicit local adapter processing: text, nullable language, completion/empty status, exact observed input path/bytes/SHA-256 | Bounded untruncated transcript |
| `chats list` / `chats show` | `chats` array / one chat DTO | Archived, pinned, stored mute deadline |
| `chats mark-read/mark-unread/archive/unarchive --chat PHONE_OR_JID` | Explicit live action: requested scope, observed public account/target, SDK outcome and local mirror | Same result |
| `contacts list/search` / `contacts show` | `contacts` array / one contact DTO | System name, tags, metadata update timestamp |
| `contacts resolve` | `resolutions` array, one per input | Untruncated names |
| `history coverage` | `coverage` array with local counts/dates/anchor status; opt-in `--evidence` adds independent `recovery_evidence` | Untruncated names; evidence options, anchors and checkpoint measurements |
| `history backfill --chat JID` | Explicit live action: chat, attempt ID, requests/responses, net growth, stop reason and persisted `evidence` | Evidence options, anchors and scoped global sync counter |
| `draft show/create/update/discard` | Frozen local revision, identity, payload hash and state | Untruncated content and document/image expected snapshot path |
| `draft list` | Stored summary array with page metadata | Same bounded summaries |
| `draft cleanup preview D` | Catalogue revision eligibility, full hashes, page-only counts and navigation | Derived expected document snapshot paths, without stat |
| `draft cleanup apply D --revision R --if-revision HEAD --expect-hash HASH` | Explicit local byte removal effect, outcome, directory sync and logical removed bytes | Same result |
| `outbound send D --revision R --expect-hash H --key K` | Explicit live action: operation, duplicate, persistence, known result/ACK, history warning and SDK retry policy | Nullable checkpoint timestamps |
| `outbound show/list` | Operation with observation page / operations array; frozen binding, phase/result and derived evidence | Nullable retained checkpoint timestamps |
| `sync status` | Live existing follow owner readiness via scoped IPC; no archive/session open or connection | Same bounded snapshot |
| `auth status` | Local authentication observation, public linked JID/phone when known, `session_revoked`, `connected` | Same observation |
| `doctor` (offline) | Auth observation, lock state, FTS availability, heartbeat activity date and `app_state` replay debt | Store counts and `last_message_at` |

Other commands, including mutations outside these explicit actions, media backfill, `history fill`, and `doctor --connect`, fail before their argument validators or store/network operations with `unsupported_command`. Help, root help without a command, and version retain their normal text semantics. Unknown commands/flags are usage errors. Shell completion is outside the agent contract. `--events` is rejected with `--agent` to keep one error envelope on stderr.

All archive queries use the existing read-only opener, validate the archive schema without migration, and work while the writer lock is held. They never initialize a missing archive. Authentication status reads the public session JID and local revocation observation, without loading credentials or opening a WhatsApp client; an existing directory without a session reports unauthenticated. Offline doctor requires a readable current-schema archive; archive or auth-source failures are sanitized error envelopes with exit 4 and no auth data. Inspect session state separately with `auth status --agent` even if `wacli.db` is unreadable, or run legacy `doctor --json` (without `--agent` or `--connect`) to retain its diagnostic report and exit 0, including archive/auth-source errors in `store_error`. Legacy `authenticated=false` accompanied by an auth-source error is not a confirmed unauthenticated observation; known auth/JID data survives an archive-only failure. Neither command connects.

Offline doctor's `app_state` is identical in compact/full: `reconciliation=required|none_recorded|unknown`, sorted/distinct `pending_collections` (`[]` only for a successful empty query, `null` when unavailable), and optional sanitized `error.code="recovery_state_unavailable"`. A recovery-query-only failure is embedded without changing exits; existing archive/schema/auth-source failures still exit 4. This debt read uses only `wacli.db`, without a session, writer lock or migration; existing auth inspection is separate. Legacy `app_state.recovery_observations=null` describes the absence of invocation-local outcomes in offline doctor; retained historical outcomes are separate in `observations.sync.recovery_observations` when available. Debt also arises preventively at normal shutdown; neither debt nor its absence proves recovery failure, integrity, completeness or freshness. See [sync's per-invocation observations](sync.md#app-state-summary) to inspect a known failed/cancelled/unconfirmed collection, phase and code. Sync remains unsupported with `--agent`.

## Media observations and explicit output

`media status` is an offline read capability. `media download` has its own capability with `meta.source="live"`, including parsing and policy errors and cache-only results; this identifies its allowed network/output behavior, not a claim that HTTP occurred. Both always open the archive read-only, without a session, writer LOCK, IPC, sync or migration, including when `--read-only` is absent. Download requires explicit `--output` and permits that file write with `--read-only` or `WACLI_READONLY=1`; `recorded=false` means no local-path/download-date update was requested.

Selection is the exact stored chat JID plus message ID. Names, bare phones, alias expansion and tombstones cannot select a download. Status may also observe `--output PATH` without creating it. See [media](media.md#agent-status-and-download) for roots, file checks, memory costs and error details. `messages show --detail full` retains legacy `full.downloaded`: it means a local path was recorded, not that the file exists or matches the message. Status without `--verify` reports existence only. Absence of a local path or file does not prove remote loss.

`data` includes `local`, optional `output`, `declared_bytes` (null when unknown), `binding`, `download_metadata`, and `remote={current:"unknown",observation:null|{state:"unavailable",observed_at,source:"retained_phone_and_cdn"}}`. A dated retained unavailable observation can coexist with verified local bytes and is not current remote availability. Metadata being present is also no availability proof. Status is `cached` for verified archive cache, `existing` for an observed output file or an unverified existing cache, or `unknown` otherwise. Download uses `cached` for bytes copied from verified cache, `existing` for verified bytes already at the destination, and `downloaded` only for this call's direct HTTP retrieval. Copying a cache never becomes `downloaded`.

Local/output observations contain state, nullable path/bytes, verification, checks, and an observed plaintext digest/verification date only when measured. `verified` requires plaintext SHA-256; declared size is checked only when positive. Hash presence is separate from the network key/path requirements. Stat alone never verifies bytes. Paths, metadata and files may change independently; no atomic snapshot or future stability is promised.

Usage exits 2, missing item/type/binding/network metadata or tombstone exits 3, unreadable archive exits 4. Operational errors exit 1: `media_expired`, `integrity_failed`, `media_too_large`, `output_conflict`, `path_not_allowed`, `media_changed`, `cancelled`, `download_failed`, or `output_failed`. Causes and parsing values are sanitized. Optional `error.media` retains selection, unknown status, `file_publication=not_written|written|unknown`, `recorded=false`, and measured output knowledge when applicable. Broken stdout after publication is an error, not rollback or permission to replay. CDN 403/404/410 alone yields `media_expired` without marking unavailable. Media backfill remains unsupported with agent output. Exact retry is the separate writable capability below; legacy retry without exact flags remains bulk.

## Explicit local transcription

```bash
wacli media transcribe --file ./audio.ogg --adapter /absolute/executable --agent
wacli --read-only media transcribe --file ./audio --adapter /absolute/executable --mime-type audio/ogg --expect-sha256 LOWERCASE_SHA256 --agent --detail full
```

Only this explicit executable-processing command gains `agentMediaTranscription`; `meta.source="local"` applies to success, parse and all other errors. It describes local input processing, **not an executable sandbox or guarantee of no network/external writes**. There is no provider default, fallback, discovery, installation, automatic download transcription, transcript persistence, journal, IPC, recovery or replay. The command needs no archive/session/LOCK/WA; selected account/store supplies envelope identity, with account registry resolution only. Readonly flag/env prevents no authorized adapter effects and means only that wacli does not write the archive.

`data={text,language,status,input:{path,bytes,sha256},text_truncated}` uses a nullable language and `completed|empty` status. Empty/whitespace adapter text is legitimate `empty`, not proof of recognized speech or silence. `completed` describes protocol completion, not transcription accuracy. Input path is the caller's explicitly requested path; bytes/hash describe the exact bounded snapshot sent on stdin, without promising source-file immutability. No binary/base64, executable path, raw streams or private causes are exposed.

Input is regular, confined through PR17 roots/FD/control-file checks, opened once and capped at 25 MiB during reading. Optional SHA-256 is canonical lowercase and mismatch prevents adapter execution. MIME is explicit or detected locally from those bytes, never filename; unknown bytes require `--mime-type`. Execution has fixed `--protocol wacli-transcribe-v1 --mime-type MIME` argv and exact stdin bytes. Strict single-object JSON v1 requires string text, allows optional string language, refuses unknown/duplicate fields, invalid types/nulls/raw UTF-8/trailing data, and caps stdout at 256 KiB, text at 128 KiB, language at 64 bytes. Stderr is privately bounded/drained. Timeout is positive, at most/default 300s; context cancellation plus 250ms WaitDelay bounds inherited pipe waits without promising arbitrary descendant termination. See [media transcription](media.md#explicit-local-transcription) for the adapter contract, memory and filesystem limits.

Compact text is capped at 320 Unicode code points and full preserves bounded text, using existing 1/8 MiB envelope caps. Short and empty transcripts have no recovery hint. Only `text_truncated=true` adds a warning that the transcript was not stored: obtaining full text would require **another explicit adapter execution** with `--detail full`, never automatic repetition or retrieval of the earlier result. Usage and `adapter_not_configured` exit 2; absent input (`input_not_found`) exits 3; selection/config failures remain exit 4. Path/input/hash/size/process/timeout/cancel/output failures exit 1 through fixed sanitized messages, including parsing values. Broken stdout fails without saving the result and never implies rollback of external program effects. Validation covers synthetic offline protocol stubs only, not real STT or remote/private audio.

## Exact media recovery

```bash
wacli --agent media retry --chat 123@s.whatsapp.net --id ABC123 --output ./downloads/photo.jpg --wait 30s --timeout 2m
```

Only exact `media retry` gains this standalone writable capability. `meta.source="live"` applies to success, cache/destination reuse, usage/readonly failures and operational errors; it identifies capability, not proof of a phone request. Chat JID, message ID and output are mandatory, and explicitly supplied bulk `--before/--limit/--batch` are rejected. Global timeout is 1s–5m (default 5m), wait per attempt 1s–120s (default 30s). Readonly flag/env and malformed options fail before opening a store, LOCK, session, file or socket. Missing/old/incompatible archives and absent/tombstoned/invalid selections are preflighted without initialization or migration.

The existing standalone writer LOCK is required. An active owner yields `store_locked` before WA, with no IPC, automatic stop/delegation/fallback or new executor. Verified cache/destination bytes can conclude without `OpenWA`, a key/path or a connection. Network work opens/checks/connects the existing client once without QR or historical LID migration, preserving App handshake/revocation observations. Selection and binding are rechecked after connection and at effect boundaries. Exact selection ignores stale local-path and retained unavailable markers; it never chooses a pending row using a limit.

`data` retains the media reference/observations and adds `retry={phone,cdn,observed_at}`, independent `file_publication`, `recorded` and optional `recorded_at`. Statuses are `downloaded`, `cached`, `existing`, `no_response`, `unavailable`, or `unknown`. Phone values include `not_requested`, `unknown`, `no_response`, `not_found`, and `reuploaded`; CDN values are `not_requested`, `unknown`, `expired`, or `downloaded`. `remote.current` always remains unknown. Phone not-found plus stored-CDN 403/404/410 can persist unavailable; CDN expiry alone, silence, timeout, 5xx or integrity failure cannot. At most two application receipts use the existing protocol, and only non-responders get the second. A late matching notification inside an open window is not a new application correlation/freshness guarantee.

The PR17 roots/FD/digest/no-replace publisher verifies the same bytes it publishes. Archive markers/dates are asserted only after successful persistence. Optional `error.media` preserves status, `file_publication`, `recorded`, `recorded_at`, `current_availability="unknown"`, retained `unavailable_at`, dated `retry` and measured output knowledge. A file can be written without a confirmed DB update; a persisted result survives stdout failure/cancellation. Errors sanitize parsing values, URLs, direct paths, keys, ciphertext hashes, SQL and raw causes. Usage/readonly exits 2; missing media/binding metadata or tombstones exits 3; store/session/authentication failures and LOCK refusal (`store_locked`) exit 4; cancellation, changed binding, retry/download/integrity/publication/output errors exit 1. No uncertain result licenses automatic replay. See [exact agent retry](media.md#exact-agent-retry) for behavior, file/DB/session atomicity limits, buffering and fixture-only validation.

## Explicit read/unread/archive actions

```bash
wacli --agent --account personal chats mark-read --chat +15550000001
wacli --agent --account personal chats mark-unread --chat +15550000001
wacli --agent --store /path/to/archive chats archive --chat 300@lid
wacli --agent --account personal chats unarchive --chat 123456-789@g.us
```

Only `mark-read`, `mark-unread`, `archive` and `unarchive` gain this narrow capability. Supply an explicit phone number or DM/group JID; names and `--pick` (including zero) are rejected. Agent `mark-read` uses app-state only: any `--receipts` flag, including `--receipts=false`, is rejected before effects. Its global timeout must be positive and at most five minutes (default five minutes); other actions retain their existing timeout policy. Receipts, pin and mute remain unsupported with `--agent`. Read-only flag/env and malformed options fail before opening, connecting or submitting IPC. `meta.source="live"` also applies to parsing, policy and other errors; freshness/completeness remain unknown.

The selected store is resolved once. Standalone execution uses its writable LOCK, authenticated client and existing one-shot persistence handler; it never pairs. Mark-read checks authentication without migrating historical LID rows; other actions retain their existing authentication path. A same-store connected owner receives the distinct typed `agent_chat_state` kind through the existing socket, outside the send slot and pacer. These actions serialize through the existing app-state semaphore and recovery, sharing the caller deadline. Independent outbound work can proceed while chat state waits for recovery; no task reserves another thread's work.

After connection and pre-write recovery, a strict readonly public account/map transaction supplies `observation.account_identity` and `observation.target`. The connected client's own PN and any observed own LID must match. Device facts must be valid; target mapping queries and their reverses must succeed consistently. Missing target aliases are allowed: PN-only, LID-only and groups retain their exact identity. No discovery or best-effort resolver selects this target. A known pair provides the canonical PN for the frozen local mirror; the SDK builder still receives the **exact normalized requested JID**, including a requested LID. Builders index that JID directly and do not need outbound's PN-to-LID translation. Account/session/map files and client state are shared and not atomically bound; this observation does not prevent a later external replacement or prove current remote identity/state.

Success `data` contains `request={version,store_ref,requested,action}`, the public `observation`, `outcome="sdk_completed"` and `local_mirror="persisted"`. This means the SDK call returned nil and the command's local mirror persisted; it does not confirm the remote state still has that value. `mark-read` requires one valid stored anchor in the exact frozen mirror chat, with a positive timestamp and valid message key. It captures once and uses the same timestamp/key for the SDK patch and local unread boundary, preserving later arrivals, including later insertions in the same second. No anchor, a failed SELECT or an invalid anchor refuses with `not_dispatched/unknown` (exit 1) before the mutation and local clear; this also applies to empty/already-read chats. Alias-only history is not searched or migrated to invent a mirror anchor, and the current clock is never substituted. Connection/recovery may have persisted state before refusal. This is a WhatsApp chat-state action, not a local-only acknowledgement or per-message receipt. `mark-unread` sets the marker without inventing message counts; archive also unpins, and unarchive does not restore the pin. Local list/show/context reads never mark a chat read. Read/unread/archive are WhatsApp state, not agent processing status or proof of human reading.

Errors carry concrete `error.chat_state={requested,action,outcome,local_mirror}` plus known public `own_pn/own_lid/target_jid/target_pn/target_lid`. Outcomes are `not_dispatched` before invoking the mutating WA method, `uncertain` after that invocation without nil SDK completion, or `sdk_completed` if nil was returned. Mirror knowledge is `unknown`, `persisted` or `unconfirmed`; SDK nil plus persistence failure retains `sdk_completed/unconfirmed`, and stdout failure retains the known result. Pre-write recovery can persist state even for `not_dispatched`. `beforeApply` reserves persistence before `SendAppState` preparation and proves no network frame. The SDK retains its internal conflict retry; the application adds no repeated mutation after uncertainty.

Usage/read-only failures exit 2; unavailable store/identity exits 4; operational, uncertain, mirror or output failures exit 1. No absence of a chat row proves remote not-found. Causes are sanitized; `invalid_arguments` uses a fixed public message even for flag-parsing failures, retaining the internal cause and public invocation correlation. Request/response version, action, store, identity structure, capability and a bounded 16 KiB newline frame are checked per connection; no persisted ID, journal or schema is added. An absent socket before submission leaves the LOCK/unavailable result. Old/untyped owners, EOF, transport loss or mismatched replies after submission yield `uncertain`, without fallback or interpreting raw error text as proof of no effects. Restart an older owner explicitly; do not automatically replay an uncertain action.

Existing recovery debt, late persistence and handler draining before DB/LOCK release are retained. App-state snapshot recovery now retains debt and returns completion unconfirmed: the SDK may apply the snapshot, but its public completion event has no request ID. A correlated primary-device response and server ACK do not authorize the dependent mutation; collection/version/proximity cannot establish attribution. This can leave the chat-state action `not_dispatched` even after a snapshot was applied. Observed SDK state events still persist; an existing full fetch must reconcile before the action proceeds. No additional fetch retry or repeated mutation is added, and this does not establish or resolve a remote LTHash cause. Timeout is not rollback, a definitive failed remote change, or a delivery result. Inspect local `chats show --agent --detail full` and account status, then make an explicit decision using the available observations. Frozen text replies, explicit vCards, document snapshots and outbound receipt observations already use drafts/outbound; no additional layer is introduced here. Live WhatsApp interoperability remains opt-in and was not exercised by fixture validation.

## History recovery

`--agent history backfill --chat JID` explicitly requests recovery using the existing standalone runner or connected sync owner. It does not authorize any other mutation, connection command, or download. The central command capability remains an explicit allowlist: unsupported, local read, media observation/download, explicit local adapter transcription, standalone exact media recovery, local draft write, history recovery, outbound dispatch, or the four explicit read/unread/archive actions. `--read-only` and `WACLI_READONLY=1` reject this action with `read_only` (exit 2); malformed options and `--events` fail with exit 2 before opening/delegating. Bounds remain count 1–500, up to 100 batches, wait/idle up to five minutes (legacy nonpositive defaults preserved). There is no new executor, replay, journal or automatic retry policy.

`meta.source="live"` describes the action's capability, including parsing, policy and pre-dispatch failures; it does not assert a request reached the primary. Coverage remains `source="local"`. Both freshness and completeness stay `unknown`. Success exposes only the final successful operation observation; partial results on errors are available through retained evidence when persistence succeeded.

Recovery failures may include optional typed `error.history = {attempt_id, phase, outcome, correlation_confirmed}`. Other commands omit it. Phases are `preparing`, `observing`, `dispatch_possible`, `finalizing`; outcomes are `not_dispatched` or `uncertain`. A generated 32-hex attempt ID is carried unchanged through IPC, persistence and replies. It is internal correlation, not an idempotency/replay key or public flag. A missing/mismatched reply or old owner cannot confirm retention: correlation is false, and the outcome is uncertain after possible dispatch. An ID can already have been replaced in the latest slot. Do not parse `recovery` prose to recover correlation.

`no_local_anchor` (exit 3) and `store_state` (exit 4) describe proven pre-dispatch conditions only. Operational refusal is exit 1. Once dispatch is possible, errors/cancellation/lost output prioritize `backfill_outcome_uncertain` (exit 1), with safe phase/correlation and no SQL/path/anchor causes. `not_dispatched` does not mean the archive was untouched: standalone connection or ordinary owner sync may have persisted messages. A delivered success followed by output failure also never proves rollback. A broken stdout pipe after an agent action reports that uncertainty with the attempt ID and confirmed correlation; read queries and legacy JSON retain their closed-pipe behavior. Inspect evidence and local coverage before deciding whether another explicit attempt is appropriate.

`history coverage --evidence` retains legacy `data.coverage` and adds top-level `data.recovery_evidence`, independent of chat rows. Explicit `--chat` inputs and their currently verified aliases are consulted even without a chat/anchor or when coverage excludes blocked chats; without `--chat`, only returned coverage scopes are consulted. At most 200 inputs / 400 deduplicated keys are fetched by primary key; no entire evidence catalogue scan. Each requested key has nullable `latest` and `last_success`: null means **no retained record**, not never executed. Two slots per input bound retained attempts for that input; new inputs grow the table. Reads never migrate, repair or write evidence.

Compact evidence contains state, ID, operation times, immutable scope, useful counters/growth/stop reason and callback observation times. Full adds options/anchors/checkpoint counts and the explicitly scoped legacy global counter. `unfinalized` is neither a running nor a crashed assertion. Local raw counts/dates are separate observations, including removals/edits/revocations; absent chat rows do not prove no orphan messages. `primary_no_more_messages` is a response observed during that window, not a completeness certificate for global/future history. There is no network correlation ID: late replies within an open window can be observed, while callbacks after closure are ignored. Identity relation compares account and verified pair facts only; account changes invalidate matching, absent facts remain unknown, and `matching_snapshot` promises no continuity after restoration or mapping changes. No age-based freshness or file/catalog generation is inferred.

```bash
wacli --account personal --agent history coverage --chat 123@s.whatsapp.net --evidence
wacli --account personal --agent history backfill --chat 123@s.whatsapp.net --requests 1
```

## Envelope and identity

Success is a single JSON line on **stdout**. A failure is a single JSON line on **stderr**, with no success payload on stdout. Public fields use snake_case. Success contains `data` and no `error`; failure contains `error` and no `data`.

```json
{"schema_version":1,"success":true,"account":{"store_ref":"/path/to/archive"},"meta":{"source":"local","detail":"compact","completeness":"unknown","freshness":"unknown","limit":20,"excluded":["tombstones"],"page":{"returned":1,"has_more":false,"next_cursor":null}},"data":{"messages":[{"id":"ABC","chat_jid":"123@s.whatsapp.net","sender_jid":"456@lid","from_me":false,"timestamp":"2026-01-01T00:00:00Z","text":"Hello","text_truncated":false,"type":"text"}]}}
```

`account.store_ref` is the absolute, resolved selected store path, once per envelope. `account.name` is included when a named/default account supplied the selection. A name alone never identifies a manual store. This transparent reference does not create a UUID or write local state. When parsing/config resolution cannot establish the selected store, `store_ref` is null; a syntactically selected manual `--store` can still be identified on preflight failures. The reference is a path, not a promise of canonical inode identity through symlinks.

Message IDs and stored chat/sender/quote JIDs are preserved. Chat views keep stored JIDs even when lookup used a verified alternate PN/LID. Contacts retain the existing verified PN/LID view; an unmapped LID has no invented phone number. `contacts resolve` accepts identities, not names: it uses only the persisted local pair map, produces exactly one result per input, and has no limited candidate search or interactive disambiguation. `resolved=true` means a local PN/LID pair is known; it is not proof of remote existence, current freshness, or safe recipient selection. Unknown pairs explicitly return `resolved=false`, preserving the known input identity.

## Bounds and omissions

Agent lists default to **20** rows; explicit `--limit` must be **1–200** in both details. Context retains defaults of five messages before/after; values must be nonnegative and `before + after + 1` must be at most 200. Its metadata includes the requested `before`, `after`, and total `limit`. Resolve accepts at most 200 inputs and thus at most 200 total results; its metadata reports that cap. `contacts list/search`, `chats list`, `messages list` and temporal `messages search --sort time` support local keyset pagination (below). Other lists have no cursors or inferred next pages. Reaching or falling below a limit does not prove complete WhatsApp history.

Compact message `text` is display text, capped at **320 Unicode code points**, preserving valid UTF-8. `text_truncated` explicitly records cutting; there is no appended ellipsis that changes the text. Other selected labels (names, aliases, filenames) have the same cap and report affected fields in `fields_truncated`. Identifiers are never truncated. Only actual truncation adds a query hint in `meta.recovery`, using the matching domain: `messages show`, `chats show`, `contacts show`, `contacts resolve`, or `history coverage`, with full detail in the selected archive. Short/empty queries, auth status and doctor have no generic message-retrieval hint. Independent warnings about identity, uncertainty, retained evidence or document inspection remain applicable. Absence of a truncation hint does not mean full-only fields were included. Full exposes only selected public DTO fields: it does not serialize internal structs, cryptographic keys, authenticated media URLs, session paths, raw protobufs or blobs. It does not expose local download paths or button URLs; `downloaded`/`downloaded_at` are the selected download observations. The selected `account.store_ref` is an intentional path exception. The new [draft contract](drafts.md) also deliberately exposes a derived document `snapshot_path` only in full detail, without opening/statting bytes or certifying current integrity.

The total encoded envelope, including newline, is capped at **1 MiB compact / 8 MiB full**, checked before writing any success bytes. `payload_too_large` is a typed exit-1 error with stdout entirely empty and guidance to narrow the query; full also suggests `--detail compact`. Full removes per-text truncation, not row/envelope bounds. These are encoded **output** bounds, not total memory bounds: database rows/DTOs and the JSON envelope are materialized before the size check. `json.Marshal` encodes the entire envelope before rejection, so even an oversized result that writes no bytes can allocate more than its output cap. Neither these caps nor page limits promise bounded process memory.

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

For message pagination, `--cursor` requires `--agent messages list` or `--agent messages search QUERY --sort time`. Other supported domains have their own scopes and bounds: [contacts](#contact-pagination), [chats](#local-chat-pagination), [draft list/cleanup preview](drafts.md) and [outbound list/show](outbound.md). Unsupported cursor contexts fail before store/network effects. Flags can precede or follow the command. Message tokens are opaque, versioned, at most **512 bytes**, strictly decoded, and contain no credential or authorization. No server, secret, persistent token state or store UUID is needed. Tokens are not tamper-proof and must not be interpreted as access controls. Malformed, oversized, unsupported-version and mismatched tokens return `invalid_cursor` with exit 2; errors never reproduce the token or SQL.

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

Query normalization follows the matching engine: FTS quotes each whitespace-delimited token, preserving case, token order and punctuation; equivalent whitespace spellings share a scope. LIKE preserves the exact query string, including whitespace, and escapes literal percent, underscore and backslash. No case folding or token rearrangement is imposed on the cursor. Type filters normalize case/whitespace. Agent search normalizes `--from` JIDs in both relevance and temporal order like list's sender filter, without expanding sender aliases. Temporal search also binds the normalized chat identity. FTS/LIKE matching fields, sanitization and FTS snippets are shared with existing search.

SQLite returns at most limit+1 matching rows to Go; no complete match set or offset is materialized in Go. This does not bound SQLite's work: LIKE can scan candidates and FTS temporal ordering can inspect/sort matching rows on each page, depending on query/filter selectivity. No new index or persistent pagination state is introduced.

These are live local search pages with the same limits described above, without snapshot or exactly-once guarantees. Edits can change matching without changing timestamps; ingestion can move timestamps across the boundary. Chat/sender names also participate in matching, and PN/LID mapping changes invalidate the scope. Messages can be omitted or repeated across changing reads. A local end does not imply complete remote history; restart without a cursor after relevant changes.

## Evidence and errors

For local queries, `meta.source` is `local`; `completeness` and `freshness` are **unknown** in v1. Local message bounds, row counts and anchor status describe only the archive. Coverage `ready` means a local anchor exists, not complete history. Missing timestamps are null. `last_message_at` is a message date, never a synchronization date. `last_activity_at` is the heartbeat date (possibly stale); a lock or heartbeat does not prove connectivity. Offline `connected` is always **unknown**.

An empty local search after successful sync/offline replay is still only a local observation. `history backfill` paginates before its oldest local anchor and does not automatically target a newer offline gap. Keep coverage unknown, retain authorized sync lifecycle evidence, and never resend an uncertain operation to fill a missing archive row; see [missing offline messages](sync.md#missing-messages-after-an-offline-interval).

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

`outbound show/list` are pure `local_read` queries over schema 31. They open no session/media/socket, acquire no LOCK and never initialize or migrate. Both support scoped agent cursors; show paginates observations while deriving status from all retained facts in the read snapshot. Full adds nullable checkpoint timestamps. Persisted corruption uses sanitized `store_error`, exit 4. `outbound send` is the separate explicit live capability; no recover/retry/resume action exists. See [outbound operations](outbound.md) for state/certainty, participant scope, idempotency/restoration limits and dispatch and protocol retry boundaries.

`draft create/update/discard` and explicit `draft cleanup apply` use `local_draft_write`; `draft show/list` and `draft cleanup preview` use `local_read`. Every draft envelope, including policy/parsing failures, remains v1 `source=local`. Other mutation capabilities remain blocked. See [local drafts](drafts.md) for payload/field/file limits, compact review, identity freezing, expected snapshot paths, live pagination and growing retention.

For a single draft revision, complete guidance is canonical at `data.recovery`; `meta.recovery` is `See data.recovery.` when guidance exists. Both fields remain public. Untruncated text/contact revisions omit both; document revisions retain creation-time verification, separate byte-inspection and no-approval warnings. The 512-code-point compact cutoff remains unchanged, and compact can encode more bytes than full near that cutoff. Draft list still provides only stored summaries and keeps its navigation guidance in `meta.recovery`.

Draft policy/usage/cursor errors use exit 2, not-found exit 3, store/identity/document failures exit 4, and CAS/uncertainty exit 1. Optional `error.draft={draft_id,revision_id,hash}` is confined to this operation. `local_write_uncertain` includes output failure after commit or unconfirmed IPC correlation; check exact IDs without automatic replay. Hashes never authorize sending. Show/list require no current session and never inspect document bytes.

Typed `reply.sender` validation failures retain `invalid_arguments` (exit 2) with fixed, sanitized guidance to inspect the local quoted message and select a compatible quote or explicitly recreate without `--reply-to`; identity validation is unchanged. Correlated owner refusals may carry the private optional `draft_validation_field="reply.sender"` category, never the validation reason or raw error. The client uses it only after validating the refusal code, request hash, IDs and optional payload hash, for create/update with a requested quote. Missing, unknown or incompatible categories retain the base error; older owners preserve conditional input/quote guidance on a new client, and older clients ignore the additive field. Neither specific nor generic refusals establish that a draft exists or recommend automatic replay.

Unsupported quote content has fixed guidance to inspect the local message in full, select a supported text quote or explicitly prepare complete create/update input without `--reply-to`, retaining `invalid_arguments` (exit 2). The existing private optional field also allowlists `reply.unsupported` under the same create/update, quote and refusal-correlation checks. The category carries no reason or sender-validity assertion; unavailable content in any verified alias retains priority over unsupported content. Missing/unknown categories, older owners/clients and uncertain correlation keep their existing behavior. Media, buttons and reactions remain unsupported quotes; nothing is removed automatically.

Cleanup preview never acquires LOCK, opens a session, stats snapshots or migrates; it paginates immutable revision ordinals with store/draft/policy-scoped cursors, limits 1–200 and page-only counts. Catalogue eligibility does not certify bytes present or reclaimable disk space. Apply is rejected by readonly policy before effects, preflights a compatible readonly catalogue, then owns the existing LOCK or owner operation slot through streaming validation, checked leased FULL revalidation, unlink and directory sync, with no WA or pacing. Only document bytes of discarded, outbound-unreferenced revisions (including a sole head) are eligible. All immutable rows, head, keys and receipts remain. No schema, index, automatic GC, orphan purge or whole-archive compaction is added.

Apply success and optional `error.cleanup` carry `{draft_id,revision_id,hash,effect,outcome,directory_sync,removed_bytes}`. Effects are `not_removed|removed|unknown`; prior absence is `already_absent_unknown` with zero bytes removed in this call. Counts are logical content, never freed blocks. Commit/restoration failure never permits unlink; filesystem/DB are not atomic, and `updated_at` is not a removal journal. Wrong/old/lost owner replies or timeout after submission report unknown effect without replay; stdout failure retains known effects. Error codes and durability/TOCTOU limits are documented in [explicit cleanup](drafts.md#explicit-document-byte-cleanup).

## Outbound dispatch capability

Only `outbound send D --revision R --expect-hash H --key K` gains the outbound live capability. Source remains `live` for usage/readonly errors and pure-local duplicates. Other mutation allowlists remain unchanged. The same retained binding returns the original OP/message IDs before session/network checks; no automatic application retry or fallback occurs.

`error.outbound` carries request/operation/message IDs, D/R/H/key/frozen own PN, phase, retained attempt result, known result, persistence confirmation and optional known ACK timestamp. Output failure after an effect retains this query correlation. Compact/full are capped at 1/8 MiB; no body, protobuf, media secrets or raw internal causes are exposed. The adapter's one invocation does not limit SDK frame or retry-receipt retransmission, including after uncertainty/cancellation. Accepted never establishes delivered/read. See [outbound operations](outbound.md) for deadlines, IPC, snapshot handling, certainty and restoration limits.

Static image drafts expose `kind=image` and `image={mime,caption,size,sha256,width,height,thumbnail_bytes,thumbnail_sha256,verified_at_create}`. Full adds only the derived expected `snapshot_path`; compact may truncate the literal caption with explicit recovery guidance. Thumbnail bytes/base64 and import paths are private and never part of the public DTO. Metadata describes preparation only; visual inspection is separate and no human approval is recorded. Output remains minified v1 JSON with existing limits/source/uncertainty. See [drafts](drafts.md) for JPEG/PNG/APNG validation, retention and old-owner request-v2 compatibility.

## Live owner status

[`sync status`](sync.md#live-owner-readiness) is a readonly `sync_status` capability
with `source=live`. It queries only the existing follow owner's scoped Unix IPC,
without writer LOCK, archive/session open, migration or a second WhatsApp client.
Read `data.ready` positively; an exit-0 status can be `absent` or `unknown`.
Local initialization plus current authenticated connectivity means ready, never
history completeness or guaranteed future dispatch. Timeout, incompatible/mismatched
owners and Windows unsupported IPC remain explicit unknown observations. Offline
`auth status` / `doctor` retain `connected=unknown`; no auth-history fallback occurs.

## Doctor historical evidence

Doctor compact/full add `data.observations` version 1 with independent nullable
connection/Sync snapshots and `historical=true`; see [doctor](doctor.md#retained-connection-and-sync-observations).
Each slot carries its own execution ID and dates; Sync separately references its
connection execution. `doctor --connect` remains outside the agent capability.
Offline reads do not connect, acquire writer LOCK, migrate or repair snapshots.
Recovery outcomes are bounded historical facts, separate from current preventive
reconciliation debt; legacy `app_state.recovery_observations` remains null here.
`auth.connected`, `meta.freshness` and `meta.completeness` remain unknown even with
a confirmed historical login, replay completion, `progress=100` or cleanup date.
Null slots and `diagnostics_unavailable` mean unknown. An observation-only read
failure is embedded and does not change exits; archive/auth-source errors retain
exit 4. Readonly schema errors require an explicit writable upgrade, never an
automatic connection. A failed checkpoint may leave an older execution saved;
current-run `persistence_unconfirmed` and correlation must not be read as durable
success or permission to automatically retry an uncertain action.

## Durable change consumption

For fixture validation of continuous operation and the distinction between received events, retained state and remote completeness, see [continuous acceptance](continuous-acceptance.md). Its passkey/native-flow boundaries do not add agent capabilities or authorize fallback.

Use [`changes list`](changes.md) to consume persisted message mutations and public SDK receipt observations. Start without `--cursor`, save `meta.page.next_cursor` after processing every page, and keep using it when `has_more=false` or `changes=[]` to see later commits after restart. Unlike list traversal, this feed retains event references; it does not freeze content or certify remote coverage. Limit/detail can change, account/store selection cannot. `invalid_cursor` and `cursor_expired` exit 2 without resetting; schema/read failures are sanitized `store_unavailable` with exit 4. Schema 33 is writable-only, no retroactive import or automatic truncation is performed, and clone/restore detection has explicit limits. Compact/full are identical reference DTOs and normal envelope caps remain.
