# media

Read when: downloading media from a synced message.

`wacli media` downloads media referenced by messages already stored in `wacli.db`.

## Commands

```bash
wacli media download --chat JID --id MSG_ID [--output PATH]
wacli media backfill [--chat JID] [--limit N] [--workers N]
wacli media retry [--chat JID] [--before YYYY-MM-DD] [--limit N] [--batch N] [--wait DUR]
wacli --agent media status --chat JID --id MSG_ID [--verify] [--output PATH]
wacli --agent media download --chat JID --id MSG_ID --output PATH
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

An old `media_unavailable_at` is returned as a dated retained phone-and-CDN observation, while `remote.current` remains `unknown`. It may coexist with local bytes. Missing cache/path and expired CDN do not establish unrecoverability. HTTP 403/404/410 produces sanitized `media_expired`; it does not update that marker. Exact message retry is not yet exposed. Existing legacy `media retry` is bulk; `--limit 1` chooses a pending row, not a requested message. No automatic recovery or availability guarantee is offered.

Agent IO explicitly extends `WACLI_MEDIA_ROOTS` to cache reads and output writes, using a separate helper; legacy upload enforcement is unchanged. When configured, cache must satisfy both the selected archive media boundary and an allowed root, and output must satisfy an allowed root. Configured symlink roots resolve once; symlinks beneath a boundary, including file symlinks that stay inside it, are refused. Without roots, output uses its explicit filesystem path with the same no-symlink component rule. The selected store permits output only below `media`, never DB/session/LOCK/socket/heartbeat or other control paths. Hardlinks to top-level store control files are also refused by file identity. Descriptors and directory/file identity are rechecked after opening and before publication; this is confinement, not an OS sandbox against an authorized concurrent writer.

Existing destinations are never overwritten by agent download. Different, unverified or nonregular destinations fail; choose another explicit path. New parent directories are 0700 and temporary/output files are 0600 without chmod of preexisting directories. Verified bytes are copied into a private temporary, then published by atomic same-filesystem link without replacement. A concurrent destination creation cannot be clobbered. Unsupported link filesystems fail safely; there is no rename fallback. Cancellation/integrity failure before publication removes only this attempt's temporary; created parent directories can remain. `not_written` refers to the final file. Ambiguous publication can retain it; there is no cleanup/replay service. Publication or stdout failure retains known effects in `error.media`; timeout and lost output do not imply rollback. Files are synced before publication, but directory/power-loss durability and an atomic DB/filesystem snapshot are not promised.

The existing direct crypto path buffers ciphertext, plaintext and a CBC copy; peak memory can be several times the 100 MiB file cap. The bounded copy buffer is 32 KiB, not the downloader's total memory limit. Agent deadlines must be positive and at most five minutes. URLs, direct paths, media keys, ciphertext hashes, HTTP bodies and raw internal causes never appear in its DTO/errors. Compact/full differ in filename truncation; binary content is never included in the envelope. See [agent contract](agent.md#media-observations-and-explicit-output).

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
