# sync

Read when: running continuous capture, one-shot sync, contact/group refresh, or background media download.

`wacli sync` requires an existing authenticated store and never displays a QR code. It captures WhatsApp Web events into the local SQLite store.

For scheduled routines and separate one-off tasks sharing an account, see [concurrent use](concurrent-use.md).

Startup repairs historical LID identities using indexed message lookups without rebuilding unchanged search content. Interrupting startup stops identity repair between individual identities; the next run resumes any remaining repairs.

Remote logout stops sync and emits `logged_out`; it retains the existing successful-stop exit status. `auth status` and `doctor` remember the observed revocation until a confirmed login.

## Command

```bash
wacli sync [--once] [--follow] [--idle-exit 30s] [--max-reconnect 5m] [--stale-threshold DURATION] [--presence-mode normal|quiet] [--send-spacing DURATION|MIN-MAX] [--max-messages N] [--max-db-size SIZE] [--download-media] [--refresh-contacts] [--refresh-groups] [--refresh-channels] [--events] [--webhook URL] [--webhook-secret SECRET] [--webhook-events LIST]
```

## Modes

- Default behavior follows continuously.
- `--once` exits after sync becomes idle.
- `--idle-exit` controls idle exit timing in once mode. Idle exit waits for admitted event callbacks, including history downloads and persistence, and starts a fresh idle window after their activity finishes. Cancellation, supplied context deadlines, logout and storage-limit exits still stop without this idle wait. A stalled callback requires interruption or an external deadline; the global `--timeout` flag applies to non-sync commands and does not bound sync. Cleanup still drains known local writers before closing the archive; a callback that ignores cancellation can delay cleanup.
- `--max-reconnect 0` keeps reconnecting indefinitely.
- If WhatsApp revokes the linked session, sync emits a terminal `logged_out` event, cancels any reconnect already in progress, and exits cleanly. Re-pair with `wacli auth logout` followed by `wacli auth --phone`.
- `--max-messages N` stops before storing more than `N` total messages locally.
- `--max-db-size SIZE` stops when `wacli.db` plus SQLite sidecars reaches `SIZE` (`500MB`, `2GB`, etc.).
- `--download-media` runs a bounded media downloader for sync events. Clean one-shot and bootstrap runs finish queued downloads before exiting; cancellation, errors, and storage-limit exits stop immediately.
- `--send-spacing DURATION|MIN-MAX` paces serialized operations delegated to a running follow process. A single duration such as `2s` sets a fixed minimum gap; a range such as `500ms-5s` chooses a fresh random gap for each operation. It is disabled by default. With or without pacing, the caller's command timeout includes time queued behind earlier operations, pacing, and the operation itself; a request that is still queued when its caller's deadline is reached is refused with an explicit "it was not sent" error and is never dispatched later (#446). If the caller times out waiting for a reply after dispatch, the error says the operation may still have gone through, so check before retrying. Delegated `chats mark-read` and `chats mark-unread` share this queue and timeout budget; delegated archive, pin, and mute changes skip the queue and pacing but keep the same timeout budget.
- `--refresh-contacts` imports contacts from the session store.
- `--refresh-groups` fetches joined groups live and updates local group metadata and participant snapshots.
- `--refresh-channels` fetches subscribed WhatsApp Channels live and updates local chat rows.
- `--webhook URL` posts successfully stored live message events as JSON on a bounded background worker. The payload includes `ChatName` when a locally resolved chat name is available.
- `--webhook-secret SECRET` signs webhook payloads with `X-Wacli-Signature: sha256=<hmac>`.
- `--webhook-events LIST` selects which event types are posted, as a comma-separated list of `message`, `receipt`, and `chat_presence`. The default is `message`, which preserves the earlier message event shape. A list that omits `message` stops message posts, so `--webhook-events receipt` posts receipts only. `chat_presence` needs `--presence-mode normal` (the default): WhatsApp only sends typing notifications to devices that mark themselves available. See [Webhook payloads](#webhook-payloads).
- Webhook delivery is best-effort: failures, request timeouts, and full-queue drops are logged as warnings and do not stop sync. Retries/backoff are intentionally out of scope for this flag.
- If neither storage cap is configured, sync prints one warning because WhatsApp history can grow the local database substantially.
- `WACLI_SYNC_MAX_MESSAGES` and `WACLI_SYNC_MAX_DB_SIZE` apply the same caps to `auth` bootstrap sync and `sync`.
- After `sync --follow` finishes startup and opens its local delegate socket, these commands for the same store are delegated to it so they do not fail on the store lock:
  - `send text`, `send file`, `send sticker`, `send voice`, `send react`, `send location`, `send poll`, and `send select`.
  - `poll vote`, `presence typing`, `presence paused`, and `messages edit`.
  - `chats mark-read`, `chats mark-unread`, `chats archive`, `chats unarchive`, `chats pin`, `chats unpin`, `chats mute`, and `chats unmute`.
- `send status` is not delegated and still requires the direct store lock.
- After connecting, sync fetches WhatsApp chat app-state deltas (`regular_high` and `regular_low`) so starred, delete-for-me, mute, archive, pin, and mark-read changes made while `wacli` was offline are caught up instead of relying only on live push notifications.
- Each startup fetch records recovery debt before the SDK can advance its cursor, persists returned events in order, and clears only its completed generation. A failed page, cancellation or local persistence failure retains debt for a later full replay; normal message handling continues with a warning.
- Sync records debt for the mirrored `regular_high`, `regular_low` and `regular` collections before removing its persistence handler during shutdown. App close reaffirms debt after closing the session store and draining known local writers, because SDK callbacks can outlive disconnect. Even a graceful stop can therefore require full collection reads on the next startup, within the existing recovery budgets. A shutdown marker failure returns an error and keeps the handler during disconnect; it does not claim a successful durable checkpoint. Recovery never resends a user's chat-state mutation.
- Sync imports messages sent from your other linked devices into the destination chat with `from_me=true`, so local history covers both incoming and outgoing conversation sides.
- Sync decrypts encrypted message edits and updates the original local row only when the authenticated sender, chat, and target message match. Malformed, redirected, or unsupported edits are rejected without changing local history or emitting a message webhook.
- If whatsmeow reports an app-state LTHash mismatch, sync attempts one full refresh for that collection before requesting a phone snapshot. Recovery uses a durable intent and ordered local persistence; interrupted work is replayed at the next startup before incremental fetches. Full refresh and phone recovery have independent timeouts, and each collection gets at most one automatic recovery sequence per sync run. Failed or unconfirmed recovery retains its intent and emits a warning while normal message/history handling continues. Snapshot requests use an ID generated before observation begins. Only a matching protocol response from the exact known own PN/LID primary device is observed; history/re-request events are excluded. Server ACK, correlated response and snapshot completion are different facts: public SDK completion events have no request ID, so neither collection/version nor temporal proximity can attribute them. Snapshot alone therefore never clears debt or authorizes a dependent chat-state mutation, even if the SDK applied it. Real SDK events observed during recovery are still drained into the archive, including standalone calls and error/cancellation paths. A later successful existing full fetch can reconcile; recovery adds no full-fetch retry or repeated mutation. This conservative behavior fixes local certainty, not the remote LTHash cause.
- Empty app-state key shares are rejected before key storage, and their account-scoped unavailable status is retained in `wacli.db` across client restarts. A later usable key share clears that status. A collection that needs a known empty key uses the bounded full-refresh and phone-snapshot recovery path when it is next fetched or decoded, including one-shot chat-state commands. Ordinary missing keys keep waiting for normal delivery. Snapshot application requires usable keys, but public SDK events cannot confirm its completion for the request. Snapshot recovery preserves debt and prevents the requested chat-state write until an existing full fetch reconciles it.
- Sync stores WhatsApp call signaling and call-log metadata in `call_events`; inspect it with `wacli calls list`.
- Sync stores WhatsApp status broadcasts in `status_messages`, separate from normal chat `messages`.
- Sync stores location pins and live-location shares in `message_locations`, keyed by (`chat_jid`, `msg_id`); the message row keeps `media_type=location` (or `live_location`). Pins synced before this table existed have no coordinates and cannot be backfilled.
- Unreadable messages emit an `undecryptable_message` warning with chat, sender, and message ID. Its `recovery=requested_if_possible` field is conditional: the SDK requests a primary-device copy immediately when ciphertext is absent, or after a short delay on an eligible first decryption failure; repeated failures, cancelled requests, and bot-message secret failures can take different paths. `is_unavailable` also covers missing group sender keys, so it does not identify the exact retry path. Typed unavailable content, including `view_once`, can still prompt a protocol request without guaranteeing a readable copy. Restart an existing `sync --follow` process after upgrading to enable the updated client behavior.
- In an interactive terminal, routine connected/history/progress updates share one updating stderr status line. Warnings and errors still print as separate lines so they remain visible.
- `--stale-threshold DURATION` in follow mode detects keepalive failures. If whatsmeow reports that the last successful keepalive is older than this duration, sync force-closes the connection and reconnects. Healthy quiet sessions are not reconnected just because no chat events arrive. Disabled by default (`0`); accepted values are `1s` up to but not including `2m20s`, which reserves one maximum keepalive probe interval plus response deadline before whatsmeow's own 3-minute auto-reconnect window.
- `--presence-mode normal|quiet` controls global linked-device presence during sync. `normal` is the default and preserves the existing behavior: sync sends available presence after connecting or receiving a push-name update, then sends unavailable presence on cleanup. `quiet` suppresses the available-presence sends while keeping the final unavailable cleanup; use it for personal-number mirrors where keeping primary-phone notifications audible matters. WhatsApp ultimately controls notification routing, so this mode avoids the active linked-device signal but cannot guarantee phone behavior on every platform.
- A `stale` NDJSON event is emitted when the threshold is exceeded, containing `threshold`, `idle_duration`, `error_count`, and `source` fields.
- While `sync --follow` is running, a `HEARTBEAT` file is written to the store directory (at most once per minute) with the last observed follow activity timestamp in RFC 3339 format. External watchdogs or `wacli doctor` can read this as an activity marker; quiet healthy sessions may not update it because successful keepalives are silent, and keepalive health is reported separately through `stale` events.
- `--events` emits one NDJSON lifecycle event per stderr line for machine consumers. Routine human progress/status lines, interrupt prompts, and command errors are emitted as events while events are enabled.
- `offline_sync_preview` reports the server's announced reconnect backlog with `total`, `messages`, `receipts`, `notifications`, and `app_data_changes`; `offline_sync_completed` reports the server's final `count`. Without `--events`, both print as status lines. Completion can arrive without a preview, including when there is no backlog.
- These are server replay signals on stderr. Webhooks use a separate background queue, so completion does not mean queued HTTP deliveries have finished. Storage failures or webhook drops can also make delivery counts differ from the announced counts. Do not use these signals to classify individual webhook messages as replayed or live. Webhook payloads keep their existing shape.
- Both offline replay signals extend the one-shot idle window. Sync does not wait indefinitely for an unobserved completion signal or for future history notifications.
- `history_sync` retains `conversations` and adds `sync_type`, `chunk_order`, and `progress` from the downloaded/received SDK history blob; absent optional chunk/progress values are null. These describe that blob, not a per-chat coverage interval, successful persistence count or complete archive. In particular, `progress=100` is not a local completeness certificate.

## Missing messages after an offline interval

A successful `sync --once`, even with `messages_stored=0` and a completed offline replay, does not prove that messages sent from another linked device are all present locally. The replay count describes the backlog announced for this connection; it is not an inventory of WhatsApp Web or the primary phone. A short idle interval can end before a later history notification arrives. The callback wait protects work already admitted, not future arrivals.

Inspect the selected account's local chat/message IDs and retain `--events` diagnostics from an explicitly authorized sync. A longer observation window or an authorized `sync --follow` can capture later arrivals, but neither guarantees remote coverage. An empty search establishes only absence from that local query at that time, not definitive remote loss. App-state reconciliation debt is separate from message history; ordinary shutdown can record preventive debt without a recovery failure.

`history backfill` requests messages **before the oldest local anchor**. The pinned SDK's [history request builder](https://github.com/tulir/whatsmeow/blob/35ae40906e74/send.go#L571-L595) describes messages immediately before a supplied known message, not messages since disconnection. Backfill therefore does not automatically recover a newer missing interval. Sync does not invent a future message ID/anchor or automatically request/retry history to claim coverage. A later genuine local anchor can change the interval eligible for a separately authorized backfill. Missing history never authorizes resending an uncertain outbound message, replacing a session or relinking an account.

## App-state summary

The final human/legacy JSON summary includes `app_state`, read after App closes the session, drains known local writers and reaffirms preventive replay debt. Sync keeps its writer lock until this read-only archive snapshot and report finish. `success` and `synced` preserve the existing successful-stop contract, including cancellation and observed logout; they do not certify queue integrity, remote completeness or freshness. Existing command errors and exits are unchanged; an unavailable diagnostic read adds `reconciliation="unknown"`, `pending_collections=null` and only `error.code="recovery_state_unavailable"`, without changing the exit.

`reconciliation` is `required` when debt is retained, or `none_recorded` when the query succeeds without any. `pending_collections` is sorted and distinct, and is `[]` only for a successful empty query. Successful recovery and failed recovery can both end with `required`: ordinary shutdown records preventive debt for the next covered startup. Do not interpret debt alone as a recovery failure, or an empty list as a healthy/fresh mirror.

`recovery_observations` contains facts from **this invocation**, captured by its workers and collected after they drain. It is `[]` when no monitored recovery/failure was observed; doctor returns `null` because these outcomes are not persisted. Each entry has a collection, phase, an `outcomes` set (`completed`, `failed`, `cancelled`, `unconfirmed`) and sanitized `error_codes`. A full-refresh failure followed by a correlated snapshot response retains the failure and a snapshot `unconfirmed` observation with `completion_unconfirmed`; repeated outcomes in one phase are deduplicated, and a later completion cannot erase an earlier failure. These sets do not encode attempt order or count. Storage is fixed to the five known app-state collections and seven phases (`prepare`, `full_sync`, `snapshot`, `persist`, `checkpoint`, `delta`, `shutdown`), at most 35 entries. `completed` describes the observed phase, not a remote integrity check or a guarantee that all debt was cleared. Shutdown observations record marker failures, not ordinary preventive markers. `unconfirmed` is neither remote success nor remote rejection. Deadline without a response remains a failed local wait with remote application unknown. The transient `app_state_recovery_observed` event reports only `requested_collection`, `ack_confirmed`, `response_received` and `completion="unconfirmed"`; false ACK/response fields mean not confirmed/observed, not remote absence. Partial facts survive send errors/cancellation. No snapshot contents, keys or hashes are exposed.

For example, a command can retain its legacy successful exit while reporting a failed recovery:

```json
{"success":true,"data":{"synced":true,"messages_stored":0,"app_state":{"reconciliation":"required","pending_collections":["regular","regular_high","regular_low"],"recovery_observations":[{"collection":"regular_low","phase":"full_sync","outcomes":["failed"],"error_codes":["lthash_mismatch"]},{"collection":"regular_low","phase":"snapshot","outcomes":["failed"],"error_codes":["deadline_exceeded"]}]}},"error":null}
```

The 30-second recovery step contexts are not hard wall-clock ceilings: the pinned SDK mutex and reconnect wait can outlive cancellation, and ordered local persistence is drained before database close. Budgets and SDK retries are unchanged.

When a failure, cancellation or unconfirmed completion is known, inspect its collection, phase and code and retain them for investigation. When the read is unavailable, check local archive readability and schema compatibility. Neither case establishes the original cause of a remote LTHash mismatch.

## Webhook payloads

Webhook payloads remain flat JSON objects. Receipt and chat-presence payloads carry
an `EventType` discriminator. Message payloads deliberately omit it so existing
consumers retain the established object shape; a missing `EventType` means
`message`. Every JID field uses the same identity namespace as the local store:
known LIDs are resolved to phone JIDs, while unknown LIDs remain unchanged. Every
`Timestamp` is UTC (RFC 3339, `Z`), independent of the host's zone, matching the
store and the CLI's JSON output.

Messages use the stored live message payload documented above:

```json
{"Chat":"15551234567@s.whatsapp.net","ID":"3EB0…","SenderJID":"15551234567@s.whatsapp.net","Timestamp":"2026-07-25T10:00:00Z","FromMe":false,"Text":"hi","ChatName":"Alice"}
```

Media messages include a `Media` object containing only `Type`, `Caption`, `Filename`, `MimeType`, and `FileLength`; messages without media retain `Media: null`. Attachment retrieval fields (`MediaKey`, `DirectPath`, `FileSHA256`, and `FileEncSHA256`) are not exported. Older releases exposed these fields unintentionally: consumers that downloaded from them should use `--download-media` or `media download` instead. Local download and retry data remains available in the store. Review retained webhook logs and queues from older releases for attachment keys.

`EventType: "receipt"` reports delivery and read state for messages you sent. Only
`delivered`, `read`, and `played` cross the webhook; the protocol bookkeeping types
(`sender`, `retry`, `read-self`, `played-self`, `inactive`, `server-error`, `peer_msg`,
`hist_sync`) are dropped at the source so they cannot crowd out real messages. The
`delivered` type is spelled out explicitly, even though WhatsApp sends it as an empty
string on the wire. `MessageIDs` keeps WhatsApp's batching (one POST per receipt, not per message,
minus any blank IDs), and in groups `Sender` is the participant the receipt came
from:

```json
{"EventType":"receipt","Chat":"120363000000000000@g.us","Sender":"15551234567@s.whatsapp.net","MessageIDs":["3EB0…"],"Timestamp":"2026-07-25T10:00:01Z","Type":"delivered","IsFromMe":false}
```

`EventType: "chat_presence"` reports per-chat typing state. `Media` is `audio` while the
contact records a voice message and empty otherwise. Global presence (`online` / last
seen) is deliberately not forwarded:

```json
{"EventType":"chat_presence","Chat":"15551234567@s.whatsapp.net","Sender":"15551234567@s.whatsapp.net","State":"composing","Media":""}
```

## Examples

```bash
wacli sync --once
wacli sync --follow --max-reconnect 10m
wacli sync --follow --stale-threshold 2m
wacli sync --follow --presence-mode quiet
wacli sync --follow --send-spacing 500ms-5s
wacli sync --follow --max-messages 250000 --max-db-size 2GB
wacli sync --once --refresh-contacts --refresh-groups --refresh-channels
wacli sync --follow --download-media
wacli sync --once --events 2>events.ndjson
wacli sync --follow --stale-threshold 2m --events 2>events.ndjson
wacli sync --follow --webhook https://example.com/wacli --webhook-secret "$WACLI_WEBHOOK_SECRET"
wacli sync --follow --webhook https://example.com/wacli --webhook-events message,receipt,chat_presence
```

## Historical execution checkpoints

Sync summaries add the versioned `observations` described in [doctor](doctor.md#retained-connection-and-sync-observations).
The existing `synced`/exit and `app_state.recovery_observations` contracts remain;
`observations.sync` identifies this invocation even if a saved older checkpoint
remains after a persistence failure. `stop_reason` is `idle`, `cancelled`,
`deadline_exceeded`, `logged_out` or `failed`; idle/stopped is not completeness.
Cancellation/logout can still keep the existing successful command exit status.
Startup/connection errors and storage-limit failures do not certify normal sync.

Checkpoints happen at start, binding to a connection execution, observed connection
transitions, history blobs, replay signals, new recovery outcomes/codes, stop and
post-drain cleanup. No diagnostic write is added per message, keepalive, or
HEARTBEAT tick, and no new worker or connection is started. Recovery uses the
existing five collections × seven phases (at most 35 observations), retaining
all completed/failed/cancelled/unconfirmed kinds and fixed diagnostic codes in
that execution. `first_observed_at` and `last_observed_at` bound observations;
`completed_at`, `failed_at`, `cancelled_at`, `unconfirmed_at` retain the most recent
date of each kind. Later completion cannot erase an earlier failure or uncertainty.
A new Sync replaces the old Sync slot rather than accumulating executions.
Preventive shutdown replay debt stays separate and can exist with zero failures.

With `--events`, `sync_started`, `sync_stopped` and
`sync_observations_finalized` add execution correlation. Connection, history,
replay, idle/stop/reconnect/progress and app-state recovery-request lifecycle
signals add `execution_id` and `observed_at` to their existing data. The stopped
event precedes App cleanup; the finalized event and successful CLI summary include
known drained callbacks and final replay restoration. Persistence failure is
explicit even in a finalized event; finalized never promises durable success.
These stderr events are transient diagnostics, not a durable consumer feed.
