# Local drafts

Read when: preparing durable offline previews without sending, or inspecting retained revisions and document snapshots.

`draft create/show/list/update/discard` prepares only local records in the selected account's existing `wacli.db` (schema 30). There is no `draft send`, outbound journal, automatic replay, download, transcription, export command or garbage collection. No command opens/connects a WhatsApp client. A preview does not verify that a recipient exists remotely.

## Commands

Selection preserves the existing `--account`, `--store`, environment and default rules. The resolved store is fixed once before LOCK/IPC and always appears as `account.store_ref` in JSON. Create/update require exactly one locally persisted own PN identity; missing, ambiguous or unreadable public session identity returns exit 4, with no session creation or connection. Show/list/discard use the frozen record even without a current session.

```bash
wacli --account personal draft create --to +15550000002 --message 'literal text' --agent
wacli --account personal draft show DRAFT_ID --revision REVISION_ID --agent --detail full
wacli --account personal draft list --agent --limit 20
wacli --account personal draft update DRAFT_ID --if-revision CURRENT_REVISION --to 15550000002@s.whatsapp.net --message 'complete replacement' --agent
wacli --account personal draft discard DRAFT_ID --if-revision CURRENT_REVISION --agent
```

IDs are random lowercase 32-character hex strings. A draft's current pointer changes on update; every revision remains immutable and individually retrievable. Update supplies complete input, including recipient, rather than a patch. Both update and discard compare `--if-revision` at the writer; conflicts use exit 1. Discard marks the draft abandoned, keeps its current pointer and retains every revision/snapshot. It neither requires the current own identity nor deletes bytes. To inspect abandoned drafts, use `list --include-discarded`.

List reads stored summaries, not full payloads or media. Defaults are 20, bounds 1–200. Agent `--cursor` is a strict canonical versioned token, capped at 16 KiB and scoped to store and inclusion of discarded drafts. Ordering is immutable `(created_at, id)`; size/detail can change between pages. Status filtering is live: discarded drafts disappear on subsequent pages. Cursors are neither credentials nor authorization.

## Recipient and payload

`--to` accepts an explicit formatted PN phone (7–15 digits), numeric PN/LID JID, or numeric group JID. Device suffixes are syntactically normalized. Names are not accepted: first use `contacts search/resolve` or `chats list` and pass the resulting JID. Broadcasts, status, newsletters and other servers are rejected. Mentions accept only explicit users, up to 200 input entries; canonical duplicates merge and ordering is deterministic. Locally established self targets are rejected. Unknown self status remains unknown.

Each revision freezes the raw requested target, normalized requested JID, canonical recipient, observed PN/LID pair, display name and public own PN/LID identity. An unmapped LID remains a LID with an empty PN. Public mapping query errors are errors, not absence; no phone is inferred from LID digits. Stores/accounts remain separate. A later map/name change does not rewrite a preview. Future sending must compare relevant identity facts and require a new revision for relevant changes.

Choose exactly one variant:

- Text: `--message TEXT` or `--message-file PATH|-`. UTF-8, newline and literal backslashes are preserved exactly, without trim, NFC normalization or automatic escape decoding. File/stdin reads are bounded while streaming. `--mention USER` is repeatable.
- Fixed document: `--file PATH [--filename NAME] [--mime MIME] [--caption TEXT]`. The display filename defaults to the source basename. MIME defaults to sniffing the first bytes from the same snapshot read; an explicit concrete MIME override is normalized. No automatic image/audio/video selection.
- One explicit contact: `--contact-name NAME --contact-phone PN`. The exact display name and normalized explicit PN number appear in the DTO. Deterministic textual vCard 3 uses CRLF, escapes backslash/newline/semicolon/comma in name fields and confines the phone to validated digits. Adversarial names cannot inject properties. WhatsApp compatibility has not been tested live.

Text/document may use `--reply-to MESSAGE_ID` for an existing local textual message in this exact chat or its verified alias. The revision freezes chat+real ID, sender identity, `from_me` and actual text. Missing/unknown sender, unavailable/purged/revoked/deleted text, unsupported media/buttons/reactions and divergent records with the same ID in verified aliases are rejected. Incoming DM quotes must match the peer through shared observed PN/LID facts; contradictory pairs or an observed own sender with `from_me=false` are rejected. Outgoing quotes must match own identity. Group quotes allow any valid user sender without claiming membership. Unmapped distinct identities cannot prove peer equality. All checks precede snapshot/commit. No arbitrary quote input or remote lookup exists. Later message edits do not replace the frozen quote.

CLI limits are 64 KiB per field, 256 KiB canonical payload (including JSON escaping), 200 mentions, and 100 MiB per file. Escaped vCard and quote fields also obey limits. These are CLI quotas, not verified WhatsApp limits. Effective defaults explicitly disable link preview, ephemeral mode/expiration and self targeting.

The payload hash is SHA-256 over the UTF-8 prefix `wacli-draft-payload-v1`, one NUL byte, then deterministic canonical struct JSON with effective defaults, identity, canonical recipient, content, mentions and frozen quote. Revision IDs, timestamps, requested-input spelling and review/display metadata are outside this hash. Internal input rejects ambiguous unions, unknown/duplicate fields, malformed UTF-8, noncanonical encoding and nonfinite values. Hash equality neither authorizes sending nor proves a person read the preview.

## Preview and document bytes

Agent output preserves contract v1 and `source=local`, unknown freshness/completeness, and compact/full envelope caps (1 MiB / 8 MiB). Legacy `--json` includes the selected account and draft; `--full` selects full draft detail without agent mode. Compact cuts selected content/name fields at 512 Unicode code points and reports `truncated_fields`; identifiers, mentions, mappings, defaults and digest/size stay complete. Compact can suffice when all relevant fields are present; truncated fields require full recovery. No read/approved state is recorded.

A document revision reports digest, size and `verified_at_create`, describing only the creation-time read. Full additionally exposes `document.snapshot_path`, derived from the **currently selected fixed store + validated revision ID**, at `draft-media/REVISION_ID.blob`. It never exposes the original import path or internal metadata. Inspect it explicitly with appropriate local tools:

```bash
wacli --account personal draft show DRAFT_ID --revision REVISION_ID --agent --detail full
# Use the returned document.snapshot_path for separate local byte inspection.
```

This deliberate path exception is restricted to document draft detail. The path is an expected location, can be absent/altered and follows store relocation. Neither full nor compact certifies current integrity or approval. **Show/list never open or stat media**, including old revisions after update/discard. Future outbound handling must revalidate size/digest on the same bytes actually used for sending; it must not reload current quote text.

Snapshot creation rechecks `WACLI_MEDIA_ROOTS` in the process opening the source. It validates the opened FD as regular, then copies in cancellable 32 KiB chunks with a 100 MiB+1 limit, computing digest, size and MIME sniff on the copied bytes. Opening-time stat is not a path-immutability guarantee: concurrent writes can affect the captured sequence; the retained bytes/digest describe that sequence. Cancellation is checked between reads/writes, not an interrupt guarantee for blocked filesystem syscalls.

Each document revision gets its own exclusive managed file (0700 directory / 0600 files on Unix); no global dedupe or content-addressable cache. Publication uses a no-overwrite hard link, then directory sync before the SQLite revision commit. A filesystem must support hard links. Windows skips unsupported directory sync; this is not a power-loss transaction spanning filesystem and DB.

Publication/commit uncertainty preserves artifacts. A crash after publication and before DB commit can leave an unreferenced blob; no unknown artifact is deleted on recovery. Only the known unpublished temporary file belonging to the current failed attempt is eligible for cleanup. A synthetic Linux streaming+SHA benchmark (100 MiB, 3 iterations, i9-13950HX) measured approximately 86 ms / 1.2 GB/s and 33 KiB allocated per iteration; it excludes disk copying and fsync and is not a filesystem performance guarantee. Retention grows after updates/discards and consumes one full document copy per revision. **Follow-up before global acceptance:** define and separately review explicit safe orphan/revision cleanup and retention; this delivery performs no GC/purge.

## Writes, IPC and errors

Reads use the existing readonly archive opener without LOCK, migrations, permission normalization or media reads. An old archive needs an explicit writable upgrade. SQLite active-WAL bookkeeping retains the existing store limitations.

Create/update/discard reject `--read-only` / `WACLI_READONLY=1` (exit 2) before store/delegation effects. Writes hold the existing store LOCK; only an `ErrLocked` attempts delegation to the existing sync owner's socket/serialized slot. The local handler neither accesses WAClient nor consumes/updates send pacing. One deadline covers queueing and local work. It rechecks CAS, fixed store scope, own account for preparation, media roots and input bounds.

Pre-generated IDs and a versioned complete-request hash correlate IPC results; returned canonical payload/hash are revalidated. They are not dedupe/replay keys. Partial encoding, lost/decode-failed replies, old untyped owners, ID/hash mismatch, uncertain DB commit and stdout failures after mutation return `local_write_uncertain` (exit 1), with typed draft/revision IDs when available. Never automatically replay or fall back to a second writer after dispatch. Inspect the exact IDs in the selected archive. A typed owner refusal can describe a local no-write policy/conflict/error, but cannot claim rollback of a published snapshot.

Errors are sanitized and remain operation-specific: usage/read-only/invalid cursor exit 2; not found exit 3; archive/identity/document availability exit 4; CAS conflict/known pre-dispatch deadline/uncertain exit 1. Malformed, noncanonical, unsupported-version, hash/identity-mismatched or incompatible persisted payload/review data use a sanitized store error with exit 4 for show/discard, preserving the internal cause; caller ID/input/cursor validation remains exit 2. Optional `error.draft` contains only draft/revision/hash correlation and is absent from unrelated commands. The legacy overall IPC decoder is not claimed to have a new total-memory bound: typed draft input is strictly validated and the draft response read is capped, while the existing outer decoder is unchanged.

Stage06 alone will handle network sending, durable outbound states and idempotency.
