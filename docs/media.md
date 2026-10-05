# media

Read when: downloading media from a synced message or explicitly transcribing a local file.

`wacli media` downloads media referenced by messages already stored in `wacli.db`.
Complementary `media transcribe` processes an explicit local file through a caller-selected executable; it does not require a message or archive.

## Commands

```bash
wacli media download --chat JID --id MSG_ID [--output PATH]
wacli media backfill [--chat JID] [--limit N] [--workers N]
wacli media retry [--chat JID] [--before YYYY-MM-DD] [--limit N] [--batch N] [--wait DUR]
wacli --agent media status --chat JID --id MSG_ID [--verify] [--output PATH]
wacli --agent media download --chat JID --id MSG_ID --output PATH
wacli --agent media retry --chat JID --id MSG_ID --output PATH [--wait DUR]
wacli media transcribe --file PATH --adapter /absolute/executable [--expect-sha256 HEX] [--mime-type MIME] --agent
```

## download

Downloads media for a single message.

### Notes

- The target message must already be synced.
- Media downloads are capped at 100 MiB.
- `--output` may be a file path or directory.
- If `--output` is omitted, media is written under the store media directory.
- `--read-only` is supported only with explicit `--output`; it writes the file without opening the WhatsApp session store or recording `local_path` / `downloaded_at`.
- Because it never opens the store for writing, `--read-only` also takes **no store lock**, so it is the way to fetch media while `sync --follow` is running. A follow session holds the lock for its entire run, so a plain `media download` cannot proceed until it stops, and `--lock-wait` only turns the immediate failure into a timeout.

### Examples

```bash
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123 --output ./downloads
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123 --output ./photo.jpg
wacli --read-only media download --chat 1234567890@s.whatsapp.net --id ABC123 --output /tmp/photo.jpg
```

## Agent status and download

These two commands select one exact stored chat JID and message ID. They open only the current-schema archive read-only, even without `--read-only`. They need no authenticated session, LOCK or owner IPC and work alongside `sync --follow`. They do not initialize or migrate archives, pair, sync, backfill, retry, or update `local_path`/`downloaded_at`. Download's explicit output file is the existing read-only policy exception; `recorded=false` describes the archive update policy. Legacy commands and JSON/overwrite behavior remain unchanged.

```bash
wacli --agent media status --chat 1234567890@s.whatsapp.net --id ABC123
wacli --agent media status --chat 1234567890@s.whatsapp.net --id ABC123 --verify
wacli --agent media download --chat 1234567890@s.whatsapp.net --id ABC123 --output ./downloads/photo.jpg
```

`status` performs no network or filesystem writes. `--verify` reads and hashes at most 100 MiB. Optional `--output` observes that destination as well, without creating directories. A stat-only observation is `existing/not_checked`, never verified. A matching stored plaintext SHA-256 gives `verified/sha256_verified`; `checks` names successful `sha256` and, only when positive, `declared_size` checks. Legacy declared length zero means unknown (`declared_bytes=null`), not an exact zero-byte claim. Local byte counts are independently measured. Missing/malformed hash leaves binding unknown even if a size check succeeds. Size/hash mismatches are explicit verification observations.

`download` always verifies an existing destination and cache before reuse. A verified destination returns `existing`; copying verified archive cache returns `cached`; direct HTTP returns `downloaded`. SHA-256 alone suffices to verify/copy a local artifact: it needs no media key or direct path. HTTP additionally requires a 32-byte key, supported media type, valid stored direct path, and valid hashes. Tombstones and invalid metadata cannot initiate network downloads. No arbitrary DB path authorizes local reads: cache must be inside this archive's `media` directory.

An old `media_unavailable_at` is returned as a dated retained phone-and-CDN observation, while `remote.current` remains `unknown`. It may coexist with local bytes. Missing cache/path and expired CDN do not establish unrecoverability. HTTP 403/404/410 produces sanitized `media_expired`; it does not update that marker. Explicit [exact recovery](#exact-agent-retry) is a separate writable capability. Existing legacy `media retry` is bulk; `--limit 1` chooses a pending row, not a requested message. No automatic recovery or availability guarantee is offered.

Agent IO explicitly extends `WACLI_MEDIA_ROOTS` to cache reads and output writes, using a separate helper; legacy upload enforcement is unchanged. When configured, cache must satisfy both the selected archive media boundary and an allowed root, and output must satisfy an allowed root. Configured symlink roots resolve once; symlinks beneath a boundary, including file symlinks that stay inside it, are refused. Without roots, output uses its explicit filesystem path with the same no-symlink component rule. The selected store permits output only below `media`, never DB/session/LOCK/socket/heartbeat or other control paths. Hardlinks to top-level store control files are also refused by file identity. Descriptors and directory/file identity are rechecked after opening and before publication; this is confinement, not an OS sandbox against an authorized concurrent writer.

Existing destinations are never overwritten by agent download. Different, unverified or nonregular destinations fail; choose another explicit path. New parent directories are 0700 and temporary/output files are 0600 without chmod of preexisting directories. Verified bytes are copied into a private temporary, then published by atomic same-filesystem link without replacement. A concurrent destination creation cannot be clobbered. Unsupported link filesystems fail safely; there is no rename fallback. Cancellation/integrity failure before publication removes only this attempt's temporary; created parent directories can remain. `not_written` refers to the final file. Ambiguous publication can retain it; there is no cleanup/replay service. Publication or stdout failure retains known effects in `error.media`; timeout and lost output do not imply rollback. Files are synced before publication, but directory/power-loss durability and an atomic DB/filesystem snapshot are not promised.

The existing direct crypto path buffers ciphertext, plaintext and a CBC copy; peak memory can be several times the 100 MiB file cap. The bounded copy buffer is 32 KiB, not the downloader's total memory limit. Agent deadlines must be positive and at most five minutes. URLs, direct paths, media keys, ciphertext hashes, HTTP bodies and raw internal causes never appear in its DTO/errors. Compact/full differ in filename truncation; binary content is never included in the envelope. See [agent contract](agent.md#media-observations-and-explicit-output).

Standalone writable media commands record recovery debt for mirrored AppState collections before connecting, because the SDK can advance session state without the sync persistence handler. A later sync replays these collections before incremental fetches. Exact retry does not import unrelated chats or run historical identity migration. Verified cache/destination reuse in exact agent retry stays offline and adds no recovery debt.

## Exact agent retry

```bash
wacli media retry --chat 1234567890@s.whatsapp.net --id ABC123 --output ./downloads/photo.jpg --agent
wacli --agent --timeout 2m media retry --chat 1234567890@s.whatsapp.net --id ABC123 --output ./downloads/photo.jpg --wait 30s
```

This explicitly selects **one stored chat JID and message ID**. Both and `--output` are required. Names, bare phones, guessed aliases and bulk selection are not accepted. Explicit `--before`, `--limit` or `--batch` fail even when empty, zero or equal to the legacy default. The exact `--id`/`--output` flags require `--agent`; legacy retry without them retains its bulk selection, JSON, permissions and writer behavior. There is no scan of pending candidates: the composite indexed getter selects the requested row, even if an old `local_path` points to an absent file or `media_unavailable_at` retains a past failure. Tombstones (including nonpositive deletion dates), negative declared sizes, unsupported types and unverified bindings are refused.

Retry's capability is writable. `--read-only` and `WACLI_READONLY=1` reject it before any store, LOCK, session, file or network effects, including when local bytes might suffice. Parsing, bulk flags, roots configuration, wait and timeout are also validated before opening the archive. One global `--timeout` covers preflight, LOCK wait, connection, receipts, download and publication: **1s–5m**, default 5m. Per-attempt `--wait` is **1s–120s**, default 30s. A longer wait never extends that global deadline.

An existing compatible archive and exact row are preflighted read-only, without initialization, migration or permission repair. Execution then acquires the existing standalone writer LOCK and opens writable. A `sync --follow` owner holding LOCK yields sanitized `store_locked` before WA: retry never contacts owner IPC, stops the owner, delegates, falls back or creates another executor. This also applies to cache/destination reuse. PR17's read-only `media status/download` continue to work alongside the owner.

An already verified destination returns `existing/not_written/recorded=false`; copying a verified cache returns `cached/written` and records the output path only after publication. These paths require a 32-byte plaintext SHA-256 but no key, CDN path, `OpenWA` or connection. Needed network work additionally requires a 32-byte media key. The lazy hook opens the same standalone client, checks authentication and calls the normal `App.Connect` once without QR, preserving its handshake/revocation observation. It avoids `EnsureAuthed`'s broad historical LID migration. The selected row/binding is rechecked after connection, before each receipt, before publication and before recording, without selecting another row or expanding an unknown alias. Normal session opening/connection can update session observations; there is no atomic archive/session snapshot.

The shared legacy receipt engine sends at most **two application media-retry receipts**, with a second attempt only for a non-responder. It retains existing PN/LID resolution, group/broadcast sender/addressing and MediaRetry decryption. Known response aliases can match the selected composite key; unknown or contradictory aliases, another chat with the same ID and another message are ignored. A stored LID may be echoed literally without guessing a reverse alias. Closed/cancelled callbacks cannot update a later operation; notifications do not carry a new application correlation nonce, so a late matching protocol response observed within an open operation is not proof of response freshness. The SDK's own transport behavior is unchanged.

A successfully decrypted reupload downloads through the shared authenticated byte decryptor without the original ciphertext hash, which may change on reupload. Its original plaintext SHA-256, key HMAC, positive declared size and 100 MiB cap still apply. Phone not-found instead tries the stored CDN path **with its stored 32-byte ciphertext hash**; a missing or malformed ciphertext hash refuses that fallback before HTTP and cannot establish unavailability. Only phone not-found plus CDN 403/404/410 records a new dated unavailable marker. A valid CDN fallback returns `downloaded`; expiry after a reupload, no response, timeout, 5xx, decrypt or integrity failure cannot establish unavailability. No-response is `no_response`, never proof of absence. The current `remote.current` remains `unknown`, even with dated phone/CDN observations or verified local bytes.

Results use `downloaded|cached|existing|no_response|unavailable|unknown`, `retry={phone,cdn,observed_at}`, `file_publication=not_written|written|unknown`, `recorded` and nullable `recorded_at`. A successful archive update records `local_path`/`downloaded_at` and clears the old unavailable marker; a successful unavailable update records only that dated observation. `recorded_at` describes persistence in this invocation, not a synchronization date. Retained historical unavailability can coexist with successful output.

Roots, descriptor rechecks, actual copied-byte digest, 0600 output and atomic no-replace link publication reuse PR17 without a legacy clobber writer or rename fallback. An existing mismatch or concurrent destination wins safely; select a different output. File and DB effects are independent: a DB failure or changed metadata after publication preserves `written/recorded=false` and measured output evidence. Cancellation or broken stdout after recording preserves the known persisted result. Typed `error.media` carries this knowledge on stderr without keys, ciphertext hashes, direct paths, URLs, SQL or raw causes. Temporary cleanup before publication does not roll back an already published file; ambiguous publication remains unknown. No journal, cleanup daemon or application replay is introduced. Inspect available observations and decide explicitly before another operation; uncertainty never authorizes automatic repetition.

Validation uses synthetic archives, App/WAFactory fakes and local HTTP/crypto fixtures only. Live WhatsApp interoperability, CDN longevity and future file stability are unvalidated. The bounded downloader still buffers ciphertext/plaintext/CBC copies as described above.

## Explicit local transcription

```bash
wacli --store /path/to/selected-identity media transcribe --file ./audio.ogg --adapter /absolute/executable --agent
wacli media transcribe --file ./audio --adapter /absolute/executable --mime-type audio/ogg --expect-sha256 LOWERCASE_SHA256 --agent --detail full --read-only
```

There is no default transcription provider. An omitted/empty `--adapter` returns `adapter_not_configured` (exit 2); wacli does not discover, install, download, substitute or retry an adapter. Transcription is an explicit complementary command, never an automatic download step. It needs `--agent` and accepts no positional arguments. Global timeout defaults to 300 seconds and must be positive and at most 300 seconds, covering reading and execution.

The input must be a regular local file of at most 25 MiB. The command reuses agent roots/descriptor confinement: configured `WACLI_MEDIA_ROOTS` symlink boundaries resolve once, links beneath the boundary are refused, and selected-store control paths/hardlinks are excluded. Missing stores are not created. Selected account/store is envelope identity only; account selection can read its registry, while the command opens no archive, session, credentials, LOCK or owner IPC and makes no WhatsApp connection. Control-file identities are compared using filesystem metadata without reading their contents. Readonly flag/env is supported because wacli does not write the archive; it does not sandbox the selected executable.

The input is opened once, read in bounded chunks with context checks and a 25 MiB cap, and rechecked for detectable path/descriptor identity, size and modification changes. Kernel filesystem calls are not an arbitrary-filesystem interruption guarantee. A private in-memory copy is hashed and passed on stdin: `input.bytes` and `input.sha256` describe **those same observed bytes**. Optional `--expect-sha256` requires canonical lowercase SHA-256 and mismatch prevents execution. No later path reopen supplies the adapter. These checks do not promise an immutable audio file or an atomic snapshot against an authorized concurrent writer. Memory includes the bounded read buffer and private input copy, plus capped streams/JSON; 25 MiB is an input limit, not a total process RSS limit.

`--mime-type` is validated and bounded to 256 bytes. When omitted, MIME is detected locally from the same observed bytes using standard content sniffing, never the filename, stored message text or an external service. Unrecognized `application/octet-stream` requires an explicit MIME flag. An explicit MIME is a caller declaration, not a guarantee that the file contains supported speech. The selected adapter receives the MIME literally as one argv value.

The executable contract is provider-neutral:

```text
argv: --protocol wacli-transcribe-v1 --mime-type MIME
stdin: exact observed file bytes, followed by EOF
stdout: {"schema_version":1,"text":"...","language":"pt-BR"}
```

The executable path must be absolute, regular and executable (on Windows, a native `.exe`). Wacli starts it directly through `exec.CommandContext`, without a shell, command template or extra user argv. File paths, URLs, keys and binary/base64 are not put in argv or the result envelope. `input.path` is the caller's explicit requested input-path exception. `language` is optional in adapter stdout and bounded to 64 UTF-8 bytes. The single JSON object must have version 1 and a present string `text`; missing/null/wrong-type text, null/wrong-type language, unknown fields, duplicate keys, malformed/raw-invalid-UTF-8 JSON and trailing values fail. Total stdout is capped during reading at 256 KiB and decoded text at 128 KiB. Empty or whitespace-only text returns status `empty`: it establishes neither successful speech recognition nor silence. Nonempty text returns `completed`, describing adapter protocol completion, not accuracy.

Stderr is privately bounded to 8 KiB while excess is drained; raw stdout/stderr, signals, argv and private causes never appear in errors. Nonzero exit, start/pipe failure, timeout/cancellation or invalid/oversized output fail without storing a transcript. Oversized stdout terminates the direct adapter; context cancellation and a 250 ms `WaitDelay` bound pipe waits, including inherited pipes. This does not promise termination of arbitrary descendants. **The selected executable is not a sandbox:** it can make its own network requests, read other files, use inherited environment credentials, write files or launch processes. `source=local` describes this capability's local input/processing path, not a guarantee of no network or external writes. Adapter effects are not rolled back by failure, cancellation or lost output. Wacli itself sends no input/MIME/transcript to a service.

Agent data is explicit `text`, nullable `language`, `status`, `input={path,bytes,sha256}` and `text_truncated`. Compact text has at most 320 Unicode code points; full retains the bounded text. Existing whole-envelope caps remain 1 MiB compact / 8 MiB full. Full retrieval requires a new explicit invocation, which executes the adapter again; no transcript, journal, automatic recovery or replay exists. Broken stdout returns `output_failed` (exit 1) and never implies rollback.

Usage/configuration errors (including absent adapter) exit 2; missing input is `input_not_found` (exit 3); account/store-selection failures keep `store_unavailable` (exit 4). Operational errors exit 1: `path_not_allowed`, `input_unreadable`, `input_too_large`, `hash_mismatch`, `adapter_failed`, `transcription_timeout`, `cancelled`, `adapter_output_invalid`, or `output_failed`. Messages are fixed and sanitized, including parse errors. Validation uses synthetic local stubs only; no real STT quality, private audio or remote provider has been validated. Private/live audio with a remote provider requires separate authorization.

## backfill

Downloads media for every already-synced message that has downloadable metadata
but no local copy yet, over a single connection.

`sync --download-media` only downloads media for messages that *arrive during*
the sync session. Media for messages synced earlier is never fetched by sync;
`media backfill` closes that gap by scanning existing rows.

### Notes

- Files are written under the store media directory (same layout as `download`).
- `--chat` scopes the backfill to a single chat JID.
- `--limit` caps how many files to download (0 = all); newest messages first.
- `--workers` sets the number of concurrent downloads (default 4).
- Runs until completion or interruption by default; explicitly set global `--timeout` to cap a run.
- Requires a writable store; not available in `--read-only` mode.
- Reports counts: pending (total matching), attempted, downloaded, skipped, failed.

### Examples

```bash
wacli media backfill                                   # download all pending media
wacli media backfill --limit 50                        # download the 50 newest pending
wacli media backfill --chat 1234567890@s.whatsapp.net  # one chat only
wacli media backfill --json                            # machine-readable counts
```

## retry

Recovers media that expired off WhatsApp's CDN. `media download` and `media
backfill` fetch directly from WhatsApp's servers, which only keep media for a
limited time — older media returns HTTP 403. `media retry` instead asks the
primary device (your phone) to re-upload the media via WhatsApp's media-retry
protocol (the same mechanism WhatsApp Web uses), then downloads it.

Recovery only works while the phone is online and still holds the media. Media
the phone no longer has is marked unavailable only after the stored CDN path is
also confirmed expired, so later runs can skip genuinely unavailable rows.

### Notes

- Requires a writable store and an online phone; not available in `--read-only` mode.
- Run `media backfill` first; retry is intended for media whose direct CDN download failed.
- Successful retry responses download the re-upload without reusing the original ciphertext hash. The original media-key HMAC, plaintext SHA-256, size limit, and declared file length still apply; failed verification leaves the media pending and does not replace a local file.
- Retry receipts are sent in batches (`--batch`, default 32) with a second
  attempt for non-responders; `--wait` (default 30s) bounds each attempt.
- `--chat` scopes to one chat; `--before YYYY-MM-DD` scopes to media older than a date.
- `--limit` caps how many messages to retry (0 = all pending); newest first.
- Runs until completion or interruption by default; explicitly set global `--timeout` to cap a run.
- Reports counts: requested, recovered, not_on_phone (gone), no_response, failed.
- `no_response` means the phone did not answer in time (often transient) — those
  stay pending and can be retried later; only `not_on_phone` is marked gone.

### Examples

```bash
wacli media retry                                    # try to recover all pending media
wacli media retry --chat 1234567890@s.whatsapp.net   # one chat only
wacli media retry --before 2026-01-01                # only media older than a date
wacli media retry --limit 50 --wait 45s --json       # bounded run, machine-readable
```
