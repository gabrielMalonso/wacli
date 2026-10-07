# chats

Read when: listing known chats, filtering chat state, archiving/pinning/muting/marking chats, or pruning stale local chat rows.

`wacli chats` reads chat rows from `wacli.db`. It can use session-backed PN/LID mappings to make historical `@lid` chat rows display as phone-number chats when possible. State commands normally send WhatsApp app-state patches through the authenticated session and update the local index after WhatsApp accepts the change. Explicit receipt mode uses the independent network receipt path described below.

Archive and pin persistence reads the SDK's public local ChatSettings cache for the exact observed JID at its ordered persistence turn. This also preserves the SDK's unpin on archive, even if a callback arrives after a newer replay. Missing settings or a cache read error leave recovery debt instead of guessing a value or another identity. The cache is not a fresh remote-state guarantee; unread remains driven by its separate events and read boundaries. Recovery reads state and never repeats a user's mutation.

## Commands

```bash
wacli chats list [--query TEXT] [--limit N] [--archived|--no-archived] [--pinned|--no-pinned] [--muted|--no-muted] [--unread|--no-unread]
wacli chats show --jid JID
wacli chats archive --chat CHAT [--pick N]
wacli chats unarchive --chat CHAT [--pick N]
wacli chats pin --chat CHAT [--pick N]
wacli chats unpin --chat CHAT [--pick N]
wacli chats mute --chat CHAT [--duration DURATION] [--pick N]
wacli chats unmute --chat CHAT [--pick N]
wacli chats mark-read --chat CHAT [--pick N] [--receipts]
wacli chats mark-unread --chat CHAT [--pick N]
wacli chats cleanup [--days N] [--jid JID] [--dry-run] [--confirm]
```

## Agent pagination

`--agent chats list` defaults to 20 rows (1–200), always returns `meta.page`, and accepts `--cursor TOKEN` with any existing query/state filters. Keep store/query/filters unchanged; limit and `--detail compact|full` may change. `has_more` observes one extra local row; empty/exact-limit ends return a null cursor, with completeness/freshness still unknown.

Agent pages preserve raw stored PN/LID identities and counts. Order is normalized pin DESC (NULL/zero → 0, nonzero → 1), stored activity DESC (NULL → 0, negative values preserved), then stored JID BINARY ASC. Unknown/nonpositive timestamps still display as null. The anchor does not need to survive deletion. Legacy JSON/tables and their display resolution/ordering stay unchanged.

The separate strict/versioned chat token is compact for ordinary identities, with an exceptional ceiling of 16 KiB; message tokens cannot be exchanged with chat tokens. Scope binds the absolute selected store, effective literal query and every tri-state state filter (unset differs from false); malformed or mismatched tokens return `invalid_cursor` exit 2 without echoing the token. Whitespace-only queries disable matching; otherwise percent, underscore and backslash match literally, preserving input whitespace in the scope. A stored identity that cannot generate a supported bounded continuation returns `internal_error` exit 1 with stdout empty, no JID echo and no invalid next page.

Agent filters normalize NULL/zero flags to false and nonzero to true. Muted means forever (-1) or a deadline strictly after one current time per query; NULL, zero, other negatives and equality/earlier deadlines are unmuted. Time is re-evaluated each page, so expiration changes membership without a write. Reads are live: sync identity fusion, pin/activity/state/name changes can move/remove rows and cause repeats/omissions. Restart for a fresh traversal; local end is not evidence of complete remote history.

No OFFSET, schema or index is added. SQL returns limit+1 rows to Go, but SQLite may scan/sort candidates each page. See [the agent contract](agent.md#local-chat-pagination) for token and live-read details.

## Explicit agent state changes

`--agent` permits only `mark-unread`, `archive` and `unarchive`, with explicit phone/DM/group JID and no names or `--pick`. They use the existing app-state recovery/semaphore, outside owner send pacing/queueing; legacy mark-read/unread queue behavior below is unchanged. Results distinguish SDK completion, uncertain invocation and local mirror persistence without confirming current remote state. Read-only flag/env rejects these actions before effects. See [the agent contract](agent.md#explicit-unread-archive-actions) for identity, deadlines, IPC errors and late persistence.

## Notes

- `list` is local and sorted by pinned chats first, then newest known message timestamp. Legacy display resolution keeps that priority after PN/LID rows are fused; equal pin/activity keys retain their incoming order. Fusion continues to retain the first row's pin/archive/mute flags and the newest activity, without inferring remote state.
- WhatsApp system events, such as a changed security code or a group notice, arrive as payloads with no content and are stored as `(message)` rows. They do not set that timestamp, so they cannot move a chat up the list; a chat that holds nothing else has no timestamp and sorts last.
- On the next writable open, activity matching the newest locally stored message is recomputed from stored content. Activity newer than every local message is preserved. If an unstored message advertised by history has the exact same second as a local placeholder, the repair uses local content; syncing that message restores its activity.
- `--query` filters by chat name or JID.
- `list --json` and `show --json` include `archived`, `pinned`, `muted_until`, `unread`, and `unread_count`; `unread` is true for counted unread messages and marker-only unread chats, while `unread_count` only counts unread messages.
- `list --unread` matches counted and marker-only unread chats; `list --no-unread` excludes both.
- Replayed read signals reduce the unread count only through the messages they cover; older reads cannot restore already-read messages. Known message IDs distinguish arrivals in the same second, using their local insertion order. Without a known ID boundary, messages at the cutoff second remain unread. Content-free system events, reactions, and revocations do not add to live unread counts.
- `mark-unread` sets the unread marker without inventing an unread count; plain `mark-read` clears the marker and count through one captured local message boundary; arrivals beyond it, including later insertions in the same second, remain unread. A missing/unreadable anchor, nonpositive timestamp or invalid message key now refuses the operation before its mutation and local clear, including in empty or already-read chats. Connection/recovery may already have persisted other state; refusal is not rollback. No clock-based boundary is substituted. Receipt mode keeps its separate selection policy below.
- Reading a chat on the phone clears it here too while `sync` is connected. WhatsApp reports that read as a `read-self` receipt only while read receipts are turned off; with them on it arrives as an ordinary read receipt sent by this account, and both are honoured.
- `show` accepts the stored JID. If a phone JID maps to a historical `@lid` row, it can show that row too.
- Legacy state commands use `--chat` and resolve names, phone numbers, groups, and JIDs like send commands. Use `--pick N` for ambiguous matches.
- After a same-store `sync --follow` process finishes startup and opens its local delegate socket, all state commands are delegated to it while it owns the store lock.
- Legacy `mark-read` and `mark-unread` run in the follow process's serialized delegate queue. `archive`, `unarchive`, `pin`, `unpin`, `mute`, and `unmute` run outside it, so an app-state sync or recovery before the change cannot hold queued sends; the caller's `--timeout` still bounds the operation.
- Restart an older `sync --follow` process after upgrading before using delegated state commands; older daemons return an unsupported `mark_read` or `chat_state` kind error.
- State commands print a compact success line by default and a stable JSON object with `--json`.
- `mute --duration 0` or omitting `--duration` mutes forever. Use `unmute` to clear it.
- Run `wacli sync` to catch up chat-state changes made on other devices; run `wacli contacts refresh` to improve chat names.
- `cleanup` only deletes local `wacli.db` rows. It does not delete chats or messages from WhatsApp.
- `cleanup --days N` skips chats with no known local activity timestamp; use `--jid` for an explicit local row.
- Use `cleanup --dry-run` before deleting and `--confirm` only for scripts that already reviewed the target list.

## Read receipts

Use `wacli chats mark-read --chat CHAT --receipts` to send receipts for stored unread incoming messages. Plain `mark-read` keeps its existing app-state behavior and does not notify message senders. `mark-unread` remains available through a running sync process.

Receipt mode does not wait for app-state recovery, so it can run while the primary phone cannot provide an app-state snapshot. It works directly or through a same-store `sync --follow` process. Restart older sync processes after upgrading: the separate receipt request is rejected by an older daemon before it changes the chat.

Each call handles at most 100 messages, starting with the oldest messages in the current unread selection. Repeat the command for larger counts; unsent messages and arrivals during dispatch remain unread. Reactions, outgoing messages, content-free placeholders, and deleted messages are excluded before the limit. Group messages are batched by sender. Missing group sender identities or an unread count larger than the eligible local history fail before dispatch; sync the missing history first. A marker-only chat uses its latest eligible incoming message, or clears only the local marker when no such message is stored.

WhatsApp's read-receipt privacy setting is respected and never changed. JSON output adds `receipts`, the number of dispatched messages. When nonzero it also includes `receipt` and `sender_notified`:

- `receipt: "read-self"`, `sender_notified: false`: explicitly sent only to the account's own devices, including when receipts are disabled.
- `receipt: "unknown"`, `sender_notified: null`: normal receipts were requested, but the transport may apply a newer privacy setting and does not report the emitted type. This is not confirmation that a sender saw blue ticks. Mixed group-batch outcomes are also reported as unknown.

The local read boundary advances after every selected batch has dispatched successfully. If a later batch fails, the error reports how many messages were already dispatched; retrying may resend those receipts. Receipt mode does not send an additional chat-state patch. `--read-only` rejects it because sending receipts changes WhatsApp state.

## Examples

```bash
wacli chats list
wacli chats list --query family --limit 20
wacli chats list --pinned
wacli chats show --jid 1234567890@s.whatsapp.net
wacli chats mute --chat "+1 555 123 4567" --duration 8h
wacli chats mark-read --chat family --pick 1
wacli chats mark-read --chat family --pick 1 --receipts --json
wacli chats cleanup --days 365 --dry-run
```
