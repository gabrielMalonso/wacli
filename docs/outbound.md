# Retained outbound operations

Read when: inspecting durable local outbound attempts, their frozen draft binding and observed evidence.

Schema **31** adds the typed local nucleus and **only `outbound show/list`**. There is no outbound send/reserve/retry/resume/recover/cancel command, outbound IPC kind, agent network capability, WA event handler, queue or executor. Records can be populated by the internal store boundary and synthetic fixtures. Sending requires a separately reviewed integration.

## Offline queries

```bash
wacli --account personal outbound list --agent
wacli --account personal outbound list --account-jid 15550000001@s.whatsapp.net --agent --limit 20
wacli --account personal outbound show OPERATION_ID --agent
wacli --account personal outbound show --key 'literal-key' --account-jid 15550000001@s.whatsapp.net --agent --detail full
wacli --account personal outbound show OPERATION_ID --agent --limit 20 --cursor TOKEN
```

The existing account/store selection is frozen once. `account.store_ref` identifies that archive. `--account-jid` filters **frozen own PN**, never selects a different store, reads current identity or grants authorization. Show accepts an operation ID or key+own PN, never both. IDs are lowercase 32-hex. Keys are literal case-sensitive ASCII `!`–`~`, 1–128 bytes, without spaces or normalization.

Queries use the current-schema readonly opener without initialization, migrations, permission changes, LOCK, socket, session, media reads/stats or connection. They work while the existing owner holds LOCK and alongside WAL writers, subject to ordinary SQLite SHM bookkeeping. They require no current session, including after replacement or draft discard. An old archive needs an explicit writable upgrade; queries never perform it.

Agent output remains v1, `source=local`, unknown freshness/completeness, with complete bounded identifiers, key, binding, identities, phase/result and evidence. Full adds nullable checkpoint timestamps. No body/protobuf/media secrets or source/snapshot paths appear. Inspect content separately with `draft show D --revision R` using the exact retained binding. The local payload hash is not an upload/protobuf hash, permission to send or proof of human review.

Legacy output uses the existing JSON envelope; `--full` adds checkpoint detail outside agent mode. Agent compact/full envelopes are capped at 1/8 MiB before writing success bytes. Fields in this delivery are bounded and need no text truncation.

## Pagination and errors

List returns `data.operations`; show returns `data.operation` and `data.observations`. Both expose `meta.page={returned,has_more,next_cursor}`. On show, the page applies to **observations**; status derives from all retained facts in the same read snapshot. Defaults are 20, bounds 1–200. `--cursor` requires agent mode. Page size/detail may change.

Ordering is immutable `(created_at,id)` for operations and local insertion ID for observations, irrespective of protocol timestamp. Canonical versioned tokens are at most 16 KiB, have distinct domains, and bind the selected store plus account filter or operation. Key and ID lookups can continue the same operation page. They are not credentials or replay tokens. Pages are live between calls; new facts can change status/counts. Local exhaustion does not establish remote completeness/freshness.

Caller syntax uses exit 2; malformed/mismatched tokens use `invalid_cursor`, exit 2. Missing records use exit 3, meaning only no retained local match. Malformed/incompatible operation/observation/revision data uses sanitized **`store_error`, exit 4**, even when a model validator is the internal cause. Other archive-open failures retain exit 4. Errors omit raw SQL, paths, stored malformed values and internal causes.

List loads immutable revision summaries, not full payloads. Show additionally validates the referenced canonical revision/hash. Fact derivation validates even observations outside the selected page. Memory is O(page size + observed participant identity scopes); work is proportional to retained observations, for up to 200 returned operations. Envelope/page bounds do not bound historical DB work or promise constant native memory.

## Binding and local idempotency

An internal reservation binds contract version, frozen own PN, D/R and expected PR11 payload hash, referencing the immutable revision without copying its payload. An **explicit old revision of an active draft** can be reserved; head is never followed or rewritten. New reservation checks active state, exact hash and current public own PN/LID supplied by the future caller. The nucleus never reads a session or checks remote recipient existence/group membership.

`UNIQUE(account_jid,idempotency_key)` is per selected DB. Reserve resolves existing binding before checking discard or current identity: same binding returns the original operation/message IDs; different D/R/H conflicts. Candidate IDs do not replace retained IDs. Another account has its own key scope; stores stay isolated. New reservation after discard fails. Later discard retains the operation/revision and does not cancel possible effects.

The future explicit send requires D/R/expected hash/key and permits at most **one adapter/SDK invocation per logical operation**. The pinned SDK may retransmit the same ID/frame during that active call; this is not one-frame delivery or exactly-once. No second invocation, fallback, automatic replay/resume is authorized after error/uncertainty. This nucleus accepts a validated bounded message ID from its caller; real SDK generation belongs to the future adapter, before network dispatch. Fixtures never construct WA clients for IDs.

A key protects only the retained catalogue. Loss/rollback/restoration/replacement can remove bindings and markers. `archive_continuity` is always `unknown`; a UUID inside that same DB cannot detect its rollback. A new key can duplicate remotely.

## State and evidence

| Dimension | Retained values |
| --- | --- |
| Phase | `reserved`, `preparing`, `upload_possible`, `upload_returned`, `dispatch_possible`, `finalized` |
| Attempt result | `pending`, `accepted`, `rejected`, `not_dispatched`, `uncertain` |
| Observed facts | `ack`, `own_echo`, `delivered`, `read`, `server_error`, with scoped chat/actor/alias/device, source and timestamps |

`reserved`, `pending`, `incomplete` and record age do not assert running/crashed status or permission to replay. Unmeasured timestamps/error codes are null. `upload_returned` describes an uploader response, never message acceptance. A retained dispatch marker without resolution derives `status=uncertain`, including crash between marker and actual call. Document upload-only uncertainty remains distinguishable by its nullable dispatch marker. Missing markers/facts do not prove no historical activity after catalogue restoration.

CAS permits reserved→preparing; text/contact then dispatch possible; document upload possible→upload returned before dispatch. Finalization is terminal. Accepted requires an ack retained atomically; rejected requires possible dispatch; not-dispatched cannot follow dispatch; uncertain requires possible upload/dispatch. Inserted facts and checkpoints advance generation; exact duplicate facts leave the operation unchanged. SQL uniqueness, binding/CAS/order constraints and immutable triggers protect retained records. No transaction spans network work.

Facts deduplicate by operation, kind/source, chat, actor/alias/device, protocol timestamp including absence, and sanitized machine code. Protocol timestamps can arrive out of order; observation time remains separate. Claimed aliases must be complementary public PN/LID forms; contradictory assertions are rejected. The future adapter must establish public relations before submitting facts: syntax alone proves neither alias ownership nor membership.

Properly scoped DM read implies delivery/acceptance, without fabricating an ack or their timestamps. Group receipts imply acceptance and per-participant evidence; operation-wide delivery/read remain **unknown**. `observed_*_participant_scopes` count distinct observed public identity scopes, not group size or every participant's outcome; unknown aliases may leave two scopes for one person. Device evidence stays separate. Counts are null for DMs; zero group counts mean zero retained matching scopes, not zero remote deliveries.

Ack derives accepted. Own/history echo alone never derives acceptance/delivery/read; `messages` rows are not consulted as proof. Positive receipts do not erase attempt errors: `status=read` can coexist with `attempt_result=uncertain`. Server errors remain separate facts. Absence stays unknown; reading may never be observable and is not proof of human comprehension. Boolean `*_observed=false` means no such retained observation.

## Critical commits

The writer leases a connection from the **same archive pool**, reads/checks its synchronous setting, sets/checks FULL before BEGIN IMMEDIATE, and performs short reservation/CAS/observation transactions. Local reserve/discard races serialize; production callers still must hold existing LOCK/owner slot. Queries never invoke writers.

Configuration/cancellation/transaction errors stop progression. Commit errors return `write_uncertain` and no usable operation value. Rollback/restoration use an independent bounded two-second cleanup context. Restoration is checked before releasing the lease; failed rollback/restoration/check discards the physical connection through database/sql `ErrBadConn`. No unexpected settings return to the pool, and legacy configuration is not globally changed. Contexts do not guarantee interruption of every SQLite/OS/filesystem syscall.

FULL requests SQLite/OS flushes; it does not promise survival of hardware/filesystems ignoring flush, catalogue restoration/loss, atomicity with network/session DB or message acceptance. The future adapter must check every critical commit before the next possible effect, and finalize responses under an independent bounded context when dispatch deadlines expire.

## Validation and remaining integration

Fixtures use generated identities/IDs and temporary archives only: binding/unique/CAS/discard races, old revisions, migration/reopen/checkpoints, account/store isolation, monotonic/scoped facts, readonly/corruption/cursors/caps, FULL cleanup/cancellation/error/lease eviction. Opt-in `WACLI_OUTBOUND_E2E_BINARY` tests the freshly built binary against a private fake owner, never a WA executor. Live WhatsApp is **not validated**.

Sending still needs reviewed IPC/deadline/pacing correlation, identity rechecks/frozen LID routing, one SDK invocation with documented internal retransmission, and persistent retry-receipt policy across cache/EventBuffer/restart. Managed snapshots need a distinct opening boundary: import-source media roots must not accidentally exclude snapshots. Derive managed bytes by revision under the selected store root, read once, and check digest/size of the exact uploader buffer. No network path is enabled here.
