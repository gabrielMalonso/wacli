package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func fixtureHistoryAgentResult(id, chat string) app.BackfillResult {
	now := time.Now().UTC()
	rec := &store.HistoryAttempt{AttemptID: id, RequestedChatJID: chat, State: store.HistorySucceeded, Phase: store.HistoryFinalizing,
		StartedAt: now, FinishedAt: &now, CheckpointAt: now, AccountJID: "999@s.whatsapp.net", WindowChatJID: chat,
		DispatchPossible: true, CountersFinal: true, RequestsSent: 1, ResponsesSeen: 1, BaselineCount: new(int64(1)), FinalCount: new(int64(3)), NetGrowth: new(int64(2)),
		StopReason: string(app.BackfillStopPrimaryNoMore), ResponseChatJID: chat, ResponseObservedAt: &now, PrimaryResponseChatJID: chat, PrimaryNoMoreObservedAt: &now,
		Count: 50, Requests: 1, WaitMS: 1, IdleMS: 1, ExecutionMode: "sync_owner", FirstAnchorID: "anchor", MessagesSynced: new(int64(9))}
	return app.BackfillResult{AttemptID: id, ChatJID: chat, RequestsSent: 1, ResponsesSeen: 1, MessagesAdded: 2, MessagesSynced: 9, StopReason: app.BackfillStopPrimaryNoMore, Evidence: rec}
}

func TestHistoryAgentPreflightSourceAndPolicy(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--read-only"}, "read_only"},
		{[]string{"--count", "501"}, "invalid_arguments"},
		{[]string{"--chat", "123@g.us@SECRET"}, "invalid_arguments"},
		{[]string{"--wait", "6m"}, "invalid_arguments"},
		{[]string{"--events"}, "invalid_arguments"},
		{[]string{"--cursor", "SECRET"}, "invalid_arguments"},
		{[]string{"--detail", "bad"}, "invalid_arguments"},
		{[]string{"--count", "SECRET_BAD_COUNT"}, "invalid_arguments"},
		{[]string{"--unknown-secret"}, "invalid_arguments"},
		{[]string{"SECRET_POSITIONAL"}, "invalid_arguments"},
	} {
		args := append([]string{"--agent", "--store", t.TempDir(), "history", "backfill", "--chat", "123@g.us"}, tc.args...)
		stdout, stderr, err := runAgentTest(t, args...)
		if err == nil || stdout != "" || commandExitCode(err) != 2 {
			t.Fatalf("preflight: %v %s %s", err, stdout, stderr)
		}
		e := decodeAgentTest(t, stderr)
		if e.Meta.Source != "live" || e.Error.Code != tc.code || strings.Contains(stderr, "SECRET") || e.Meta.Freshness != "unknown" || e.Meta.Completeness != "unknown" {
			t.Fatalf("unsafe preflight: %s", stderr)
		}
	}
	// Intent scanning consumes flag values and does not interpret query words as actions.
	stdout, stderr, err := runAgentTest(t, "--agent", "--store", t.TempDir(), "messages", "search", "history backfill", "--bad")
	if stdout != "" || err == nil || decodeAgentTest(t, stderr).Meta.Source != "local" {
		t.Fatalf("intent: %s %s %v", stdout, stderr, err)
	}
	t.Setenv("WACLI_READONLY", "1")
	_, stderr, err = runAgentTest(t, "--agent", "history", "backfill", "--chat", "123@g.us")
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatalf("environment policy: %v %s", err, stderr)
	}
}

func TestHistoryAgentFailurePublicClassification(t *testing.T) {
	for _, tc := range []struct {
		phase         store.HistoryAttemptPhase
		outcome, code string
		exit          int
		want          string
	}{
		{store.HistoryObserving, "not_dispatched", "no_local_anchor", 3, "no_local_anchor"},
		{store.HistoryPreparing, "not_dispatched", "store_state", 4, "store_state"},
		{store.HistoryPreparing, "not_dispatched", "backfill_not_dispatched", 1, "backfill_not_dispatched"},
		{store.HistoryDispatchPossible, "uncertain", "no_local_anchor", 1, "backfill_outcome_uncertain"},
		{store.HistoryFinalizing, "uncertain", "store_state", 1, "backfill_outcome_uncertain"},
	} {
		err := &app.BackfillError{History: app.HistoryFailure{AttemptID: "0123456789abcdef0123456789abcdef", Phase: tc.phase, Outcome: tc.outcome, Code: tc.code, CorrelationConfirmed: true}, Cause: errors.New("SECRET_SQL_PATH")}
		e := classifyHistoryAgentError(err, "")
		if e.Code != tc.want || e.ExitCode != tc.exit || e.History == nil || e.History.AttemptID != err.History.AttemptID || strings.Contains(e.Message+e.Recovery, "SECRET") {
			t.Fatalf("classification: %+v", e)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		e := classifyHistoryAgentError(cause, "0123456789abcdef0123456789abcdef")
		if e.ExitCode != 1 || e.History == nil || e.History.Outcome != "not_dispatched" {
			t.Fatalf("early cancellation: %+v", e)
		}
	}
	// An ordinary query error retains its original envelope; history is action-only.
	if classifyAgentError(errors.New("secret")).History != nil {
		t.Fatal("history leaked into query error")
	}
}

func TestHistoryAgentConnectedOwnerOutputAndUncertainty(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	t.Setenv("WACLI_READONLY", "0")
	dir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	requests := make(chan sendDelegateRequest, 4)
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		requests <- req
		res := fixtureHistoryAgentResult(req.Backfill.AttemptID, req.Backfill.ChatJID)
		if req.Backfill.Count == 51 {
			return sendDelegateResponse{}, &app.BackfillError{History: app.HistoryFailure{AttemptID: req.Backfill.AttemptID, Phase: store.HistoryFinalizing, Outcome: "uncertain", Code: "backfill_outcome_uncertain", CorrelationConfirmed: true}, Cause: errors.New("SECRET_PRIMARY_THEN_SQL")}
		}
		if req.Backfill.Count == 53 {
			res.Evidence = nil
		}
		if req.Backfill.Count == 52 {
			res.AttemptID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}
		return sendDelegateResponse{OK: true, Backfill: &res}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, detail := range []string{"compact", "full"} {
		stdout, stderr, err := runAgentTest(t, "--agent", "--detail", detail, "--store", dir, "history", "backfill", "--chat", "123@g.us")
		if err != nil || stderr != "" {
			t.Fatalf("owner output: %v %s", err, stderr)
		}
		e := decodeAgentTest(t, stdout)
		req := <-requests
		if !e.Success || e.Meta.Source != "live" || e.Meta.Freshness != "unknown" || e.Meta.Completeness != "unknown" || !strings.Contains(stdout, req.Backfill.AttemptID) || !strings.Contains(stdout, `"primary_no_more_observed_at"`) {
			t.Fatalf("output: %s", stdout)
		}
		if strings.Contains(stdout, `"first_anchor_id"`) != (detail == "full") || strings.Contains(stdout, `"global_messages_synced"`) != (detail == "full") {
			t.Fatalf("detail: %s", stdout)
		}
	}
	for _, count := range []string{"51", "52", "53"} {
		stdout, stderr, err := runAgentTest(t, "--agent", "--store", dir, "history", "backfill", "--chat", "123@g.us", "--count", count)
		req := <-requests
		e := decodeAgentTest(t, stderr)
		if err == nil || stdout != "" || commandExitCode(err) != 1 || e.Error.Code != "backfill_outcome_uncertain" || e.Meta.Source != "live" || e.Error.History == nil || e.Error.History.AttemptID != req.Backfill.AttemptID || strings.Contains(stderr, "SECRET") {
			t.Fatalf("uncertain: %v %s %s", err, stdout, stderr)
		}
		if e.Error.History.CorrelationConfirmed != (count == "51") {
			t.Fatalf("correlation: %s", stderr)
		}
	}
}

func TestHistoryAgentOutputFailureRemainsUncertain(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	res := fixtureHistoryAgentResult(id, "123@g.us")
	res.Evidence.FirstAnchorID = strings.Repeat("x", 9<<20)
	flags := &rootFlags{agent: true, agentCapability: agentHistoryRecovery, agentHistoryAttemptID: id, detail: "full"}
	var err error
	stdout := captureRootStdout(t, func() { err = writeHistoryBackfillResult(flags, res) })
	e := classifyHistoryAgentError(err, id)
	if err == nil || stdout != "" || e.Code != "backfill_outcome_uncertain" || e.History == nil || !e.History.CorrelationConfirmed {
		t.Fatalf("lost output: %v %s %+v", err, stdout, e)
	}
	var cause *out.AgentError
	if !errors.As(err, &cause) || cause.Code != "payload_too_large" {
		t.Fatalf("missing internal output cause: %v", err)
	}
}

func TestHistoryAgentStandaloneOpenerUsesSilentEvents(t *testing.T) {
	dir := seedLocalReadStore(t)
	flags := &rootFlags{agent: true, agentCapability: agentHistoryRecovery, storeDir: dir}
	a, lk, err := newApp(context.Background(), flags, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeApp(a, lk)
	stderr := captureRootStderr(t, func() {
		if err := a.Events().Emit("fixture_warning", nil); err != nil {
			t.Fatal(err)
		}
	})
	if !a.Events().Enabled() || stderr != "" {
		t.Fatalf("opener diagnostics: %q", stderr)
	}
}
