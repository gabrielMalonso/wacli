---
title: Overview
permalink: /
description: "wacli is a single Go CLI that pairs as a linked WhatsApp Web device, mirrors message history into local SQLite with FTS5 search, and exposes send, media, contact, and group workflows for terminals, scripts, and coding agents."
---

# wacli

A script-friendly WhatsApp CLI built on [`whatsmeow`](https://github.com/tulir/whatsmeow). One binary pairs as a linked WhatsApp Web device, syncs messages and status broadcasts into a local SQLite store, and exposes search, send, media, contact, and group commands with predictable output for terminals, shell pipelines, and coding agents.

## Why wacli

- **Local mirror, fast search.** All synced messages land in a SQLite store with an FTS5 index; offline `messages search` returns hits in milliseconds.
- **Chat state controls.** Archive, pin, mute, and mark chats read/unread from the CLI, then filter `chats list` by those states.
- **Stable output.** Human-readable tables by default, `--json` to stdout for scripts, NDJSON `--events` for long-running commands. Human progress, prompts, and errors stay on stderr so pipes stay clean.
- **Single binary.** No daemon, no plugin host. Run `wacli auth`, then `wacli sync --follow` to keep the store warm.
- **Built for agents.** `--read-only` (or `WACLI_READONLY=1`) rejects intentional WhatsApp mutations and archive writes by wacli. Requested [export files](messages.md#export), [download output](media.md#download), [explicit adapter execution](media.md#explicit-local-transcription), and [SQLite WAL/SHM bookkeeping](store.md#local-reads-by-default) remain permitted. Downloads may use the network; the chosen adapter is not sandboxed and may make network requests or file writes. Store locks prevent two instances from racing on the same device identity.
- **Boundable storage.** `sync` warns when storage is uncapped; `--max-messages` / `--max-db-size` cap local growth. Send retries are bounded; media uploads/downloads cap at 100 MiB.
- **Best-effort history.** `history coverage` shows local anchors, `history fill --dry-run` plans candidate chats, and `history backfill` requests older messages per chat from your primary device.

## Pick your path

- **Trying it.** Read [Install](install.md), then [Quickstart](quickstart.md). Pair, sync, and send your first message in under five minutes.
- **Daily agent work.** Start with [Agent daily use](agent-daily-use.md) for inbox review, continuous sync, drafts, sends, files/audio and handoff; use [Browser CLI coverage](browser-cli-coverage.md) to check workflow equivalents and limitations.
- **Using multiple WhatsApp accounts.** Read [Accounts](accounts.md) for named account stores and `--account`.
- **Searching old chats.** Read [Sync](sync.md) for the sync model and [History](history.md) for coverage planning and on-demand backfill.
- **Managing chat state.** Read [Chats](chats.md) for archive, pin, mute, and read/unread commands.
- **Managing local storage.** Read [Store](store.md) for stats, dry-run cleanup, and local-only pruning.
- **Preparing offline previews.** Read [Local drafts](drafts.md) for durable revisions and document snapshots without sending.
- **Inspecting retained attempts.** Read [Outbound operations](outbound.md) for offline evidence queries and explicit dispatch of a reviewed revision/hash.
- **Sending from scripts.** Read [Send](send.md) for recipient resolution, channels, status broadcasts, replies, mentions, files, and reactions.
- **Mirroring address-book names.** Read [Contacts import-system](contacts-import-system.md) to import macOS Contacts display names into local wacli metadata.
- **Wiring up an agent.** Use the [Agent output contract](agent.md) for bounded offline queries, explicit archive identity, and stable errors; read [Doctor](doctor.md) for self-checks.
- **Checking the fork offline.** Use [Offline acceptance](offline-acceptance.md) for the generic requirement matrix and fixture recipe, and [Fork maintenance](fork-maintenance.md) for reviewed upstream updates.
- **Building companion tools.** Read [Companion integrations](integrations.md) for safe read-only SQLite and JSON integration patterns.
- **Looking up a flag.** Open the per-command pages from [Overview](overview.md).

## Status

Core implementation is in place. The [CHANGELOG](https://github.com/openclaw/wacli/blob/main/CHANGELOG.md) tracks shipped behavior. WhatsApp Web is not a published API; expect occasional breakage from upstream protocol changes — `wacli` follows `whatsmeow` upstream.

## Out of scope

- Guaranteed full-history export (WhatsApp Web history is best-effort).
- A daemon, MCP server, web UI, or GUI.
- End-to-end "contact creation" inside WhatsApp; local aliases and tags only.

## Disclaimer

`wacli` is a third-party tool that uses the WhatsApp Web protocol via `whatsmeow`. It is **not affiliated with WhatsApp or Meta**. Use at your own risk; pairing as a linked device is subject to WhatsApp's terms.

Released under the [MIT license](https://github.com/openclaw/wacli/blob/main/LICENSE).
