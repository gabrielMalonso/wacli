# history

Read when: trying to fetch older messages for a known chat.

`wacli history` inspects local archive coverage and can send on-demand history sync requests to the primary device. Backfill is best-effort and depends on the phone being online and WhatsApp returning older messages.

## Commands

```bash
wacli history coverage [--query TEXT] [--kind KIND] [--include-blocked] [--only-actionable]
wacli history fill --dry-run [--query TEXT] [--kind KIND] [--limit 100]
wacli history backfill --chat JID [--count 50] [--requests N] [--wait 1m] [--idle-exit 5s] [--events]
```

## Coverage and planning

- `history coverage` reads only the local `wacli.db` store.
- `ready` chats have at least one local message, so `history backfill` has an anchor.
- `blocked` / `no_local_anchor` chats have no local message yet; run `wacli sync` first.
- `history fill --dry-run` lists matching ready chats that would be selected for a future multi-chat fill workflow. It does not connect to WhatsApp or write state.

## Limits

- `--count` defaults to 50 and must be at most 500.
- `--requests` defaults to 1 and must be at most 100. Each requested batch may retry the other verified identity and one later anchor after timeouts.
- `--wait` and `--idle-exit` each default as shown above and must be at most 5 minutes. Nonpositive count/request/wait/idle values retain their legacy defaults.
- Requests are per chat.
- The anchor starts at the oldest locally stored message in that chat. If the phone does not answer within `--wait`, backfill retries once using the next chronological local message (timestamp, then row ID). No message IDs or content types are filtered out, and no local rows are deleted.
- A phone-number JID and its verified mapped LID refer to the same local chat. Backfill uses the phone JID for local anchors and results, requests history with the corresponding LID when available, and accepts responses under either identity. The primary device answers some 1:1 chats only by LID and others only by phone number (#444), so an unanswered request is retried for the same anchor with the other identity, and the identity of a successful attempt is preferred for later batches. Responses do not identify the triggering request, so a late reply may temporarily favor the other identity; the bounded fallback remains available on every batch. A `backfill_identity_retry` warning reports the switch. Unmapped JIDs and groups retain their original identity.
- Each attempt gets its own `--wait`; a batch can therefore wait up to twice that duration for responses, or four times for a mapped 1:1 chat whose identities are both silent (two identities for each of two anchors). If there is no next anchor, or the retry also times out, backfill stops with an error naming the unanswered anchor. Transport errors and cancellation are not retried.
- A successful retry must add history older than the original local anchor to continue to another batch. Returning only already-stored messages stops backfill normally.
- Backfill evaluates progress after the history response has been processed into the local store, including asynchronously delivered responses.
- Sync owns the manual-history download setting. Backfill uses the same persistence handler for both directly delivered history events and manually downloaded on-demand notifications; a notification is downloaded and persisted once.
- `--events` emits NDJSON request/response/stop lifecycle events on stderr. Requests include `anchor_msg_id` and `request_chat_jid` (the identity sent to the phone), and a `warning` with code `backfill_anchor_retry` identifies the unanswered anchor and its replacement. Human output reports the same retry on stderr. The result's request count includes retry attempts.

## Backfill alongside sync follow

When `sync --follow` already holds this archive's `LOCK`, `history backfill`
delegates through the existing private `.send.sock` to that same connected
process. Account/store selection is resolved once for locking and delegation.
No second writer, connection, daemon, or database is opened. Without an owner,
the existing standalone connect/sync/idle flow runs. `--read-only` and `--agent`
continue to reject backfill before any effects. Older follow owners explicitly
reject the new `history_backfill` kind; restart with the updated binary.

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
extends the idle window; typing and keepalive notifications do not. Success waits
for callbacks captured by this observer to finish persisting. Cancellation closes
the observer promptly and does not wait indefinitely for an in-flight download.
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
| `no_progress` | A nonempty reply left the original oldest local anchor unchanged; this is a heuristic, not evidence of history completeness. |
| `empty_response` | The matching conversation returned no messages and no explicit primary end marker. |
| `primary_no_more_messages` | The primary reported `COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY` for this conversation. This wins over empty/duplicate-response heuristics. |

The primary marker means **no more messages available on that primary device**.
It does not prove an unlimited global history or a complete local archive.
Backfill does not persist a coverage/completeness claim.

For compatibility, `backfill_stopped` lifecycle events retain the old `reason`
values `no_older_messages_added`, `no_messages_returned`, and
`start_of_history_reached`; their interpretations are respectively `no_progress`,
`empty_response`, and `primary_no_more_messages`. They also carry `stop_reason`.
The batch-limit event uses `requested_batch_limit` for both fields. A batch-stop
event can precede a final idle/counting failure; only the final successful result
confirms the operation finished normally. `history backfill` remains unsupported
in `--agent` mode.

## Examples

```bash
wacli history coverage --include-blocked
wacli history coverage --query family --only-actionable
wacli history fill --dry-run --kind group --limit 20
wacli history backfill --chat 1234567890@s.whatsapp.net --requests 10 --count 50
wacli history backfill --chat 123456789@g.us --requests 3 --wait 90s
```
