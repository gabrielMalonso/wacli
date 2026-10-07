package app

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type blockedHistoryWA struct {
	*fakeWA
	started chan struct{}
	release chan struct{}
	history *waHistorySync.HistorySync
	err     error
}

func (f *blockedHistoryWA) DownloadHistorySync(ctx context.Context, _ *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
	close(f.started)
	select {
	case <-f.release:
		return f.history, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSyncIdleWaitsForAdmittedHistory(t *testing.T) {
	for _, mode := range []SyncMode{SyncModeOnce, SyncModeBootstrap} {
		for _, scenario := range []struct {
			name                  string
			native, downloadFails bool
		}{{name: "download_completed"}, {name: "download_failed", downloadFails: true}, {name: "native_persistence", native: true}} {
			t.Run(string(mode)+"/"+scenario.name, func(t *testing.T) {
				a := newTestApp(t)
				chat := types.NewJID("123", types.DefaultUserServer)
				if scenario.native {
					chat = types.NewJID("123-456", types.GroupServer)
				}
				history := historySyncWithTextMessages(chat, time.Now(), "other-device").Data
				history.SyncType = waHistorySync.HistorySync_RECENT.Enum()
				history.Conversations[0].Messages[0].Message.Key.FromMe = proto.Bool(true)
				f := &blockedHistoryWA{fakeWA: newFakeWA(), started: make(chan struct{}), release: make(chan struct{}), history: history}
				if scenario.downloadFails {
					f.err = errors.New("synthetic download failure")
				}
				a.wa = f
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if scenario.native {
					// Group metadata lookup is part of the real persistence path.
					f.onGroupInfo = func() {
						close(f.started)
						select {
						case <-f.release:
						case <-ctx.Done():
						}
					}
				}
				drained := make(chan struct{})
				t.Cleanup(func() {
					cancel()
					select {
					case <-drained:
					case <-time.After(time.Second):
						t.Error("history callback did not drain")
					}
				})
				type outcome struct {
					result SyncResult
					err    error
				}
				done := make(chan outcome, 1)
				go func() {
					res, err := a.Sync(ctx, SyncOptions{Mode: mode, IdleExit: 100 * time.Millisecond, AfterConnect: func(context.Context) error {
						go func() {
							defer close(drained)
							if scenario.native {
								f.emit(&events.HistorySync{Data: history})
								return
							}
							f.emit(&events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_RECENT.Enum()}}}})
						}()
						<-f.started
						return nil
					}})
					done <- outcome{res, err}
				}()
				<-f.started
				// Cross two idle-loop ticks while the SDK download is still admitted.
				select {
				case got := <-done:
					t.Fatalf("sync exited during history callback: %+v", got)
				case <-time.After(600 * time.Millisecond):
				}
				close(f.release)
				finished := time.Now()
				got := <-done
				wantStored := int64(1)
				if scenario.downloadFails {
					wantStored = 0
				}
				if got.err != nil || got.result.MessagesStored != wantStored {
					t.Fatalf("Sync = %+v", got)
				}
				if time.Since(finished) < 100*time.Millisecond {
					t.Fatal("sync did not wait a fresh idle window after history finished")
				}
				if !scenario.downloadFails {
					msg, err := a.db.GetMessage(chat.String(), "other-device")
					if err != nil || !msg.FromMe || msg.SenderJID != f.LinkedJID() {
						t.Fatalf("own history message = %+v, err=%v", msg, err)
					}
				}
			})
		}
	}
}

func TestSyncCancellationInterruptsAdmittedHistoryDownload(t *testing.T) {
	for _, mode := range []SyncMode{SyncModeOnce, SyncModeBootstrap} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%t", mode, deadline), func(t *testing.T) {
				a := newTestApp(t)
				f := &blockedHistoryWA{fakeWA: newFakeWA(), started: make(chan struct{}), release: make(chan struct{})}
				a.wa = f
				ctx, cancel := context.WithCancel(t.Context())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				}
				defer cancel()
				drained := make(chan struct{})
				res, err := a.Sync(ctx, SyncOptions{Mode: mode, IdleExit: time.Minute, AfterConnect: func(context.Context) error {
					go func() {
						defer close(drained)
						f.emit(&events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_RECENT.Enum()}}}})
					}()
					<-f.started
					if !deadline {
						cancel()
					}
					return nil
				}})
				if err != nil || res.MessagesStored != 0 {
					t.Fatalf("changed legacy cancellation result: %+v, %v", res, err)
				}
				select {
				case <-drained:
				case <-time.After(time.Second):
					t.Fatal("download was not cancelled")
				}
			})
		}
	}
}

func TestSyncReplaySignalsExtendIdleWindow(t *testing.T) {
	for _, evt := range []any{&events.OfflineSyncPreview{Total: 4, Messages: 0}, &events.OfflineSyncCompleted{Count: 4}} {
		t.Run(fmt.Sprintf("%T", evt), func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			o := newHistoryObserver(nil, nil)
			o.last = time.Now().Add(-time.Hour)
			var stored, lastEvent atomic.Int64
			lastEvent.Store(o.last.UnixNano())
			a.addSyncEventHandler(t.Context(), SyncOptions{historyObserver: o}, &stored, &lastEvent,
				make(chan struct{}, 1), make(chan struct{}, 1), make(chan staleReconnectRequest, 1),
				func(string, string) {}, nil, nil, &syncPresence{}, nil)
			started := time.Now()
			f.emit(evt)
			if time.Unix(0, lastEvent.Load()).Before(started) {
				t.Fatal("replay signal did not refresh sync activity")
			}
			if _, closed := o.closeIfIdle(time.Second); closed {
				t.Fatal("replay signal did not refresh the callback idle window")
			}
			if stored.Load() != 0 {
				t.Fatal("replay signals changed the legacy message counter")
			}
		})
	}
}

func TestHistoryCallbackCompletionRestartsIdleWindow(t *testing.T) {
	for _, evt := range []any{&events.Message{}, &events.HistorySync{}} {
		t.Run(fmt.Sprintf("%T", evt), func(t *testing.T) {
			a := newTestApp(t)
			o := newHistoryObserver(nil, nil)
			// The idle window already expired before this callback was admitted.
			// A large interval avoids depending on polling or scheduler timing.
			o.last = time.Now().Add(-2 * time.Hour)
			_, finish := a.historyEventOptions(SyncOptions{historyObserver: o}, evt)
			if _, closed := o.closeIfIdle(time.Hour); closed {
				t.Fatal("idle window closed during an admitted callback")
			}
			finish()
			if remaining, closed := o.closeIfIdle(time.Hour); closed || remaining <= 0 {
				t.Fatal("callback completion did not restart the expired idle window")
			}
		})
	}
}
