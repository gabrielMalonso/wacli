package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func startBackfillFollow(t *testing.T, a *App) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
			close(ready)
			return nil
		}})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("follow stopped before ready: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("follow not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("follow shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("follow did not stop")
		}
	})
	return cancel, done
}

func assertBackfillOwnerHealthy(t *testing.T, a *App, f *fakeWA, chat string, base time.Time) {
	t.Helper()
	f.mu.Lock()
	connected, calls, manual := f.connected, f.connectCalls, append([]bool(nil), f.manualHistorySyncCalls...)
	f.mu.Unlock()
	if !connected || calls != 1 || len(manual) != 1 || !manual[0] {
		t.Fatalf("owner lifecycle: connected=%v connect=%d manual=%v", connected, calls, manual)
	}
	a.historyMu.Lock()
	observer := a.historyObserver
	a.historyMu.Unlock()
	if observer != nil {
		t.Fatal("backfill observer leaked")
	}
	jid, _ := types.ParseJID(chat)
	f.emit(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "owner-still-stores", Timestamp: base},
		Message: &waE2E.Message{Conversation: proto.String("live fixture")},
	})
	if _, err := a.db.GetMessage(chat, "owner-still-stores"); err != nil {
		t.Fatalf("owner stopped processing messages: %v", err)
	}
}

func TestBackfillConnectedPersistsOnceAndKeepsLegacyCounters(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "manual"}[manual], func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor")
			response := func(types.MessageInfo, int) *events.HistorySync {
				f.emit(backfillTestResponse("other@g.us", "other", base))
				return backfillTestResponse(chat, "older", base.Add(-time.Second))
			}
			f.onDemandHistory = response
			if manual {
				installManualBackfillResponse(f, response)
			}
			startBackfillFollow(t, a)
			// Activity before the operation must not appear in its global delta.
			f.emit(backfillTestResponse(chat, "before-window", base))
			fixture, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.Close()
			if _, err := fixture.Exec(`CREATE TABLE persistence_probe (n INTEGER); INSERT INTO persistence_probe VALUES (0);
				CREATE TRIGGER count_history BEFORE INSERT ON messages WHEN NEW.msg_id = 'older'
				BEGIN UPDATE persistence_probe SET n = n + 1; END`); err != nil {
				t.Fatal(err)
			}
			res, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat))
			wantSynced := int64(2)
			if manual {
				wantSynced = 1 // other native event; manual blob stays separate
			}
			if err != nil || res.MessagesAdded != 1 || res.MessagesSynced != wantSynced || res.RequestsSent != 1 || res.ResponsesSeen != 1 || res.StopReason != BackfillStopRequestedBatchLimit {
				t.Fatalf("backfill = %+v, %v", res, err)
			}
			var writes int
			if err := fixture.QueryRow(`SELECT n FROM persistence_probe`).Scan(&writes); err != nil || writes != 1 {
				t.Fatalf("persisted %d times: %v", writes, err)
			}
			assertBackfillOwnerHealthy(t, a, f, chat, base)
		})
	}
}

func TestBackfillConnectedRejectsUnavailableOwner(t *testing.T) {
	a, f, chat, _ := newBackfillRetryTest(t, "anchor")
	if _, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat)); err == nil {
		t.Fatal("accepted absent owner")
	}
	// An event delivered while connecting runs before the runtime is ready.
	f.connectEvents = []any{&events.HistorySync{Data: &waHistorySync.HistorySync{}}}
	var earlyErr error
	f.AddEventHandler(func(any) {
		if earlyErr == nil {
			_, earlyErr = a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat))
		}
	})
	startBackfillFollow(t, a)
	if earlyErr == nil {
		t.Fatal("runtime exposed before connection/migration")
	}
	f.Disconnect() // no Disconnected event: follow alone owns any reconnect
	_, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat))
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("disconnected owner: %v", err)
	}
	f.mu.Lock()
	calls := f.connectCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("backfill connected the owner again: %d", calls)
	}
}

func TestBackfillConnectedErrorsAndRetriesLeaveOwnerHealthy(t *testing.T) {
	for _, scenario := range []string{"transport", "timeout", "persistence", "cancel_idle", "retry", "alias"} {
		t.Run(scenario, func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor", "second")
			if scenario == "alias" {
				pn, lid := types.NewJID("15550000001", types.DefaultUserServer), types.NewJID("100000000001", types.HiddenUserServer)
				f.lids[lid] = pn
				chat = pn.String()
				if err := a.db.UpsertChat(lid.String(), "dm", "alias fixture", base); err != nil {
					t.Fatal(err)
				}
				if err := a.db.UpsertMessage(storeUpsertMessage(lid.String(), "alias-anchor", base, "fixture")); err != nil {
					t.Fatal(err)
				}
			}
			startBackfillFollow(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int64
			f.onDemandHistory = func(info types.MessageInfo, _ int) *events.HistorySync {
				calls.Add(1)
				if scenario == "timeout" || (scenario == "retry" && info.ID == "anchor") {
					return nil
				}
				hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
				if scenario == "persistence" {
					hs = backfillTestResponse(chat, "fail", base.Add(-time.Second))
				}
				if scenario == "alias" {
					alias := "100000000001@lid"
					hs.Data.Conversations[0].ID = &alias
				}
				if scenario == "cancel_idle" {
					go func() { time.Sleep(5 * time.Millisecond); cancel() }()
				}
				return hs
			}
			if scenario == "transport" {
				f.onDemandErr = errors.New("fixture transport error")
			}
			if scenario == "persistence" {
				fixture, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer fixture.Close()
				if _, err := fixture.Exec(`CREATE TRIGGER fail_backfill BEFORE INSERT ON messages WHEN NEW.msg_id = 'fail'
					BEGIN SELECT RAISE(ABORT, 'fixture persistence error'); END`); err != nil {
					t.Fatal(err)
				}
			}
			opts := backfillRetryOptions(chat)
			if scenario == "cancel_idle" {
				opts.IdleExit = 100 * time.Millisecond
			}
			res, err := a.BackfillHistoryConnected(ctx, opts)
			if scenario == "retry" || scenario == "alias" {
				if err != nil || res.MessagesAdded != 1 {
					t.Fatalf("result=%+v err=%v", res, err)
				}
				if scenario == "retry" && calls.Load() != 2 {
					t.Fatalf("retry requests=%d", calls.Load())
				}
			} else if err == nil || res != (BackfillResult{}) {
				t.Fatalf("error case returned %+v, %v", res, err)
			}
			assertBackfillOwnerHealthy(t, a, f, chat, base)
		})
	}
}

func TestBackfillConnectedOwnerCancellation(t *testing.T) {
	a, f, chat, _ := newBackfillRetryTest(t, "anchor")
	requested := make(chan struct{})
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { close(requested); return nil }
	stopOwner, _ := startBackfillFollow(t, a)
	done := make(chan error, 1)
	go func() {
		opts := backfillRetryOptions(chat)
		opts.WaitPerRequest = time.Minute
		_, err := a.BackfillHistoryConnected(context.Background(), opts)
		done <- err
	}()
	<-requested
	stopOwner()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("operation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not interrupt operation")
	}
}

func TestBackfillConnectedLateCallbackKeepsOriginalObserver(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	startBackfillFollow(t, a)
	fixture, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	if _, err := fixture.Exec(`CREATE TRIGGER fail_late BEFORE INSERT ON messages WHEN NEW.msg_id = 'late'
		BEGIN SELECT RAISE(ABORT, 'late fixture persistence error'); END`); err != nil {
		t.Fatal(err)
	}
	notif := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}
	started, release, downloaded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.onDemandEvent = func(types.MessageInfo, int) any {
		go func() {
			f.emit(&events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notif}}})
			close(downloaded)
		}()
		return nil
	}
	f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
		close(started)
		<-release
		hs := backfillTestResponse(chat, "late", base.Add(-time.Second))
		hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
		return hs.Data, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := a.BackfillHistoryConnected(ctx, backfillRetryOptions(chat)); first <- err }()
	<-started
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for blocked callback")
	}
	requested := make(chan struct{})
	f.mu.Lock()
	f.onDemandEvent = nil
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { close(requested); return nil }
	f.mu.Unlock()
	second := make(chan error, 1)
	go func() {
		opts := backfillRetryOptions(chat)
		opts.WaitPerRequest = time.Second
		_, err := a.BackfillHistoryConnected(context.Background(), opts)
		second <- err
	}()
	<-requested
	close(release)
	<-downloaded
	select {
	case err := <-second:
		t.Fatalf("old callback completed new request: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	f.emit(backfillTestResponse(chat, "new-operation", base.Add(-2*time.Second)))
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	assertBackfillOwnerHealthy(t, a, f, chat, base)
}

func TestHistoryObserverSuccessWaitsForInFlightPersistence(t *testing.T) {
	a := newTestApp(t)
	var stored atomic.Int64
	r := a.startHistoryRuntime(context.Background(), &stored)
	defer a.stopHistoryRuntime(r)
	o := newHistoryObserver(nil, nil)
	if err := a.registerHistoryObserver(r, o); err != nil {
		t.Fatal(err)
	}
	_, finish := a.historyEventOptions(SyncOptions{}, &events.HistorySync{})
	done := make(chan error, 1)
	go func() { done <- o.waitIdle(context.Background(), time.Millisecond) }()
	select {
	case <-done:
		t.Fatal("idle succeeded while persistence callback was active")
	case <-time.After(10 * time.Millisecond):
	}
	finish()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.removeHistoryObserver(o)
}

func TestBackfillConnectedIdleIncludesConcurrentSelectedActivity(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	startBackfillFollow(t, a)
	completed := make(chan struct{})
	requested := make(chan struct{})
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		close(requested)
		return backfillTestResponse(chat, "older", base.Add(-time.Second))
	}
	go func() {
		<-requested
		defer close(completed)
		for i := 0; i < 3; i++ {
			time.Sleep(15 * time.Millisecond)
			jid, _ := types.ParseJID(chat)
			f.emit(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: types.MessageID(fmt.Sprintf("live-%d", i)), Timestamp: base},
				Message: &waE2E.Message{Conversation: proto.String("concurrent fixture")}})
		}
	}()
	opts := backfillRetryOptions(chat)
	opts.IdleExit = 30 * time.Millisecond
	started := time.Now()
	res, err := a.BackfillHistoryConnected(context.Background(), opts)
	<-completed
	if err != nil || res.MessagesAdded != 4 || res.MessagesSynced != 4 || time.Since(started) < 70*time.Millisecond {
		t.Fatalf("idle result=%+v elapsed=%s err=%v", res, time.Since(started), err)
	}
	assertBackfillOwnerHealthy(t, a, f, chat, base)
}

func TestBackfillConnectedRefusesOverlappingObservers(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	startBackfillFollow(t, a)
	requested := make(chan struct{})
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { close(requested); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		opts := backfillRetryOptions(chat)
		opts.WaitPerRequest = time.Second
		_, err := a.BackfillHistoryConnected(ctx, opts)
		done <- err
	}()
	<-requested
	_, err := a.BackfillHistoryConnected(context.Background(), backfillRetryOptions(chat))
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("overlapping observer: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertBackfillOwnerHealthy(t, a, f, chat, base)
}
