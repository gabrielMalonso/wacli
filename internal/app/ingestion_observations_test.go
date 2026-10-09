package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func ingestionTestRun(t *testing.T, a *App) (context.Context, *diagnosticRun) {
	t.Helper()
	recovery := &appStateRecoveryRun{}
	a.waMu.Lock()
	a.appStateRecoveryOnClose = recovery
	a.waMu.Unlock()
	r := newDiagnosticRun(a, SyncModeFollow, recovery)
	a.waMu.Lock()
	recovery.diagnostic = r
	a.waMu.Unlock()
	return context.WithValue(t.Context(), appStateRecoveryRunKey{}, recovery), r
}

func ingestionLiveMessage() *events.Message {
	chat := types.NewJID("15550000002", types.DefaultUserServer)
	return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}, ID: "fixture-live", Timestamp: time.Unix(1700000000, 0)}, Message: &waE2E.Message{Conversation: proto.String("private fixture body")}}
}

func TestIngestionLiveWriteFailureSanitized(t *testing.T) {
	for _, eventMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "events"}[eventMode], func(t *testing.T) {
			a := newTestApp(t)
			a.wa = newFakeWA()
			ctx, r := ingestionTestRun(t, a)
			evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_ingestion BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'private raw SQL https://signed.example/key'); END`)
			var stored atomic.Int64
			webhooks, downloads := 0, 0
			var output string
			stdout := os.Stdout
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			os.Stdout = writer
			defer func() { os.Stdout = stdout }()
			output = captureStderr(t, func() {
				a.opts.Events = out.NewEventWriter(os.Stderr, eventMode)
				a.handleLiveSyncMessage(ctx, SyncOptions{DownloadMedia: true}, ingestionLiveMessage(), &stored, func(string, string) { downloads++ }, func(wa.ParsedMessage) { webhooks++ })
			})
			writer.Close()
			stdoutBytes, err := io.ReadAll(reader)
			if err != nil || len(stdoutBytes) != 0 {
				t.Fatalf("diagnostics wrote stdout: %q, %v", stdoutBytes, err)
			}
			if !strings.Contains(output, "ingestion") || strings.Contains(output, "private") || strings.Contains(output, "signed.example") || strings.Contains(output, "fixture-live") || strings.Contains(output, "15550000002") {
				t.Fatalf("unsanitized/missing diagnostic: %s", output)
			}
			if eventMode {
				for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
					if !json.Valid([]byte(line)) {
						t.Fatalf("invalid NDJSON: %s", line)
					}
				}
			}
			v := a.IngestionSnapshot()
			if !v.Degraded || v.LiveFailures != 1 || v.LiveProcessed != 0 || v.LastFailure.Operation != "live" || v.LastFailure.Reason != "persistence_failed" || stored.Load() != 0 || webhooks != 0 || downloads != 0 {
				t.Fatalf("incorrect failure observation: %+v", v)
			}
			if _, err := a.db.GetMessage(ingestionLiveMessage().Info.Chat.String(), "fixture-live"); err == nil {
				t.Fatal("failed write appeared persisted")
			}
			v.LastFailure.Reason = "mutated"
			if a.IngestionSnapshot().LastFailure.Reason != "persistence_failed" {
				t.Fatal("snapshot exposed mutable state")
			}
			r.finishSync(nil, nil, 0)
			r.close()
			saved := ReadDiagnosticObservations(a.db)
			if saved.Error != nil || !saved.Sync.Ingestion.Degraded || saved.Sync.Ingestion.LiveFailures != 1 {
				t.Fatalf("checkpoint lost failure: %+v", saved)
			}
		})
	}
}

func TestIngestionHistorySkipsReplayAndOwnUnicode(t *testing.T) {
	a := newTestApp(t)
	a.wa = newHistorySenderWA()
	var log bytes.Buffer
	a.opts.Events = out.NewEventWriter(&log, true)
	ctx, r := ingestionTestRun(t, a)
	own := historySenderMessage(historyPeerLID, true)
	own.Message.Conversation = proto.String("Olá 🦀 漢字\ntexto próprio")
	noID := proto.Clone(own).(*waWeb.WebMessageInfo)
	noID.Key.ID = nil
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{
		{ID: proto.String(historyPeerLID), Messages: []*waHistorySync.HistorySyncMsg{nil, {}, {Message: noID}, {Message: own}}},
		{Messages: []*waHistorySync.HistorySyncMsg{{Message: own}}}, nil,
	}}}
	var stored, last atomic.Int64
	a.handleHistorySync(ctx, SyncOptions{}, hs, &stored, &last, func(string, string) {})
	v := a.IngestionSnapshot()
	if !v.Degraded || v.HistoryResponses != 1 || v.History.Received != 5 || v.History.Valid != 1 || v.History.Content != 1 || v.History.Processed != 1 || v.History.Skipped != 4 || v.History.Failed != 0 || v.SkipReasons.MissingInfo != 2 || v.SkipReasons.MissingID != 1 || v.SkipReasons.MissingChat != 1 || v.LastFailure != nil {
		t.Fatalf("wrong history observation: %+v", v)
	}
	m, err := a.db.GetMessage(historyPeerPN, own.GetKey().GetID())
	if err != nil || m.Text != own.Message.GetConversation() || !m.FromMe || m.SenderJID != historyOwnPN {
		t.Fatalf("own LID/Unicode failed: %+v, %v", m, err)
	}
	pn := proto.Clone(own).(*waWeb.WebMessageInfo)
	pn.Key.RemoteJID = proto.String(historyPeerPN)
	replay := &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(historyPeerPN), Messages: []*waHistorySync.HistorySyncMsg{{Message: pn}}}}}}
	a.handleHistorySync(ctx, SyncOptions{}, replay, &stored, &last, func(string, string) {})
	if count, err := a.db.CountMessages(); err != nil || count != 1 {
		t.Fatalf("replay fabricated growth: %d, %v", count, err)
	}
	v = a.IngestionSnapshot()
	if stored.Load() != 2 || v.History.Processed != 2 || v.LastHistory.Processed != 1 || v.History.Additions != nil || v.History.Replays != nil || v.History.PurgeSuppressed != nil {
		t.Fatalf("replay was misreported as growth: %+v", v)
	}
	v.LastHistory.Processed = 100
	if a.IngestionSnapshot().LastHistory.Processed != 1 {
		t.Fatal("mutable history summary escaped")
	}
	// A protected tombstone still counts as successful processing, not a new row.
	evidenceFixtureSQL(t, a, `INSERT INTO message_payload_purges(chat_jid,msg_id,purged_at,deleted_at,deletion_reason) VALUES ('15550000002@s.whatsapp.net','synthetic-history',1700000001,1700000001,'revoked')`)
	evidenceFixtureSQL(t, a, `DELETE FROM messages WHERE msg_id='synthetic-history'`)
	a.handleHistorySync(ctx, SyncOptions{}, replay, &stored, &last, func(string, string) {})
	if count, err := a.db.CountMessages(); err != nil || count != 0 {
		t.Fatalf("purged payload restored: %d, %v", count, err)
	}
	if a.IngestionSnapshot().History.Processed != 3 || stored.Load() != 3 {
		t.Fatal("purge changed processing contract")
	}
	metadataOnly := proto.Clone(pn).(*waWeb.WebMessageInfo)
	metadataOnly.Key.ID = proto.String("metadata-only")
	metadataOnly.Message = nil
	replay.Data.Conversations[0].Messages = []*waHistorySync.HistorySyncMsg{{Message: metadataOnly}}
	a.handleHistorySync(ctx, SyncOptions{}, replay, &stored, &last, func(string, string) {})
	if h := a.IngestionSnapshot().LastHistory; h.Valid != 1 || h.Content != 0 || h.Processed != 1 {
		t.Fatalf("structural validity invented content: %+v", h)
	}
	r.close()
	if saved := ReadDiagnosticObservations(a.db); saved.Error != nil || saved.Sync.Ingestion.History.Processed != 4 {
		t.Fatalf("invalid saved ingestion: %+v", saved)
	}
	if strings.Contains(log.String(), own.Message.GetConversation()) {
		t.Fatal("payload leaked in summary")
	}
	var sawSummary bool
	for _, line := range bytes.Split(bytes.TrimSpace(log.Bytes()), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatalf("invalid NDJSON: %s", line)
		}
		if bytes.Contains(line, []byte(`"event":"history_ingestion"`)) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("missing response summary")
	}
}

func TestIngestionOnDemandWriteErrorAndPartialSummary(t *testing.T) {
	a := newTestApp(t)
	a.wa = newHistorySenderWA()
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	ctx, _ := ingestionTestRun(t, a)
	evidenceFixtureSQL(t, a, `CREATE TRIGGER fail_history BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'private history failure'); END`)
	history := historySyncWithTextMessages(types.NewJID("15550000002", types.DefaultUserServer), time.Unix(1700000000, 0), "first", "remaining")
	history.Data.SyncType = waHistorySync.HistorySync_ON_DEMAND.Enum()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var propagated error
	var stored, last atomic.Int64
	a.handleHistorySync(ctx, SyncOptions{historyStoreError: func(chat types.JID, err error) { propagated = err; cancel() }}, history, &stored, &last, func(string, string) {})
	v := a.IngestionSnapshot()
	if propagated == nil || !strings.Contains(propagated.Error(), "private history failure") || v.History.Failed != 1 || v.History.Processed != 0 || v.History.Unprocessed != 1 || v.LastFailure.Reason != "persistence_failed" || stored.Load() != 0 {
		t.Fatalf("lost existing error/partial summary: %+v, %v", v, propagated)
	}
	v.LastHistory.LastFailure.Reason = "mutated"
	if a.IngestionSnapshot().LastHistory.LastFailure.Reason != "persistence_failed" {
		t.Fatal("mutable response failure escaped")
	}
}

func TestIngestionRunResetAndConcurrentLifecycle(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	if a.IngestionSnapshot() != nil {
		t.Fatal("invented run before Sync")
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = a.IngestionSnapshot()
			}
		}
	})
	defer func() { close(done); wg.Wait() }()
	var oldCtx context.Context
	var first SyncResult
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(runCtx context.Context) error {
			if i == 0 {
				oldCtx = runCtx
				a.observeLiveIngestion(runCtx, errors.New("private"))
			}
			cancel()
			return nil
		}})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = result
		} else if v := a.IngestionSnapshot(); v.Degraded || v.LiveReceived != 0 || v.ExecutionID == first.recovery.diagnostic.sync.ExecutionID {
			t.Fatalf("run did not reset: %+v", v)
		}
	}
	// A copied old callback retains its own run, not the currently published one.
	a.observeLiveIngestion(oldCtx, errors.New("late private failure"))
	if a.IngestionSnapshot().Degraded {
		t.Fatal("old callback polluted current run")
	}
	a.Close()
	old := first.ObservationsSnapshot().Sync.Ingestion
	if old.LiveFailures != 2 {
		t.Fatal("old admitted callback lost its own run")
	}
	before := a.IngestionSnapshot()
	a.observeLiveIngestion(context.WithValue(t.Context(), appStateRecoveryRunKey{}, a.appStateRecoveryOnClose), errors.New("after close"))
	if a.IngestionSnapshot().LiveFailures != before.LiveFailures {
		t.Fatal("closed run changed")
	}
}

func TestIngestionRetainedSnapshotCompatibility(t *testing.T) {
	a := newTestApp(t)
	a.opts.Events = out.NewEventWriter(io.Discard, true)
	ctx, r := ingestionTestRun(t, a)
	a.observeLiveIngestion(ctx, errors.New("private fixture"))
	r.close()
	evidenceFixtureSQL(t, a, `UPDATE diagnostic_snapshots SET payload=json_remove(payload,'$.ingestion') WHERE slot='sync'`)
	if saved := ReadDiagnosticObservations(a.db); saved.Error != nil || saved.Sync == nil || saved.Sync.Ingestion != nil {
		t.Fatalf("older snapshot invented health or became unreadable: %+v", saved)
	}
	ctx, r = ingestionTestRun(t, a)
	a.observeLiveIngestion(ctx, errors.New("private fixture"))
	r.close()
	evidenceFixtureSQL(t, a, `UPDATE diagnostic_snapshots SET payload=json_set(payload,'$.ingestion.last_failure.reason','private unsafe cause') WHERE slot='sync'`)
	saved := ReadDiagnosticObservations(a.db)
	raw, _ := json.Marshal(saved)
	if saved.Error == nil || saved.Sync != nil || bytes.Contains(raw, []byte("private")) {
		t.Fatalf("invalid reason escaped sanitizer: %s", raw)
	}
}
