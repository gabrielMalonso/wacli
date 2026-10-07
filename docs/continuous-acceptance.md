# Continuous operation acceptance

Read when: validating a generic agent's continuous capture, investigating a missing offline message, or assessing the pinned SDK's pairing/interactive limits. This extends [offline acceptance](offline-acceptance.md) with synthetic events and real SQLite only. No account, live WhatsApp connection, private archive or remote send is used.

## Scope and result

The implementation baseline is fork `main` `70bd0b264c8ff10995ef3b17ef70c68386c178d2`, integrating the history callback, bounded diagnostics, schema-33 changes and static capabilities increments. The SDK is `go.mau.fi/whatsmeow v0.0.0-20260921121126-35ae40906e74` ([source commit](https://github.com/tulir/whatsmeow/tree/35ae40906e74b6235dab13b69bd15d845f2863d8)). Existing guarantees remain in [sync](sync.md), [history](history.md), [agent](agent.md), [concurrent use](concurrent-use.md) and [outbound](outbound.md).

Two ordinary Go tests in `internal/app/sync_continuous_test.go` extend the existing runner; no new harness or production behavior is introduced:

- `TestSyncContinuousReconnectRestartPreservesArchive` runs production Sync follow, StreamReplaced/reconnect, cancellation/close, then a new App/WA lifetime on the same SQLite archive. A live other-device envelope passes through the pinned SDK's `UnwrapRaw`; its destination LID resolves to PN and its sender remains the own PN. Original history replay before/after reopening cannot overwrite a newer edit, duplicate distinct IDs or resurrect a purged delete-for-me tombstone. Seven successful persistence attempts in the first run describe three distinct rows; counters are not net growth.
- `TestSyncEmptyOfflineReplayDoesNotInventCoverage` supplies zero announced messages/four replay events, observes idle success with zero stored messages, and verifies the absent ID and preventive shutdown debt with no recorded recovery failure. A separately supplied own-history event in a later invocation can add that ID. The fixture asserts conditional ingestion, not that WhatsApp will deliver this event or that it explains the earlier absence.

No client defect was reproduced in this matrix. That finding is limited to the exercised event/persistence paths; it does not resolve the cause of a message absent from an actual offline run. A synthetic preview/completion is a server signal fixture, not four manufactured durable messages.

Recorded implementation start: 2026-10-07 23:08:29 UTC. Diagnosis/scope was reported to the coordinator during inspection; focused acceptance finished at 23:16:42 UTC (8m13s from the recorded start). Go 1.27.1/Linux and the installed caches above were used. The tested Go source is the source committed with this record; the final commit SHA and later gate/PR timestamps are recorded externally to avoid a self-referential commit hash.

| Executed check | Result / retained log SHA-256 |
| --- | --- |
| Plain matrix, three packages, `-count=1 -v` | Exit 0; 40 top-level cases PASS, no SKIP/FAIL. `dist/.tmp/continuous-plain.log`: `227a010ac999bf136efbf8df7e9803f1a7262bbf591bc9b8f2bc46ed9a68b150` |
| FTS matrix, same cases | Exit 0; 40 top-level cases PASS, no SKIP/FAIL. `dist/.tmp/continuous-fts.log`: `3889afd0c56329170f52898d74fa0d9de757293b88b732d6be92008299569f93` |
| FTS race, two new cases, `-count=1 -v -race` | Exit 0; both PASS, no race report. `dist/.tmp/continuous-race.log`: `343bcc5a397a5f82d6ea608e46a7927438f2645cfae4f64602b4e2234a7e8674` |
| Documentation recipe shell syntax, site build/link validation, Go formatting and `git diff --check` | PASS; these are focused checks, not fullgate. |
| Exact final-SHA fullgate / PR publication | Separate coordinator gate-only handoff required below; no PASS claimed by this record. |

Logs are local ignored artifacts, not a public payload. Repeated executions change timing/log digests; compare executed cases and exits as well as the pinned source. Direct handlers' progress can share a line with Go test output, so account for carriage returns/progress when counting top-level PASS records.

## Reused acceptance matrix

Every row below is exercised by the commands below. Test names identify the source fixture, and all run in both plain and FTS modes. Assertions cover local application behavior unless explicitly identified as pinned SDK behavior.

| Case | Existing tests / useful observations | Limit |
| --- | --- | --- |
| Continuous, reconnect, quiet presence | `TestSyncFollowReconnectsAfterStreamReplaced`, `TestSyncFollowReconnectsWhenStaleThresholdExceeded`, `TestSyncFollowIgnoresKeepAliveTimeoutFromPreviousConnection`, `TestSyncQuietPresenceModeSkipsAvailablePresenceAfterReconnect`, new continuous test | Fake connection transitions; no remote uptime or notification-routing guarantee. |
| Offline/history/idle/interruption | `TestSyncIdleWaitsForAdmittedHistory`, `TestSyncCancellationInterruptsAdmittedHistoryDownload`, `TestSyncReplaySignalsExtendIdleWindow`, `TestHistoryCallbackCompletionRestartsIdleWindow`, new empty replay test | Native/downloaded callbacks are exercised, including download failure/cancellation. Only admitted callbacks are drained; future notifications and missing intervals remain unknown. |
| State failure and next startup | `TestFailedAppStateReplayPersistsIntentAndRecoversAtStartup`, `TestSyncNewCoverageCanRetirePriorConnectionDebt`, `TestChatStateLateCallbackAfterCloseCannotPersist` | Ordered local replay/debt across real SQLite reopening; fake fetch success does not explain or repair a real remote LTHash cause. |
| Historical diagnostics after restart | `TestDiagnosticRestartAndConnectionOnlyPreservesSync` | Separate execution slots retain dated observations, not current liveness or global durability. |
| Own authors, PN/LID, edits/tombstones | `TestHistorySenderImportMatrix`, `TestHistorySenderReimportRetainsProtectedRows`, `TestSecretEditWithSDKCrypto`, new continuous test | SDK crypto fixture covers matching/rejected encrypted edits; local verified aliases are not timeless remote identity proof. |
| Decryption failure and primary re-request | `TestSyncEventHandlerWarnsOnUndecryptableMessage`, `TestSDKFailedDecryptionRequestsPrimary`, `TestSDKDisabledAndRepeatedFailuresDoNotRequestAnotherCopy`, `TestSDKPrimaryRerequestCanBeCancelled`, `TestSDKTypedUnavailableStillAttemptsPrimary`, `TestSDKPrimaryResponseDeliversNormalMessage` | Pinned SDK node/crypto/store handling runs offline. Primary-request boundary is deliberately refused; a separately supplied response becomes a normal message. This proves neither primary reachability nor successful recovery. |
| Media and interruption/failure | `TestSyncDownloadMediaCanonicalizesLIDChatBeforeEnqueue`, `TestSyncOnceDrainsMediaBeforeExit`, `TestMediaWorkerSurvivesPanic`, `TestDownloadMediaDirectToFile`, `TestDownloadRetriedMediaAuthenticatesReupload` | Fake queue downloads plus local HTTP encrypted/integrity fixtures. No CDN availability, real media retry, playback or rendering claim. |
| Logout and storage caps | `TestRunSyncFollowLoggedOutWinsOverPendingReconnect`, `TestRunSyncFollowStopsWhenLoggedOutDuringReconnect`, `TestSyncStopsAtMaxMessages` | Terminal signals/cancellation retain legacy exits; no process-kill/power-loss simulation. |
| Uncertain outbound and late history | `TestOutboundMilestonesFailuresAndNoApplicationRetry`, `TestOutboundEventSyncCloseDuringManualHistory` | Application invocation/checkpoint/drain rules; no wire-once promise, real ACK or permission to resend. |
| Readonly, legacy output, change cursor | `TestAgentSupportedCommandsPreserveReadOnlyStore`, `TestAgentFlagIntentAndLegacyBoundaries`, `TestChangesCLIReadOnlyPageResumeUnderWriterLock` | CLI runners and isolated SQLite/LOCK; no change-feed backfill, consumer acknowledgement, GC or indistinguishable-clone detection. |
| Passkey and native-flow boundary | `TestQRChannelEventError`, `TestParseInteractiveMessageWithNativeFlowButtons`, `TestParseInteractiveTemplateWithNativeFlowButtons`, `TestBuildSelectResponseMessageTemplateAndNativeFlow` | Parsing and explicit refusal only, not successful pairing or a remote phone tap. |

## Reproduce

Use the installed toolchain/caches only, with no network module fallback. Do not source an old `dist/dev-env.sh`, select a real account/store, or enable opt-in live tests. The new tests run automatically under `pnpm test`'s existing plain/FTS runners.

```bash
set -euo pipefail
export PATH='/home/gabriel-alonso/.local/share/wacli-dev/go1.27.1-a4f23ee/setup.0Oosn7/go/bin:/home/gabriel-alonso/.local/bin':"$PATH"
export GOTOOLCHAIN=local GOSUMDB=off
export GOCACHE='/home/gabriel-alonso/.local/share/wacli-dev/go1.27.1-a4f23ee/cache'
export GOMODCACHE='/home/gabriel-alonso/.local/share/wacli-dev/go1.27.1-a4f23ee/modules'
export GOPATH='/home/gabriel-alonso/.local/share/wacli-dev/go1.27.1-a4f23ee/gopath'
export GOPROXY="file://$GOMODCACHE/cache/download"
go version
git rev-parse HEAD
git status --short
mkdir -p dist/.tmp

app_cases='TestSync(ContinuousReconnectRestartPreservesArchive|EmptyOfflineReplayDoesNotInventCoverage|IdleWaitsForAdmittedHistory|CancellationInterruptsAdmittedHistoryDownload|ReplaySignalsExtendIdleWindow|FollowReconnectsAfterStreamReplaced|FollowReconnectsWhenStaleThresholdExceeded|FollowIgnoresKeepAliveTimeoutFromPreviousConnection|QuietPresenceModeSkipsAvailablePresenceAfterReconnect|NewCoverageCanRetirePriorConnectionDebt|EventHandlerWarnsOnUndecryptableMessage|DownloadMediaCanonicalizesLIDChatBeforeEnqueue|OnceDrainsMediaBeforeExit|StopsAtMaxMessages)|TestHistory(CallbackCompletionRestartsIdleWindow|SenderImportMatrix|SenderReimportRetainsProtectedRows)|TestFailedAppStateReplayPersistsIntentAndRecoversAtStartup|TestChatStateLateCallbackAfterCloseCannotPersist|TestDiagnosticRestartAndConnectionOnlyPreservesSync|TestSecretEditWithSDKCrypto|TestMediaWorkerSurvivesPanic|TestRunSyncFollow(LoggedOutWinsOverPendingReconnect|StopsWhenLoggedOutDuringReconnect)|TestOutbound(MilestonesFailuresAndNoApplicationRetry|EventSyncCloseDuringManualHistory)'
wa_cases='TestSDK(FailedDecryptionRequestsPrimary|DisabledAndRepeatedFailuresDoNotRequestAnotherCopy|PrimaryRerequestCanBeCancelled|TypedUnavailableStillAttemptsPrimary|PrimaryResponseDeliversNormalMessage)|TestDownload(MediaDirectToFile|RetriedMediaAuthenticatesReupload)|TestQRChannelEventError|TestParseInteractive(MessageWithNativeFlowButtons|TemplateWithNativeFlowButtons)'
cli_cases='TestBuildSelectResponseMessageTemplateAndNativeFlow|TestAgent(SupportedCommandsPreserveReadOnlyStore|FlagIntentAndLegacyBoundaries)|TestChangesCLIReadOnlyPageResumeUnderWriterLock'
continuous_cases="^($app_cases|$wa_cases|$cli_cases)$"

for acceptance_mode in plain fts; do
  acceptance_tags=()
  if [ "$acceptance_mode" = fts ]; then acceptance_tags=(-tags sqlite_fts5); fi
  go test -count=1 -v "${acceptance_tags[@]}" ./internal/app ./internal/wa ./cmd/wacli \
    -run "$continuous_cases" > "dist/.tmp/continuous-$acceptance_mode.log" 2>&1
done
go test -count=1 -v -race -tags sqlite_fts5 ./internal/app \
  -run '^TestSync(ContinuousReconnectRestartPreservesArchive|EmptyOfflineReplayDoesNotInventCoverage)$' \
  > dist/.tmp/continuous-race.log 2>&1
sha256sum dist/.tmp/continuous-{plain,fts,race}.log
```

Inspect logs for all selected top-level cases executing with PASS, not SKIP; a package-level success alone does not prove the selection ran. Preserve failed logs and record command exit codes. Fakes and synthetic paired SDK session fixtures never Connect to WhatsApp. Local HTTP endpoints are fixture-owned.

The exact required pre-PR gate remains:

```bash
pnpm format:check && pnpm lint && pnpm lint:deadcode && pnpm test && pnpm build && pnpm docs:site && git diff --check
```

For this implementation, the T3 worktree's long physical path is unsuitable for the full socket-containing gate. After the final clean commit, the coordinator creates a new gate-only T3 thread in its own physically short worktree, with a physical child TMPDIR. Run the integral sequence there on the exact final SHA before publishing the PR. Do not reuse older gate worktrees, create/move worktrees with shell, change guards/tooling, use symlink or `/proc` TMPDIR aliases, or claim focused runs as fullgate proof. Gate SHA/command/exit/timestamps/log digest belong in the PR/coordinator handoff; this committed record does not certify a future gate.

## SDK boundaries

Passkey is a **WACLI integration limit**, not an absence of SDK primitives. The pinned [passkey implementation](https://github.com/tulir/whatsmeow/blob/35ae40906e74b6235dab13b69bd15d845f2863d8/pair-passkey.go) exposes `SendPasskeyResponse` after a request and an authenticator's WebAuthn response, followed by confirmation with retained linking state. The pinned [QR channel](https://github.com/tulir/whatsmeow/blob/35ae40906e74b6235dab13b69bd15d845f2863d8/qrchan.go) surfaces request/confirmation events; it can auto-confirm `SkipHandoffUX`. WACLI's surfaced passkey events stop with an actionable error (`qrChannelEventError`); no authenticator UI/response integration is implemented. Fixtures verify that refusal, not a real pairing exchange. Preserve existing authenticated stores; an error never authorizes reset/relink.

Native-flow parsing, response construction and remote success are separate. WACLI recognizes inbound `cta_url`, `cta_call` and `quick_reply` controls; quick replies retain `response_type=interactive_response`. The pinned [protobuf schema](https://github.com/tulir/whatsmeow/blob/35ae40906e74b6235dab13b69bd15d845f2863d8/proto/waE2E/WAWebProtobufsE2E.proto) has `InteractiveResponseMessage` and `NativeFlowResponseMessage` name/paramsJSON/version fields. The SDK's [send implementation](https://github.com/tulir/whatsmeow/blob/35ae40906e74b6235dab13b69bd15d845f2863d8/send.go) classifies these as native-flow responses but leaves specialized payload construction to callers. This is schema/transport support, not a guaranteed compatible phone-tap payload. WACLI's `buildSelectResponseMessage` explicitly refuses native-flow quick replies. Classic buttons/lists produce quoted text; template replies use their existing typed message. None proves remote workflow completion. Compatibility would require separately authorized observed protocol fixtures and live QA, not guessed JSON or a dependency upgrade.

## Historical absence and remaining live acceptance

The sanitized historical observation on build `bef4a09` was: another-device marker visible in WhatsApp Web while WACLI was offline; `sync --once --idle-exit 3s --max-reconnect 10s --presence-mode quiet` stopped normally in about 6.05 seconds with zero stored messages, zero announced messages/four replay events; local marker search returned `[]`. This establishes absence in that local execution/query only. It establishes neither permanent loss nor the cause. Preventive `regular`, `regular_high`, `regular_low` debt with `recovery_observations=[]` is not a reproduced recovery failure. That run is historical context, not an acceptance run performed here.

A future authorized test account/phone can investigate actual offline/restart delivery, remote state changes, decryption retry outcomes and media availability with dated IDs/windows and sanitized events. Until then, complete history, since-offline recovery, SDK/network liveness, power-loss durability, passkey pairing and native-flow remote completion remain unproven. Backfill starts before the oldest local anchor; it does not automatically target a newer gap. Missing history never permits replay of an uncertain outbound operation.
