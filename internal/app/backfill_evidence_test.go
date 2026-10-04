package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func readAttempt(t *testing.T, a *App, chat string) store.HistoryRecoveryEvidence {
	t.Helper()
	got, err := a.db.ListHistoryRecoveryEvidence(context.Background(), []string{chat})
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}
func evidenceFixtureSQL(t *testing.T, a *App, query string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func TestBackfillEvidenceSuccessAndNoAnchorIndependentOfChat(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		return backfillTestResponse(chat, "older", base.Add(-time.Second))
	}
	opts := backfillRetryOptions(chat)
	opts.AttemptID = "0123456789abcdef0123456789abcdef"
	res, err := a.BackfillHistory(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := readAttempt(t, a, chat)
	if res.AttemptID != opts.AttemptID || got.Latest.AttemptID != res.AttemptID || got.LastSuccess.AttemptID != res.AttemptID || got.Latest.NetGrowth == nil || *got.Latest.NetGrowth != 1 {
		t.Fatalf("correlation: %+v %+v", res, got)
	}
	evidenceFixtureSQL(t, a, `DELETE FROM messages; DELETE FROM chats`)
	opts.AttemptID = "1123456789abcdef0123456789abcdef"
	_, err = a.BackfillHistory(context.Background(), opts)
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Code != "no_local_anchor" || typed.History.Outcome != "not_dispatched" {
		t.Fatalf("no anchor: %v", err)
	}
	got = readAttempt(t, a, chat)
	if got.Latest.State != store.HistoryError || got.Latest.AttemptID != opts.AttemptID || got.LastSuccess.AttemptID == opts.AttemptID || got.Latest.NetGrowth != nil {
		t.Fatalf("lost prior observation: %+v", got)
	}
	coverage, err := a.db.ListHistoryCoverage(store.ListHistoryCoverageParams{})
	if err != nil || len(coverage) != 0 {
		t.Fatalf("invented chat: %v %v", coverage, err)
	}
}
func TestBackfillEvidenceCheckpointFailurePreventsNetworkRequest(t *testing.T) {
	a, f, chat, _ := newBackfillRetryTest(t, "anchor")
	var calls atomic.Int32
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { calls.Add(1); return nil }
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fixture_block_dispatch BEFORE UPDATE ON history_recovery_evidence WHEN new.dispatch_possible=1 BEGIN SELECT RAISE(ABORT,'fixture checkpoint failure'); END`)
	_, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	if err == nil {
		t.Fatal("success despite checkpoint failure")
	}
	var failure *BackfillError
	if !errors.As(err, &failure) || failure.History.Outcome != "not_dispatched" || failure.History.Phase != store.HistoryObserving || failure.History.Code != "store_state" {
		t.Fatalf("pre-dispatch certainty: %v", err)
	}
	requests := calls.Load()
	if requests != 0 {
		t.Fatalf("network after failed checkpoint: %d", requests)
	}
}
func TestBackfillEvidencePrimaryObservedBeforeFinalError(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fixture_fail_success BEFORE UPDATE ON history_recovery_evidence WHEN new.state='succeeded' BEGIN SELECT RAISE(ABORT,'fixture outcome failure'); END`)
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs
	}
	_, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Outcome != "uncertain" {
		t.Fatalf("write failure: %v", err)
	}
	got := readAttempt(t, a, chat)
	if got.Latest.State != store.HistoryUnfinalized || got.Latest.PrimaryNoMoreObservedAt == nil || got.Latest.ResponseChatJID != chat || got.LastSuccess != nil {
		t.Fatalf("primary observation lost/invented success: %+v", got.Latest)
	}
}
func TestBackfillEvidenceCancellationAfterPrimaryKeepsObservation(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs
	}
	// Cancel after receiving/persisting the primary observation, before final idle.
	a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
		if bytes.Contains(p, []byte(`"event":"backfill_stopped"`)) {
			cancel()
		}
	}), true)
	_, err := a.BackfillHistory(ctx, backfillRetryOptions(chat))
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Outcome != "uncertain" {
		t.Fatalf("cancel: %v", err)
	}
	got := readAttempt(t, a, chat)
	if got.Latest.State != store.HistoryCancelled || got.Latest.PrimaryNoMoreObservedAt == nil || got.Latest.FinishedAt == nil || got.Latest.NetGrowth != nil || got.LastSuccess != nil {
		t.Fatalf("cancel record: %+v", got.Latest)
	}
}
func TestHistoryIdentityRelationDoesNotTransferEvidence(t *testing.T) {
	a := store.HistoryAttempt{AccountJID: "100@s.whatsapp.net", WindowChatJID: "200@s.whatsapp.net", WindowAliasJID: "300@lid"}
	cases := []struct {
		current HistoryIdentity
		want    string
	}{
		{HistoryIdentity{AccountJID: a.AccountJID, ChatJID: a.WindowChatJID, AliasJID: a.WindowAliasJID}, "matching_snapshot"},
		{HistoryIdentity{AccountJID: "101@s.whatsapp.net", ChatJID: a.WindowChatJID, AliasJID: a.WindowAliasJID}, "changed"},
		{HistoryIdentity{AccountJID: a.AccountJID, ChatJID: a.WindowChatJID, AliasJID: "301@lid"}, "changed"},
		{HistoryIdentity{AccountJID: a.AccountJID, ChatJID: a.WindowChatJID}, "unknown"},
		{HistoryIdentity{ChatJID: a.WindowChatJID, AliasJID: a.WindowAliasJID}, "unknown"},
	}
	for _, tc := range cases {
		if got := HistoryIdentityRelation(a, tc.current); got != tc.want {
			t.Fatalf("relation=%s want %s", got, tc.want)
		}
	}
}

func TestBackfillEvidenceNoAnchorAfterRequestRemainsUncertain(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		return backfillTestResponse(chat, "older", base.Add(-time.Second))
	}
	// Remove the anchor only after a real reply, leaving the next lookup empty.
	a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
		if bytes.Contains(p, []byte(`"event":"backfill_response"`)) {
			if err := a.db.DeleteChat(chat); err != nil {
				t.Error(err)
			}
		}
	}), true)
	_, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Code != "backfill_outcome_uncertain" || typed.History.Outcome != "uncertain" {
		t.Fatalf("dispatch uncertainty lost: %v", err)
	}
	got := readAttempt(t, a, chat)
	if !got.Latest.DispatchPossible || got.Latest.State != store.HistoryError || got.Latest.NetGrowth != nil {
		t.Fatalf("false outcome: %+v", got.Latest)
	}
}

func TestBackfillEvidencePrimaryKeepsFrozenScopeOnMappingChange(t *testing.T) {
	a, f, _, base := newBackfillRetryTest(t)
	pn := types.NewJID("15550000001", types.DefaultUserServer)
	lid := types.NewJID("100000000001", types.HiddenUserServer)
	f.lids[lid] = pn
	if err := a.db.UpsertChat(pn.String(), "dm", "fixture", base); err != nil {
		t.Fatal(err)
	}
	if err := a.db.UpsertMessage(storeUpsertMessage(pn.String(), "anchor", base, "fixture")); err != nil {
		t.Fatal(err)
	}
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		f.mu.Lock()
		f.lids[lid] = types.NewJID("15550000002", types.DefaultUserServer)
		f.mu.Unlock()
		hs := backfillTestResponse(lid.String(), "older", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs
	}
	_, err := a.BackfillHistory(context.Background(), backfillRetryOptions(pn.String()))
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Outcome != "uncertain" {
		t.Fatalf("changed mapping succeeded: %v", err)
	}
	got := readAttempt(t, a, pn.String())
	if got.Latest.WindowChatJID != pn.String() || got.Latest.WindowAliasJID != lid.String() || got.Latest.PrimaryNoMoreObservedAt == nil || got.Latest.PrimaryResponseChatJID != lid.String() {
		t.Fatalf("lost/reassigned observation: %+v", got.Latest)
	}
}

func TestBackfillEvidenceSilentStandaloneDiagnostics(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "first", "second")
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	f.onDemandHistory = func(info types.MessageInfo, _ int) *events.HistorySync {
		if info.ID == "first" {
			return nil
		}
		return backfillTestResponse(chat, "older", base.Add(-time.Second))
	}
	var err error
	stderr := captureStderr(t, func() { _, err = a.BackfillHistory(context.Background(), backfillRetryOptions(chat)) })
	if err != nil || stderr != "" {
		t.Fatalf("agent diagnostics: %v %q", err, stderr)
	}
}
