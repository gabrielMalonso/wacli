package app

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/sqliteutil"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Offline proof: real SDK patch encode/decode, cursor, ChatSettings and dispatch;
// page transport and dispatch timing are synthetic, with no socket or account network.
type sdkMirrorWA struct {
	*fakeWA
	sdk           *whatsmeow.Client
	replay        []any
	onUnavailable func()
	onDeltaFetch  func() error
}

func (f *sdkMirrorWA) FetchAppStateEvents(_ context.Context, name string, full, _ bool) ([]any, error) {
	if name == string(appstate.WAPatchRegularLow) && !full && f.onDeltaFetch != nil {
		if err := f.onDeltaFetch(); err != nil {
			return nil, err
		}
		return f.replay, nil
	}
	if name == string(appstate.WAPatchRegularLow) && full {
		return f.replay, nil
	}
	return nil, nil
}
func (f *sdkMirrorWA) GetChatSettings(ctx context.Context, jid types.JID) (types.LocalChatSettings, error) {
	return f.sdk.Store.ChatSettings.GetChatSettings(ctx, jid)
}
func (f *sdkMirrorWA) SendPresence(ctx context.Context, p types.Presence) error {
	if p == types.PresenceUnavailable && f.onUnavailable != nil {
		f.onUnavailable()
	}
	return f.fakeWA.SendPresence(ctx, p)
}
func sdkMirrorFixture(t *testing.T) (*App, *sdkMirrorWA, types.JID) {
	t.Helper()
	a := newTestApp(t)
	container, err := sqlstore.New(t.Context(), "sqlite3", sqliteutil.FileURI(filepath.Join(t.TempDir(), "synthetic-session.db"), "_foreign_keys=on"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := container.NewDevice()
	device.ID = &types.JID{User: "15550000000", Device: 7, Server: types.DefaultUserServer}
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := &sdkMirrorWA{fakeWA: newFakeWA(), sdk: whatsmeow.NewClient(device, nil)}
	if err := device.AppStateKeys.PutAppStateSyncKey(t.Context(), []byte{1, 2, 3}, store.AppStateSyncKey{Data: bytes.Repeat([]byte{0xAB}, 32), Fingerprint: []byte{1}, Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	f.sdk.AddEventHandler(f.emit)
	a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
	chat := types.NewJID("120363000000001", types.GroupServer)
	if err := a.db.UpsertChat(chat.String(), "group", "Synthetic", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetChatPinned(chat.String(), true); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetChatUnread(chat.String(), true); err != nil {
		t.Fatal(err)
	}
	return a, f, chat
}
func applySDKMirrorPatch(t *testing.T, f *sdkMirrorWA, patch appstate.PatchInfo, version uint64) []any {
	t.Helper()
	current, hash, err := f.sdk.Store.AppState.GetAppStateVersion(t.Context(), string(patch.Type))
	if err != nil {
		t.Fatal(err)
	}
	state := appstate.HashState{Version: current, Hash: hash}
	patch.Timestamp = time.Unix(int64(version), 0)
	raw, err := appstate.NewProcessor(f.sdk.Store, waLog.Noop).EncodePatch(t.Context(), []byte{1, 2, 3}, state, patch)
	if err != nil {
		t.Fatal(err)
	}
	encoded := &waServerSync.SyncdPatch{}
	if err := proto.Unmarshal(raw, encoded); err != nil {
		t.Fatal(err)
	}
	encoded.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(version)}
	var evts []any
	next, err := f.sdk.DangerousInternals().ApplyAppStatePatches(t.Context(), patch.Type, state, &appstate.PatchList{Name: patch.Type, Patches: []*waServerSync.SyncdPatch{encoded}, HasMorePatches: true}, false, &evts)
	if err != nil || next.Version != version {
		t.Fatalf("patch apply version=%v err=%v", next.Version, err)
	}
	return evts
}
func dispatchSDKMirrorEvents(f *sdkMirrorWA, evts []any) {
	for _, evt := range evts {
		f.sdk.DangerousInternals().DispatchEvent(evt)
	}
}
func assertSDKMirror(t *testing.T, a *App, f *sdkMirrorWA, chat types.JID, archived, pinned, debt bool) {
	t.Helper()
	row, err := a.db.GetChat(chat.String())
	if err != nil {
		t.Fatal(err)
	}
	if row.Archived != archived || row.Pinned != pinned {
		t.Fatalf("mirror archived=%v pinned=%v", row.Archived, row.Pinned)
	}
	required, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
	if err != nil || required != debt {
		t.Fatalf("debt=%v err=%v", required, err)
	}
	sdk, err := f.sdk.Store.ChatSettings.GetChatSettings(t.Context(), chat)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mirror=(%v,%v) SDK=(%v,%v) debt=%v", row.Archived, row.Pinned, sdk.Archived, sdk.Pinned, required)
}
func restartSDKMirror(t *testing.T, a *App, f *sdkMirrorWA) *App {
	t.Helper()
	dir := a.StoreDir()
	a.Close()
	next, err := New(Options{StoreDir: dir, WAFactory: func(wa.Options) (WAClient, error) { return f, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = next.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet, AfterConnect: func(context.Context) error { cancel(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestChatStateDelayedSDKSendAfterReplay(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_replay_recovers", true: "after_replay_stays_current"}[after], func(t *testing.T) {
			a, f, chat := sdkMirrorFixture(t)
			if err := a.OpenWA(); err != nil {
				t.Fatal(err)
			}
			remove, err := a.AddChatStatePersistenceHandler(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			old := applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, true, time.Time{}, nil), 1)
			f.replay = append(applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, false, time.Time{}, nil), 2), applySDKMirrorPatch(t, f, appstate.BuildPin(chat, true), 3)...)
			// This successful local send leaves the production durable intent.
			if err := a.PinChat(t.Context(), chat, true); err != nil {
				t.Fatal(err)
			}
			if !after {
				dispatchSDKMirrorEvents(f, old)
			}
			if err := a.syncChatStateBeforeWrite(t.Context(), appstate.WAPatchRegularLow); err != nil {
				t.Fatal(err)
			}
			if after {
				dispatchSDKMirrorEvents(f, old)
			}
			assertSDKMirror(t, a, f, chat, false, true, false)
			remove()
			next := restartSDKMirror(t, a, f)
			assertSDKMirror(t, next, f, chat, false, true, true)
		})
	}
}
func TestStandaloneAppStateReplayIntent(t *testing.T) {
	for _, debt := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh_intent_recovers", true: "existing_debt_recovers"}[debt], func(t *testing.T) {
			a, f, chat := sdkMirrorFixture(t)
			f.replay = applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, true, time.Time{}, nil), 1)
			f.connectEvents = f.replay
			if debt {
				if err := a.db.MarkAppStateRecoveryRequired(string(appstate.WAPatchRegularLow)); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.RequireAppStateReplay(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := a.Connect(t.Context(), false, nil); err != nil {
				t.Fatal(err)
			}
			assertSDKMirror(t, a, f, chat, false, true, true)
			f.connectEvents = nil
			next := restartSDKMirror(t, a, f)
			assertSDKMirror(t, next, f, chat, true, false, true)
		})
	}
}
func TestSyncCleanupPreservesAppStateReplay(t *testing.T) {
	a, f, chat := sdkMirrorFixture(t)
	f.onUnavailable = func() {
		f.replay = applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, true, time.Time{}, nil), 1)
		dispatchSDKMirrorEvents(f, f.replay)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet, AfterConnect: func(context.Context) error { cancel(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	assertSDKMirror(t, a, f, chat, false, true, true)
	f.onUnavailable = nil
	next := restartSDKMirror(t, a, f)
	assertSDKMirror(t, next, f, chat, true, false, true)
}

func TestIncrementalPartialFailurePreservesAppStateReplay(t *testing.T) {
	a, f, chat := sdkMirrorFixture(t)
	f.onDeltaFetch = func() error {
		required, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
		if err != nil || !required {
			t.Fatal("missing intent before first page", required, err)
		}
		// SDK commits/collects page 1, then fetchAppState returns nil events if
		// fetching page 2 fails (appstate.go:79-92); no callback reaches the mirror.
		f.replay = applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, true, time.Time{}, nil), 1)
		return errors.New("synthetic failure fetching second page")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet, AfterConnect: func(context.Context) error { cancel(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	assertSDKMirror(t, a, f, chat, false, true, true)
	f.onDeltaFetch = nil
	next := restartSDKMirror(t, a, f)
	assertSDKMirror(t, next, f, chat, true, false, true)
}

func TestIncrementalAppStateIntentCoversCompletionAndCancellation(t *testing.T) {
	for _, mode := range []string{"success", "cancelled", "partial_page_error", "persistence_error", "marker_error"} {
		t.Run(mode, func(t *testing.T) {
			a, f, chat := sdkMirrorFixture(t)
			if err := a.OpenWA(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pages := 0
			f.onDeltaFetch = func() error {
				pages++
				pending, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
				if err != nil || !pending {
					t.Fatal("cursor could advance before intent", pending, err)
				}
				f.replay = applySDKMirrorPatch(t, f, appstate.BuildArchive(chat, true, time.Time{}, nil), 1)
				if mode == "cancelled" {
					cancel()
				}
				if mode == "partial_page_error" {
					return errors.New("synthetic second page error")
				}
				return nil
			}
			if mode == "persistence_error" {
				evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_mirror BEFORE UPDATE OF archived ON chats BEGIN SELECT RAISE(ABORT,'synthetic persistence failure'); END`)
			}
			if mode == "marker_error" {
				evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_intent BEFORE INSERT ON app_state_recovery_intents BEGIN SELECT RAISE(ABORT,'synthetic marker failure'); END`)
			}
			err := a.syncAndPersistAppStateDelta(ctx, appstate.WAPatchRegularLow, false)
			if (err == nil) != (mode == "success") {
				t.Fatalf("completion %s: %v", mode, err)
			}
			pending, checkErr := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
			if checkErr != nil || pending != (mode != "success" && mode != "marker_error") {
				t.Fatal(pending, checkErr)
			}
			if mode == "marker_error" {
				if pages != 0 {
					t.Fatal("fetch crossed failed marker")
				}
				evidenceFixtureSQL(t, a, `DROP TRIGGER fail_intent`)
				return
			}
			if pages != 1 {
				t.Fatal("unexpected incremental retry", pages)
			}
			if mode == "persistence_error" {
				evidenceFixtureSQL(t, a, `DROP TRIGGER fail_mirror`)
			}
			f.onDeltaFetch = nil
			if mode != "success" {
				if err := a.syncAndPersistAppStateDelta(t.Context(), appstate.WAPatchRegularLow, false); err != nil {
					t.Fatal(err)
				}
			}
			assertSDKMirror(t, a, f, chat, true, false, false)
			if len(f.archiveCalls) != 0 || len(f.pinCalls) != 0 || len(f.markReadCalls) != 0 {
				t.Fatal("recovery reexecuted user mutation")
			}
		})
	}
}
