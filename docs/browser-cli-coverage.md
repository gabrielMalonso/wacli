---
title: Browser CLI coverage
description: "Coverage of a reference browser-based service-desk workflow, with WACLI equivalents and explicit limitations."
---

# Browser CLI coverage

This is the coverage checklist for [Agent daily use](agent-daily-use.md), not a claim of identical behavior or complete WhatsApp history. The baseline is the service-desk `whatsapp_cli` command surface and WhatsApp Web operations manual reviewed on **2026-10-08** (`tools/cli/python/whatsapp_cli/__main__.py`, `tools/cli/docs/whatsapp-cli.md`, and `central/sistemas/whatsapp-web.md` in the separate central repository). No central configuration or customer data is required to use wacli.

The wacli side includes the merged live owner status, explicit local history anchor and continuous cursor watcher. Examples describe the merged source; verify the installed binary and restart a managed older follow owner explicitly before relying on its new IPC capabilities.

**Available** means a documented wacli operation exists. **Different** means the task is supported with different state/evidence semantics. **Caller integration** belongs to the future companion application. **Missing** means this fork has no equivalent. **UI-only** means the browser mechanism is unnecessary in a headless workflow. These labels describe implementation coverage, not live acceptance of every operation.

| Browser command(s) | Coverage | Wacli daily workflow and limits |
| --- | --- | --- |
| `status` | Different | [Account/health checks](agent-daily-use.md#1-bind-the-account-and-check-local-health): `auth status`, offline `doctor`, plus live `sync status` for local owner readiness/transport. Current authentication remains unknown; no current tab/header. |
| `doctor` | Different | Offline [doctor](doctor.md) checks store/auth/FTS/locks and dated observations. Legacy `doctor --connect` requires a standalone writer and is not supported under `--agent`. No equivalent one-command browser UI/full send check-up; an authorized test uses the ordinary reviewed send workflow. |
| `unread`, `open-next-unread` | Different | [Inbox review](agent-daily-use.md#3-select-a-recipient-and-read-the-conversation): `chats list --unread --no-archived`, choose a returned JID, read messages, then re-list. Local unread state can lag; no task reservation or automatic read-on-open. Caller tracks handoffs/exclusions. |
| `list-visible`, `scan-open` | Different | Paginate `chats list` over the local archive rather than DOM windows. Local exhaustion proves neither all remote chats nor fresh state. |
| `filter` (`all`, `unread`, `groups`) | Different | Query all chats or use `--unread`; the caller can classify group JIDs by `@g.us`. No persistent UI filter or dedicated group filter is required. |
| `filter` / list/scan with `favorites` | Missing | No documented WhatsApp favorite-chat filter. Pinned chats, starred messages and local contact tags have different meanings; do not substitute them silently. |
| `open-phone`, `open-visible`, `open-unread`, `open-visible-index` | Different | Select a trusted phone or reviewed returned JID explicitly. There is no global selected chat or stable row index. A new target may have no local history; immutable sending requires the recipient identity facts specified by [outbound](outbound.md). |
| `open-agenda` | Caller integration | Appointment ID → trusted recipient must come from the companion application. Wacli then reads/prepares with that explicit phone/JID. No appointment lookup is built in. |
| `inspect`, `current` | Available | `chats show`, `messages list/show/context`; compact/full detail and exact stored text are covered in [conversation reading](agent-daily-use.md#3-select-a-recipient-and-read-the-conversation). |
| `brief` | Caller integration | Wacli supplies bounded conversation/identity data. The combined business/customer-memory decision packet must be assembled by the caller; no central memory backend is built in. |
| `open-contact-panel`, `contact-phone` | Different | `contacts show/resolve` observes stored PN/LID facts without opening a panel. An unknown PN remains unknown; local resolution is not a fresh remote profile inspection. |
| `paste-draft` | Available | [Prepare a draft](agent-daily-use.md#4-prepare-review-send-and-inspect) with `draft create --message-file PATH` or stdin (`-`), preserving UTF-8 and real newlines without browser clipboard handling. |
| `template-draft` | Caller integration | The caller renders the approved business template; `draft create` freezes the resulting text. No template generator or appointment variables are built in. |
| `clear-draft` | Different | `draft discard --if-revision` abandons a local wacli draft and retains evidence. It does not clear another linked device's UI composer or cancel a dispatched operation. |
| `preflight` | Different | Review the exact full immutable draft/account/recipient/content/quote/file, then supply its revision/hash for dispatch. No header/DOM/composer preflight is needed; neither preview nor hash records approval. |
| `send-draft`, `safe-reply` | Available | `draft create/show` → authorized `outbound send` → `outbound show`, with an exact revision/hash and retained key. No one-call `safe-reply` equivalent. ACK, echo, delivery and read remain separate observations. |
| `confirmation-transaction` | Caller integration | Per-message idempotency/evidence exists. Appointment/patient/date/template binding, ordered multi-message completion, transaction receipt and handoff must be coordinated by the companion application. No atomic multi-message business transaction is built in. |
| `send-contact` | Available | [Explicit contact draft](agent-daily-use.md#5-handle-contacts-documents-images-and-audio) with a reviewed card name/phone; no central-specific contact nicknames. Immutable card interoperability has fixture validation, not live verification. |
| `send-document` | Available | Immutable document snapshot, reviewed filename/MIME/bytes, then ordinary outbound dispatch. The path supports documents beyond the browser baseline's PDF command. |
| `download-audio`, `download-attachment` | Available | `messages show` → `media status` → explicit-output read-only `media download` for the exact chat/message. File verification replaces browser download-event correlation. Expired/missing media can require explicit recovery. |
| `transcribe-audio` | Different | Explicit download + file validation + `media transcribe --adapter`; no bundled/default speech engine. The caller supplies a protocol-compatible adapter and reviews its output. |
| `mark-unread` | Available | [Handoff](agent-daily-use.md#6-finish-or-hand-off-deliberately): `chats mark-unread --chat JID`; marker is not a caller task record or an invented unread count. Inspect mutation evidence. |
| `archive-current` | Different | `chats archive --chat JID`; caller reviews target, drafts/attempts and protected chats. Archive unpins; wacli does not enforce the browser's protected parking-chat policy. |
| `park` | UI-only | No selected UI conversation. Finish the logical task, record pending work and preserve explicit recipient binding for the next task. |
| `cleanup-tabs` | UI-only | No browser tabs/deep links. Do not replace cleanup with logout, session deletion or killing another process. |

## Boundaries to carry into an integration

- **Continuous updates:** manage/reuse one [follow owner](agent-daily-use.md#2-keep-one-continuous-sync-per-account), observe it with `sync status`, and optionally consume [continuous durable changes](changes.md#continuous-local-watch) with saved cursors. Status distinguishes local initialization from transport and unsupported current authentication. Watch observes local commits; it is not remote ingestion, a heartbeat or a complete chat-state notification stream.
- **Historical coverage:** [history recovery](history.md#explicit-recovery-anchor) can use `--before-id` for one window before a genuine stored message, or default to the oldest local anchor. Local mirror gaps can remain even when the browser displays a message; browser-only/arbitrary IDs cannot anchor recovery, and no completeness certificate is provided.
- **Customer/appointment work:** account routing, blacklist/protected chats, templates, external memory, worker ownership and business transactions remain caller responsibilities. This separation is intentional for a generic WhatsApp tool.
- **Recovery:** use the [daily recovery table](agent-daily-use.md#7-recover-without-guessing). Unknown outcomes never authorize a second send. Browser fallback, if chosen by the caller, must reconcile the same logical operation before any external effect.

For capabilities beyond this baseline (groups, channels, reactions, other legacy sends), consult the [full command overview](overview.md) and [agent capability discovery](agent.md#static-capability-discovery). Legacy availability does not imply support under `--agent`.
