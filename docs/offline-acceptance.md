# Offline acceptance

Read when: checking the generic reference CLI scope with synthetic fixtures before a separately authorized live acceptance.

Baseline: `3ba371e713cc162e828e29095fbdd0623eb5d805` (fork PRs 1–20). Static inspection found no blocking generic gap requiring new production code. This is local contract acceptance, not WhatsApp Web parity or live approval. The recipe below reuses existing tests; it adds no test harness or production feature.

Reference requirements come only from generic portions of `tools/cli/python/whatsapp_cli/__main__.py`, `whatsapp.py`, and `tools/cli/docs/whatsapp-cli.md` in the reference checkout. These files were read, never imported or executed. No memory, patient, secret configuration or clinical routine is an acceptance input.

## Requirement map

Commands below use `--agent` unless explicitly marked legacy. Evidence links pin the inspected baseline; test names identify the relevant assertions rather than claiming remote success.

| Generic requirement / reference operation | Existing wacli command | Code / test evidence | Limit or pending validation |
| --- | --- | --- | --- |
| Status / `status` | `auth status`; offline `doctor` | [Agent queries](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/agent%5Ftest.go), `TestAgentIdentityAndOfflineEvidence` | Stored public identity/auth observation; no browser/CDP or current connectivity claim. |
| List/select conversations / `unread`, `list_visible`, `scan_open`, `open_visible`, `open_phone` | `chats list --unread --cursor TOKEN`; `chats show --jid JID` | [Chat query/page code](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/store/chats%5Fpage.go); [page tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/chats%5Fpage%5Ftest.go) | Local filters/order, explicit JID selection; no visible index, current tab, favorites-filter parity or exclusive work claim. |
| List/search/read/context / `inspect_current`, `current`, `INSPECT_JS_TEMPLATE` | `messages list`; `messages search QUERY --sort time`; `messages show/context --chat JID --id ID` | [Read commands](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/messages%5Fread.go); [agent tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/agent%5Ftest.go), `TestAgentUnicodeAndPublicMessageContent`; [context tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/store/messages%5Ftest.go) | Stored text, direction, quote/media metadata; context is local adjacency, not retrieval of all quoted content. |
| Bounded projections and traversal / output projections, `scan_open` | `--detail compact` / `--detail full`, `--limit N`, `--cursor TOKEN` on supported lists/temporal search | [Message pages](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/store/messages%5Fpage.go); [search pages](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/store/search%5Fpage.go); [scope/size tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/agent%5Ftest.go) | Compact reports truncation; full is bounded. Cursors bind store/query/filters and read live pages: concurrent writes can repeat/omit rows. Relevance search has no cursor. |
| Older history / visible scroll inspection | `history coverage --chat JID --evidence`; explicit live `history backfill --chat JID` | [Recovery code](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/history%5Fagent.go); [binary fixture](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/history%5Fe2e%5Ftest.go) | Dated requests/responses/anchors and stop reasons never prove complete history; phone availability is live pending. |
| Contacts/phone resolution / `contact_phone`, `open_contact_panel` | `contacts list/search/show`; `contacts resolve PHONE_OR_JID` | [Resolver](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/contacts%5Fresolve.go); [locked-store fixture](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/contacts%5Fresolve%5Ftest.go), `TestContactsResolveReadsSyntheticStoreWhileLocked` | Uses retained public PN/LID mappings; unknown remains unresolved, no phone inferred from LID digits or remote contact discovery. |
| Unread/archive / `mark_unread`, `archive_current` | `chats mark-unread/archive/unarchive --chat PHONE_OR_JID` | [State code](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/chats%5Fagent.go); [state/IPC fixtures](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/chats%5Fagent%5Ftest.go) | Explicit target replaces header checks. SDK outcome and local mirror are separate; current remote state remains unknown. |
| Read state / opening/reading the browser conversation | Legacy `chats mark-read --chat JID --json [--receipts]` | [Legacy state code](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/chats%5Fstate.go); [receipt tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/read%5Freceipts%5Ftest.go) | Archive reads do not mark read. `mark-read` is outside `--agent`; plain mode uses app state, optional receipts are bounded/privacy-aware. Neither proves sender-visible blue ticks. |
| Prepare/review/replace/discard / `paste_draft`, `preflight`, `clear_draft` | `draft create --to JID --message TEXT`; `draft show D --revision R --detail full`; `draft update/discard D --if-revision R` | [Draft preparation](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/drafts.go); [revision/review binary tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/drafts%5Fipc%5Ftest.go) | Frozen own/recipient identity, exact payload hash and CAS; preview does not record approval or prove remote recipient existence. Review all relevant full fields when compact truncates. |
| Send/reply and confirm / `send_draft`, `safe_reply`, `confirm_sent` | Text/document draft `--reply-to ID`; `outbound send D --revision R --expect-hash H --key K`; `outbound show/list` | [App dispatch](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/outbound.go); [typed/frozen-quote tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/outbound%5Ftest.go); [SDK boundary](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/wa/outbound%5Ftest.go) | Reply freezes eligible local textual quote. One application `SendMessage` invocation per retained operation; SDK same-ID retries remain permitted during/after it. No exactly-once remote promise. |
| Document/card / `send_document`, `send_contact` | `draft create --to JID --file PATH`; or `--contact-name NAME --contact-phone PN`, then explicit outbound | [Payload preparation](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/drafts.go); [standalone document/card fixture](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/drafts%5Fipc%5Ftest.go); [typed dispatch](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/outbound%5Ftest.go) | Document snapshot bytes/hash rechecked at send. Card is explicit escaped vCard, not reference contact allowlist/picker UX; compatibility and uploads remain live pending. |
| Download audio/attachment / `download_audio`, `download_attachment` | `media status --chat JID --id ID [--verify]`; `media download --chat JID --id ID --output PATH` | [Media commands](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/media%5Fagent.go); [binary local TLS fixture](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/media%5Fagent%5Fe2e%5Ftest.go) | Read-only archive, no LOCK/IPC; explicit output is a read-only-policy exception, `recorded=false`. Verified local bytes do not prove current CDN availability. |
| Recover one media item | `media retry --chat JID --id ID --output PATH` | [Exact retry](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/media%5Fretry%5Fexact.go); [guards/cache/binary fixtures](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/media%5Fretry%5Fagent%5Ftest.go), `TestAgentMediaRetryOwnerLockNeverTouchesIPCOrWA` | Writable standalone LOCK only, no owner IPC. Held owner lock yields `store_locked` exit 4 before WA; phone/CDN failure is dated evidence, not permanent absence. |
| Transcription / `transcribe_audio` | `media transcribe --file PATH --adapter /absolute/executable` | [Adapter execution](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/app/transcription.go); [production binary stub tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/media%5Ftranscribe%5Ftest.go) | No default provider, installation or sandbox. Stub checks protocol/bytes/errors, not speech recognition or STT quality; full reruns the adapter. |
| Independent tasks on one account | Existing local reads/drafts and owner outbound IPC | [Independent request fixtures](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/independent%5Frequests%5Ftest.go) | Distinct keys/operations keep correlated results under existing pacing/serialization. No claims, fencing, reservations of work, consumer cursor or queue distribution. |
| Explicit snapshot retention cleanup | `draft cleanup preview D`; `draft cleanup apply D --revision R --if-revision HEAD --expect-hash H` | [Cleanup catalogue tests](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/internal/store/drafts%5Fcleanup%5Ftest.go) | Only discarded document revisions with no outbound reference; immutable rows/keys/evidence retained. No automatic GC or orphan sweep. |

Archive reads use [the read-only opener](https://github.com/gabrielMalonso/wacli/blob/3ba371e713cc162e828e29095fbdd0623eb5d805/cmd/wacli/root.go) without LOCK, migrations or store initialization. Selected account/store is frozen before later opens/IPC (`TestAgentSelectionStaysWithResolvedAccount`). See [agent contract](agent.md), [drafts](drafts.md), [history](history.md), [chats](chats.md) and [media](media.md) for the authoritative bounds/errors.

[Outbound](outbound.md) separates `attempt_result` (`accepted`, `rejected`, `not_dispatched`, `uncertain`, `pending`) from derived acceptance/delivery/read and server-error facts. Group receipts are per participant, never proof that everyone received/read. Own echo alone is not acceptance. `UNIQUE(account_jid,idempotency_key)` binds exact D/R/H in one retained archive; restore/loss can remove that protection. Existing owner IPC validates correlated typed replies; possible dispatch plus lost/mismatched reply never permits standalone fallback or application replay.

Browser-only behavior is excluded: CDP profiles/tabs, DOM headers, visible indexes, clipboard/composer, parking, tab cleanup and visual confirmation. Clinical behavior is excluded: agenda/patient/memory lookup, templates, routing, hard stops, confirmation manifests and archiving policy. These exclusions do not justify adding matching Python command names to wacli.

## Reproduce with fixtures only

Run on Linux from the fork checkout. Use the already installed project toolchain; replace the source path only with another existing toolchain. No dependency/tooling installation is part of this recipe. Use the cached file proxy for Go module/tool metadata; all required modules/tools must already be cached (no network fallback). `TMPDIR=/tmp` keeps UNIX socket paths and native filesystem roots suitable for the existing tests.

```bash
set -euo pipefail
source /home/gabriel-alonso/Projetos/wacli/dist/dev-env.sh
export TMPDIR=/tmp GOSUMDB=off
export GOPROXY="file://$GOMODCACHE/cache/download"
workspace_dir="$(pwd -P)"
mkdir -p "$workspace_dir/dist/.tmp"
acceptance_dir="$(mktemp -d "$workspace_dir/dist/.tmp/offline-acceptance.XXXXXX")"
# Isolate Linux account registry/default store; never repurpose HOME.
export XDG_STATE_HOME="$acceptance_dir/state"
mkdir -p "$XDG_STATE_HOME/wacli"
unset WACLI_STORE_DIR
export WACLI_READONLY=1
pnpm build
acceptance_binary="$workspace_dir/dist/wacli"

# Existing production CLI list/search and chat traversal fixtures.
bash scripts/e2e-agent-pagination.sh "$acceptance_binary"
bash scripts/e2e-agent-chats-pagination.sh "$acceptance_binary"

# Tests deliberately write their fixtures and set their own read-only barriers.
# A global store override would defeat named-account routing tests.
export WACLI_READONLY=0
# Opt-in tests create their own synthetic archives/identities and fake owners.
export WACLI_DRAFT_E2E_BINARY="$acceptance_binary"
export WACLI_OUTBOUND_E2E_BINARY="$acceptance_binary"
export WACLI_HISTORY_E2E_BINARY="$acceptance_binary"
export WACLI_CHAT_STATE_E2E_BINARY="$acceptance_binary"
export WACLI_MEDIA_E2E_BINARY="$acceptance_binary"
export WACLI_MEDIA_RETRY_E2E_BINARY="$acceptance_binary"
export WACLI_TRANSCRIBE_E2E_BINARY="$acceptance_binary"
go test -count=1 -v -tags sqlite_fts5 ./cmd/wacli \
  -run '^(Test(DraftProductionBinary.*|DraftCleanupProductionBinary.*|OutboundProductionBinary.*|HistoryEvidenceProductionBinaryOffline|AgentChatStateProductionBinaryOwner|AgentMediaProductionBinaryHTTPSUnderOwnerLock|AgentMediaRetryProductionBinary|AgentTranscriptionProductionBinary.*))$'

# Required pre-PR gate: plain + FTS tests, fake app/WA, SDK retry and IPC risks.
pnpm format:check && pnpm lint && pnpm lint:deadcode && pnpm test && pnpm build && pnpm docs:site && git diff --check
printf 'Retained acceptance scratch: %s\n' "$acceptance_dir"
```

Test names must execute with `PASS`, not `SKIP`; check exit status and logs. The media binary fixture routes its production hostname through a fixture CONNECT proxy exclusively to local TLS; other HTTP fixtures use loopback. Outbound/history/state owners use fake adapters or synthetic sockets, never a WhatsApp connection. Transcription uses generated bytes and a local protocol stub. Existing fixture cleanup remains test-owned; the chat traversal fixture and acceptance scratch are retained under ignored `dist/.tmp`.

## Live acceptance still pending

The manager must separately authorize/use a test account and phone to validate pairing/sync freshness, real PN/LID/group targets, text/reply/document/card compatibility, ACK and receipt attribution (including group participants/privacy), app-state read/unread/archive, phone history and exact media recovery/CDN behavior. Test standalone recovery without a follow owner, and owner actions with a compatible running follow process. STT needs an explicitly chosen trusted adapter/environment and authorized audio to measure language/quality; this offline stub cannot approve it. No personal store, private logs/JIDs, credentials or clinical data belong in this acceptance record.

## Immutable image increment

The image feature is an approved extension reviewed from `92df3f9d8243367531b011e4071c4ca24ce13298`, distinct from the historical baseline above. Fixtures exercise static JPEG/PNG preparation, APNG/truncated/format/pixel rejection with small headers, old canonical hashes, exact snapshots, readonly/version/union guards, CLI standalone/owner, original-byte MediaImage dispatch, SDK-boundary failures/idempotency, image history/echo and metadata-only DTOs. They do not establish live WhatsApp compatibility or human visual review. Voice/audio are excluded.

With the existing installed toolchain and cached modules only, reproduce the real base-owner proof by extracting the pinned source and copying **only** the test helper, leaving its production decoder/validator/executor unchanged:

```bash
source /home/gabriel-alonso/Projetos/wacli/dist/dev-env.sh
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local
mkdir -p dist/.tmp
export TMPDIR=/tmp GOTMPDIR=/tmp
image_base_dir="$(mktemp -d "$PWD/dist/.tmp/image-base.XXXXXX")"
git archive 92df3f9d8243367531b011e4071c4ca24ce13298 | tar -x -C "$image_base_dir"
python3 - "$image_base_dir" <<'PY'
from pathlib import Path
import sys
source = Path('cmd/wacli/drafts_image_compat_test.go').read_text()
helper = source[source.index('func TestImageBaseOwnerProcess'):source.index('func TestDraftImageRealBaseOwnerAndOldClient')]
imports = '''package main
import("context";"errors";"fmt";"io";"os";"sync/atomic";"testing";
"github.com/openclaw/wacli/internal/app";"github.com/openclaw/wacli/internal/lock";"github.com/openclaw/wacli/internal/wa")
'''
Path(sys.argv[1], 'cmd/wacli/image_base_owner_test.go').write_text(imports + helper)
PY
(cd "$image_base_dir" && go test -c -tags sqlite_fts5 -o "$image_base_dir/base-tests" ./cmd/wacli && CGO_ENABLED=1 CGO_CFLAGS=-Wno-error=missing-braces go build -tags sqlite_fts5 -o "$image_base_dir/base-cli" ./cmd/wacli)
export WACLI_IMAGE_BASE_TEST_BINARY="$image_base_dir/base-tests"
export WACLI_IMAGE_BASE_BINARY="$image_base_dir/base-cli"
go test -count=1 -v -tags sqlite_fts5 ./cmd/wacli -run '^TestDraftImageRealBaseOwnerAndOldClient$'
```

For the full gate, use ordinary short real `TMPDIR=/tmp` and `GOTMPDIR=/tmp`, without `/proc` aliases, a media-root override or namespace/mount changes. Normal test/toolchain cleanup of its own temporary files is part of execution; do not manually clean outside the workspace. Neither HOME nor any account/store is repurposed. Retain the base copy and proof logs under ignored `dist`. For the already-cached deadcode tool metadata, use `GOPROXY="file://$GOMODCACHE/cache/download"` with `GOSUMDB=off`, without network fallback or installations.

Run image tests in both plain and FTS modes, plus a proportional race run. After building the final CLI, set `WACLI_DRAFT_E2E_BINARY` and `WACLI_OUTBOUND_E2E_BINARY` to that absolute binary path and run `TestDraftImageProductionBinary` and `TestOutboundImageProductionBinary` explicitly with PASS. The base test also verifies old-client `--image` refusal, old-owner create/update refusal and old-owner stored-image dispatch failure before any WA factory call. Public-source paths, thumbnail blobs and false delivery/read claims must remain absent from returned DTOs. Keep the exact pre-PR full gate and final-SHA evidence described above; fixtures never authorize live sends.

## Immutable voice acceptance extension

Voice is an approved additive increment from integrated image main `0f74dcd142738fe63cc9a60d5cd094120c5bf095` (same tree as image `9727cb914d15f14617b07fdc02bcd4e24546d669`). It does not extend authorization to live accounts, providers or transcription. [The local profile](drafts.md#immutable-voice-ptt) distinguishes unsupported broader formats from malformed structure and quotas from WhatsApp limits. Test bounded synthetic framing, not PCM decoding/quality.

`TestVoiceOggOpusDesignFixtures` checks the 58 design vectors. `TestVoiceOggCRCIndependentVector` uses fixed bytes/checksum independently reproduced with installed libogg during preparation; Go tests have no libogg dependency. Nonempty CBR/VBR frames, padding/length/count boundaries, fragment terminating zero, granule/pre-skip/trim, malformed/chained streams and local quota edge/+1 cases are included. Reduced internal limits avoid maximum allocations/stress. A short optional framing fuzz run can use `GOMAXPROCS=2 go test ./internal/wa -run '^$' -fuzz '^FuzzVoiceOpusFraming$' -fuzztime=2s -parallel=1`.

Run focused voice and adjacent draft/outbound/legacy/quote tests in plain/FTS and proportional race. The golden voice hash is independently computed; the pre-voice image hash is pinned from the unchanged image tree, alongside existing text/document/contact goldens. Tests cover CLI standalone/IPC, readonly before source/socket, immutable snapshots and source replacement, metadata comparison before network, absent optional wire fields, key conflicts/duplicates/checkpoints, ACK/history failure, raw PTT echoes, unknown/played receipts and document-only cleanup. Categorical optional advice must preserve correlations and post-publication/transport uncertainty, including missing/unknown advice.

For actual old-owner compatibility, archive the image base under an ignored fixture directory and compile its **unchanged** test helper `TestImageBaseOwnerProcess` plus CLI. It runs the base decoder/validator/executor with a zero-count refusing WA factory, not a canned response. Set `WACLI_VOICE_IMAGE_OWNER_TEST_BINARY` and `WACLI_VOICE_IMAGE_OWNER_BINARY` to those absolute artifacts and run `TestDraftVoiceRealOlderOwnersAndOldClient`. Supply the already documented `WACLI_IMAGE_BASE_TEST_BINARY`/`WACLI_IMAGE_BASE_BINARY` to also exercise the real pre-image owner/client. This checks voice v3/v2/wrong legacy envelope refusal and stored-voice dispatch rejection before WA access. Never use a real store for this fixture.

After the exact full gate on the final commit with ordinary `/tmp`, set `WACLI_DRAFT_E2E_BINARY` and `WACLI_OUTBOUND_E2E_BINARY` to its freshly built absolute CLI path. Run `TestDraftVoiceProductionBinary`, `TestOutboundVoiceProductionBinary` and the actual older-owner test explicitly with PASS, not skipped. Retain final SHA/environment/command/exit/log digest and failed fixture logs. Public DTOs must omit original paths, tags and blobs; metadata does not certify decoded duration, speech quality or human approval. Actual WhatsApp playback/presentation with absent waveform/seconds remains separate authorized live QA, not established by these fixtures.
