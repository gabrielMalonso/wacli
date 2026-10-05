package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestBackfillStopEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, id, legacy string
		empty, primary   bool
		want             BackfillStopReason
		wantAdded        int64
	}{
		{"duplicate with primary end", "anchor", "start_of_history_reached", false, true, BackfillStopPrimaryNoMore, 0},
		{"empty with primary end", "", "start_of_history_reached", true, true, BackfillStopPrimaryNoMore, 0},
		{"duplicate without end", "anchor", "no_older_messages_added", false, false, BackfillStopNoProgress, 0},
		{"empty without end", "", "no_messages_returned", true, false, BackfillStopEmptyResponse, 0},
		{"batch limit", "older", "requested_batch_limit", false, false, BackfillStopRequestedBatchLimit, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor")
			var log bytes.Buffer
			a.opts.Events = out.NewEventWriter(&log, true)
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				hs := backfillTestResponse(chat, tc.id, base.Add(-time.Second))
				if tc.empty {
					hs.Data.Conversations[0].Messages = nil
				}
				if tc.primary {
					hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
				}
				return hs
			}
			res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
			if err != nil || res.StopReason != tc.want || res.MessagesAdded != tc.wantAdded || res.RequestsSent != 1 || res.ResponsesSeen != 1 {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if !strings.Contains(log.String(), `"reason":"`+tc.legacy+`"`) || !strings.Contains(log.String(), `"stop_reason":"`+string(tc.want)+`"`) {
				t.Fatalf("missing stop evidence: %s", &log)
			}
		})
	}
}

func TestBackfillCountExcludesOtherChatsButIncludesConcurrentSelectedActivity(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		// Both live traffic and other conversations in the response are global
		// Sync activity, not exclusively messages from the requested history.
		f.emit(backfillTestResponse("other@g.us", "live-other", base))
		jid, _ := types.ParseJID(chat)
		f.emit(&events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "live-selected", Timestamp: base.Add(time.Second)},
			Message: &waE2E.Message{Conversation: proto.String("live text")},
		})
		hs := backfillTestResponse(chat, "older", base.Add(-time.Second))
		hs.Data.Conversations = append(hs.Data.Conversations, backfillTestResponse("other@g.us", "history-other", base).Data.Conversations[0])
		return hs
	}
	a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
		if bytes.Contains(p, []byte(`"event":"idle_exit"`)) {
			// Synthetic arrivals at the idle boundary exercise the full counting
			// window, independently of the earlier on-demand response.
			for _, target := range []string{chat, "other@g.us"} {
				if err := a.db.UpsertMessage(storeUpsertMessage(target, "idle-arrival", base.Add(2*time.Second), "idle activity")); err != nil {
					t.Error(err)
				}
			}
		}
	}), true)
	res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	if err != nil || res.MessagesAdded != 3 || res.MessagesSynced != 4 {
		t.Fatalf("result = %+v, %v; want conversation growth 3 and global synced 4", res, err)
	}
}

func TestBackfillBaselineAfterConnectAliasMerge(t *testing.T) {
	for _, inputLID := range []bool{false, true} {
		t.Run(map[bool]string{false: "PN input", true: "LID input"}[inputLID], func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			pn := types.NewJID("15550000001", types.DefaultUserServer)
			lid := types.NewJID("100000000001", types.HiddenUserServer)
			base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, jid := range []types.JID{pn, lid} {
				if err := a.db.UpsertChat(jid.String(), "dm", "fixture", base); err != nil {
					t.Fatal(err)
				}
				if err := a.db.UpsertMessage(storeUpsertMessage(jid.String(), "anchor", base, "existing")); err != nil {
					t.Fatal(err)
				}
			}
			f.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.Connected); ok {
					f.mu.Lock()
					f.lids[lid] = pn
					f.mu.Unlock()
				}
			})
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				// Replay an existing ID under the mapped LID plus one new ID.
				hs := backfillTestResponse(lid.String(), "older", base.Add(-time.Second))
				hs.Data.Conversations[0].Messages = append(hs.Data.Conversations[0].Messages, backfillTestResponse(lid.String(), "anchor", base).Data.Conversations[0].Messages...)
				return hs
			}
			input := pn
			if inputLID {
				input = lid
			}
			res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(input.String()))
			if err != nil || res.ChatJID != pn.String() || res.MessagesAdded != 1 || res.MessagesSynced != 2 {
				t.Fatalf("result = %+v, %v; connect dedup must not reduce added count", res, err)
			}
		})
	}
}

func TestBackfillKnownAliasMergeDuringWindow(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	pn := types.NewJID("15550000001", types.DefaultUserServer)
	lid := types.NewJID("100000000001", types.HiddenUserServer)
	f.lids[lid] = pn
	base := time.Now().UTC().Truncate(time.Second)
	for _, jid := range []types.JID{pn, lid} {
		if err := a.db.UpsertChat(jid.String(), "dm", "fixture", base); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.db.UpsertMessage(storeUpsertMessage(pn.String(), "anchor", base, "existing")); err != nil {
		t.Fatal(err)
	}
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		if err := a.db.UpsertChat(lid.String(), "dm", "fixture alias", base); err != nil {
			t.Fatal(err)
		}
		if err := a.db.UpsertMessage(storeUpsertMessage(lid.String(), "anchor", base, "alias duplicate")); err != nil {
			t.Fatal(err)
		}
		if err := a.db.MigrateLIDToPN(lid.String(), pn.String()); err != nil {
			t.Fatal(err)
		}
		return backfillTestResponse(lid.String(), "older", base.Add(-time.Second))
	}
	res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(pn.String()))
	if err != nil || res.MessagesAdded != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

// Hooks run synchronously at lifecycle boundaries and must not emit more events.
// This makes failures after persistence and during idle deterministic.
type backfillEventHook func([]byte)

func (hook backfillEventHook) Write(p []byte) (int, error) {
	hook(p)
	return len(p), nil
}

func TestBackfillUnreliableCountingWindowFails(t *testing.T) {
	for _, change := range []string{"identities", "count"} {
		t.Run(change, func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor", "extra")
			if change == "identities" {
				chat = types.NewJID("100000000001", types.HiddenUserServer).String()
				if err := a.db.UpsertChat(chat, "dm", "fixture", base); err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"anchor", "extra"} {
					if err := a.db.UpsertMessage(storeUpsertMessage(chat, id, base, "existing")); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				return backfillTestResponse(chat, "anchor", base)
			}
			a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
				if !bytes.Contains(p, []byte(`"event":"idle_exit"`)) {
					return
				}
				if change == "identities" {
					jid, _ := types.ParseJID(chat)
					f.mu.Lock()
					f.lids[jid] = types.NewJID("15550000001", types.DefaultUserServer)
					f.mu.Unlock()
				} else {
					if err := a.db.DeleteChat(chat); err != nil {
						t.Error(err)
					}
				}
			}), true)
			res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
			if err == nil || !strings.Contains(err.Error(), "could not be measured reliably") || !strings.Contains(err.Error(), "already persisted messages may remain") || res != (BackfillResult{}) {
				t.Fatalf("result = %+v, error = %v", res, err)
			}
		})
	}
}

func TestBackfillReadErrorsAfterResponseAndIdle(t *testing.T) {
	for _, tc := range []struct{ event, want string }{
		{"backfill_response", "read oldest backfill message after response"},
		// Sync must now persist coverage debt before returning to the count.
		{"idle_exit", "mark WhatsApp app state replay"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor")
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				return backfillTestResponse(chat, "older", base.Add(-time.Second))
			}
			a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
				if bytes.Contains(p, []byte(`"event":"`+tc.event+`"`)) {
					if err := a.db.Close(); err != nil {
						t.Error(err)
					}
				}
			}), true)
			res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
			if err == nil || !strings.Contains(err.Error(), tc.want) || res != (BackfillResult{}) {
				t.Fatalf("result = %+v, error = %v", res, err)
			}
		})
	}
}

func TestBackfillCancellationAfterResponseIsError(t *testing.T) {
	for _, event := range []string{"backfill_response", "backfill_stopped", "idle_exit"} {
		t.Run(event, func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				return backfillTestResponse(chat, "older", base.Add(-time.Second))
			}
			a.opts.Events = out.NewEventWriter(backfillEventHook(func(p []byte) {
				if bytes.Contains(p, []byte(`"event":"`+event+`"`)) {
					cancel()
				}
			}), true)
			res, err := a.BackfillHistory(ctx, backfillRetryOptions(chat))
			if !errors.Is(err, context.Canceled) || res != (BackfillResult{}) {
				t.Fatalf("result = %+v, error = %v; want cancellation", res, err)
			}
		})
	}
}

func TestBackfillStoreErrorScopedToSelectedOperation(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "history event", true: "manual download"}[manual], func(t *testing.T) {
			a, f, chat, base := newBackfillRetryTest(t, "anchor")
			if err := a.OpenWA(); err != nil {
				t.Fatal(err)
			}
			baselineHandlers := len(f.handlers) // App's persistent revocation watcher
			fixtureSQL, err := sql.Open("sqlite3", filepath.Join(a.opts.StoreDir, "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer fixtureSQL.Close()
			if _, err := fixtureSQL.Exec(`CREATE TRIGGER fail_backfill BEFORE INSERT ON messages
				WHEN NEW.msg_id = 'fail' BEGIN SELECT RAISE(ABORT, 'fixture write failure'); END`); err != nil {
				t.Fatal(err)
			}
			response := func(types.MessageInfo, int) *events.HistorySync {
				return backfillTestResponse(chat, "fail", base.Add(-time.Second))
			}
			f.onDemandHistory = response
			if manual {
				installManualBackfillResponse(f, response)
			}
			res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
			if err == nil || !strings.Contains(err.Error(), "fixture write failure") || res != (BackfillResult{}) {
				t.Fatalf("result = %+v, error = %v", res, err)
			}
			// The failed operation must restore manual sync and remove its handlers.
			f.mu.Lock()
			handlers := len(f.handlers)
			calls := append([]bool(nil), f.manualHistorySyncCalls...)
			f.mu.Unlock()
			if handlers != baselineHandlers || len(calls) != 2 || !calls[0] || calls[1] {
				t.Fatalf("handlers = %d, manual calls = %v", handlers, calls)
			}
			// An unrelated persistence error must not poison the next operation.
			f.onDemandEvent = nil
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				f.emit(backfillTestResponse("unrelated@g.us", "fail", base))
				initial := backfillTestResponse(chat, "fail", base)
				initial.Data.SyncType = waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum()
				f.emit(initial) // ordinary sync failures are outside this observer
				return backfillTestResponse(chat, "older", base.Add(-time.Second))
			}
			res, err = a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
			if err != nil || res.MessagesAdded != 1 {
				t.Fatalf("next operation = %+v, %v", res, err)
			}
		})
	}
}

func installManualBackfillResponse(f *fakeWA, response func(types.MessageInfo, int) *events.HistorySync) {
	notif := &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}
	var pending *events.HistorySync
	f.onDemandEvent = func(info types.MessageInfo, count int) any {
		pending = response(info, count)
		return &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: notif}}}
	}
	f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
		return pending.Data, nil
	}
}

type backfillBaselineFailureWA struct {
	*fakeWA
	beforeCount func()
}

func (f *backfillBaselineFailureWA) ResolvePNToLID(ctx context.Context, jid types.JID) types.JID {
	if f.beforeCount != nil {
		f.beforeCount()
		f.beforeCount = nil
	}
	return f.fakeWA.ResolvePNToLID(ctx, jid)
}

func TestBackfillBaselineCountErrorPreventsRequests(t *testing.T) {
	a, f, chat, _ := newBackfillRetryTest(t, "anchor")
	a.wa = &backfillBaselineFailureWA{fakeWA: f, beforeCount: func() {
		if err := a.db.Close(); err != nil {
			t.Error(err)
		}
	}}
	var log bytes.Buffer
	a.opts.Events = out.NewEventWriter(&log, true)
	res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	if err == nil || !strings.Contains(err.Error(), "count backfill conversation before requests") || res != (BackfillResult{}) || strings.Contains(log.String(), "backfill_requesting") {
		t.Fatalf("result = %+v, error = %v, events = %s", res, err, &log)
	}
}

func TestBackfillCancelledBeforePersistenceDoesNotLeakCallback(t *testing.T) {
	a, f, chat, base := newBackfillRetryTest(t, "anchor")
	delayed := &delayedBackfillWA{
		fakeWA: f, beforeStore: make(chan struct{}), releaseStore: make(chan struct{}), delivered: make(chan struct{}),
		response: backfillTestResponse(chat, "older", base.Add(-time.Second)),
	}
	a.wa = delayed
	a.opts.Events = out.NewEventWriter(backfillEventHook(func([]byte) {}), true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		res, err := a.BackfillHistory(ctx, backfillRetryOptions(chat))
		if res != (BackfillResult{}) {
			done <- errors.New("cancelled backfill returned a success result")
			return
		}
		done <- err
	}()
	select {
	case <-delayed.beforeStore:
	case <-time.After(3 * time.Second):
		t.Fatal("response did not reach persistence handler")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v; want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("backfill did not stop on cancellation")
	}
	// A handler snapshot may still finish after RemoveEventHandler. The closed
	// operation-local observer must remain safe while that callback drains.
	close(delayed.releaseStore)
	select {
	case <-delayed.delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("late callback did not drain")
	}
	a.wa = f
	f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
		return backfillTestResponse(chat, "oldest", base.Add(-2*time.Second))
	}
	res, err := a.BackfillHistory(context.Background(), backfillRetryOptions(chat))
	if err != nil || res.MessagesAdded != 1 {
		t.Fatalf("next operation = %+v, %v", res, err)
	}
}
