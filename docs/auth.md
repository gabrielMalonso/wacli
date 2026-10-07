# auth

Read when: pairing a store, checking auth state, logging out, or choosing QR vs phone pairing.

`wacli auth` connects interactively and bootstraps sync after successful pairing. `wacli sync` never shows a QR code, so use `auth` first for a new store or named account.

## Commands

```bash
wacli auth [--follow] [--idle-exit 30s] [--download-media] [--qr-format terminal|text] [--phone PHONE] [--events]
wacli auth status
wacli auth logout
wacli --account work auth status
```

## Notes

- Default pairing prints a terminal QR code.
- `--qr-format text` prints the raw QR payload for external renderers.
- `--phone PHONE` uses WhatsApp phone-number pairing instead of QR pairing.
- Transient websocket drops before pairing completes are retried with a fresh QR/code.
- Passkey-gated pairing is not yet supported. If WhatsApp requests passkey verification or confirmation, auth stops with an actionable error instead of continuing to rotate unusable QR codes.
- The pinned SDK exposes passkey/WebAuthn response and confirmation primitives; WACLI has no authenticator integration. See [the tested integration boundary](continuous-acceptance.md#sdk-boundaries); a refusal does not authorize clearing an existing store.
- After pairing, auth runs bootstrap sync until idle unless `--follow` is set.
- Bootstrap sync honors `WACLI_SYNC_MAX_MESSAGES` and `WACLI_SYNC_MAX_DB_SIZE` to cap local history growth.
- `--events` emits NDJSON lifecycle events on stderr, including raw QR and phone-pairing codes for external renderers.
- `auth status` reads the existing session read-only by default and creates no store, session, or writer lock. A missing session reports unauthenticated. It reports whether the local store is authenticated. A recorded remote logout overrides a stale device row until WhatsApp confirms a new login.
- `auth status` and offline `doctor` use normal SQLite readonly locking. WAL/SHM bookkeeping may be created or updated even when the session starts without sidecars; required permission failures are not retried as immutable reads. See [store readonly access](store.md#local-reads-by-default) for the no-create and live-read limits.
- Legacy offline `doctor` keeps diagnostic exit 0 and reports an unreadable auth source in `store_error`. Its existing `authenticated=false` alongside that auth-source error does not prove logout or loss of authentication; `linked_jid` is omitted. Known auth/JID data is retained when only the archive fails. An absent session remains normally unauthenticated. Agent doctor reports a sanitized error with exit 4 and no auth data on a failed read.
- `auth logout` invalidates the linked-device session and requires writable mode.
- For multiple accounts, prefer `wacli accounts add NAME`; it creates an isolated account store and runs the same auth/bootstrap flow.

## Examples

```bash
wacli auth
wacli auth --qr-format text
wacli auth --phone "+1 (234) 567-8900"
wacli auth --download-media
wacli auth status --json
wacli auth logout
```
