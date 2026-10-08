---
title: Quickstart
description: "Pair as a linked WhatsApp Web device, sync, search, and send your first message in under five minutes."
---

# Quickstart

Five minutes from a clean machine to authenticated sync, search, and send. For deeper reading, follow the links at the bottom of each step.

Already paired and working as an agent? Start with [Agent daily use](agent-daily-use.md) for the routine workflow and [browser CLI coverage](browser-cli-coverage.md) for task equivalents and limitations.

## 1. Install

```bash
brew install openclaw/tap/wacli
wacli --version
```

Other options (release archives, source builds, GCC 15 notes) are documented on [Install](install.md).

## 2. Pair as a linked device

Choose the account explicitly. Bind `wacli_account` to the exact requested name reviewed in `data.accounts` (or an intentionally chosen new name for pairing). `example-account` below is illustrative, not a default. Keep the selected variables in the same Bash session for the later examples:

```bash
wacli --read-only accounts list --json || exit "$?"
wacli_account='example-account' # Replace with the reviewed existing or intended new name.
: "${wacli_account:?Choose an account name}"
```

Agents supply the binding from validated task input/account output without prompting. A human may optionally replace the assignment with `read -r -p 'Account name: ' wacli_account`, then check it against that output. To inspect an existing archive without pairing or syncing, go directly to [step 4](#4-search-and-read) with the reviewed binding.

For a **new** account, create its isolated store and start pairing:

```bash
wacli accounts add "$wacli_account"
```

For an **existing** account that needs pairing, use this instead; skip pairing if it is already authenticated:

```bash
wacli --account "$wacli_account" auth
```

Both use the normal `auth` flow, which prints a QR code in your terminal. On your phone, open WhatsApp → **Linked devices** → **Link a device**, scan the QR, and approve. As soon as pairing succeeds, `auth` immediately starts the initial sync — keep it running until it idles out or press `Ctrl+C` once it has caught up.

If the terminal QR does not scan, try `--qr-format text` and render that raw QR payload in another app, or pair via phone-number code with `--phone +15551234567`.

> Refresh tokens last as long as the linked device stays linked on your phone. Unlinking from the phone (or `wacli auth logout`) ends the session and requires a fresh QR.

Verify:

```bash
wacli --account "$wacli_account" --read-only auth status
```

## 3. Keep the store warm

Use a separate Bash terminal for this foreground process. Select the same account there; the original terminal retains the variables needed for reading and later examples:

```bash
wacli --read-only accounts list --json || exit "$?"
wacli_account='example-account' # Replace with the exact name selected in step 2.
: "${wacli_account:?Select the intended account}"
wacli --account "$wacli_account" sync --follow
```

`sync` never shows a QR; it requires a previously paired session and runs until you stop it. `--once` exits after one idle window; `--follow` reconnects on errors. Both honor `--max-messages` / `--max-db-size` (and the `WACLI_SYNC_MAX_*` env equivalents) so the local store stays bounded.

See [Sync](sync.md) for refresh-contacts/refresh-groups, `--download-media`, and the idle-exit knobs.

## 4. Search and read

Tables are the default; `--full` keeps full IDs in tables and legacy `--json` remains available. The selection examples below use `--agent` to expose complete IDs and bounded results. Review the account and returned identities before copying them; no example guesses a message ID. An empty response or error is a stopping point, not permission to invent one.

```bash
# Full-text search (FTS5 when the binary was built with -tags sqlite_fts5; LIKE otherwise)
wacli --account "$wacli_account" --read-only --agent messages search "meeting"

# Search media-bearing messages
wacli --account "$wacli_account" --read-only --agent messages search "meeting" --has-media

# Select a chat from data.chats[].jid (or a search result's chat_jid).
wacli --account "$wacli_account" --read-only --agent chats list || exit "$?"
```

Bind `wacli_chat` to the exact reviewed `data.chats[].jid` or search result's `chat_jid`. The caller supplies this binding from that response; stop on empty/error output or an ambiguous selection.

```bash
: "${wacli_chat:?Bind the reviewed chat JID from the preceding response}"

# List messages from that chat, oldest first; select data.messages[].id.
wacli --account "$wacli_account" --read-only --agent messages list --chat "$wacli_chat" --asc || exit "$?"
```

Bind `wacli_message_id` to the exact reviewed `data.messages[].id` from that chat response. Keep both returned identifiers unchanged; the account/chat combination is part of the selection.

```bash
: "${wacli_message_id:?Bind the reviewed message ID from that chat response}"

# Show that exact message with full agent detail.
wacli --account "$wacli_account" --read-only --agent messages show --chat "$wacli_chat" --id "$wacli_message_id" --detail full || exit "$?"

# Show context around a message
wacli --account "$wacli_account" --read-only --agent messages context --chat "$wacli_chat" --id "$wacli_message_id" --before 5 --after 5
```

On supported lists, follow `meta.page.next_cursor` unchanged while keeping the same selection and filters. Local exhaustion, counts and dates do not establish complete/fresh remote history. Read [Messages](messages.md) for filters and the [agent contract](agent.md) for errors and compact/full recovery; `--full` does not select full agent detail.

## 5. Send a message

The following are separate live actions: run only the ones you intend and are authorized to perform. Review the selected account, recipient and complete content first. Replies/reactions below target the exact chat/message inspected in step 4; posting status sends a broadcast. File examples require the intended local files to exist.

```bash
# Choose the intended recipient: a phone, JID, or synced contact/group/chat name.
: "${wacli_recipient:?Bind the explicitly authorized recipient from task input or reviewed resolution}"
wacli --account "$wacli_account" send text --to "$wacli_recipient" --message "hello"

# Send a quoted reply
wacli --account "$wacli_account" send text --to "$wacli_chat" --message "replying" --reply-to "$wacli_message_id"

# Send a file with a caption
wacli --account "$wacli_account" send file --to "$wacli_recipient" --file ./pic.jpg --caption "hi"

# Send a 512x512 WebP sticker
wacli --account "$wacli_account" send sticker --to "$wacli_recipient" --file ./sticker-512.webp

# Send a native voice note (OGG/Opus)
wacli --account "$wacli_account" send voice --to "$wacli_recipient" --file ./voice.ogg

# React (omit --reaction for the default thumbs-up; use --reaction "" to clear)
wacli --account "$wacli_account" send react --to "$wacli_chat" --id "$wacli_message_id" --reaction "🎉"

# Post a WhatsApp status broadcast
wacli --account "$wacli_account" send status --message "available today" --background-color '#1f7a8c'
```

Recipient resolution and disambiguation (`--pick N`, ambiguous-name prompts), link-preview behavior, status broadcasts, and post-send waits are documented in [Send](send.md).

For agents preparing an exact revision for later review/dispatch, use [local drafts](drafts.md) and [outbound](outbound.md). These legacy `send` examples are outside the agent contract. No preview/hash records approval, and an uncertain result is not permission to resend automatically.

## 6. Backfill older history (optional, best-effort)

`sync` only stores what WhatsApp Web pushes. To request older messages for a specific chat from your **primary device** (your phone), use:

```bash
wacli --account "$wacli_account" --read-only history coverage --include-blocked
wacli --account "$wacli_account" --read-only history fill --dry-run --limit 20
wacli --account "$wacli_account" history backfill --chat "$wacli_chat" --requests 10 --count 50
```

The phone must be online for `backfill`. WhatsApp may not return full history. See [History](history.md) for coverage planning, limits, and patterns.

## 7. Named accounts (optional)

If you run more than one WhatsApp number, named accounts give each an isolated store, session, and lock:

```bash
# Add a second account and pair it immediately
wacli accounts add work

# List all configured accounts
wacli --read-only accounts list --json

# Use a named account with any command
wacli --account work sync --follow
wacli --account work --read-only chats list
```

Use `--no-auth` to create the account entry without pairing immediately. Two accounts can sync concurrently — their locks are independent. See [Accounts](accounts.md) for YAML config and migration from manual `--store` paths.

## 8. Diagnostics and safety

```bash
wacli --account "$wacli_account" --read-only doctor
wacli --account "$wacli_account" doctor --connect

# Read-only mode for agents / sandboxes
wacli --account "$wacli_account" --read-only --agent messages search "invoice"
WACLI_READONLY=1 wacli --account "$wacli_account" send text --to "$wacli_recipient" --message "hi"   # exits with a clear error
```

`doctor` checks the store, schema, FTS5 availability, and (with `--connect`) live connectivity. See [Doctor](doctor.md).

Readonly rejects intentional WhatsApp mutations and archive writes; it does not mean every command is offline or has no filesystem effects. Requested exports/download output and explicit adapter execution remain permitted; downloads may use the network and adapters are not sandboxed. SQLite may update WAL/SHM bookkeeping. See [local reads](store.md#local-reads-by-default) and [media](media.md).

## 9. Shell completion (optional)

Choose the example for your shell; these optional commands write completion files:

```bash
wacli completion bash >> ~/.bash_completion
```

```zsh
wacli completion zsh > "${fpath[1]}/_wacli"
```

```fish
wacli completion fish > ~/.config/fish/completions/wacli.fish
```

## Where next

- [Overview](overview.md) — global flags, store model, full command map.
- [Accounts](accounts.md) — named accounts, isolated stores, YAML config.
- [Send](send.md) — every recipient form, replies, reactions, mentions, link previews.
- [Channels](channels.md) — read and follow WhatsApp Channels.
- [Groups](groups.md) — list, refresh, info, rename, participants, invite links.
- [Spec](spec.md) — design notes, storage layout, locking model, non-goals.
- [Doctor](doctor.md) — self-checks and connectivity probe.
