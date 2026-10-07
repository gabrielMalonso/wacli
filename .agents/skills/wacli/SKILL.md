---
name: wacli
description: "Use wacli for account selection, local WhatsApp archive queries, explicit draft/outbound/media actions, and repository or release work."
---

# Wacli

Use the installed `wacli`, or `./dist/wacli` after building the current checkout. Check `--version` and command `--help` against the documentation for that build. Repository work follows [AGENTS.md](../../../AGENTS.md); see the [command map](../../../docs/overview.md) for task-specific pages.

Discover this binary's static CLI/agent support before account selection:

```sh
wacli --read-only capabilities --agent
```

Run unbound, without account/store selectors. Executing discovery rejects explicit selectors; textual help may ignore legacy `--account`/`--store` without resolving selection or accessing config/store, while `-a`/`--for-account` remains refused for discovery help. Discovery opens no config/store/session and reports `account_availability="unknown"`; static requirements and `read_only` policy are not evidence of readiness, authorization or zero external effects. Check the contract version and requested command's `agent_mode`; refuse unknown/unsupported modes rather than falling back to a mutation. Use command help and the [discovery contract](../../../docs/agent.md#static-capability-discovery) for restrictions and limits.

## Select an account and read

Discover existing account names and resolved stores with legacy JSON; `accounts` is outside the agent contract:

```sh
wacli --read-only accounts list --json
```

Bind `wacli_account` to the exact listed name requested by the task, then keep it explicit on each operation. For example, if the requested/listed name is `example-account`:

```bash
wacli_account='example-account' # Replace with the reviewed name from data.accounts.
: "${wacli_account:?Select an existing account}"
wacli -a "$wacli_account" --read-only --agent auth status
wacli -a "$wacli_account" --read-only --agent doctor
wacli -a "$wacli_account" --read-only --agent messages list --limit 20
wacli -a "$wacli_account" --read-only --agent messages search 'query' --sort time
```

`-a NAME` / `--for-account NAME` requires the exact existing name, overrides env/default and fixes the resolved selection for this invocation. A different/empty `--account`, a different binding or any `--store` is refused before command effects, even with help/version; equal repetitions are allowed. Selection/config errors exit 4 in agent mode; invalid names/conflicts exit 2. Inspect and correct the selection explicitly, without fallback or pairing. Global `accounts` calls run without binding. Legacy `--account` keeps its previous selection behavior; selecting either way grants no mutation authorization.

For a manual archive, use an explicit `--store DIR` instead of named selection; do not combine them. Do not guess paths: account config, XDG state roots, custom stores and binding limits follow [account selection](../../../docs/accounts.md). `accounts show NAME --json` returns `data.store_dir`; agent envelopes identify the selected archive with `account.name` when applicable and `account.store_ref`.

## Output, certainty and recovery

- Use `--agent` for supported queries and explicit actions in the [v1 contract](../../../docs/agent.md). Success is one JSON line on stdout; failure is one on stderr with a nonzero exit. Read `error.code` and any recovery guidance, not only the text. Usage/policy errors exit 2, missing local items 3, store failures 4, and operational failures 1; action-specific codes are documented on their pages.
- Legacy `--json` remains useful for accounts and commands outside that contract. Auth/sync lifecycle events use `--events` on stderr; do not combine it with `--agent`. Help/version remain text. An `unsupported_command` refusal is not permission to switch modes and execute a mutation.
- Compact is the default. Follow `meta.page.next_cursor` unchanged with the same selection/filters/order on supported lists. Local exhaustion does not certify remote completeness or freshness. For truncated messages, inspect the returned exact `chat_jid` and `id` using `messages show --agent --detail full`; `--full` controls tables, not agent detail. See the [quickstart](../../../docs/quickstart.md) for complete selection examples.
- For durable local change consumption, use `changes list --agent` with explicit account/store selection. Save `meta.page.next_cursor` after processing every page, including empty/final pages, and resume unchanged after restart. Repeated pages keep event IDs; no-op message replays add nothing. The reference log starts at schema 33, has no automatic retention, and does not certify WhatsApp completeness, receipt delivery or continuity across every restore. See [changes](../../../docs/changes.md) for coverage and cursor failures.
- Local auth, heartbeat, lock, row counts and `meta.source` do not prove current connectivity or complete history. Missing mappings remain unknown; never infer a phone number from a LID. Agent doctor can fail with exit 4 while legacy offline `doctor --json` returns a diagnostic report with exit 0 and `store_error`; inspect that field.
- On an uncertain write/send, retain exact IDs and inspect local state. Do not automatically replay, change an outbound key, drop a refused quote or fall back to a second writer. Follow operation-specific guidance.

## Explicit effects

- Use `--read-only` or `WACLI_READONLY=1` for inspection. This rejects intentional WhatsApp mutations and archive writes, but permits requested export/download output and explicit adapter execution. Downloads may use the network; adapters are not sandboxed. SQLite may create/update WAL/SHM bookkeeping. See [store reads](../../../docs/store.md#local-reads-by-default) and [media](../../../docs/media.md).
- Pairing/sync, draft writes, outbound dispatch and chat/group mutations require authorization for that effect. A draft hash or preview records no human approval. Start with [local drafts](../../../docs/drafts.md), then [outbound](../../../docs/outbound.md) only when sending is authorized. Acceptance, delivery and read observations are distinct; retained idempotency does not protect against archive rollback/loss.
- Named accounts have isolated stores. Never merge their data into one `wacli.db` or write `session.db` directly. Prefer the CLI; for local analytics/health queries use the escaped readonly recipes and optional FTS checks in [integrations](../../../docs/integrations.md#read-only-sqlite).

For authorized pairing/sync, `auth` pairs and bootstraps; `sync` requires an authenticated store and never shows QR. Use the chosen account explicitly. Read [auth](../../../docs/auth.md) or [sync](../../../docs/sync.md) only as needed; `--events` must keep stderr as NDJSON and warnings visible.

## Repository and release work

Inspect Git/worktree state and read the relevant checkout documentation before editing. Keep commits scoped to explicit file paths; preserve unrelated work. Use synthetic fixtures/fakes for validation, with plain/FTS/race checks proportional to the change. Do not install tools or use a real account to compensate for missing prerequisites without authorization.

Before any PR, run the exact [AGENTS.md](../../../AGENTS.md) gate on the final SHA:

```sh
pnpm format:check && pnpm lint && pnpm lint:deadcode && pnpm test && pnpm build && pnpm docs:site && git diff --check
```

Update task-relevant docs and release notes for user-facing changes while honoring active reservations. For this fork, read [fork maintenance](../../../docs/fork-maintenance.md): PR/push destination is `gabrielMalonso/wacli`, upstream is read/fetch only. Release/tag/deploy is a separately authorized task; read [release](../../../docs/release.md) and report completed versus failed jobs precisely. Existing upstream installation/site links do not authorize upstream writes.
