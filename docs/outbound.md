# Retained outbound operations

Read when: inspecting durable local outbound attempts, their frozen draft binding and observed evidence.

Schema **31** retains operations bound to immutable drafts. `outbound send` explicitly dispatches one revision; `outbound show/list` remain pure local queries. There is no recover/retry/resume/cancel command or additional executor. Receipt/echo persistence through live handlers is a separate integration; this action retains only its validated SDK ACK. Without retained facts, delivered/read stay unknown.

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

An internal reservation binds contract version, frozen own PN, D/R and expected PR11 payload hash, referencing the immutable revision without copying its payload. An **explicit old revision of an active draft** can be reserved; head is never followed or rewritten. New reservation checks active state, exact hash and current public own PN/LID supplied by the sending adapter. The nucleus never reads a session or checks remote recipient existence/group membership.

`UNIQUE(account_jid,idempotency_key)` is per selected DB. Reserve resolves existing binding before checking discard or current identity: same binding returns the original operation/message IDs; different D/R/H conflicts. Candidate IDs do not replace retained IDs. Another account has its own key scope; stores stay isolated. New reservation after discard fails. Later discard retains the operation/revision and does not cancel possible effects.

The explicit send requires D/R/expected hash/key and permits at most **one adapter/SDK invocation per logical operation**. The pinned SDK may retransmit the same ID/frame during and after that call, including retry receipts after an uncertain result; this is not one-frame delivery or exactly-once. No second invocation, fallback, automatic replay/resume is authorized after error/uncertainty. This nucleus accepts a validated bounded message ID from its caller; the adapter uses normal SDK ID generation, persisted before network dispatch. IDs have no custom namespace.

A key protects only the retained catalogue. Loss/rollback/restoration/replacement can remove bindings and checkpoints. `archive_continuity` is always `unknown`; a UUID inside that same DB cannot detect its rollback. A new key can duplicate remotely.

## State and evidence

| Dimension | Retained values |
| --- | --- |
| Phase | `reserved`, `preparing`, `upload_possible`, `upload_returned`, `dispatch_possible`, `finalized` |
| Attempt result | `pending`, `accepted`, `rejected`, `not_dispatched`, `uncertain` |
| Observed facts | `ack`, `own_echo`, `delivered`, `read`, `server_error`, with scoped chat/actor/alias/device, source and timestamps |

`reserved`, `pending`, `incomplete` and record age do not assert running/crashed status or permission to replay. Unmeasured timestamps/error codes are null. `upload_returned` describes an uploader response, never message acceptance. A retained dispatch marker without resolution derives `status=uncertain`, including crash between marker and actual call. Document upload-only uncertainty remains distinguishable by its nullable dispatch marker. Missing markers/facts do not prove no historical activity after catalogue restoration.

CAS permits reserved→preparing; text/contact then dispatch possible; document upload possible→upload returned before dispatch. Finalization is terminal. Accepted requires an ack retained atomically; rejected requires possible dispatch; not-dispatched cannot follow dispatch; uncertain requires possible upload/dispatch. Inserted facts and checkpoints advance generation; exact duplicate facts leave the operation unchanged. SQL uniqueness, binding/CAS/order constraints and immutable triggers protect retained records. No transaction spans network work.

Facts deduplicate by operation, kind/source, chat, actor/alias/device, protocol timestamp including absence, and sanitized machine code. Protocol timestamps can arrive out of order; observation time remains separate. Claimed aliases must be complementary public PN/LID forms; contradictory assertions are rejected. The adapter must establish public relations before submitting facts: syntax alone proves neither alias ownership nor membership.

Properly scoped DM read implies delivery/acceptance, without fabricating an ack or their timestamps. Group receipts imply acceptance and per-participant evidence; operation-wide delivery/read remain **unknown**. `observed_*_participant_scopes` count distinct observed public identity scopes, not group size or every participant's outcome; unknown aliases may leave two scopes for one person. Device evidence stays separate. Counts are null for DMs; zero group counts mean zero retained matching scopes, not zero remote deliveries.

Ack derives accepted. Own/history echo alone never derives acceptance/delivery/read; `messages` rows are not consulted as proof. Positive receipts do not erase attempt errors: `status=read` can coexist with `attempt_result=uncertain`. Server errors remain separate facts. Absence stays unknown; reading may never be observable and is not proof of human comprehension. Boolean `*_observed=false` means no such retained observation.

## Critical commits

The writer leases a connection from the **same archive pool**, reads/checks its synchronous setting, sets/checks FULL before BEGIN IMMEDIATE, and performs short reservation/CAS/observation transactions. Local reserve/discard races serialize; production callers still must hold existing LOCK/owner slot. Queries never invoke writers.

Configuration/cancellation/transaction errors stop progression. Commit errors return `write_uncertain` and no usable operation value. Rollback/restoration use an independent bounded two-second cleanup context. Restoration is checked before releasing the lease; failed rollback/restoration/check discards the physical connection through database/sql `ErrBadConn`. No unexpected settings return to the pool, and legacy configuration is not globally changed. Contexts do not guarantee interruption of every SQLite/OS/filesystem syscall.

FULL requests SQLite/OS flushes; it does not promise survival of hardware/filesystems ignoring flush, catalogue restoration/loss, atomicity with network/session DB or message acceptance. The adapter checks every critical commit before the next possible effect, and finalizes responses under an independent bounded context when dispatch deadlines expire.

## Explicit dispatch

```bash
wacli --account personal outbound send DRAFT_ID --revision REVISION_ID --expect-hash SHA256 --key 'literal-key' --agent
```

D/R/H and the key are mandatory. Readonly policy and malformed selection fail before archive initialization/migration, LOCK, snapshot, socket or WA access. Store selection is frozen once. The revision supplies the frozen own PN; the request fixes it across IPC and the owner revalidates it. The same retained version/own PN/D/R/H/key returns the original operation locally before current identity, active/head, document or network checks, even when pending, failed, uncertain or discarded. A different binding conflicts. No duplicate pacing or rapid-send bookkeeping occurs.

A new operation reserves under the existing writer/owner slot. It permits an explicitly selected old revision of an active draft and never follows head. A strict public reader revalidates frozen own/target/mention/quote PN/LID relations before reservation and after connection. A PN without a frozen known LID requires a new revision; no discovery or silent retargeting occurs. Group membership remains unknown. Typed text/mentions/quote, contact card and document payloads use the exact immutable revision, with no preview, ephemeral or self defaults.

Documents are opened under `os.Root` for the selected store, deriving the managed path strictly from revision ID. Import-source paths/allow-roots are not reapplied to this managed snapshot. Symlink/nonregular descriptors, changed inode, size or digest fail before Upload. The reader consumes at most 100 MiB + 1, validates length/SHA-256, and hands the same verified buffer to Upload once. Upload response length/digest/key dimensions are checked before constructing the message. The SDK can allocate plaintext/encryption copies; 100 MiB is a content bound, not an RSS bound. Peak memory includes the verified buffer, SDK copies, protobuf and driver/runtime overhead.

Preparation, `upload_possible`, `upload_returned` and `dispatch_possible` are separate checked FULL commits. A failed commit stops the next effect boundary. There is at most one application Upload and one SendMessage invocation, using the persisted normal SDK ID. SDK/HTTP/frame/retry-receipt retries remain enabled, including after the action returns. This favors protocol recovery of messages that would otherwise remain “Waiting for message”, with possible later retransmission of the same message/ID. It never promises a single frame, wire-once or exactly-once delivery. Cancellation, socket close or draft discard cannot revoke payload already passed to the SDK.

Before possible dispatch the message result is `not_dispatched`; upload milestones separately preserve possible media effects. Once dispatch is possible, errors, cancellation and timeout are `uncertain` unless a coherent SDK response proves rejection. Accepted requires coherent ID/chat/own sender/timestamp and ACK retained transactionally. Accepted means SDK acceptance, not delivery. A secondary history projection failure sets `history_warning` without undoing acceptance.

Finalization uses an independent two-second context while retaining the writer/slot until the synchronous adapter returns and the result is handled. Reread/CAS handles concurrent observation generations and lost commit replies without repeating SDK calls. A bounded exhausted finalizer reports `persistence_unconfirmed` with known result/ACK, IDs and binding. Only FULL leases save/set/check/restore a short SQLite busy timeout (at most 100 ms, reduced by the lease deadline); legacy pool settings are restored and verified. Native driver/OS/filesystem calls do not provide a strict wall-clock guarantee.

IPC uses versioned `outbound_send_v1` in the existing UNIX socket/slot. Typed request content is capped at 8 KiB and the outbound envelope at 16 KiB; typed replies are capped at 1 MiB. Queueing, pacing and checkpoints share the request deadline; finalization alone has an independent budget. New operations share existing pacing. A failed dial before request bytes preserves the lock/unavailable error. After possible dispatch, lost/untyped/old-version/mismatched request ID, hash, account, operation or binding is typed uncertainty with no fallback or replay. Client cancellation cannot confirm owner/SDK cancellation.

Action envelopes are v1 `source=live`, including errors and locally resolved duplicates. Compact/full output caps remain 1/8 MiB; identifiers/hash/certainty are complete and no protobuf or raw error is serialized. `error.outbound` carries query correlation and known persistence/result/ACK. Usage/readonly exit 2, missing records 3, store/persistence failures 4, operational conflicts/uncertainty 1. Stdout failure after effect reports `output_unconfirmed` with OP/key query information and never sends again.

Restore protects only retained bindings. Restoring only `wacli.db` may preserve/remove local keys independently of `session.db` payload recovery; restoring both can lose both. The SDK cache is finite and outgoing retry rows expire after seven days. Reopening loses cache; persisted retry recovery depends on retained session rows and normal SDK policy. No archive continuity or protection after rollback is promised.

## Offline validation

Real SQLite/core and UNIX socket fixtures cover bindings, exact revisions, CAS, commit failures, deadlines, output failure, isolation and bounded DTOs. Fake adapters cover typed text/card/document/quote, exact bytes/ID, upload/dispatch boundaries, response contradictions, cancellation and concurrent facts. Real pinned SDK tests cover ID injection, retry-store failure and payload recovery after local failure; existing retry guards/lifecycle are preserved. These are application invocation and local persistence tests, not counts of transport frames, remote success or real ACK/upload. No Connect, real account or WhatsApp service is used by fixtures. Opt-in `WACLI_OUTBOUND_E2E_BINARY` runs the production binary against a private synthetic UNIX owner.
