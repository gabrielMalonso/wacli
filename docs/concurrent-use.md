# Independent tasks on one account

Read when: a scheduled routine and a separate one-off task use the same WhatsApp account concurrently.

Select the same named account with `--account NAME` in each invocation, or the same explicit archive with `--store DIR`. Account selection follows the [existing rules](accounts.md); `--account` and `--store` cannot be combined. A continuous [`changes watch`](changes.md#continuous-local-watch) reader streams committed local feed pages while the owner syncs; it also works without an owner, starts no connection, and needs no live readiness. Save its cursor only after processing a complete NDJSON frame. Separate tasks can read the local archive concurrently and submit independent writes using the existing store LOCK and connected owner's IPC. This coordinates transport access; it does not distribute customer conversations or tasks between workers.

| Operation | Existing coordination |
| --- | --- |
| `sync status` | Queries live follow owner IPC outside the operation slot, without opening databases, writer LOCK or a second connection; `owner_ready` is local initialization without terminal cleanup, independent of transport; current authenticated readiness is unsupported (`ready=false`). |
| Local messages/chats/contacts, changes list/watch, draft and outbound queries | Readonly archive access without the writer LOCK or a second WhatsApp connection; reads continue alongside WAL writers. |
| `outbound send`, draft mutations, `history backfill` and supported legacy delegated operations | Direct writer LOCK, or the existing `sync --follow` owner's socket when the store is locked. The shared operation slot serializes these requests; explicit history `--before-id` requires its distinct compatible owner kind, with no oldest-anchor or direct-writer fallback. |
| Delegated archive/pin/mute changes | Existing app-state coordination, outside the send slot and send pacing. |
| Commands without delegation, including `send status` | Require their existing direct LOCK; they can wait with `--lock-wait` or fail while another process owns it. |

The follow process exposes its socket immediately before Sync connects, ahead of bootstrap. Use [`sync status`](sync.md#live-owner-readiness) for positive local readiness and transport observations; current authenticated readiness remains a pinned SDK gap. Socket presence alone is insufficient. An ordinary standalone writer holding LOCK is not automatically an IPC owner. Without a compatible owner, LOCK failure remains a failure; `--lock-wait` only controls waiting to acquire LOCK. See [sync](sync.md) for supported legacy delegation and [drafts](drafts.md)/[history](history.md) for their contracts. Local reads need an initialized compatible archive; they do not migrate it. `--read-only` or `WACLI_READONLY=1` continues to reject writes before delegation.

Each delegated request uses its own connection and response. Draft, history and outbound requests additionally validate their typed correlation. Queueing, configured send pacing and execution share that request's timeout. A request explicitly refused before dispatch does not execute later. A lost reply is not proof of refusal. There is no FIFO or priority guarantee, and a slow adapter may keep subsequent operations waiting after its caller has timed out. The owner retains the slot through the synchronous adapter return and outbound finalization; closing the caller's socket cannot revoke effects already passed to the SDK.

For sends whose uncertain outcome must remain inspectable, use [retained outbound](outbound.md): prepare separate drafts, select each exact revision/hash and choose a distinct literal key for each independent operation.

```bash
# Owner, in its own terminal/process:
wacli --account personal sync --follow

# Separate tasks, each using its own prepared D/R/H and key:
wacli --account personal --timeout 30s outbound send DRAFT_A --revision REV_A --expect-hash HASH_A --key 'scheduled-2026-10-04' --agent
wacli --account personal --timeout 30s outbound send DRAFT_B --revision REV_B --expect-hash HASH_B --key 'one-off-2026-10-04' --agent

# Local inspection can run while either send is waiting or executing:
wacli --account personal outbound show --key 'scheduled-2026-10-04' --account-jid OWN_PN_JID --agent
```

These are separate invocations; placeholders refer to previously prepared drafts and the frozen own PN. Keep each command's stdout, stderr and exit status with that task. Inspect the returned operation ID, or its exact key plus frozen own PN, after uncertainty. Reusing the same retained binding returns its known result and IDs without another application send invocation, including when pending or uncertain. A new key can duplicate a remote message; archive loss or rollback can remove the retained protection. Do not automatically resend to resolve uncertainty. The SDK may retransmit the same message/ID during or after the original call, including after an uncertain result.

Legacy sends retain their existing bounded application retry and output contracts; they do not gain frozen revision/hash/key idempotency or retained uncertain-operation lookup. `accepted` is not delivery or reading evidence. Separate requests still share the account's WhatsApp conversation state, read markers, archive/pin/mute settings and local history. There is no transaction spanning requests, the network and both databases, no conversation reservation, and no worker assignment or consumer processing checkpoint.

Offline doctor reads bounded [historical checkpoints](doctor.md#retained-connection-and-sync-observations)
through readonly SQLite while an owner holds LOCK and writes WAL. LOCK, socket,
authentication and HEARTBEAT are separate observations; none make a saved login
or unfinalized Sync checkpoint proof of current liveness. Readability failure
stays unknown and never grants a second writer/connection or automatic retry.
