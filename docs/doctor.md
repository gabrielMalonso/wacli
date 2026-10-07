# doctor

Read when: diagnosing store layout, auth state, FTS/search support, locks, or optional live connectivity.

`wacli doctor` reports local health information and can optionally connect to WhatsApp.

## Command

```bash
wacli doctor [--connect]
```

## Notes

- Without `--connect`, doctor reads local state read-only, probes the existing lock without creating it, and opens no writable WhatsApp session. Missing or incompatible stores are reported in `store_error`; see [store compatibility](store.md#local-reads-by-default).
- `--connect` requires auth and the store lock, and waits for confirmed WhatsApp login before reporting `connected: true`. A socket handshake alone is not a successful login.
- Output includes local store counts, auth identity when available, FTS/search state, lock details, and `session_revoked`. An observed remote logout reports `authenticated: false` and `connection_state: "logged_out"` even when an old device row remains. Rejected or timed-out login attempts retain that state; confirmed login clears it.
- Human output and legacy/agent JSON include `app_state`: `reconciliation` is `required` when retained collection replay debt exists, `none_recorded` when the query succeeds without debt, or `unknown` when it cannot be read. `pending_collections` is sorted and distinct, `[]` for a successful empty query, and `null` when unavailable; observation errors use only `error.code="recovery_state_unavailable"`. This archive-only read needs no session, writer lock or migration. Existing auth inspection remains separate and read-only; agent archive/auth-source errors retain exit 4, while a recovery-query-only failure is embedded in the report without a new exit code.
- The legacy `app_state.recovery_observations` remains `null` in doctor because that field describes invocation-local recovery. Historical results are exposed separately in `observations.sync.recovery_observations` when retained. Debt can be preventive, including after a normal sync shutdown; its presence does not establish a failed recovery or an incorrect mirror. Absence of debt does not certify queue integrity, freshness or remote completeness. Inspect dated retained observations and a sync run's [app-state summary](sync.md#app-state-summary) for its known failed/cancelled collection, phase and diagnostic code; running another one-shot sync does not guarantee an empty post-shutdown debt list.
- `--json` includes `store.last_activity_at` when a `HEARTBEAT` file is present, reflecting the last time `sync --follow` recorded observed activity. It is not a process-liveness marker; quiet healthy sessions may not update it because successful keepalives are silent. This is distinct from `store.last_sync_at`, which reflects the newest stored message timestamp.
- Use `--json` for machine-readable diagnostics.

## Examples

```bash
wacli doctor
wacli doctor --json
wacli doctor --connect
```

## Retained connection and sync observations

Human output and legacy/agent JSON add `observations` version 1, always labelled
`historical: true`. Two independent slots in the selected `wacli.db` retain the
last connection execution and last Sync execution, each with its own random
`execution_id`, start/checkpoint dates and optional stop/cleanup dates. Opening
`doctor --connect` can replace the connection slot; it does not replace Sync or
its recovery facts. `sync.connection_execution_id` references the connection
execution used by that run, which may differ from the latest connection slot.
The archive/store selection scopes these observations; no JIDs, message bodies,
phone numbers, raw SDK/SQL errors or secret material are added to these snapshots.
An externally replaced/relinked/restored archive does not prove account continuity.

`connection.login_confirmed_at` records an SDK confirmed-login event;
`disconnected_at`, `logged_out_at`, `rejected_at` and `error_at` record observed SDK events.
`last_event` is `unobserved`, `login_confirmed`, `disconnected`, `logged_out`,
`login_rejected` or `connection_error`. Stream/token/reconnect errors are not
labelled login rejection. These are dated historical observations. Local cleanup records
`closed_at` separately: explicit SDK disconnect does not emit a Disconnected
event, so cleanup does not manufacture one. Logout is terminal for that client;
a delayed Connected callback cannot erase it. Opening a client without any of
these events leaves `unobserved`, not a successful login.

Sync retains `state=unfinalized|stopped`, `stop_reason`, dates, the last observed
history blob's type/chunk/progress, server replay preview/completion dates and
completion count, and bounded recovery outcomes with dates.
`messages_stored` is the legacy global Sync counter sampled at stop, including
replays/updates and other chats, excluding manually downloaded on-demand blobs.
It is null before a stop checkpoint; it is not remote coverage or unique growth.
Cleanup can retain late admitted writes without changing that sampled counter.
`unfinalized` means no terminal checkpoint was retained, not that a process is
running or crashed. `cleanup_at` means known local writers were drained before
that checkpoint. No field proves current connectivity, liveness, freshness,
complete history or an intact mirror. `last_sync_at` (newest message date),
HEARTBEAT activity, auth rows, writer LOCK and IPC socket remain distinct signals.
Legacy offline `connected` and `connection_state` keep their existing meanings;
agent offline `auth.connected`, freshness and completeness remain `unknown`.

Missing slots are null/unknown, never a healthy default. Failed/malformed reads
return only `observations.error.code=diagnostics_unavailable`. An observation-only
failure preserves doctor exit status; archive/session-source errors retain the
existing legacy `store_error` and agent exit 4. Schema 32 creates the two slots
only on a writable open. Older archives keep the existing incompatible-schema
error until an explicitly authorized writable upgrade; readonly doctor never
migrates them, takes the writer LOCK or connects. Reads use ordinary readonly
SQLite and can see committed WAL checkpoints alongside an owner.

Snapshots are best effort under the archive's existing WAL/synchronous policy,
not FULL durability guarantees or transactions across `session.db` and `wacli.db`.
A failed write emits sanitized `diagnostics_persistence_unconfirmed`; current-run
summary/events mark `persistence_unconfirmed=true`. It remains true for that run
even if later writes succeed. Offline doctor can only read the last saved
checkpoint, possibly from an older execution, and cannot recover facts that were
never saved. Compare execution IDs and dates; absence does not prove no activity.
Each slot is capped at 64 KiB; replacing a slot discards its older execution.
This is not a growing journal, change feed, retry authorization or monitor.
