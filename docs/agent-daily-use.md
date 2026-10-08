---
title: Agent daily use
description: "A practical daily workflow for agents: account binding, continuous sync, inbox review, drafts, sending, files, audio, and handoff."
---

# Agent daily use

Read this first for routine WhatsApp work. Use the linked command references when a task needs more detail. This guide assumes an existing paired account and describes this fork; check the actual binary's capabilities before using it.

The daily loop is **bind account → keep sync running → select recipient → read context → prepare and review → dispatch → inspect result → finish or hand off**. The [browser CLI coverage checklist](browser-cli-coverage.md) maps every command in the reference browser workflow to this guide, including differences and missing equivalents.

WhatsApp operations belong here. Business rules, approved templates, customer memory, appointment lookup, authorization and work assignment belong to the calling agent/application. Selecting an account or preparing a draft does not authorize sending.

## 1. Bind the account and check local health

Discover capabilities and accounts without an account selector. Then bind every account-scoped invocation explicitly; do not rely on the default account or a previous command's selection.

```bash
wacli --read-only capabilities --agent
wacli --read-only accounts list --json
# Set this from the requested account and reviewed data.accounts, in the same shell.
wacli_account='example-account'
: "${wacli_account:?Bind the reviewed existing account}"
wacli -a "$wacli_account" --read-only --agent auth status
wacli -a "$wacli_account" --read-only --agent doctor
```

Stop on an error before using its output. `-a` requires an existing configured account and refuses conflicting selectors. Account discovery uses legacy JSON; supported task commands use `--agent`. Success JSON is on stdout, errors on stderr; inspect the exit status and typed error. Do not silently switch to legacy sending when agent mode refuses a command.

Doctor checks local auth, archive, search and lock observations. Use `sync status` below for a live owner observation; offline doctor is not that query. Missing/incompatible archives, revoked sessions or historical observations require diagnosis; do not pair, migrate, relink or run a writer automatically to repair a read query. Pairing/setup is a separate authorized task.

References: [accounts](accounts.md), [agent contract](agent.md), [doctor](doctor.md), [auth](auth.md).

## 2. Keep one continuous sync per account

At the start of an authorized live work session, reuse the account's existing compatible sync owner, or start one in a persistent terminal/service owned by the caller:

Bind the same reviewed `wacli_account` in that terminal/service too; shell variables are not carried into a new terminal automatically.

```bash
# Long-running command, in a separate terminal/service; sync does not use --agent.
wacli -a "$wacli_account" sync --follow --presence-mode quiet --events
```

Follow mode connects, ingests available history/events and keeps updating SQLite while the agent works. Local reads see committed updates without acquiring the writer lock. Draft writes, outbound sends and supported chat-state/history actions can use the owner's IPC; do not start a separate sync before each read/send. `quiet` suppresses available presence, which can help preserve primary-phone notifications; it is not a notification guarantee.

Observe the existing follow owner without starting sync, opening databases or making another WhatsApp connection:

```bash
wacli -a "$wacli_account" --read-only --agent sync status --timeout 2s
```

Startup can include migration and app-state recovery. The status socket opens before bootstrap completes; `data.owner_ready=true` means local initialization has returned, the owner answers scoped IPC and has not entered terminal cleanup. `data.transport_connected` separately observes the SDK transport. Neither establishes current authentication, replay completeness or successful sending. A `connected` event, lock, socket file or heartbeat alone is insufficient.

Current authenticated readiness is still unsupported by the pinned SDK: a reachable owner returns `authenticated="unknown"`, strict `ready=false` and `readiness_reason="current_authentication_unsupported"`. Do not wait indefinitely for `ready=true` or block local archive reads because it is false. Local readiness can remain true while disconnected. Inspect state/reason and dated observations; unavailable, incompatible or unsupported IPC stays unknown, and exit 0 alone does not mean ready. There is no automatic wait-for-ready/restart service. Manage the caller's process lifecycle explicitly, and never repeat a possibly dispatched action when a reply is lost.

Owner status uses Unix IPC; Windows reports it explicitly unsupported. The new feature acceptance used offline fixtures, not live WhatsApp or native Windows validation; local/transport observations do not change those verification limits.

`--events` emits lifecycle diagnostics on stderr, not a durable message stream. Follow reconnects within its configured budget; `--max-reconnect 0` permits indefinite reconnect attempts, but logout remains terminal. A completed `sync --once` only ingests what arrived during that run; it does not establish complete history or ongoing freshness. Stop only a process the caller owns when the session ends; leave a shared owner to its supervisor.

For an application that needs to wake agents on new data, consume the durable local feed in a separate managed reader. A finite page can establish or resume its checkpoint; a continuous watcher then waits for new committed changes:

```bash
wacli -a "$wacli_account" --read-only --agent changes list --limit 20
# Continue from the returned meta.page.next_cursor, including an empty/end page.
wacli -a "$wacli_account" --read-only --agent changes list --cursor "$wacli_change_cursor" --limit 200
# In the managed consumer, using the last completely processed page's cursor.
wacli -a "$wacli_account" --read-only --agent --timeout 0 changes watch --cursor "$wacli_change_cursor" --interval 1s --limit 20
```

`changes watch` emits one complete agent page per stdout line (NDJSON). It drains backlog, emits an initial checkpoint even when empty, then waits; `has_more=false` does not end the stream, and unchanged empty polls emit no heartbeat. Default polling is 1s. Explicit `--timeout 0` keeps watching until cancellation; without it, the command defaults to 5m. Watch reads only the existing local archive: it starts no sync, uses no owner IPC and works independently of live authenticated readiness. The follow owner supplies remote ingestion; watch observes only what was committed locally.

The consumer saves `meta.page.next_cursor` only after processing the complete page and handles duplicate event IDs across restarts. List/watch cursors are interchangeable with the same account/store binding. On a partial final line or stream error, resume from the last processed checkpoint; there is no final exit checkpoint or automatic cursor reset. With no cursor, consumption starts at the beginning of the retained feed, not implicitly "from now". This feed covers message mutations and selected receipts, not all chat/contact metadata or a work queue. Let the application consume the stream and wake the agent for relevant work; the agent does not need to remember to sync before every operation.

References: [sync](sync.md), [concurrent use](concurrent-use.md), [changes](changes.md).

## 3. Select a recipient and read the conversation

```bash
wacli -a "$wacli_account" --read-only --agent chats list --unread --no-archived --limit 20
wacli -a "$wacli_account" --read-only --agent chats list --query 'Example contact' --limit 20
wacli -a "$wacli_account" --read-only --agent contacts search 'Example contact' --limit 20
# Bind the exact reviewed returned JID; this value is illustrative.
wacli_chat='15550000002@s.whatsapp.net'
: "${wacli_chat:?Bind the reviewed recipient}"
wacli -a "$wacli_account" --read-only --agent contacts resolve "$wacli_chat"
wacli -a "$wacli_account" --read-only --agent chats show --jid "$wacli_chat"
wacli -a "$wacli_account" --read-only --agent messages list --chat "$wacli_chat" --limit 14
```

- Select by verified identity, not list position or display name alone. A supplied trusted phone can be used as an explicit draft target even if no chat row exists; absent local rows do not prove remote absence. An appointment/customer ID must first be resolved by the calling application to a trusted phone/JID.
- `contacts resolve` observes stored PN/LID mappings. An unresolved LID remains a LID, not a phone number: never infer a phone from its digits. An unknown mapping does not prove a different person or authorize changing the target.
- Recalculate the unread queue after each completed/handoff task. Apply the caller's exclusions and work ownership; for a DM-only queue, exclude group JIDs ending in `@g.us`. Keep handed-off/skipped targets in the caller's round state so marking unread does not immediately select them again. Wacli does not reserve conversations for agents.
- List reads default to newest first. Review direction, timestamps, quoted context, attachments and the current question before replying. Reads do **not** mark the chat read. Unread markers/counts describe WhatsApp state, not whether a worker completed a task.
- Compact detail saves context. If relevant fields are truncated or unclear, retrieve full detail before deciding. For exact locally stored text, `messages show --detail full` exposes `full.content`/`full.caption`; `data.text` is formatted presentation.

```bash
# Set wacli_message_id from the reviewed message, never from a guessed list index.
wacli -a "$wacli_account" --read-only --agent messages show --chat "$wacli_chat" --id "$wacli_message_id" --detail full
wacli -a "$wacli_account" --read-only --agent messages context --chat "$wacli_chat" --id "$wacli_message_id" --before 5 --after 5
wacli -a "$wacli_account" --read-only --agent messages search 'invoice' --chat "$wacli_chat" --sort time --limit 20
```

For more rows, follow `meta.page.next_cursor` with the same account and filters while `has_more` is true. Lists/search are live local reads: concurrent ingestion or identity changes can cause repeats/omissions; restart traversal when that matters. Empty results/end cursors do not certify remote completeness. If expected history is missing, inspect [coverage and recovery](#7-recover-without-guessing).

References: [chats](chats.md), [contacts](contacts.md), [messages](messages.md), [pagination](agent.md).

## 4. Prepare, review, send and inspect

Prepare text locally with real newlines. The caller can render its approved template into a UTF-8 file and pass `--message-file PATH`; wacli does not generate business templates.

```bash
wacli -a "$wacli_account" --agent draft create --to "$wacli_chat" --message-file - --detail full <<'EOF'
Hello! Here are the details we discussed:

Please let us know if you have any questions.
EOF
```

Capture the returned draft ID, immutable revision ID and payload hash. Review the **whole** recipient/account binding, content, paragraph breaks and any quote or attachment. Full output is useful when complete review is already needed. A hash/preview records no approval; apply the caller's authorization rules before dispatch.

```bash
# Bind these values from the returned revision; never substitute a mutable "latest".
wacli -a "$wacli_account" --read-only --agent draft show "$wacli_draft_id" --revision "$wacli_revision_id" --detail full
# Only after review and authorization. Keep one key per logical message.
wacli -a "$wacli_account" --agent outbound send "$wacli_draft_id" --revision "$wacli_revision_id" --expect-hash "$wacli_payload_hash" --key "$wacli_send_key"
# Capture the operation ID from the send result/error correlation, then inspect it.
wacli -a "$wacli_account" --read-only --agent outbound show "$wacli_operation_id" --detail full
```

The idempotency key is caller-assigned ASCII without spaces, 1–128 bytes. Save the account, key, draft/revision/hash and operation ID in the caller's task record. The same retained binding/key returns the original operation without a second application dispatch; another key can duplicate a remote message. This protection depends on the retained archive and does not promise exactly-once delivery after restoration/loss.

Accepted/ACK is not delivered/read. Inspect retained outbound evidence; a local history row or own echo alone does not prove delivery. Unknown/pending/uncertain results are a reason to inspect, not resend. If the operation ID was lost, use `outbound show --key "$wacli_send_key" --account-jid "$wacli_own_pn"`, with the exact frozen own PN JID from the reviewed binding. Preserve any typed error correlation.

To correct a draft, use `draft update DRAFT_ID --if-revision CURRENT_HEAD` with **complete** replacement input, then review the returned new revision/hash. For an unsent abandoned draft, use `draft discard DRAFT_ID --if-revision CURRENT_HEAD`. Discard retains records and does not cancel an already reserved/dispatched operation or clear a WhatsApp Web compositor. For a textual quote, add `--reply-to MESSAGE_ID` during preparation; unsupported quote content is refused rather than silently omitted.

References: [drafts](drafts.md), [outbound](outbound.md).

## 5. Handle contacts, documents, images and audio

These outgoing variants use the same review/send/result workflow above:

```bash
wacli -a "$wacli_account" --agent draft create --to "$wacli_chat" --contact-name "$wacli_card_name" --contact-phone "$wacli_card_phone" --detail full
wacli -a "$wacli_account" --agent draft create --to "$wacli_chat" --file "$wacli_document_path" --filename 'Details.pdf' --mime application/pdf --detail full
wacli -a "$wacli_account" --agent draft create --to "$wacli_chat" --image "$wacli_image_path" --caption 'Requested image' --detail full
```

Bind each variable to reviewed input. Contact cards use an explicit name/phone, not business-specific aliases. Review the shared contact separately from the recipient; the immutable contact-card path has fixture validation, not a live interoperability guarantee. For documents/images, inspect the frozen bytes at the returned snapshot path as well as the filename, size, digest and caption. Documents are capped at 100 MiB; static images are JPEG/PNG only. A file path or hash alone does not prove the content is appropriate. Voice sending is also available as `--voice PATH` for the strict Ogg/Opus PTT profile; conversion and general audio/video sending are outside this draft path.

For received files, select the exact stored chat/message first:

```bash
wacli -a "$wacli_account" --read-only --agent media status --chat "$wacli_chat" --id "$wacli_message_id" --verify
wacli -a "$wacli_account" --read-only --agent media download --chat "$wacli_chat" --id "$wacli_message_id" --output "$wacli_download_path"
```

Explicit-output read-only download works beside a follow owner; it can use the network and write the requested file, but does not update archive download metadata. Use the returned file and verification observations, not an unrelated file from a downloads directory. Missing encrypted-media keys or expired URLs are not fixed by guessing an alias or retrying repeatedly. Exact `media retry` is an authorized recovery action requiring a standalone writer; it does not delegate through a running follow owner.

For audio, validate the selected file/type/duration with local tools, then explicitly invoke an adapter implementing the [transcription protocol](media.md#explicit-local-transcription):

```bash
wacli -a "$wacli_account" --read-only --agent media transcribe --file "$wacli_download_path" --adapter "$wacli_transcription_adapter" --expect-sha256 "$wacli_audio_sha256" --detail full
```

Set the digest from verification of those bytes and the adapter to an approved absolute executable. There is no bundled/default speech engine. Transcription input is capped at 25 MiB; review the output and conversation order before deciding. Read-only does not sandbox the adapter; whether processing stays local depends on that executable. Do not forward audio to another WhatsApp contact as a transcription workaround.

References: [draft variants](drafts.md), [media download/recovery/transcription](media.md).

## 6. Finish or hand off deliberately

Only make the state change the caller's workflow requests:

```bash
wacli -a "$wacli_account" --agent chats mark-read --chat "$wacli_chat"
wacli -a "$wacli_account" --agent chats mark-unread --chat "$wacli_chat"
wacli -a "$wacli_account" --agent chats archive --chat "$wacli_chat"
```

These are separate examples, not a sequence to run on every chat. Agent mark-read uses an exact stored message boundary and no sender receipts; it refuses missing/invalid anchors. Archive also unpins, so check caller exclusions/protected chats before using it. Results distinguish SDK completion, uncertainty and local mirror persistence; they do not certify current remote state. Do not automatically repeat an uncertain mutation.

Record the task outcome, outstanding question and outbound evidence in the caller's memory/work system. Mark-unread is a WhatsApp handoff signal, not that task record. Re-list the queue for the next target. Wacli has no selected conversation, parking chat, modal or browser tabs: record completed and pending drafts/attempts before ending the logical task. Browser cleanup is unnecessary; it is not a reason to logout or delete a session.

References: [chat state](chats.md), [concurrency and work ownership](concurrent-use.md).

## 7. Recover without guessing

For a missing recent interval, first inspect coverage and select a genuine **stored** message after that interval in the exact chat. An authorized explicit backfill requests the window immediately before that anchor:

```bash
wacli -a "$wacli_account" --read-only --agent history coverage --chat "$wacli_chat" --evidence
# Set wacli_anchor_id from a reviewed messages show/list result in this exact chat.
wacli -a "$wacli_account" --agent history backfill --chat "$wacli_chat" --before-id "$wacli_anchor_id" --count 50 --requests 1
```

The explicit anchor must have valid retained timestamp/author/account facts and usable content; an arbitrary ID/date or a message visible only in a browser cannot anchor this request. This mode uses one batch, without alternate-anchor/identity retries. It can delegate through a compatible follow owner; an older owner is refused without silently falling back to the oldest anchor or a second writer. Without `--before-id`, default backfill still starts before the oldest local message.

Inspect the returned `before_id`, `messages_added_before`, attempt correlation and coverage evidence. Keep the explicit action result with the attempt ID: coverage evidence does not separately retain the selector mode/window count. Net growth before the anchor is an observation, not a completeness certificate; zero growth does not prove unavailability. The request cannot recover messages after its anchor, automatically detect every gap or guarantee that the phone returns the missing message. Do not repeat a refused/uncertain request automatically.

| Observation | Next step |
| --- | --- |
| Wrong account, ambiguous name, unresolved phone, contradictory recipient facts | Stop before effects. Rebind from trusted identity; do not guess a mapping or target. |
| Relevant text truncated/unclear | Retrieve the exact full message/revision and surrounding context. |
| Missing expected messages | Inspect coverage/evidence, then explicitly choose default oldest-anchor backfill or one `--before-id` window as above. Missing history never authorizes another send. |
| Old/missing store or revoked session | Diagnose with offline doctor/auth status; authorize setup/upgrade separately. |
| Writer lock, missing/incompatible owner, startup not finished | Inspect `sync status` and the managed process; distinguish local initialization, transport and unknown authentication. Do not remove LOCK, start a competing connection or kill a shared owner. |
| Watch idle, timeout or broken output | Idle is not a freshness/liveness check. Handle complete frames, retain the last processed cursor and resume explicitly; do not reset it or save partial JSON. |
| Local draft write lost its reply | Inspect correlated draft/revision IDs; do not automatically recreate/update it. |
| Send timeout, lost reply, pending/uncertain result | Inspect outbound by operation ID or key + frozen own PN. Do not switch keys, use legacy sending or replay to resolve uncertainty. |
| Chat-state action uncertain | Inspect `error.chat_state`, local `chats show` and dated observations; timeout is not rollback. |
| File unavailable or adapter refuses input | Inspect exact media/file observations and the adapter contract; arrange explicit recovery or hand off. |

References: [history](history.md), [outbound evidence](outbound.md), [media](media.md), [doctor](doctor.md). For the complete command/flag contract, use [agent reference](agent.md) and command help.
