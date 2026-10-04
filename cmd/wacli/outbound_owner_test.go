package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Only the client boundary is fake: App, archive, login observation, executor,
// UNIX server, slot and pacer are production code. No SDK transport is opened.
type outboundOwnerWA struct {
	app.WAClient                                               // Unexpected legacy/discovery calls fail the fixture.
	mu                                                         sync.Mutex
	handler                                                    func(any)
	connected                                                  bool
	opens, connects, sends, ids, removed, disconnected, closed atomic.Int64
	onSend                                                     func(context.Context, types.JID, string, *waE2E.Message) (whatsmeow.SendResponse, error)
}

const outboundOwnerMessageID = "3EB0OWNERFIXTURE"

func (f *outboundOwnerWA) LinkedJID() string { return "15550000009@s.whatsapp.net" }
func (f *outboundOwnerWA) LinkedLID() string { return "100000000009@lid" }
func (f *outboundOwnerWA) AddEventHandler(h func(any)) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	return 1
}
func (f *outboundOwnerWA) RemoveEventHandler(id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = nil
	f.removed.Add(1)
}
func (f *outboundOwnerWA) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}
func (f *outboundOwnerWA) Connect(_ context.Context, opts wa.ConnectOptions) error {
	if opts.AllowQR || opts.OnQRCode != nil {
		return errors.New("unexpected auth")
	}
	f.connects.Add(1)
	f.mu.Lock()
	f.connected = true
	h := f.handler
	f.mu.Unlock()
	h(&events.Connected{})
	return nil
}
func (f *outboundOwnerWA) Disconnect() {
	f.disconnected.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
}
func (f *outboundOwnerWA) Close() { f.closed.Add(1) }
func (f *outboundOwnerWA) GenerateOutboundMessageID() (string, error) {
	f.ids.Add(1)
	return outboundOwnerMessageID, nil
}
func (f *outboundOwnerWA) SendOutbound(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	f.sends.Add(1)
	return f.onSend(ctx, to, id, msg)
}

func outboundOwnerFixture(t *testing.T) (string, *app.App, app.OutboundSendRequest, *outboundOwnerWA) {
	t.Helper()
	f := &outboundOwnerWA{}
	t.Cleanup(func() {
		if f.opens.Load() != 0 && (f.closed.Load() != 1 || f.disconnected.Load() != 1 || f.removed.Load() != 1) {
			t.Error("client lifetime", f.closed.Load(), f.disconnected.Load(), f.removed.Load())
		}
	})
	dir, a := draftOwnerFixtureOptions(t, app.Options{WAFactory: func(opts wa.Options) (app.WAClient, error) {
		f.opens.Add(1)
		if opts.StorePath == "" || opts.KeyStateStore == nil {
			t.Error("missing factory options")
		}
		return f, nil
	}})
	p, err := store.NewDraftPayload(store.DraftPayloadData{
		Account:   store.DraftIdentity{PN: f.LinkedJID(), LID: f.LinkedLID()},
		Recipient: store.DraftRecipient{JID: localReadPN, PN: localReadPN, LID: localReadLID},
		Kind:      store.DraftTextKind, Text: &store.DraftText{Text: "synthetic socket payload\\n\n👋"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := store.NewDraftRevision(strings.Repeat("1", 32), strings.Repeat("2", 32), time.Now().UTC(), p, store.DraftReviewSnapshot{RequestedRaw: localReadPN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), rev, ""); err != nil {
		t.Fatal(err)
	}
	r := app.OutboundSendRequest{Version: 1, RequestID: strings.Repeat("e", 32), StoreRef: dir, OwnPN: p.Data().Account.PN, DraftID: rev.DraftID(), RevisionID: rev.ID(), Hash: p.Hash(), Key: "socket-key"}
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		// The exact ID and dispatch checkpoint must already be durable BEFORE
		// the fake SDK boundary, rather than inferred from the final response.
		e, err := a.DB().Outbound().Read(ctx, "", r.Key, r.OwnPN, 20, "")
		if err != nil || e.Operation.MessageID != id || id != outboundOwnerMessageID || e.Operation.Phase != store.OutboundDispatchPossible || to.String() != localReadLID || !proto.Equal(msg, &waE2E.Message{Conversation: proto.String(p.Data().Text.Text)}) {
			t.Error("dispatch identity/payload/checkpoint", e, msg, to, id, err)
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	return dir, a, r, f
}

// Install the existing pacer's test sleeper while the production slot is held.
// This proves new admission waits once and duplicates bypass that wait without
// making the suite spend ten seconds on every request.
func outboundOwnerExecutor(t *testing.T, a *app.App, waits *atomic.Int64) sendDelegateExecutor {
	t.Helper()
	configured := false
	return func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if !configured {
			p := ctx.Value(outboundPacerKey{}).(*sendPacer)
			p.last, p.hasLast = p.now(), true
			p.sleep = func(_ context.Context, d time.Duration) {
				if d <= 0 {
					t.Error("nonpositive pacing wait", d)
				}
				waits.Add(1)
			}
			configured = true
		}
		return executeDelegatedSend(ctx, a, req)
	}
}

func readOwnerOperation(t *testing.T, a *app.App, r app.OutboundSendRequest, want store.OutboundResult) store.OutboundEntry {
	t.Helper()
	e, err := a.DB().Outbound().Read(t.Context(), "", r.Key, r.OwnPN, 20, "")
	if err != nil || e.Operation.Result != want || e.Operation.MessageID != outboundOwnerMessageID {
		t.Fatal(e, err)
	}
	return e
}

func exerciseOutboundSocket(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a, r, f := outboundOwnerFixture(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var waits atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{min: 10 * time.Second, max: 10 * time.Second}, outboundOwnerExecutor(t, a, &waits))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	stdout, stderr, err := runDraftBinary(t, binary, outboundSendArgs(r), false)
	if err != nil || stderr != "" {
		t.Fatal(stdout, stderr, err)
	}
	var dto outboundSendDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &dto); err != nil {
		t.Fatal(err)
	}
	e := readOwnerOperation(t, a, r, store.OutboundAccepted)
	if dto.Duplicate || dto.Operation.ID != e.Operation.ID || dto.KnownResult != store.OutboundAccepted || dto.KnownACK == nil || len(e.Observations.Items) != 1 || e.Observations.Items[0].Fact != store.OutboundAck || e.Observations.Items[0].Source != store.OutboundSendResponse || e.Evidence.Accepted != "observed" || e.Evidence.Delivered != "unknown" || e.Evidence.Read != "unknown" {
		t.Fatal(stdout, e)
	}
	ack := e.Observations.Items[0]
	if dto.Operation.MessageID != outboundOwnerMessageID || dto.KnownACK.Fact != ack.Fact || dto.KnownACK.ChatJID != ack.ChatJID || dto.KnownACK.ActorJID != f.LinkedLID() || ack.ActorJID != f.LinkedLID() || dto.KnownACK.EventAt == nil || ack.EventAt == nil || !dto.KnownACK.EventAt.Equal(*ack.EventAt) {
		t.Fatal("ACK/ID roundtrip", dto, ack)
	}
	// Owner-side duplicate exercises the executor/slot and bypasses pacing.
	resp, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
	if err != nil {
		t.Fatal(err)
	}
	again, err := validateOutboundDelegate(r, resp)
	if err != nil || !again.Duplicate || again.Entry.Operation.ID != e.Operation.ID {
		t.Fatal(again, err)
	}
	// The second CLI invocation takes the pure local retained-operation path.
	stdout, stderr, err = runDraftBinary(t, binary, outboundSendArgs(r), false)
	if err != nil || stderr != "" {
		t.Fatal(stdout, stderr, err)
	}
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &dto); err != nil || !dto.Duplicate || dto.Operation.ID != e.Operation.ID {
		t.Fatal(stdout, err)
	}
	if f.sends.Load() != 1 || f.ids.Load() != 1 || f.opens.Load() != 1 || f.connects.Load() != 1 || waits.Load() != 1 {
		t.Fatal("extra invocation/pacing", f.sends.Load(), f.ids.Load(), f.opens.Load(), f.connects.Load(), waits.Load())
	}
}

func exerciseOutboundLostOwnerReply(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	dir, a, r, f := outboundOwnerFixture(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var releaseOnce sync.Once
	checkDispatch := f.onSend
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		_, _ = checkDispatch(ctx, to, id, msg)
		close(started)
		// Model a synchronous SDK call still running after client timeout. No
		// app send goroutine or real SDK handshake is involved.
		<-release
		return whatsmeow.SendResponse{}, context.DeadlineExceeded
	}
	var waits atomic.Int64
	execute := outboundOwnerExecutor(t, a, &waits)
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{min: 10 * time.Second, max: 10 * time.Second}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		resp, err := execute(ctx, req)
		finished <- struct{}{}
		return resp, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer releaseOnce.Do(func() { close(release) })
	// The CLI loses its reply while the owner remains inside fake SendOutbound.
	args := append([]string{"--timeout", "1s"}, outboundSendArgs(r)...)
	stdout, stderr, err := runDraftBinary(t, binary, args, false)
	if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "outcome_uncertain" {
		t.Fatal(stdout, stderr, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("CLI never crossed fake dispatch")
	}
	e := readOwnerOperation(t, a, r, store.OutboundPending)
	if e.Operation.DispatchPossibleAt == nil {
		t.Fatal("no dispatch frontier", e)
	}
	// The owner must retain its slot until SDK return/finalization, even after
	// the client socket closes. A direct duplicate request cannot jump the slot.
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 150 * time.Millisecond}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
	var failure *app.OutboundSendError
	if !errors.As(err, &failure) || failure.Code != "not_dispatched" {
		t.Fatal("owner slot released early", err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not finalize")
	}
	retained := readOwnerOperation(t, a, r, store.OutboundUncertain)
	if retained.Operation.ID != e.Operation.ID || len(retained.Observations.Items) != 0 {
		t.Fatal(retained)
	}
	stdout, stderr, err = runDraftBinary(t, binary, []string{"--agent", "--store", dir, "outbound", "show", "--key", r.Key, "--account-jid", r.OwnPN}, false)
	if err != nil || stderr != "" {
		t.Fatal(stdout, stderr, err)
	}
	var query outboundEntryDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &query); err != nil || query.Operation.ID != e.Operation.ID || query.Operation.AttemptResult != store.OutboundUncertain {
		t.Fatal(stdout, err)
	}
	stdout, stderr, err = runDraftBinary(t, binary, outboundSendArgs(r), false)
	if err != nil || stderr != "" {
		t.Fatal(stdout, stderr, err)
	}
	var dto outboundSendDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &dto); err != nil || !dto.Duplicate || dto.Operation.ID != e.Operation.ID || dto.KnownResult != store.OutboundUncertain {
		t.Fatal(stdout, err)
	}
	resp, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
	if err != nil {
		t.Fatal(err)
	}
	again, err := validateOutboundDelegate(r, resp)
	if err != nil || !again.Duplicate || again.Entry.Operation.ID != e.Operation.ID || again.KnownResult != store.OutboundUncertain {
		t.Fatal(again, err)
	}
	if f.sends.Load() != 1 || f.ids.Load() != 1 || f.opens.Load() != 1 || f.connects.Load() != 1 || waits.Load() != 1 {
		t.Fatal("fallback/replay", f.sends.Load(), f.ids.Load(), f.opens.Load(), f.connects.Load(), waits.Load())
	}
}

func TestOutboundRealSocketStandaloneFixture(t *testing.T) { exerciseOutboundSocket(t, "") }
func TestOutboundRealOwnerLostReplyFixture(t *testing.T)   { exerciseOutboundLostOwnerReply(t, "") }
func TestOutboundProductionBinarySendFixture(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_OUTBOUND_E2E_BINARY to freshly built binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("binary path must be absolute")
	}
	t.Run("accepted", func(t *testing.T) { exerciseOutboundSocket(t, binary) })
	t.Run("lost_reply", func(t *testing.T) { exerciseOutboundLostOwnerReply(t, binary) })
}
