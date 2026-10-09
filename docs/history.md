# history

Read when: trying to fetch older messages for a known chat.

`wacli history` inspects local archive coverage and can send on-demand history sync requests to the primary device. Backfill is best-effort and depends on the phone being online and WhatsApp returning older messages.

## Commands

```bash
wacli history coverage [--evidence] [--query TEXT] [--kind KIND] [--include-blocked] [--only-actionable]
wacli history fill --dry-run [--query TEXT] [--kind KIND] [--limit 100]
wacli history backfill --chat JID [--before-id MESSAGE_ID] [--count 50] [--requests N] [--wait 1m] [--idle-exit 5s] [--events]
```

## Coverage and planning

- `history coverage` reads only the local `wacli.db` store.
- `ready` chats have at least one local message, so `history backfill` has an anchor.
- `blocked` / `no_local_anchor` chats have no local message yet; run `wacli sync` first.
- `history fill --dry-run` lists matching ready chats that would be selected for a future multi-chat fill workflow. It does not connect to WhatsApp or write state.

## Ingestion diagnostics

Live message persistence failures now emit a sanitized warning on stderr. With
`--events`, the warning remains one NDJSON event (`code=ingestion_persistence_failed`)
with fixed `operation=live` and `reason=persistence_failed`; no raw SQLite error,
chat/message identifier, payload, key or URL is included. Sync continues under its
existing lifecycle policy; the warning does not retry the message or certify recovery.
Media/webhook/poll work for that live message requires successful persistence.

Each admitted history response emits `history_ingestion` on the existing stderr
event stream, or a human summary on stderr without `--events`. Its `summary` contains:

| Field | Observation |
| --- | --- |
| `received` | All entries in conversation message lists, including malformed entries. |
| `valid` | Entries that reached parsing with WebMessageInfo, a nonempty ID and chat. Structural validity does not prove usable content, verified authorship or successful storage. |
| `content` | Valid entries whose initial parser result reports content. Not distinct messages, retained content or recovery. |
| `processed` | Successful persistence calls, including replay/update and suppression by existing purge protections. |
| `skipped` / `skip_reasons` | Discarded entries: `missing_info`, `missing_id`, `missing_chat`, or `unusable_edit`. Missing chat takes precedence for its whole list. |
| `failed` / `failure_reasons` | Failed author verification/retention (`author_unverified`) or persistence (`persistence_failed`, including limits). |
| `unprocessed` | Received entries left without a processing outcome when the handler returns early. |
| `additions`, `replays`, `purge_suppressed` | Always `null`: the current persistence API does not measure these separately. |
| `last_failure` | Last failure date and fixed operation/reason, without message content or identity. |

Legacy `messages_synced`/`messages_stored` still count successful processing,
including replays; they are not additions. Backfill `responses_seen` still counts
matching transport responses and its response `messages` counts list entries,
including malformed ones. Neither certifies useful content. The new response
summary covers all conversations in that response, not just a selected backfill
chat; existing selected-window growth/evidence and ON_DEMAND error propagation
remain unchanged. A partial write can leave retained rows before a later failure;
`failed` does not mean rollback.

The existing Sync diagnostic slot adds optional `ingestion`: a bounded aggregate
with execution ID, start/observation dates, live received/processed/failure counts
(for messages that reached persistence),
history response/count/reason totals and only the latest response/failure.
`degraded=true` stays set within the run after any skip, failure or unprocessed
entry. Discards are ingestion observations, not network/authentication failures.
It resets for each new Sync run; no observations means unknown, and a zero counter
only describes what this run observed. It cannot detect messages that never arrived.

Updates stay in memory on the live hot path. Existing lifecycle checkpoints and
one checkpoint at the end of each history response retain the aggregate in the
existing diagnostic snapshot; no schema, journal, flag or environment variable
is added. Saved checkpoints can lag active work or be absent/unconfirmed after a
store failure. Doctor and Sync result observations show historical snapshots.
`App.IngestionSnapshot()` offers a synchronized defensive copy of this invocation's
latest run for owner status integration, with `nil` before a run is published;
it opens no store/client and changes no readiness contract. A stopped run remains
dated evidence, not current health. Old saved snapshots without this field remain
readable and carry no ingestion health observation.

A known comparison found an own text visible on WhatsApp Web but absent locally
after a correctly anchored ON_DEMAND request. The response contained two entries
whose IDs were not captured, and no local growth was measured. Its raw payload
was unavailable, so we do not know whether those entries lacked IDs; the cause
was not attributed to these ingestion bugs. Missing-ID discards were reproduced
with fixtures, not diagnosed from that response.
These diagnostics expose failure/discard evidence, preserve PN/LID, Unicode,
idempotency and purges, and do not automatically recover that gap. No row count,
response, replay completion or progress value proves complete history.

## Imported authors and retained history

Newly imported own messages use the observed local account's public PN, never
the DM recipient as a sender fallback. Without that account identity, the sender
remains unknown. Incoming groups without an author also remain unknown. Explicit
participants and original self authors must agree with the final message author;
PN/LID equivalence needs a coherent local observation. Conflicts or identity lookup
errors refuse that message's import. Content, stars, edits and SDK crypto identities
retain their existing handling, and draft quotes still require a known, compatible
sender.

An unknown-author replay cannot replace an existing row with a retained sender:
the whole row is preserved, without certifying its old attribution. A replay with
proven authorship may update eligible rows under the existing upsert rules; newer
content, edits, tombstones and purge protections remain unchanged. Divergent
PN/LID quote records continue to be refused.

This correction does not repair all previously imported senders. Existing records
remain pending separate review, including protected rows and restored archives.
`from_me` with sender equal to chat is only a candidate for review, not proof of
the original account. Replay requires an available source, proven historical
account/aliases, and explicitly authorized chats/IDs/windows. No migration, broad
scan, automatic repair, or changes to frozen draft revisions are performed.

## Limits

- `--count` defaults to 50 and must be at most 500.
- `--requests` defaults to 1 and must be at most 100. Without `--before-id`, each requested batch may retry the other verified identity and one later anchor after timeouts.
- `--wait` and `--idle-exit` each default as shown above and must be at most 5 minutes. Nonpositive count/request/wait/idle values retain their legacy defaults.
- Requests are per chat.
- Without `--before-id`, the anchor starts at the oldest locally stored message in that chat. If the phone does not answer within `--wait`, backfill retries once using the next chronological local message (timestamp, then row ID). No message IDs or content types are filtered out, and no local rows are deleted.
- This is backward pagination, not recovery since disconnection. A missing message newer than the selected anchor is outside the requested interval. Sync replay completion and an empty local search do not prove that interval complete or permanently unavailable; see [missing offline messages](sync.md#missing-messages-after-an-offline-interval).
- In default backfill, a phone-number JID and its verified mapped LID refer to the same local chat. Backfill uses the phone JID for local anchors and results, requests history with the corresponding LID when available, and accepts responses under either identity. The primary device answers some 1:1 chats only by LID and others only by phone number (#444), so an unanswered request is retried for the same anchor with the other identity, and the identity of a successful attempt is preferred for later batches. Responses do not identify the triggering request, so a late reply may temporarily favor the other identity; the bounded fallback remains available on every batch. A `backfill_identity_retry` warning reports the switch. Unmapped JIDs and groups retain their original identity.
- Each attempt gets its own `--wait`; a batch can therefore wait up to twice that duration for responses, or four times for a mapped 1:1 chat whose identities are both silent (two identities for each of two anchors). If there is no next anchor, or the retry also times out, backfill stops with an error naming the unanswered anchor. Transport errors and cancellation are not retried.
- A successful retry must add history older than the original local anchor to continue to another batch. Returning only already-stored messages stops backfill normally.
- Backfill evaluates progress after the history response has been processed into the local store, including asynchronously delivered responses. If a response contains both identities of the frozen verified PN/LID scope, their message observations are summed into one response; a primary end marker from either identity takes precedence and retains its observed identity and time. Conversations outside that scope do not contribute observations or primary markers.
- Sync owns the manual-history download setting. Backfill uses the same persistence handler for both directly delivered history events and manually downloaded on-demand notifications; a notification is downloaded and persisted once.
- `--events` emits NDJSON request/response/stop lifecycle events on stderr. Requests include `anchor_msg_id` and `request_chat_jid` (the identity sent to the phone), and a `warning` with code `backfill_anchor_retry` identifies the unanswered anchor and its replacement. Human output reports the same retry on stderr. The result's request count includes retry attempts.

## Explicit recovery anchor

```bash
# Use an exact ID returned by messages show/list for this selected archive/chat.
wacli --account example history backfill --chat 1234567890@s.whatsapp.net --before-id REAL_MESSAGE_ID --count 50 --requests 1 --agent
```

`--before-id` requests up to `--count` messages immediately **before** one genuine
persisted message in the exact normalized requested chat. It can target a recent
window even when the archive already contains much older messages. It never
recovers a gap after the selected anchor or guarantees a missing message exists
on the phone. An arbitrary date/ID or a message visible only in a browser cannot
supply an anchor: first select a real local message in this account's archive.

This mode supports one batch only (`--requests 1`, the default), with **one
request and no anchor or identity retry**. It preserves the requested PN, LID or
group identity on the wire, without substituting a mapped chat. The writer checks
the exact chat/ID, positive persisted timestamp, valid sender, `from_me` and
current local public account/verified author relationships. Missing/wrong-chat
messages, unknown or contradictory authors, tombstones, purged payloads and read
errors refuse before recovery dispatch; initial validation precedes standalone
connection. Explicit preparation and revalidation use local identity reads only: a missing
alias leaves the requested identity alone, a candidate alias needs corroborating
PN/LID mappings in both directions, and read errors or scope changes refuse.
The account, scope and anchor are revalidated after the pre-call checkpoint and
`backfill_requesting` output, immediately before the WA invocation. That event
announces an intended request; finalized `requests_sent` counts actual WA
invocations. A refusal there records zero invocations and `not_dispatched`;
a crash or failed finalization can retain the conservative pre-call checkpoint.
These local reads and the network call are not an atomic transaction.
This does not
certify historical authorship or continuity across a replaced/restored archive:
message rows are scoped to the selected archive, without a per-row account ID.
Normal standalone Sync may migrate verified LID rows after connecting; if the
selected row moved or changed, explicit recovery refuses rather than switching
chat/anchor. Inspect the current archive and select again explicitly.

Progress uses net distinct retained IDs with timestamps strictly **before the
selected anchor**, across the frozen verified conversation scope, after captured
callbacks and the idle window drain. The global oldest need not change. Duplicate,
newer and equal-second arrivals do not demonstrate progress; the archive's second
precision cannot establish order within one second. Tombstones/purged rows are
excluded from this window metric. `no_progress` means no measured growth in that
window, not complete or unavailable history. Empty/partial replies remain only
observations; an explicit primary end marker retains its existing limited meaning.
Concurrent or late history in this conversation can contribute: WhatsApp supplies
no request ID and cannot prove exclusive attribution to this anchor/window.

Successful JSON/agent results add `before_id` and `messages_added_before` only in
this mode. Existing `messages_added` and retained evidence counts remain total
conversation growth. Evidence reuses the attempt ID, first/last actual anchor,
prepared request identity, response times and stop reason; no schema change is
needed. The selector mode and separate window count are in the action result,
not separately retained in coverage evidence. Keep that result alongside the
attempt ID; retained anchor IDs alone do not distinguish default/explicit mode.
Preflight anchor refusals before the evidence start do not create a retained
attempt. The same deadlines, cancellation/uncertain outcomes and readonly guards
apply. Never automatically repeat a refused or uncertain request.

## Backfill alongside sync follow

When `sync --follow` already holds this archive's `LOCK`, `history backfill`
delegates through the existing private `.send.sock` to that same connected
process. Account/store selection is resolved once for locking and delegation.
No second writer, connection, daemon, or database is opened. Without an owner,
the existing standalone connect/sync/idle flow runs. `--read-only` (including `WACLI_READONLY=1`)
rejects backfill before dispatch. `--agent history backfill` is an explicit live
action, subject to the same policy and limits. Older follow owners explicitly
reject unsupported history kinds. Explicit selection uses the distinct
`history_backfill_before_v1` kind: an older owner that ignores new JSON fields
cannot silently run oldest backfill. Restart with a compatible binary; no
direct-writer fallback occurs.

Backfill shares the existing serialized operation slot with delegated sends,
presence, and read receipts. While backfill occupies it, those operations wait
and can expire in the queue. Backfill does not use or update `--send-spacing`.
The follow process remains connected and processes incoming messages during and
after backfill; reconnecting remains follow's responsibility. A disconnected or
stopped owner returns an error. Request/response/stop events and progress are
written by the owner, using its own stderr/event settings.

The command's `--timeout` budget includes lock waiting, delegation, queue waiting,
requests/retries, and the final idle window (default 5 minutes). Increase it for
many batches; `--wait` remains a per-attempt limit. The owner uses the caller's
absolute deadline, reserving a small response margin, and never dispatches an
operation whose queued budget has expired. Ordinary message/history activity
extends the idle window; typing and keepalive notifications do not. Success in
both standalone and owner mode waits for callbacks captured by the operation's observer to finish persisting, including
downloads and writes already in flight during the final idle window. Cancellation
or deadline expiry closes the observer promptly without waiting for a blocked
callback. After possible dispatch, that exit is uncertain: final growth remains
unknown and the previous `last_success` is preserved.
Stopping the owner cancels its active operation without backfill reconnecting it.

An explicit pre-dispatch queue refusal means no history was requested by that
operation. A failed socket dial before sending preserves the original lock error.
There is no direct-writer fallback after attempting to send the request. A lost
reply, timeout, or error after dispatch can leave persisted history; inspect the
archive before retrying. Closing or interrupting the client does **not** acknowledge
cancellation at the owner: its work can continue until its deadline. Messages
persisted by late replies may remain even after cancellation.

Callbacks already in flight retain their original observer and cannot notify the
next operation. This does not correlate network replies: WhatsApp supplies no
request ID, so a late response whose callback starts during a later operation
can still be observed by that operation. Backfill cannot attribute every arrival
exclusively to a particular request or guarantee complete history.

## Backfill results and evidence

Successful JSON results retain `chat`, `requests_sent`, `responses_seen`,
`messages_added`, and `messages_synced`, and add `stop_reason`. Human output also
shows the stop reason and describes the measured local conversation growth.

- `messages_added` is the net increase in distinct local message IDs for the
  selected conversation and its verified PN/LID alias. The baseline is taken
  **after connecting and canonicalizing/migrating known aliases, before the first
  request**; the final count is taken after the backfill idle window. In standalone
  mode that window ends with the enclosing Sync; with a running owner it closes
  only this operation's observer. Connection-time imports and alias deduplication
  are outside the window.
  Duplicate IDs across the verified pair count once, so moving or merging these
  rows during the window does not manufacture additions or losses. Other chats
  are excluded. Concurrent messages in the selected conversation, including
  arrivals during the idle wait, are included. WhatsApp supplies no request
  correlation ID, so this is not exclusive attribution to the backfill request.
- `messages_synced` retains the enclosing Sync's **global** successful message
  persistence counter in standalone mode. With a running follow owner it reports
  the **delta of that same counter during the backfill window**, excluding activity
  before the operation, rather than the total since follow started. Both include
  other chats and updates/replays. It is not a count of requested additions and need not equal `messages_added`; manually
  downloaded on-demand blobs use a separate internal persistence counter.
- Counts are sampled only at the two window boundaries, not for each message.
  If the verified identity set changes during the window, or the distinct count
  decreases, backfill returns an error because the result could not be measured
  reliably. Messages already persisted may remain. Store count/anchor failures,
  selected-conversation on-demand message persistence failures, timeouts, and
  cancellation also return errors instead of a successful result, including
  cancellation during the final idle wait.

| `stop_reason` | Evidence |
| --- | --- |
| `requested_batch_limit` | All requested batches received replies with progress; the configured batch limit stopped further requests. |
| `no_progress` | A nonempty reply left the original oldest local anchor unchanged (default), or explicit selection measured no growth before its anchor after the idle drain; this is a heuristic, not evidence of history completeness. |
| `empty_response` | The matching conversation returned no messages and no explicit primary end marker. |
| `primary_no_more_messages` | The primary reported `COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY` for this conversation. This wins over empty/duplicate-response heuristics. |

The primary marker means **no more messages available on that primary device**.
It does not prove an unlimited global history or a complete local archive.
Backfill persists bounded recovery observations, without a coverage/completeness claim.

For compatibility, `backfill_stopped` lifecycle events retain the old `reason`
values `no_older_messages_added`, `no_messages_returned`, and
`start_of_history_reached`; their interpretations are respectively `no_progress`,
`empty_response`, and `primary_no_more_messages`. They also carry `stop_reason`.
The batch-limit event uses `requested_batch_limit` for both fields. A batch-stop
event can precede a final idle/counting failure; only the final successful result
confirms the operation finished normally. The agent contract exposes the final
correlated observation and typed uncertain outcomes; see [Agent contract](agent.md).

## Retained recovery observations

Backfill writes recovery evidence to the selected archive's existing `wacli.db`,
using the same writer lock and connected owner when present. Evidence is specific
to history recovery: it is not an operation journal, replay token or automatic
retry mechanism.

```bash
wacli history coverage --chat 123@s.whatsapp.net --evidence --json
wacli history coverage --chat 123@s.whatsapp.net --agent --evidence --detail full
```

`--evidence` adds a separate `data.recovery_evidence` list to JSON output and a
separate recovery table to human output. Without the flag, legacy coverage fields,
filters and output stay unchanged. An explicit `--chat` selects retained evidence
even when that identity has no chat row or anchor, or the coverage filters exclude
it. Without `--chat`, only identities of returned coverage rows are selected.
Queries accept at most 200 inputs and select at most 400 keys after including
currently verified PN/LID aliases; they do not scan the entire evidence catalog.
With evidence, `--limit` must be 1–200. Inputs are deduplicated and normalized
syntactically. A device-qualified PN/LID input uses its non-device identity.

Each requested identity retains at most two records: `latest` and `last_success`.
Both can reference the same successful attempt. A new failed attempt replaces
`latest` while preserving the previous success. New input identities can grow the
table; attempts for an existing input do not create a growing attempt history.
A null slot means **no record retained**, not that recovery never ran. Evidence
is independent of chat/message rows and is not deleted or reassigned when LID
messages move to a verified PN. A substituted attempt ID may no longer be retained.

Compact evidence exposes state, phase, attempt ID, observation dates, historical
conversation/account scope, final request/response counts, stop reason and net
local growth when measured. Full evidence adds options, anchors, checkpoint
counts and the explicitly labelled global Sync counter. Agent output keeps its
existing row and encoded-envelope quotas. Legacy JSON can add these full evidence
fields with `--full`.

`unfinalized` means no terminal outcome was durably recorded. It does not prove
that work is running or that the process crashed. Unfinalized request/response
totals are null in compact output; full checkpoint counts describe only the last
saved checkpoint. Errors/cancellation after a possible request can leave history
in the archive. Net growth is null when not reliably measured, rather than an
invented zero. `dispatch_possible` is a conservative persisted marker; a prepared
anchor or request identity does not prove delivery. No evidence update happens
for each message.

The runner saves the start before standalone connection, establishes its counting
window after alias migration, and saves a checkpoint before each recovery call.
A checkpoint failure prevents that next call. Normal standalone Sync activity can
still change the store before any recovery request. After possible dispatch,
errors never certify no effects. Finishing uses a bounded, synchronous write
outside the cancelled command context, before releasing the writer lock. A
failed final write can leave `latest` unfinalized despite network/store activity.
Success and `last_success` are committed together; every later update is
conditioned on the attempt ID so old cleanup cannot finalize a newer attempt.
SQLite durability follows the archive's existing WAL/synchronous settings.

An observed primary end marker includes its callback observation date and the
response's conversation identity, even when a later step fails and that partial
observation can be saved. It describes what that primary answered for that
historical scope. It does not prove a complete local archive, uninterrupted
identity mapping, remote availability now or global/future completeness. Replies
have no network correlation ID and can include concurrent or late activity.

`identity_relation=matching_snapshot` only compares the current local public
account and verified identity set with the historical snapshot. A different
account or verified set yields `changed`; unavailable comparable facts yield
`unknown`. A lookup error is an error, not proof of a missing alias. Nullable own
LIDs remain unknown own pairs and can use the persisted verified pair map.
Neither matching counts nor this identity comparison validates the message
catalog. Current raw coverage includes tombstones/placeholders and its date
interval is not proof of continuous history or retained payloads. Edits,
revocations, removal, orphaned messages and alias merging do not change these
limits. A restored archive carries only the evidence from that restored snapshot.
Unreadable, missing or incompatible archives fail rather than inventing empty
coverage or successful recovery. Reads never migrate or repair evidence; schema
29 is created only by an explicit writable archive open. A writer also refuses
unknown/newer migration versions before changing journal mode or permissions.

The CLI generates an internal random attempt ID before possible delegation and
passes it to the owner. This is correlation, not idempotency. Missing/mismatched
reply IDs, socket failures after attempting dispatch or an owner without typed
correlation produce an uncertain outcome. They do not prove the ID was persisted
or authorize a direct-writer fallback. Inspect retained observations before an
explicit new request. Closing the caller socket does not acknowledge cancellation
at the owner.

## Examples

```bash
wacli history coverage --include-blocked
wacli history coverage --query family --only-actionable
wacli history fill --dry-run --kind group --limit 20
wacli history backfill --chat 1234567890@s.whatsapp.net --requests 10 --count 50
wacli history backfill --chat 123456789@g.us --requests 3 --wait 90s
```

## Durable local change notifications

Native/manual history imports generate [`changes list`](changes.md) references only when persisted message rows actually change. Historical source timestamps do not order the local feed; unchanged replay adds no message entry. The feed begins with schema 33 and does not reconstruct preexisting history or prove WhatsApp completeness. Existing authorship, PN/LID, edit/tombstone and backfill evidence rules remain authoritative. Embedded historical receipt arrays are outside feed receipt coverage.
