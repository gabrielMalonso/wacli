package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
)

// Reuse the client boundary fixture, but generate distinct SDK IDs: these are
// independent operations on one account, not concurrent duplicates of one key.
type independentRequestsWA struct{ outboundOwnerWA }

func (f *independentRequestsWA) GenerateOutboundMessageID() (string, error) {
	return fmt.Sprintf("3EB0INDEPENDENT%02d", f.ids.Add(1)), nil
}

func (f *independentRequestsWA) Connect(ctx context.Context, opts wa.ConnectOptions) error {
	// Match the real adapter's already-connected no-op, rather than counting
	// repeated App.Connect checks as additional transport connections.
	if f.IsConnected() {
		return nil
	}
	return f.outboundOwnerWA.Connect(ctx, opts)
}

func independentRequestsFixture(t *testing.T) (string, *app.App, [2]app.OutboundSendRequest, *independentRequestsWA) {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	t.Setenv("WACLI_READONLY", "0")
	f := &independentRequestsWA{}
	// Registered before App.Close so lifetime assertions run after shutdown.
	t.Cleanup(func() {
		if f.opens.Load() != 1 || f.connects.Load() != 1 || f.closed.Load() != 1 || f.disconnected.Load() != 1 || f.removed.Load() != 1 {
			t.Error("one client lifetime", f.opens.Load(), f.connects.Load(), f.closed.Load(), f.disconnected.Load(), f.removed.Load())
		}
	})
	var dir string
	dir, a := draftOwnerFixtureOptions(t, app.Options{WAFactory: func(opts wa.Options) (app.WAClient, error) {
		f.opens.Add(1)
		if opts.StorePath != filepath.Join(dir, "session.db") || opts.KeyStateStore == nil {
			t.Error("missing factory scope/state store")
		}
		return f, nil
	}})
	// Synthetic public identity map only; no actual session or transport.
	session, err := sql.Open("sqlite3", filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Exec("INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)", "100000000002", "15550000002")
	closeErr := session.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	var requests [2]app.OutboundSendRequest
	for i := range requests {
		pn, lid := fmt.Sprintf("1555000000%d@s.whatsapp.net", i+1), fmt.Sprintf("10000000000%d@lid", i+1)
		payload, err := store.NewDraftPayload(store.DraftPayloadData{
			Account:   store.DraftIdentity{PN: f.LinkedJID(), LID: f.LinkedLID()},
			Recipient: store.DraftRecipient{JID: pn, PN: pn, LID: lid},
			Kind:      store.DraftTextKind, Text: &store.DraftText{Text: fmt.Sprintf("independent request %d", i+1)},
		})
		if err != nil {
			t.Fatal(err)
		}
		rev, err := store.NewDraftRevision(strings.Repeat(fmt.Sprint(i+1), 32), strings.Repeat(fmt.Sprint(i+3), 32), time.Now().UTC(), payload, store.DraftReviewSnapshot{RequestedRaw: pn})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.DB().WriteDraft(t.Context(), rev, ""); err != nil {
			t.Fatal(err)
		}
		requests[i] = app.OutboundSendRequest{Version: 1, RequestID: strings.Repeat(fmt.Sprint(i+5), 32), StoreRef: dir, OwnPN: f.LinkedJID(), DraftID: rev.DraftID(), RevisionID: rev.ID(), Hash: payload.Hash(), Key: fmt.Sprintf("independent-key-%d", i+1)}
	}
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		i := -1
		for n := range requests {
			if to.String() == fmt.Sprintf("10000000000%d@lid", n+1) {
				i = n
			}
		}
		if i < 0 {
			t.Error("unexpected recipient", to)
			return whatsmeow.SendResponse{}, errors.New("unexpected fixture recipient")
		}
		e, err := a.DB().Outbound().Read(ctx, "", requests[i].Key, requests[i].OwnPN, 1, "")
		if err != nil || e.Operation.MessageID != id || e.Operation.Hash != requests[i].Hash || e.Operation.Phase != store.OutboundDispatchPossible || msg.GetConversation() != fmt.Sprintf("independent request %d", i+1) {
			t.Error("dispatch binding/checkpoint", e, id, msg, err)
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Drain the owner's callbacks and close its DB before releasing LOCK.
		// The helper's later App.Close cleanup is idempotent.
		a.Close()
		_ = lk.Release()
	})
	return dir, a, requests, f
}

type independentReply struct {
	response sendDelegateResponse
	err      error
}

// net.Pipe writes synchronize request submission without polling or a sleep.
// Handler, typed envelope, shared slot/pacer and App executor are production code.
func submitIndependentRequest(t *testing.T, r app.OutboundSendRequest, timeout time.Duration, execute sendDelegateExecutor, slot chan struct{}, pacer *sendPacer) (net.Conn, <-chan independentReply, <-chan struct{}) {
	t.Helper()
	server, client := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSendDelegateConn(t.Context(), server, execute, slot, pacer)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		waitIndependentSignal(t, done)
	})
	if err := json.NewEncoder(client).Encode(sendDelegateRequest{Version: 1, Kind: outboundSendKind, Outbound: &r, TimeoutMS: durationMillis(timeout), DeadlineUnixMS: time.Now().Add(timeout).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	reply := make(chan independentReply, 1)
	go func() {
		var resp sendDelegateResponse
		err := json.NewDecoder(client).Decode(&resp)
		reply <- independentReply{resp, err}
	}()
	return client, reply, done
}

func receiveIndependentReply(t *testing.T, replies <-chan independentReply) sendDelegateResponse {
	t.Helper()
	select {
	case reply := <-replies:
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		return reply.response
	case <-time.After(5 * time.Second):
		t.Fatal("independent reply did not finish")
		return sendDelegateResponse{}
	}
}

func waitIndependentSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture synchronization did not finish")
	}
}

func assertIndependentOperation(t *testing.T, a *app.App, r app.OutboundSendRequest, want store.OutboundResult) store.OutboundOperation {
	t.Helper()
	e, err := a.DB().Outbound().Read(t.Context(), "", r.Key, r.OwnPN, 20, "")
	o := e.Operation
	if err != nil || o.Result != want || o.Key != r.Key || o.DraftID != r.DraftID || o.RevisionID != r.RevisionID || o.Hash != r.Hash || o.Account.PN != r.OwnPN {
		t.Fatal("operation correlation", e, err)
	}
	return o
}

func assertIndependentOperationMissing(t *testing.T, a *app.App, r app.OutboundSendRequest) {
	t.Helper()
	_, err := a.DB().Outbound().Read(t.Context(), "", r.Key, r.OwnPN, 1, "")
	var failure *store.OutboundError
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatal("expired request acquired an operation", err)
	}
}

func TestIndependentOutboundRequestsWaitAndKeepResults(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "first_rejected"}[rejected], func(t *testing.T) {
			_, a, requests, f := independentRequestsFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			check := f.onSend
			f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
				resp, err := check(ctx, to, id, msg)
				if msg.GetConversation() == "independent request 1" {
					close(entered)
					<-release
					if rejected {
						return resp, whatsmeow.ErrServerReturnedError
					}
				} else {
					want := store.OutboundAccepted
					if rejected {
						want = store.OutboundRejected
					}
					assertIndependentOperation(t, a, requests[0], want)
				}
				return resp, err
			}
			slot := make(chan struct{}, 1)
			slot <- struct{}{}
			var waits atomic.Int64
			execute := outboundOwnerExecutor(t, a, &waits)
			pacer := newSendPacer(sendSpacing{min: 10 * time.Second, max: 10 * time.Second})
			_, first, firstDone := submitIndependentRequest(t, requests[0], 4*time.Second, execute, slot, pacer)
			waitIndependentSignal(t, entered)
			_, second, secondDone := submitIndependentRequest(t, requests[1], 4*time.Second, execute, slot, pacer)
			if f.sends.Load() != 1 || f.ids.Load() != 1 {
				t.Fatal("second operation bypassed occupied slot")
			}
			once.Do(func() { close(release) })
			respA, respB := receiveIndependentReply(t, first), receiveIndependentReply(t, second)
			waitIndependentSignal(t, firstDone)
			waitIndependentSignal(t, secondDone)
			resultA, errA := validateOutboundDelegate(requests[0], respA)
			if rejected {
				var failure *app.OutboundSendError
				if !errors.As(errA, &failure) || failure.Code != "rejected" || failure.Request != requests[0] || failure.Result == nil || failure.Result.KnownResult != store.OutboundRejected {
					t.Fatal("first failure correlation", errA)
				}
			} else if errA != nil || resultA.KnownResult != store.OutboundAccepted {
				t.Fatal(resultA, errA)
			}
			resultB, err := validateOutboundDelegate(requests[1], respB)
			if err != nil || resultB.Duplicate || resultB.KnownResult != store.OutboundAccepted || resultB.KnownACK == nil {
				t.Fatal("second inherited first result", resultB, err)
			}
			wantA := store.OutboundAccepted
			if rejected {
				wantA = store.OutboundRejected
			}
			oA := assertIndependentOperation(t, a, requests[0], wantA)
			oB := assertIndependentOperation(t, a, requests[1], store.OutboundAccepted)
			if oA.ID == oB.ID || oA.MessageID == oB.MessageID || oA.Recipient == oB.Recipient || oA.Hash == oB.Hash || f.sends.Load() != 2 || f.ids.Load() != 2 || waits.Load() != 2 {
				t.Fatal("independent IDs/recipients/pacing", oA, oB, f.sends.Load(), waits.Load())
			}
			for _, mismatch := range []struct {
				request  app.OutboundSendRequest
				response sendDelegateResponse
			}{{requests[0], respB}, {requests[1], respA}} {
				_, err := validateOutboundDelegate(mismatch.request, mismatch.response)
				var failure *app.OutboundSendError
				if !errors.As(err, &failure) || failure.Code != "outcome_uncertain" || failure.Request != mismatch.request {
					t.Fatal("crossed response accepted", err)
				}
			}
		})
	}
}

func TestIndependentOutboundLostReplyRetainsSlotAndDoesNotReplay(t *testing.T) {
	_, a, requests, f := independentRequestsFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	dispatchedContext := make(chan context.Context, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	check := f.onSend
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		resp, err := check(ctx, to, id, msg)
		if msg.GetConversation() == "independent request 1" {
			dispatchedContext <- ctx
			close(entered)
			<-release // SDK can outlive the request deadline and closed socket.
			return whatsmeow.SendResponse{}, context.DeadlineExceeded
		}
		assertIndependentOperation(t, a, requests[0], store.OutboundUncertain)
		return resp, err
	}
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	var waits atomic.Int64
	execute := outboundOwnerExecutor(t, a, &waits)
	pacer := newSendPacer(sendSpacing{min: 10 * time.Second, max: 10 * time.Second})
	clientA, first, firstDone := submitIndependentRequest(t, requests[0], time.Second, execute, slot, pacer)
	waitIndependentSignal(t, entered)
	ctxA := <-dispatchedContext
	waitIndependentSignal(t, ctxA.Done())
	_ = clientA.Close()
	if reply := <-first; reply.err == nil {
		t.Fatal("closed client received success")
	}
	// B has its own longer budget; A's expired context must not cancel it.
	_, second, secondDone := submitIndependentRequest(t, requests[1], 4*time.Second, execute, slot, pacer)
	// A distinct invocation for B expires while A still owns the adapter/slot.
	_, expired, expiredDone := submitIndependentRequest(t, requests[1], 150*time.Millisecond, execute, slot, pacer)
	_, err := validateOutboundDelegate(requests[1], receiveIndependentReply(t, expired))
	var failure *app.OutboundSendError
	if !errors.As(err, &failure) || failure.Code != "not_dispatched" || failure.Request != requests[1] || f.sends.Load() != 1 || f.ids.Load() != 1 {
		t.Fatal("client close released owner or crossed failure correlation", err)
	}
	waitIndependentSignal(t, expiredDone)
	once.Do(func() { close(release) })
	respB := receiveIndependentReply(t, second)
	waitIndependentSignal(t, firstDone)
	waitIndependentSignal(t, secondDone)
	resultB, err := validateOutboundDelegate(requests[1], respB)
	if err != nil || resultB.KnownResult != store.OutboundAccepted {
		t.Fatal("A timeout contaminated B", resultB, err)
	}
	oA := assertIndependentOperation(t, a, requests[0], store.OutboundUncertain)
	oB := assertIndependentOperation(t, a, requests[1], store.OutboundAccepted)
	if oA.ID == oB.ID || oA.MessageID == oB.MessageID || oA.FinalizedAt == nil || oB.FinalizedAt == nil {
		t.Fatal(oA, oB)
	}
	stdout, stderr, err := runDraftBinary(t, "", []string{"--agent", "--store", requests[0].StoreRef, "outbound", "show", "--key", requests[0].Key, "--account-jid", requests[0].OwnPN}, false)
	if err != nil || stderr != "" {
		t.Fatal(stdout, stderr, err)
	}
	var query outboundEntryDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &query); err != nil || query.Operation.ID != oA.ID || query.Operation.MessageID != oA.MessageID || query.Operation.Hash != requests[0].Hash || query.Operation.AttemptResult != store.OutboundUncertain {
		t.Fatal("uncertain query returned B", stdout, err)
	}
	// Reusing A's exact binding returns retained uncertainty, not another call.
	_, duplicate, duplicateDone := submitIndependentRequest(t, requests[0], time.Second, execute, slot, pacer)
	old, err := validateOutboundDelegate(requests[0], receiveIndependentReply(t, duplicate))
	waitIndependentSignal(t, duplicateDone)
	if err != nil || !old.Duplicate || old.KnownResult != store.OutboundUncertain || old.Entry.Operation.ID != oA.ID || old.Entry.Operation.MessageID != oA.MessageID || f.sends.Load() != 2 || f.ids.Load() != 2 || waits.Load() != 2 {
		t.Fatal("application replay after uncertainty", old, err, f.sends.Load(), waits.Load())
	}
}

func TestIndependentOutboundCLIReadsAndQueueExpiry(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY") // Empty still runs two real CLI helper processes.
	if binary != "" && !filepath.IsAbs(binary) {
		t.Fatal("binary path must be absolute")
	}
	dir, a, requests, f := independentRequestsFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	check := f.onSend
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		resp, err := check(ctx, to, id, msg)
		close(entered)
		<-release
		return resp, err
	}
	stop, err := startSendDelegateServer(t.Context(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop() }()
	defer once.Do(func() { close(release) })
	type cliReply struct {
		kind           string
		stdout, stderr string
		err            error
	}
	first := make(chan cliReply, 1)
	go func() {
		stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--timeout", "4s"}, outboundSendArgs(requests[0])...), false)
		first <- cliReply{stdout: stdout, stderr: stderr, err: err}
	}()
	waitIndependentSignal(t, entered)
	sessionBefore, err := os.ReadFile(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	// These local reads finish while the real App/adapter is still blocked.
	reads := make(chan cliReply, 3)
	for _, args := range [][]string{
		{"outbound", "show", "--key", requests[0].Key, "--account-jid", requests[0].OwnPN},
		{"draft", "show", requests[1].DraftID, "--revision", requests[1].RevisionID},
		{"messages", "list", "--chat", localReadPN},
	} {
		go func() {
			stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--agent", "--store", dir}, args...), false)
			reads <- cliReply{kind: args[0], stdout: stdout, stderr: stderr, err: err}
		}()
	}
	for range 3 {
		read := <-reads
		if read.err != nil || read.stderr != "" || decodeAgentTest(t, read.stdout).Meta.Source != "local" {
			t.Fatal("local read waited/opened live transport", read)
		}
		if read.kind == "outbound" {
			var dto outboundEntryDTO
			if err := json.Unmarshal(decodeAgentTest(t, read.stdout).Data, &dto); err != nil || dto.Operation.Key != requests[0].Key || dto.Operation.Hash != requests[0].Hash || dto.Operation.AttemptResult != store.OutboundPending {
				t.Fatal("pending query correlation", read, err)
			}
		}
	}
	stdout, stderr, err := runDraftBinary(t, binary, append([]string{"--timeout", "750ms"}, outboundSendArgs(requests[1])...), false)
	if err == nil || stdout != "" {
		t.Fatal("queued B succeeded", stdout, stderr, err)
	}
	failure := decodeAgentTest(t, stderr).Error
	if failure.Code != "not_dispatched" || failure.Outbound == nil || failure.Outbound.Key != requests[1].Key || failure.Outbound.DraftID != requests[1].DraftID || failure.Outbound.Hash != requests[1].Hash || failure.Outbound.OperationID != "" || failure.Outbound.MessageID != "" {
		t.Fatal("B inherited A's IDs/error", stderr)
	}
	assertIndependentOperationMissing(t, a, requests[1])
	once.Do(func() { close(release) })
	result := <-first
	if result.err != nil || result.stderr != "" {
		t.Fatal(result)
	}
	var dto outboundSendDTO
	if err := json.Unmarshal(decodeAgentTest(t, result.stdout).Data, &dto); err != nil || dto.Operation.Key != requests[0].Key || dto.Operation.Hash != requests[0].Hash || dto.KnownResult != store.OutboundAccepted {
		t.Fatal("A result correlation", result, err)
	}
	// Shutdown joins every accepted connection: no sleep to look for late B.
	stop()
	stop = func() {}
	assertIndependentOperationMissing(t, a, requests[1])
	if f.sends.Load() != 1 || f.ids.Load() != 1 {
		t.Fatal("expired B executed after A", f.sends.Load(), f.ids.Load())
	}
	sessionAfter, err := os.ReadFile(filepath.Join(dir, "session.db"))
	if err != nil || string(sessionAfter) != string(sessionBefore) {
		t.Fatal("CLI opened/upgraded synthetic session", err)
	}
}
