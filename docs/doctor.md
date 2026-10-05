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
- `recovery_observations` is `null` in doctor because recovery results are not persisted. Debt can be preventive, including after a normal sync shutdown; its presence does not establish a failed recovery or an incorrect mirror. Absence of debt does not certify queue integrity, freshness or remote completeness. Inspect a sync run's [app-state summary](sync.md#app-state-summary) for its known failed/cancelled collection, phase and diagnostic code; running another one-shot sync does not guarantee an empty post-shutdown debt list.
- `--json` includes `store.last_activity_at` when a `HEARTBEAT` file is present, reflecting the last time `sync --follow` recorded observed activity. It is not a process-liveness marker; quiet healthy sessions may not update it because successful keepalives are silent. This is distinct from `store.last_sync_at`, which reflects the newest stored message timestamp.
- Use `--json` for machine-readable diagnostics.

## Examples

```bash
wacli doctor
wacli doctor --json
wacli doctor --connect
```
