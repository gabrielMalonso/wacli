package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncdSnapshotRecovery"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func boundaryEnvelope(t *testing.T, id string) *waE2E.Message {
	t.Helper()
	raw, err := proto.Marshal(&waSyncdSnapshotRecovery.SyncdSnapshotRecovery{CollectionName: proto.String("regular_low"), Version: &waSyncdSnapshotRecovery.SyncdVersion{Version: proto.Uint64(81)}, CollectionLthash: make([]byte, 128)})
	if err != nil {
		t.Fatal(err)
	}
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_RESPONSE_MESSAGE.Enum(), PeerDataOperationRequestResponseMessage: &waE2E.PeerDataOperationRequestResponseMessage{StanzaID: proto.String(id), PeerDataOperationRequestType: waE2E.PeerDataOperationRequestType_COMPANION_SYNCD_SNAPSHOT_FATAL_RECOVERY.Enum(), PeerDataOperationResult: []*waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult{{SyncdSnapshotFatalRecoveryResponse: &waE2E.PeerDataOperationRequestResponseMessage_PeerDataOperationResult_SyncDSnapshotFatalRecoveryResponse{CollectionSnapshot: raw}}}}}}
}

func TestBoundaryPublicTraceCannotIdentifyWhichRequestCompleted(t *testing.T) {
	var traces [2][]string
	for world := range 2 {
		_, fixture, _ := sdkMirrorFixture(t)
		sdk := fixture.sdk
		sdk.EmitAppStateEventsOnFullSync = true
		var mu sync.Mutex
		var trace []string
		sdk.AddEventHandler(func(evt any) {
			var label string
			switch e := evt.(type) {
			case *events.AppStateSyncComplete:
				label = fmt.Sprintf("complete:%s:%d:%v", e.Name, e.Version, e.Recovery)
			case *events.Message:
				response := e.Message.GetProtocolMessage().GetPeerDataOperationRequestResponseMessage()
				snapshot, err := appstate.ParseRecovery(response.GetPeerDataOperationResult()[0].GetSyncdSnapshotFatalRecoveryResponse())
				if err != nil {
					t.Error(err)
					return
				}
				label = fmt.Sprintf("message:%s:%s:%s:%d", e.Info.ID, response.GetStanzaID(), snapshot.GetCollectionName(), snapshot.GetVersion().GetVersion())
			default:
				return
			}
			mu.Lock()
			trace = append(trace, label)
			mu.Unlock()
		})
		own := sdk.Store.GetJID().ToNonAD()
		info := &types.MessageInfo{MessageSource: types.MessageSource{Chat: own, Sender: own, IsFromMe: true}, ID: "synthetic-envelope"}
		inject := func(id string) {
			// TEST-ONLY injection at decrypted input, not a production seam or a proof of cryptographic authentication.
			inboundInfo := *info
			inboundInfo.ID = types.MessageID("envelope-" + id)
			if sdk.DangerousInternals().HandleDecryptedMessage(t.Context(), &inboundInfo, boundaryEnvelope(t, id), 0) {
				t.Error("event handler failed")
			}
		}
		if world == 0 {
			// The current request succeeds. The old request is then discarded at equal version.
			inject("current-request")
			inject("old-request")
		} else {
			// The old request succeeds; pause its dispatch before its Message envelope.
			entered, release := make(chan struct{}), make(chan struct{})
			sdk.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.AppStateSyncComplete); ok {
					close(entered)
					<-release
				}
			})
			done := make(chan struct{})
			go func() { defer close(done); inject("old-request") }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("no old completion")
			}
			// The current request is discarded at equal version while old dispatch is paused.
			inject("current-request")
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("old response did not finish")
			}
		}
		version, _, err := sdk.Store.AppState.GetAppStateVersion(t.Context(), "regular_low")
		if err != nil || version != 81 {
			t.Fatalf("version=%d err=%v", version, err)
		}
		traces[world] = trace
		t.Logf("world=%d target_applied=%v public_trace=%v cursor=%d", world, world == 0, trace, version)
	}
	if fmt.Sprint(traces[0]) != fmt.Sprint(traces[1]) {
		t.Fatal("public traces differ")
	}
	t.Log("identical public trace and cursor; completion provenance differs")
}

func injectRecoveryArchive(t *testing.T, fixture *sdkMirrorWA, chat types.JID) {
	t.Helper()
	sdk := fixture.sdk
	sdk.EmitAppStateEventsOnFullSync = true
	raw, err := proto.Marshal(&waSyncdSnapshotRecovery.SyncdSnapshotRecovery{CollectionName: proto.String("regular_low"), Version: &waSyncdSnapshotRecovery.SyncdVersion{Version: proto.Uint64(81)}, CollectionLthash: make([]byte, 128), MutationRecords: []*waSyncdSnapshotRecovery.SyncdPlainTextRecord{{KeyID: []byte{1, 2, 3}, Mac: make([]byte, 32), Value: &waSyncAction.SyncActionData{Index: []byte(fmt.Sprintf(`["archive","%s"]`, chat.String())), Version: proto.Int32(3), Value: &waSyncAction.SyncActionValue{ArchiveChatAction: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	incoming := boundaryEnvelope(t, "current-request")
	incoming.ProtocolMessage.PeerDataOperationRequestResponseMessage.PeerDataOperationResult[0].SyncdSnapshotFatalRecoveryResponse.CollectionSnapshot = raw
	own := sdk.Store.GetJID().ToNonAD()
	info := &types.MessageInfo{MessageSource: types.MessageSource{Chat: own, Sender: own, IsFromMe: true}, ID: "synthetic-envelope"}
	if sdk.DangerousInternals().HandleDecryptedMessage(t.Context(), info, incoming, 0) {
		t.Fatal("synthetic SDK handler failed")
	}
}

func TestRecoverySDKEventsPersistOnEveryOutcomeWithoutCheckpoint(t *testing.T) {
	for _, scope := range []string{"standalone_no_global", "standalone_handler", "sync_handler"} {
		for _, outcome := range []string{"unconfirmed", "error", "cancel"} {
			t.Run(scope+"/"+outcome, func(t *testing.T) {
				a, fixture, chat := sdkMirrorFixture(t)
				f := &appStateContextWA{fakeWA: fixture.fakeWA}
				a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
				if err := a.OpenWA(); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if scope == "standalone_handler" {
					remove, err := a.AddChatStatePersistenceHandler(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { a.WA().Disconnect(); remove() }()
				} else if scope == "sync_handler" {
					var stored, last atomic.Int64
					id, _ := a.addSyncEventHandler(ctx, SyncOptions{}, &stored, &last, make(chan struct{}, 1), make(chan struct{}, 1), make(chan staleReconnectRequest, 1), func(string, string) {}, nil, nil, &syncPresence{}, nil)
					defer f.RemoveEventHandler(id)
				}
				generation, _, err := a.db.BeginAppStateRecovery("regular_low")
				if err != nil {
					t.Fatal(err)
				}
				sendErr := errors.New("synthetic transport failure")
				f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
					injectRecoveryArchive(t, fixture, chat)
					switch outcome {
					case "error":
						return "", sendErr
					case "cancel":
						cancel()
						return "", ctx.Err()
					}
					return "current-request", nil
				}
				err = a.recoverMismatchingAppState(ctx, appstate.WAPatchRegularLow, &appStatePersistenceTracker{}, nil)
				expected := wa.ErrAppStateCompletionUnconfirmed
				if outcome == "error" {
					expected = sendErr
				} else if outcome == "cancel" {
					expected = context.Canceled
				}
				if !errors.Is(err, expected) {
					t.Fatalf("recovery error=%v", err)
				}
				row, err := a.db.GetChat(chat.String())
				if err != nil || !row.Archived {
					t.Fatalf("SDK event lost: archived=%v err=%v", row.Archived, err)
				}
				required, err := a.db.AppStateRecoveryRequired("regular_low")
				if err != nil || !required {
					t.Fatalf("debt=%v err=%v", required, err)
				}
				// A later successful covered full fetch can reconcile; no retry was added to recovery.
				f.fetchEvents = func(context.Context, string, bool, bool) ([]any, error) { return nil, nil }
				if err := a.syncAndPersistAppStateDelta(t.Context(), appstate.WAPatchRegularHigh, true); err != nil {
					t.Fatal(err)
				}
				required, err = a.db.AppStateRecoveryRequired("regular_low")
				if err != nil || !required {
					t.Fatal("another collection erased debt")
				}
				if outcome == "unconfirmed" {
					if err := a.syncAndPersistAppStateDelta(t.Context(), appstate.WAPatchRegularLow, true); err != nil {
						t.Fatal(err)
					}
					required, err = a.db.AppStateRecoveryRequired("regular_low")
					if err != nil || required {
						t.Fatalf("later full fetch did not reconcile: debt=%v err=%v initial_generation=%d", required, err, generation)
					}
				}
			})
		}
	}
}

func TestCloseDrainsUnconfirmedSnapshotCaptureBeforeDatabaseClose(t *testing.T) {
	a, fixture, chat := sdkMirrorFixture(t)
	synctest.Test(t, func(t *testing.T) {
		f := &recoveryCloseWA{appStateContextWA: &appStateContextWA{fakeWA: fixture.fakeWA}, disconnected: make(chan struct{})}
		a.wa = f
		entered, release := make(chan struct{}), make(chan struct{})
		var lateCapture func(any)
		var releaseOnce sync.Once
		defer a.Close()
		defer releaseOnce.Do(func() { close(release) })
		f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
			injectRecoveryArchive(t, fixture, chat)
			f.mu.Lock()
			lateCapture = f.handlers[f.nextHandlerID-1]
			f.mu.Unlock()
			close(entered)
			<-release
			return "current-request", nil
		}
		done := make(chan error, 1)
		go func() {
			done <- a.recoverMismatchingAppState(t.Context(), appstate.WAPatchRegularLow, &appStatePersistenceTracker{}, nil)
		}()
		<-entered
		closed := make(chan struct{})
		go func() { a.Close(); close(closed) }()
		<-f.disconnected
		synctest.Wait()
		if f.closed.Load() {
			t.Fatal("session closed before capture drained")
		}
		select {
		case <-closed:
			t.Fatal("database closed before capture drained")
		default:
		}
		releaseOnce.Do(func() { close(release) })
		if err := <-done; !errors.Is(err, wa.ErrAppStateCompletionUnconfirmed) {
			t.Fatal(err)
		}
		<-closed
		lateCapture(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(false)}})
	})
	db, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.GetChat(chat.String())
	if err != nil || !row.Archived {
		t.Fatal("snapshot event not committed before DB.Close")
	}
}
