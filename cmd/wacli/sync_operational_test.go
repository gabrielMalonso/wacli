package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestSyncTypedOperationsDuringBlockedMetadata(t *testing.T) {
	f := &syncStatusWA{shutdownStarted: make(chan struct{}), shutdownRelease: make(chan struct{})}
	close(f.shutdownRelease)
	metadataStarted, metadataRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.fetchHook = func(ctx context.Context) ([]any, error) {
		once.Do(func() { close(metadataStarted) })
		select {
		case <-metadataRelease:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	dir, a := draftOwnerFixtureOptions(t, app.Options{Events: out.NewEventWriter(io.Discard, true), WAFactory: func(wa.Options) (app.WAClient, error) { f.opens.Add(1); return f, nil }})
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := make(chan func(), 1)
	connectRelease := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.Sync(ctx, app.SyncOptions{Mode: app.SyncModeFollow, PresenceMode: app.SyncPresenceModeQuiet, BeforeConnect: func(ctx context.Context) error {
			stop, err := startSyncDelegateServer(ctx, a, sendSpacing{})
			if err != nil {
				return err
			}
			server <- stop
			select {
			case <-connectRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
		done <- err
	}()
	stop := <-server
	defer stop()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	message := "fixture early typed send"
	draft := app.DraftWriteRequest{Version: 1, Action: "create", StoreRef: dir, DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), Input: &app.DraftInput{To: localReadPN, Message: &message}}
	flags := &rootFlags{storeDir: dir, timeout: time.Second}
	initial := querySyncStatus(t.Context(), dir)
	if initial.Reason != "" || initial.OwnerRunID == "" || initial.SendInitialized || initial.Operations.DraftWrite.Attemptable {
		t.Fatalf("initial=%+v", initial)
	}
	_, err = delegateSend(t.Context(), flags, sendDelegateRequest{Kind: draftWriteKind, Draft: &draft})
	var draftFailure *store.DraftError
	if !errors.As(err, &draftFailure) || draftFailure.Code != "local_write_not_dispatched" {
		t.Fatalf("premature draft: %v", err)
	}
	close(connectRelease)
	select {
	case <-metadataStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata did not start")
	}
	v := querySyncStatus(t.Context(), dir)
	if v.Reason != "" || v.OwnerReady || v.Initialized || !v.SendInitialized || !v.Operations.SendAttempt.Attemptable || !v.Operations.DraftWrite.Attemptable || v.Operations.ChatStateWrite.Attemptable || v.Ready || v.Authenticated != "unknown" || v.AppState.Reconciliation != app.AppStateReconciliationRequired || v.Observations == nil || v.Observations.Connection.LoginConfirmedAt == nil {
		t.Fatalf("metadata=%+v", v)
	}
	f.emit(&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum()}})
	withHistory := querySyncStatus(t.Context(), dir)
	if withHistory.Reason != "" || withHistory.Observations == nil || withHistory.Observations.Sync == nil {
		t.Fatalf("history status=%+v", withHistory)
	}
	ingestion := withHistory.Observations.Sync.Ingestion
	if ingestion == nil || ingestion.ExecutionID != v.OwnerRunID || ingestion.HistoryResponses != 1 || ingestion.LastHistory == nil || ingestion.LastHistory.SyncType != "RECENT" || ingestion.History.Additions != nil || withHistory.Ready || withHistory.Authenticated != "unknown" || withHistory.OwnerReady || !withHistory.Operations.SendAttempt.Attemptable {
		t.Fatalf("history did not preserve run/admission contract: %+v", withHistory)
	}
	resp, err := delegateSend(t.Context(), flags, sendDelegateRequest{Kind: draftWriteKind, Draft: &draft})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := validateDraftDelegateResult(draft, resp.DraftResult)
	if err != nil {
		t.Fatal(err)
	}
	p := entry.Revision.Payload().Data()
	send := app.OutboundSendRequest{Version: 1, RequestID: strings.Repeat("c", 32), StoreRef: dir, OwnPN: p.Account.PN, DraftID: draft.DraftID, RevisionID: draft.RevisionID, Hash: entry.Revision.Payload().Hash(), Key: "early-send-fixture"}
	f.onSend = func(_ context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		if id != outboundOwnerMessageID || msg.GetConversation() != message {
			t.Error("changed frozen payload")
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	resp, err = delegateSend(t.Context(), flags, sendDelegateRequest{Kind: outboundSendKind, Outbound: &send})
	if err != nil {
		t.Fatal(err)
	}
	result, err := validateOutboundDelegate(send, resp)
	if err != nil || result.Entry.Operation.Result != store.OutboundAccepted || result.Duplicate {
		t.Fatalf("send=%+v %v", result, err)
	}
	resp, err = delegateSend(t.Context(), flags, sendDelegateRequest{Kind: outboundSendKind, Outbound: &send})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := validateOutboundDelegate(send, resp)
	if err != nil || !duplicate.Duplicate || duplicate.Entry.Operation.ID != result.Entry.Operation.ID || f.sends.Load() != 1 {
		t.Fatalf("duplicate=%+v %v", duplicate, err)
	}
	// Admission cannot bypass SQLite's reservation/checkpoint safeguards.
	retryFixtureSQL(t, dir, `CREATE TRIGGER fixture_early_reserve_failure BEFORE INSERT ON outbound_operations BEGIN SELECT RAISE(ABORT,'fixture SQLite failure'); END`)
	newSend := send
	newSend.Key = "early-send-sqlite-failure"
	_, err = delegateSend(t.Context(), flags, sendDelegateRequest{Kind: outboundSendKind, Outbound: &newSend})
	if err == nil || f.sends.Load() != 1 {
		t.Fatalf("SQLite failure reached dispatch: %v", err)
	}
	retryFixtureSQL(t, dir, `DROP TRIGGER fixture_early_reserve_failure`)
	chat := app.ChatStateRequest{Version: 1, StoreRef: dir, Requested: localReadPN, Action: app.ChatStateArchive}
	resp, err = delegateSend(t.Context(), flags, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &chat})
	var chatFailure *app.ChatStateError
	if !errors.As(err, &chatFailure) || chatFailure.Code != "not_dispatched" || f.states.Load() != 0 {
		t.Fatalf("chat-state crossed bootstrap: %v", err)
	}
	for _, kind := range []string{"text", historyBackfillKind, draftCleanupKind, chatStateKind} {
		// Invalid payloads may be refused even earlier; none may reach execution.
		resp, err := delegateSend(t.Context(), flags, sendDelegateRequest{Kind: kind, To: localReadPN, Message: "guard"})
		if err == nil && resp.OK {
			t.Fatalf("legacy kind %s admitted early", kind)
		}
	}
	if f.opens.Load() != 1 || f.connects.Load() != 1 || f.sends.Load() != 1 {
		t.Fatal("extra owner/connection/send")
	}
	if now := querySyncStatus(t.Context(), dir); now.Ready || now.Authenticated != "unknown" || now.OwnerReady || now.OwnerRunID != v.OwnerRunID {
		t.Fatalf("historical ACK certified readiness: %+v", now)
	}
	close(metadataRelease)
}

func TestSyncStatusV2DowngradeAndOperationValidation(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	for _, mutation := range []string{"v2", "missing_operations", "positive_auth", "stopping_send", "missing_run", "uncorrelated_ingestion"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
				resp := statusFixtureReply(req, "ready")
				switch mutation {
				case "v2":
					resp.SyncStatus.Version = 2
				case "missing_operations":
					resp.SyncStatus.Status.Operations = app.SyncOperations{}
				case "positive_auth":
					resp.SyncStatus.Status.Authenticated = "true"
				case "stopping_send":
					resp.SyncStatus.Status.State = app.SyncLiveStopping
				case "missing_run":
					resp.SyncStatus.Status.OwnerRunID = ""
				case "uncorrelated_ingestion":
					resp.SyncStatus.Status.Observations = &app.DiagnosticObservations{Sync: &app.SyncObservation{ExecutionID: resp.SyncStatus.Status.OwnerRunID, Ingestion: &app.IngestionObservation{ExecutionID: strings.Repeat("f", 32)}}}
				}
				return resp, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			v := querySyncStatus(t.Context(), dir)
			want := "invalid_reply"
			if mutation == "v2" {
				want = "owner_incompatible"
			}
			if v.Reason != want || v.Operations != app.UnknownSyncOperations() || v.OwnerReady || v.Ready {
				t.Fatalf("unsafe downgrade=%+v", v)
			}
		})
	}
}

func TestSyncStatusAbsentOperationsGolden(t *testing.T) {
	dir := t.TempDir()
	v := querySyncStatus(t.Context(), dir)
	raw, err := json.Marshal(v.Operations)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"local_read":{"owner_required":false,"availability":"not_checked","reason":"owner_not_required"},"draft_write":{"attemptable":false,"reason":"owner_unavailable"},"send_attempt":{"attemptable":false,"reason":"owner_unavailable"},"chat_state_write":{"attemptable":false,"reason":"owner_unavailable"}}`
	if string(raw) != want {
		t.Fatalf("operations JSON=%s", raw)
	}
}
