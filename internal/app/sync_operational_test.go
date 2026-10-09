package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestSyncOperationAdmission(t *testing.T) {
	for _, test := range []struct {
		name                          string
		state                         SyncLiveState
		send, owner                   bool
		transport                     string
		wantSend, wantDraft, wantChat bool
		reason                        SyncOperationReason
	}{
		{"initializing", SyncLiveInitializing, false, false, "true", false, false, false, SyncOwnerInitializing},
		{"metadata", SyncLiveInitializing, true, false, "true", true, true, false, SyncAttemptPermitted},
		{"initialized", SyncLiveUnknown, true, true, "true", true, true, true, SyncAttemptPermitted},
		{"lost", SyncLiveUnknown, true, true, "false", false, true, false, SyncTransportUnavailable},
		{"disconnect callback", SyncLiveDisconnected, true, true, "true", false, true, false, SyncTransportUnavailable},
		{"reconnecting", SyncLiveReconnecting, true, true, "true", false, true, false, SyncOwnerReconnecting},
		{"unknown transport", SyncLiveUnknown, true, true, "unknown", false, true, false, SyncTransportUnknown},
		{"logout", SyncLiveLoggedOut, true, true, "true", false, false, false, SyncOwnerStopping},
		{"teardown", SyncLiveStopping, true, true, "true", false, false, false, SyncOwnerStopping},
		{"stopped", SyncLiveStopped, true, true, "true", false, false, false, SyncOwnerStopping},
		{"error", SyncLiveError, true, true, "true", false, false, false, SyncOwnerStopping},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := SyncLiveStatus{State: test.state, SendInitialized: test.send, OwnerReady: test.owner, TransportConnected: test.transport}
			o := SyncOperationAdmission(v)
			if o.SendAttempt.Attemptable != test.wantSend || o.DraftWrite.Attemptable != test.wantDraft || o.ChatStateWrite.Attemptable != test.wantChat || o.SendAttempt.Reason != test.reason || o.LocalRead.OwnerRequired || o.LocalRead.Availability != "not_checked" || o.LocalRead.Reason != SyncOwnerNotRequired {
				t.Fatalf("%+v", o)
			}
		})
	}
}

func TestSyncStageBudgetFailureAndRunFence(t *testing.T) {
	a := newTestApp(t)
	generation := beginOperationalTestRun(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	finish := a.measureSyncStage(ctx, "full_sync", "regular")
	v := a.SyncLiveSnapshot()
	if len(v.Stages) != 1 || v.Stages[0].BudgetMS == nil || *v.Stages[0].BudgetMS <= 0 || *v.Stages[0].BudgetMS > 1000 || v.Stages[0].FinishedAt != nil {
		t.Fatalf("active budget: %+v", v.Stages)
	}
	finish(errors.Join(context.DeadlineExceeded, errors.New("private URL token 15550000000")))
	v = a.SyncLiveSnapshot()
	if v.Stages[0].ErrorCode != "deadline_exceeded" || v.Stages[0].FinishedAt == nil || v.Stages[0].ElapsedMS < 0 {
		t.Fatalf("finished: %+v", v.Stages)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "token") {
		t.Fatal("raw error crossed operational boundary")
	}
	oldID := v.OwnerRunID
	oldFinish := a.measureSyncStage(t.Context(), "snapshot", "regular")
	a.live.begin(t.Context())
	a.live.enableSend(t.Context(), generation)
	a.live.event(&events.Connected{}, generation)
	oldFinish(nil)
	v = a.SyncLiveSnapshot()
	if v.OwnerRunID == oldID || len(v.Stages) != 0 || v.SendInitialized || v.Ready || v.Authenticated != "unknown" {
		t.Fatalf("old run revived new owner: %+v", v)
	}
}

func TestSyncStagesKeepLatestInvocationAndUnknownBudget(t *testing.T) {
	a := newTestApp(t)
	beginOperationalTestRun(t, a)
	first := a.measureSyncStage(context.Background(), "lock_wait", "regular_low")
	second := a.measureSyncStage(context.Background(), "lock_wait", "regular_low")
	second(context.Canceled)
	first(nil)
	v := a.SyncLiveSnapshot()
	if len(v.Stages) != 1 || v.Stages[0].BudgetMS != nil || v.Stages[0].ErrorCode != "cancelled" {
		t.Fatalf("late phase replaced latest or invented budget: %+v", v.Stages)
	}
	v.Stages[0].FinishedAt = nil
	for range 100 {
		a.measureSyncStage(t.Context(), "snapshot", "private/collection")(errors.New("private"))
		a.measureSyncStage(t.Context(), "private-phase", "regular")(nil)
	}
	if now := a.SyncLiveSnapshot(); len(now.Stages) != 1 || now.Stages[0].FinishedAt == nil {
		t.Fatal("snapshot exposed mutable stage or unbounded external labels")
	}
}

func beginOperationalTestRun(t *testing.T, a *App) uint64 {
	t.Helper()
	generation := a.live.begin(t.Context())
	run := &appStateRecoveryRun{}
	run.diagnostic = newDiagnosticRun(a, SyncModeFollow, run)
	a.live.mu.Lock()
	a.live.recovery, a.live.runID = run, run.diagnostic.sync.ExecutionID
	a.live.mu.Unlock()
	return generation
}

func TestSyncStageNoopWithoutPublishedRun(t *testing.T) {
	a := newTestApp(t)
	a.live.begin(t.Context())
	before := ReadAppStateDiagnostics(a.db)
	a.measureSyncStage(t.Context(), "snapshot", "regular")(context.DeadlineExceeded)
	after := a.SyncLiveSnapshot()
	if len(after.Stages) != 0 || before.Reconciliation != after.AppState.Reconciliation || len(before.PendingCollections) != len(after.AppState.PendingCollections) || after.SendInitialized || a.WA() != nil {
		t.Fatalf("unpublished measurement changed operational/debt state: %+v", after)
	}
}
