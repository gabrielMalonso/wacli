package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
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

func TestSyncStatusIngestionIsDefensiveAndRunCorrelated(t *testing.T) {
	a := newTestApp(t)
	beginOperationalTestRun(t, a)
	run := a.live.recovery
	a.waMu.Lock()
	a.appStateRecoveryOnClose = run
	a.waMu.Unlock()
	ctx := context.WithValue(t.Context(), appStateRecoveryRunKey{}, run)
	a.observeLiveIngestion(ctx, errors.New("private persistence error"))
	v := a.SyncLiveSnapshot()
	i := v.Observations.Sync.Ingestion
	if i == nil || i.ExecutionID != v.OwnerRunID || i.ExecutionID != v.Observations.Sync.ExecutionID || i.LiveReceived != 1 || i.LiveFailures != 1 || !i.Degraded || i.LastFailure == nil || i.LastFailure.Reason != "persistence_failed" || i.History.Additions != nil || v.Ready || v.Authenticated != "unknown" {
		t.Fatalf("uncorrelated ingestion: %+v", v)
	}
	i.LiveFailures = 99
	i.LastFailure.Reason = "mutated"
	if next := a.SyncLiveSnapshot().Observations.Sync.Ingestion; next.LiveFailures != 1 || next.LastFailure.Reason != "persistence_failed" {
		t.Fatal("status exposed mutable ingestion references")
	}
	// A newly published diagnostic run cannot be attributed to the old live
	// owner, including the nil interval before that new run is initialized.
	other := &appStateRecoveryRun{}
	a.waMu.Lock()
	a.appStateRecoveryOnClose = other
	a.waMu.Unlock()
	if next := a.SyncLiveSnapshot().Observations.Sync.Ingestion; next != nil {
		t.Fatal("unpublished ingestion retained in live projection")
	}
	diagnostic := newDiagnosticRun(a, SyncModeFollow, other)
	a.waMu.Lock()
	other.diagnostic = diagnostic
	a.waMu.Unlock()
	if next := a.SyncLiveSnapshot().Observations.Sync.Ingestion; next != nil {
		t.Fatal("new run ingestion attributed to old owner")
	}
}

func TestSyncStatusPreservesTerminalDuringBlockedDiagnostics(t *testing.T) {
	for _, state := range []SyncLiveState{SyncLiveLoggedOut, SyncLiveStopping, SyncLiveStopped, SyncLiveError} {
		t.Run(string(state), func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			f.mu.Lock()
			f.connected = true
			f.mu.Unlock()
			generation := beginOperationalTestRun(t, a)
			a.live.event(&events.Connected{}, generation)
			a.live.initialize(t.Context())
			r := a.live.recovery.diagnostic
			r.mu.Lock()
			done := make(chan SyncLiveStatus, 1)
			go func() { done <- a.SyncLiveSnapshot() }()
			// Wait for the diagnostic mutex, without depending on source line
			// numbers or an arbitrary sleep before changing the lifecycle.
			blocked := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				stack := make([]byte, 1<<18)
				stack = stack[:runtime.Stack(stack, true)]
				for _, goroutine := range strings.Split(string(stack), "\n\n") {
					if strings.Contains(goroutine, "(*App).SyncLiveSnapshot(") && strings.Contains(goroutine, "(*Mutex).lockSlow(") {
						blocked = true
					}
				}
				if blocked {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !blocked {
				r.mu.Unlock()
				<-done
				t.Fatal("snapshot did not reach diagnostic mutex")
			}
			a.live.transition(state)
			r.mu.Unlock()
			v := <-done
			if v.State != state || v.OwnerReady || v.SendInitialized || v.Initialized || v.Ready || v.Authenticated != "unknown" || v.Operations.SendAttempt.Reason != SyncOwnerStopping || v.Operations.DraftWrite.Attemptable || v.Operations.ChatStateWrite.Attemptable {
				t.Fatalf("terminal precedence lost: want=%s snapshot=%+v", state, v)
			}
			if v.OwnerRunID != "" || v.LinkedJID != "" || v.LinkedLID != "" || v.Observations != nil || v.Stages != nil || v.TransportConnected != "unknown" || v.AppState.RecoveryObservations != nil {
				t.Fatalf("stale evidence survived revision change: %+v", v)
			}
		})
	}
}

func TestSyncAdmissionDoesNotWaitForBlockedSQLiteDiagnostics(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	generation := beginOperationalTestRun(t, a)
	a.live.event(&events.Connected{}, generation)
	a.live.initialize(t.Context())
	// A real SQLite writer blocks debt/diagnostic writes. Hold the diagnostic
	// mutex through the wait and until this test explicitly releases it.
	writer, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	probe, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db")+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	_, err = probe.Exec("BEGIN IMMEDIATE")
	var locked sqlite3.Error
	if !errors.As(err, &locked) || locked.Code != sqlite3.ErrBusy {
		t.Fatalf("fixture did not block SQLite writes: %v", err)
	}
	r := a.live.recovery.diagnostic
	started, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		r.mu.Lock()
		close(started)
		r.persistLocked(false)
		<-release
		r.mu.Unlock()
		close(finished)
	}()
	<-started
	defer func() {
		writer.Exec("ROLLBACK")
		close(release)
		<-finished
	}()
	done := make(chan SyncLiveStatus, 1)
	go func() { done <- a.SyncAdmissionSnapshot() }()
	select {
	case v := <-done:
		if !v.Operations.SendAttempt.Attemptable || v.Ready || v.Authenticated != "unknown" || v.Observations != nil || len(v.Stages) != 0 {
			t.Fatalf("admission=%+v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("SQLite/diagnostic lock delayed admission")
	}
}
