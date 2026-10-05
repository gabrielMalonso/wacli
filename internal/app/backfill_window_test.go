package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func runBackfillMode(ctx context.Context, a *App, opts BackfillOptions, connected bool) (BackfillResult, error) {
	if connected {
		return a.BackfillHistoryConnected(ctx, opts)
	}
	return a.BackfillHistory(ctx, opts)
}

func TestBackfillAggregatesFrozenConversationScope(t *testing.T) {
	pn := types.NewJID("15550000001", types.DefaultUserServer)
	lid := types.NewJID("100000000001", types.HiddenUserServer)
	for _, connected := range []bool{false, true} {
		for _, lidFirst := range []bool{false, true} {
			for _, scenario := range []string{"primary", "progress", "both_nonempty", "outside_only"} {
				name := map[bool]string{false: "standalone", true: "owner"}[connected] + "/" + map[bool]string{false: "pn-first", true: "lid-first"}[lidFirst] + "/" + scenario
				t.Run(name, func(t *testing.T) {
					a, f, _, base := newBackfillRetryTest(t)
					f.lids[lid] = pn
					if err := a.db.UpsertChat(pn.String(), "dm", "fixture", base); err != nil {
						t.Fatal(err)
					}
					if err := a.db.UpsertMessage(storeUpsertMessage(pn.String(), "anchor", base, "fixture")); err != nil {
						t.Fatal(err)
					}
					var eventLog bytes.Buffer
					a.opts.Events = out.NewEventWriter(&eventLog, true)
					f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
						phone := &waHistorySync.Conversation{ID: proto.String(pn.String())}
						hidden := backfillTestResponse(lid.String(), "older-lid", base.Add(-time.Second)).Data.Conversations[0]
						outside := backfillTestResponse("999@lid", "outside", base.Add(-3*time.Second)).Data.Conversations[0]
						outside.EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
						if scenario == "primary" {
							hidden.EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
						}
						if scenario == "both_nonempty" {
							phone = backfillTestResponse(pn.String(), "older-pn", base.Add(-2*time.Second)).Data.Conversations[0]
						}
						cs := []*waHistorySync.Conversation{phone, outside, hidden}
						if lidFirst {
							cs = []*waHistorySync.Conversation{hidden, outside, phone}
						}
						if scenario == "outside_only" {
							cs = []*waHistorySync.Conversation{outside}
						}
						return &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: cs}}
					}
					if connected {
						startBackfillFollow(t, a)
					}
					res, err := runBackfillMode(context.Background(), a, backfillRetryOptions(pn.String()), connected)
					retained := readAttempt(t, a, pn.String())
					if scenario == "outside_only" {
						var failure *BackfillError
						if !errors.As(err, &failure) || failure.History.Outcome != "uncertain" || retained.Latest.ResponsesSeen != 0 || retained.Latest.PrimaryNoMoreObservedAt != nil || retained.LastSuccess != nil {
							t.Fatalf("outside response attributed to recovery: result=%+v error=%v evidence=%+v", res, err, retained)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					wantGrowth := int64(1)
					if scenario == "both_nonempty" {
						wantGrowth = 2
					}
					wantStop := BackfillStopRequestedBatchLimit
					if scenario == "primary" {
						wantStop = BackfillStopPrimaryNoMore
					}
					if res.MessagesAdded != wantGrowth || res.StopReason != wantStop || res.RequestsSent != 1 || res.ResponsesSeen != 1 || *retained.Latest.NetGrowth != wantGrowth || retained.LastSuccess.AttemptID != res.AttemptID {
						t.Fatalf("aggregate: result=%+v evidence=%+v", res, retained)
					}
					if scenario == "primary" {
						if retained.Latest.PrimaryNoMoreObservedAt == nil || retained.Latest.PrimaryResponseChatJID != lid.String() || retained.Latest.ResponseChatJID != lid.String() || !retained.Latest.ResponseObservedAt.Equal(*retained.Latest.PrimaryNoMoreObservedAt) {
							t.Fatalf("primary source/time lost: %+v", retained.Latest)
						}
					} else if retained.Latest.PrimaryNoMoreObservedAt != nil {
						t.Fatal("primary outside scope was retained")
					}
					for _, line := range bytes.Split(bytes.TrimSpace(eventLog.Bytes()), []byte("\n")) {
						var event struct {
							Event string `json:"event"`
							Data  struct {
								Messages int `json:"messages"`
							} `json:"data"`
						}
						if err := json.Unmarshal(line, &event); err != nil {
							t.Fatal(err)
						}
						if event.Event == "backfill_response" && event.Data.Messages != int(wantGrowth) {
							t.Fatalf("response counted unrelated/omitted alias messages: %s", line)
						}
					}
				})
			}
		}
	}
}

// Block inside message persistence, after the callback has captured its observer.
type blockedBackfillPersistenceWA struct {
	*fakeWA
	started chan struct{}
	release chan struct{}
	armed   atomic.Bool
}

func (f *blockedBackfillPersistenceWA) ResolveChatName(ctx context.Context, jid types.JID, pushName string) string {
	if f.armed.Swap(false) {
		close(f.started)
		<-f.release
	}
	return f.fakeWA.ResolveChatName(ctx, jid, pushName)
}

func TestBackfillFinalWindowWaitsForCapturedPersistence(t *testing.T) {
	for _, connected := range []bool{false, true} {
		for _, outcome := range []string{"success", "store_error", "cancel", "deadline", "logout"} {
			t.Run(map[bool]string{false: "standalone", true: "owner"}[connected]+"/"+outcome, func(t *testing.T) {
				if connected && outcome == "logout" {
					t.Skip("owner termination covered by owner cancellation tests")
				}
				a, f, _, base := newBackfillRetryTest(t)
				chat := "15550000001@s.whatsapp.net"
				if err := a.db.UpsertChat(chat, "dm", "fixture", base); err != nil {
					t.Fatal(err)
				}
				if err := a.db.UpsertMessage(storeUpsertMessage(chat, "anchor", base, "fixture")); err != nil {
					t.Fatal(err)
				}
				blocked := &blockedBackfillPersistenceWA{fakeWA: f, started: make(chan struct{}), release: make(chan struct{})}
				a.wa = blocked
				var releaseOnce sync.Once
				defer releaseOnce.Do(func() { close(blocked.release) })
				var requests atomic.Int32
				var initialPrimary atomic.Bool
				initialPrimary.Store(true)
				f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
					requests.Add(1)
					hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
					if initialPrimary.Load() {
						hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
					}
					return hs
				}
				if connected {
					startBackfillFollow(t, a)
				}
				prior, err := runBackfillMode(context.Background(), a, backfillRetryOptions(chat), connected)
				if err != nil {
					t.Fatal(err)
				}
				requests.Store(0)
				initialPrimary.Store(outcome != "success" && outcome != "store_error")
				if outcome == "store_error" {
					evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_window BEFORE INSERT ON messages WHEN NEW.msg_id='late' BEGIN SELECT RAISE(ABORT,'fixture window failure'); END`)
				}
				drained := make(chan struct{})
				a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
					if bytes.Contains(p, []byte(`"event":"backfill_stopped"`)) {
						late := backfillTestResponse(chat, "late", base.Add(-2*time.Second))
						blocked.armed.Store(true)
						late.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
						go func() { f.emit(late); close(drained) }()
					}
				}), true)
				budget := 3 * time.Second
				if outcome == "deadline" {
					budget = 500 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				type result struct {
					value BackfillResult
					err   error
				}
				done := make(chan result, 1)
				go func() {
					value, err := runBackfillMode(ctx, a, backfillRetryOptions(chat), connected)
					done <- result{value, err}
				}()
				select {
				case <-blocked.started:
				case <-ctx.Done():
					t.Fatal("callback did not enter persistence")
				}
				pending := readAttempt(t, a, chat)
				if pending.Latest.State != store.HistoryUnfinalized || pending.Latest.CountersFinal || pending.Latest.NetGrowth != nil || pending.LastSuccess.AttemptID != prior.AttemptID {
					t.Fatalf("finalized while persistence blocked: %+v", pending)
				}
				if outcome == "cancel" {
					cancel()
				}
				if outcome == "logout" {
					f.emit(&events.LoggedOut{})
				}
				if outcome == "success" || outcome == "store_error" {
					select {
					case got := <-done:
						t.Fatalf("completed before persistence drained: %+v", got)
					case <-time.After(300 * time.Millisecond):
					}
					releaseOnce.Do(func() { close(blocked.release) })
				}
				var got result
				select {
				case got = <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("backfill ignored cancellation/deadline")
				}
				retained := readAttempt(t, a, chat)
				if requests.Load() != 1 {
					t.Fatalf("replayed request: %d", requests.Load())
				}
				if outcome == "success" {
					if got.err != nil || got.value.MessagesAdded != 1 || *retained.Latest.NetGrowth != 1 || retained.LastSuccess.AttemptID != got.value.AttemptID || retained.Latest.PrimaryNoMoreObservedAt == nil {
						t.Fatalf("final result: %+v evidence=%+v", got, retained)
					}
				} else {
					var failure *BackfillError
					if !errors.As(got.err, &failure) || failure.History.Outcome != "uncertain" || !failure.History.CorrelationConfirmed || got.value != (BackfillResult{}) || retained.Latest.NetGrowth != nil || retained.LastSuccess.AttemptID != prior.AttemptID || retained.Latest.PrimaryNoMoreObservedAt == nil || !retained.Latest.CountersFinal {
						t.Fatalf("uncertainty/evidence: %+v evidence=%+v", got, retained)
					}
					if outcome == "cancel" && (!errors.Is(got.err, context.Canceled) || retained.Latest.State != store.HistoryCancelled) {
						t.Fatalf("cancel: %v %+v", got.err, retained.Latest)
					}
					if outcome == "deadline" && (!errors.Is(got.err, context.DeadlineExceeded) || retained.Latest.State != store.HistoryCancelled) {
						t.Fatalf("deadline: %v %+v", got.err, retained.Latest)
					}
					if outcome == "store_error" && retained.Latest.State != store.HistoryError {
						t.Fatalf("store error: %+v", retained.Latest)
					}
				}
				releaseOnce.Do(func() { close(blocked.release) })
				select {
				case <-drained:
				case <-time.After(time.Second):
					t.Fatal("callback did not drain")
				}
				if after := readAttempt(t, a, chat); !reflect.DeepEqual(after, retained) {
					t.Fatalf("late callback mutated finalized evidence: before=%+v after=%+v", retained, after)
				}
			})
		}
	}
}

func TestStandaloneHistoryObserverNeverCapturesNextOperation(t *testing.T) {
	a := newTestApp(t)
	old := newHistoryObserver(func(*events.HistorySync) { t.Error("closed standalone observer notified") }, nil)
	old.active = false
	var stored atomic.Int64
	runtime := a.startHistoryRuntime(context.Background(), &stored)
	defer a.stopHistoryRuntime(runtime)
	next := newHistoryObserver(func(*events.HistorySync) { t.Error("old handler captured new observer") }, nil)
	if err := a.registerHistoryObserver(runtime, next); err != nil {
		t.Fatal(err)
	}
	defer a.removeHistoryObserver(next)
	opts, finish := a.historyEventOptions(SyncOptions{historyObserver: old}, &events.HistorySync{})
	defer finish()
	if opts.afterHistorySync != nil || next.inFlight != 0 {
		t.Fatal("handler delivered after closure captured the next operation")
	}
}

func TestBackfillStandaloneLateCallbackKeepsOriginalObserver(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_old_window BEFORE INSERT ON messages WHEN NEW.msg_id='late' BEGIN SELECT RAISE(ABORT,'old callback failure'); END`)
	started, release, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	f.onDemandEvent = func(types.MessageInfo, int) any {
		go func() {
			f.emit(&events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}}}})
			close(drained)
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
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := a.BackfillHistory(ctx, backfillRetryOptions(chat)); first <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old callback did not start")
	}
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for callback")
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
		_, err := a.BackfillHistory(context.Background(), opts)
		second <- err
	}()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("next recovery did not start")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("old callback did not drain")
	}
	select {
	case err := <-second:
		t.Fatalf("old callback completed new request: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	f.emit(backfillTestResponse(chat, "new-window", base.Add(-2*time.Second)))
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	retained := readAttempt(t, a, chat)
	if retained.Latest.State != store.HistorySucceeded || retained.Latest.PrimaryNoMoreObservedAt != nil || retained.Latest.ResponsesSeen != 1 || *retained.Latest.NetGrowth != 1 {
		t.Fatalf("old callback contaminated new recovery: %+v", retained.Latest)
	}
}
