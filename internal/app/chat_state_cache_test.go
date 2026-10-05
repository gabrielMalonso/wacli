package app

import (
	"context"
	"errors"
	"testing"

	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type chatSettingsWA struct {
	*fakeWA
	read func(context.Context, types.JID) (types.LocalChatSettings, error)
}

type cleanupCoverageWA struct {
	*fakeWA
	onDisconnect func()
}

func (f *cleanupCoverageWA) Disconnect() {
	if f.onDisconnect != nil {
		f.onDisconnect()
	}
	f.fakeWA.Disconnect()
}

func TestSyncCleanupMarkerFailureKeepsCoverage(t *testing.T) {
	a := newTestApp(t)
	f := &cleanupCoverageWA{fakeWA: newFakeWA()}
	a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
	chat := types.NewJID("123", types.GroupServer)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.onDisconnect = func() {
		f.mu.Lock()
		covered := len(f.handlers) >= 2 // session observer and sync persistence
		f.mu.Unlock()
		if !covered {
			t.Fatal("removed coverage before disconnect after marker failure")
		}
	}
	_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
		evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_cleanup_intent BEFORE INSERT ON app_state_recovery_intents BEGIN SELECT RAISE(ABORT,'synthetic marker failure'); END`)
		cancel()
		return nil
	}})
	if err == nil || f.IsConnected() {
		t.Fatalf("failed cleanup did not return error/disconnect: %v", err)
	}
	evidenceFixtureSQL(t, a, `DROP TRIGGER fail_cleanup_intent`)
	// Disconnect is not a callback drain. A later callback is still covered.
	f.emit(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
	if err := a.appStatePersist.waitIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, err := a.db.GetChat(chat.String())
	if err != nil || !row.Archived {
		t.Fatal(row, err)
	}
	f.onDisconnect = nil
}

func (f *chatSettingsWA) GetChatSettings(ctx context.Context, jid types.JID) (types.LocalChatSettings, error) {
	return f.read(ctx, jid)
}

func TestChatStateCacheMissingErrorAndExactAlias(t *testing.T) {
	for _, action := range []string{"archive", "pin"} {
		for _, mode := range []string{"missing", "error", "mapped_lid", "unknown_lid", "newer_debt"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				a := newTestApp(t)
				lid := types.NewJID("300", types.HiddenUserServer)
				pn := types.NewJID("15550000001", types.DefaultUserServer)
				f := &chatSettingsWA{fakeWA: newFakeWA()}
				target := pn
				if mode == "unknown_lid" {
					target = lid
				} else {
					f.lids[lid] = pn
				}
				a.wa = f
				if err := a.db.SetChatArchived(target.String(), true); err != nil {
					t.Fatal(err)
				}
				if err := a.db.SetChatPinned(target.String(), true); err != nil {
					t.Fatal(err)
				}
				if err := a.db.SetChatUnreadCount(target.String(), 2); err != nil {
					t.Fatal(err)
				}
				reads := 0
				f.read = func(_ context.Context, jid types.JID) (types.LocalChatSettings, error) {
					reads++
					if jid != lid {
						t.Fatalf("looked up guessed alias %s instead of exact %s", jid, lid)
					}
					switch mode {
					case "missing":
						return types.LocalChatSettings{}, nil
					case "error":
						return types.LocalChatSettings{Found: true}, errors.New("synthetic settings read failure")
					case "newer_debt":
						if err := a.db.MarkAppStateRecoveryRequired(string(appstate.WAPatchRegularLow)); err != nil {
							t.Fatal(err)
						}
					}
					return types.LocalChatSettings{Found: true, Archived: false, Pinned: false}, nil
				}
				var evt any = &events.Archive{JID: lid, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}}
				if action == "pin" {
					evt = &events.Pin{JID: lid, Action: &waSyncAction.PinAction{Pinned: proto.Bool(true)}}
				}
				a.handleAppStatePersistenceEvent(t.Context(), evt, nil)
				if err := a.appStatePersist.waitIdle(t.Context()); err != nil {
					t.Fatal(err)
				}
				row, err := a.db.GetChat(target.String())
				if err != nil {
					t.Fatal(err)
				}
				failed := mode == "missing" || mode == "error"
				value := row.Archived
				if action == "pin" {
					value = row.Pinned
				}
				if value != failed || reads != 1 || !row.Unread || row.UnreadCount != 2 {
					t.Fatalf("state=%+v reads=%d", row, reads)
				}
				debt, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
				if err != nil || debt != (failed || mode == "newer_debt") {
					t.Fatalf("debt=%v err=%v", debt, err)
				}
				if target == pn {
					if _, err := a.db.GetChat(lid.String()); err == nil {
						t.Fatal("invented alias row")
					}
				}
			})
		}
	}
}

func TestChatStateCacheReadRunsAtPersistenceTurn(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	chat := types.NewJID("123", types.GroupServer)
	old := &events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}}
	f.applyChatSettings(old)
	blocker := a.appStatePersist.reserve()
	a.handleAppStatePersistenceEvent(t.Context(), old, nil)
	f.applyChatSettings(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(false)}})
	a.appStatePersist.complete(blocker, func() {})
	if err := a.appStatePersist.waitIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, err := a.db.GetChat(chat.String())
	if err != nil || row.Archived {
		t.Fatalf("captured stale cache before persistence: %+v %v", row, err)
	}
}

func TestChatStateSDKCompletedWithMissingCacheKeepsDebt(t *testing.T) {
	a := newTestApp(t)
	f := &chatSettingsWA{fakeWA: newFakeWA(), read: func(context.Context, types.JID) (types.LocalChatSettings, error) {
		return types.LocalChatSettings{}, nil
	}}
	a.wa = f
	chat := types.NewJID("123", types.GroupServer)
	if err := a.db.SetChatArchived(chat.String(), false); err != nil {
		t.Fatal(err)
	}
	outcome, mirror, err := a.archiveChatResolved(t.Context(), chat, chat.String(), true, nil)
	if err == nil || outcome != ChatStateSDKCompleted || mirror != ChatStateMirrorUnconfirmed {
		t.Fatal(outcome, mirror, err)
	}
	row, err := a.db.GetChat(chat.String())
	if err != nil || row.Archived {
		t.Fatal(row, err)
	}
	debt, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
	if err != nil || !debt {
		t.Fatal(debt, err)
	}
}

func TestIncrementalReplayCancellationKeepsExistingDebt(t *testing.T) {
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &appStateContextWA{fakeWA: newFakeWA(), fetchEvents: func(context.Context, string, bool, bool) ([]any, error) {
		cancel()
		return nil, nil // SDK success is insufficient after cancellation.
	}}
	a.wa = f
	name := appstate.WAPatchRegularLow
	if err := a.db.MarkAppStateRecoveryRequired(string(name)); err != nil {
		t.Fatal(err)
	}
	if err := a.syncAndPersistAppStateDelta(ctx, name, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	debt, err := a.db.AppStateRecoveryRequired(string(name))
	if err != nil || !debt {
		t.Fatal(debt, err)
	}
}

func TestAppStateReplayCannotClearUnobservedConnectionDebt(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	if err := a.RequireAppStateReplay(t.Context()); err != nil {
		t.Fatal(err)
	}
	// An already admitted read may start after handler retirement. Its complete
	// replay cannot cover later cursor advances on this unobserved connection.
	if err := a.syncAndPersistAppStateDelta(t.Context(), appstate.WAPatchRegularLow, false); err != nil {
		t.Fatal(err)
	}
	pending, err := a.db.AppStateRecoveryCollections()
	if err != nil || len(pending) != 3 {
		t.Fatal(pending, err)
	}
}

func TestSyncNewCoverageCanRetirePriorConnectionDebt(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
	for run := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
			pending, err := a.db.AppStateRecoveryCollections()
			if err != nil || len(pending) != 0 {
				t.Fatalf("run %d: covered startup did not retire prior debt: %v %v", run, pending, err)
			}
			cancel()
			return nil
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		pending, err := a.db.AppStateRecoveryCollections()
		if err != nil || len(pending) != 3 {
			t.Fatalf("run %d: cleanup lost coverage debt: %v %v", run, pending, err)
		}
	}
}

func TestAppStateReplayPreparationGuardsAndCloseRestoration(t *testing.T) {
	for _, mode := range []string{"cancelled", "readonly", "marker_failure", "cleared_during_cleanup"} {
		t.Run(mode, func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			if mode == "readonly" {
				a.opts.ReadOnly = true
			}
			if mode == "marker_failure" {
				evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_intent BEFORE INSERT ON app_state_recovery_intents BEGIN SELECT RAISE(ABORT,'synthetic marker failure'); END`)
			}
			err := a.RequireAppStateReplay(ctx)
			if mode != "cleared_during_cleanup" {
				if err == nil {
					t.Fatal("preparation unexpectedly succeeded")
				}
				pending, err := a.db.AppStateRecoveryCollections()
				if err != nil || len(pending) != 0 || f.connectCalls != 0 {
					t.Fatal(pending, err, f.connectCalls)
				}
				if mode == "marker_failure" {
					evidenceFixtureSQL(t, a, `DROP TRIGGER fail_intent`)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range mirroredAppStateCollections {
				if err := a.db.ClearAppStateRecoveryRequired(string(name)); err != nil {
					t.Fatal(err)
				}
			}
			dir := a.StoreDir()
			a.Close()
			reader, err := New(Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			pending, err := reader.db.AppStateRecoveryCollections()
			if err != nil || len(pending) != 3 {
				t.Fatalf("close failed to reaffirm cleared debt: %v %v", pending, err)
			}
		})
	}
}

func TestChatStateLateCallbackAfterCloseCannotPersist(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
	remove, err := a.AddChatStatePersistenceHandler(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	f.mu.Lock()
	var callbacks []func(any)
	for _, handler := range f.handlers {
		callbacks = append(callbacks, handler)
	}
	f.mu.Unlock()
	a.Close()
	chat := types.NewJID("123", types.GroupServer)
	output := captureStderr(t, func() {
		for _, callback := range callbacks {
			callback(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
		}
	})
	if output != "" {
		t.Fatalf("late callback attempted archive persistence: %s", output)
	}
}
