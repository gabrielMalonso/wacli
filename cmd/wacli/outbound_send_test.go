package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func outboundRequestFixture(dir string, o store.OutboundOperation) app.OutboundSendRequest {
	return app.OutboundSendRequest{Version: 1, RequestID: strings.Repeat("f", 32), StoreRef: dir, OwnPN: o.Account.PN, DraftID: o.DraftID, RevisionID: o.RevisionID, Hash: o.Hash, Key: o.Key}
}
func outboundSendArgs(r app.OutboundSendRequest) []string {
	return []string{"--agent", "--store", r.StoreRef, "outbound", "send", r.DraftID, "--revision", r.RevisionID, "--expect-hash", r.Hash, "--key", r.Key}
}

func TestOutboundSendPreflightZeroStoreEffects(t *testing.T) {
	for _, args := range [][]string{{"--read-only", "outbound", "send", strings.Repeat("a", 32)}, {"outbound", "send", "bad"}, {"outbound", "send", strings.Repeat("a", 32), "--revision", strings.Repeat("b", 32), "--expect-hash", strings.Repeat("c", 64), "--key", "bad key"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "missing")
			stdout, stderr, err := runAgentTest(t, append([]string{"--agent", "--store", dir}, args...)...)
			if err == nil || stdout != "" {
				t.Fatal(stdout, stderr, err)
			}
			e := decodeAgentTest(t, stderr)
			if e.Meta.Source != "live" {
				t.Fatal("action error source", stderr)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("preflight created store", err)
			}
		})
	}
	t.Setenv("WACLI_READONLY", "1")
	dir := filepath.Join(t.TempDir(), "missing")
	_, stderr, err := runAgentTest(t, "--agent", "--store", dir, "outbound", "send", strings.Repeat("a", 32))
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatal(stderr, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("env guard store effect", err)
	}
}

func TestOutboundSendDuplicateIsPureLocalAndLiveAction(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir := shortPresenceDelegateStoreDir(t)
	db, o := outboundCLISeed(t, dir, 90)
	r := outboundRequestFixture(dir, o)
	if _, err := db.DiscardDraft(t.Context(), o.DraftID, o.RevisionID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.db"), []byte("never open"), 0600); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		return sendDelegateResponse{}, errors.New("must not delegate duplicate")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	before, err := os.ReadFile(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAgentTest(t, outboundSendArgs(r)...)
	if err != nil || stderr != "" || calls.Load() != 0 {
		t.Fatal(stdout, stderr, err)
	}
	e := decodeAgentTest(t, stdout)
	var dto outboundSendDTO
	if err := json.Unmarshal(e.Data, &dto); err != nil {
		t.Fatal(err)
	}
	if e.Meta.Source != "live" || !dto.Duplicate || dto.Operation.ID != o.ID || dto.KnownResult != store.OutboundPending {
		t.Fatal(stdout)
	}
	after, err := os.ReadFile(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("duplicate modified store")
	}
}

type outboundBrokenWriter struct{}

func (outboundBrokenWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
func TestOutboundActionOutputFailureRetainsQueryCorrelation(t *testing.T) {
	dir := t.TempDir()
	db, o := outboundCLISeed(t, dir, 91)
	r := outboundRequestFixture(dir, o)
	e, err := db.Outbound().Read(t.Context(), o.ID, "", "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []bool{false, true} {
		flags := &rootFlags{agent: agent, agentCapability: agentOutboundSend, agentAccount: out.AgentAccount{StoreRef: &dir}}
		result := app.OutboundSendResult{Entry: e, Duplicate: true, Persistence: "confirmed", KnownResult: o.Result}
		err := writeOutboundSendResult(outboundBrokenWriter{}, flags, r, result)
		var failure *out.AgentError
		if !errors.As(err, &failure) || failure.Code != "output_unconfirmed" || failure.Outbound.OperationID != o.ID || failure.Outbound.MessageID != o.MessageID || !strings.Contains(failure.Error(), o.ID) {
			t.Fatal(err)
		}
	}
}

func TestOutboundIPCCorrelationFailsClosed(t *testing.T) {
	dir := t.TempDir()
	db, o := outboundCLISeed(t, dir, 92)
	r := outboundRequestFixture(dir, o)
	e, _ := db.Outbound().Read(t.Context(), o.ID, "", "", 1, "")
	valid := func() sendDelegateResponse {
		return sendDelegateResponse{OK: true, Outbound: &outboundDelegateResult{Capability: outboundSendKind, RequestID: r.RequestID, RequestHash: outboundRequestHash(r), Result: &app.OutboundSendResult{Entry: e, Duplicate: true, Persistence: "confirmed", KnownResult: o.Result}}}
	}
	if _, err := validateOutboundDelegate(r, valid()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*sendDelegateResponse){func(s *sendDelegateResponse) { s.Outbound = nil }, func(s *sendDelegateResponse) { s.Outbound.Capability = "outbound_send_v0" }, func(s *sendDelegateResponse) { s.Outbound.RequestID = strings.Repeat("a", 32) }, func(s *sendDelegateResponse) { s.Outbound.RequestHash = strings.Repeat("0", 64) }, func(s *sendDelegateResponse) {
		s.Outbound.Result.Entry.Operation.Account.PN = "15550000009@s.whatsapp.net"
	}, func(s *sendDelegateResponse) { s.Outbound.Result.Entry.Operation.Hash = strings.Repeat("a", 64) }, func(s *sendDelegateResponse) { s.Outbound.Result.Duplicate = false }} {
		s := valid()
		mutate(&s)
		_, err := validateOutboundDelegate(r, s)
		var failure *app.OutboundSendError
		if !errors.As(err, &failure) || failure.Code != "outcome_uncertain" {
			t.Fatal(s, err)
		}
	}
}

func TestOutboundOwnerDuplicateBypassesPacingAndWA(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	dir, a := draftOwnerFixture(t, false)
	_, o := outboundCLISeed(t, dir, 93)
	r := outboundRequestFixture(dir, o)
	// Production executor, real slot and pacer. A prior legacy operation records
	// spacing; the durable duplicate must return without waiting/opening WA.
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{min: 10 * time.Second, max: 10 * time.Second}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if req.Kind == "fixture-prior" {
			return sendDelegateResponse{OK: true}, nil
		}
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	flags := &rootFlags{storeDir: dir, timeout: 300 * time.Millisecond}
	if _, err := delegateSend(t.Context(), flags, sendDelegateRequest{Kind: "fixture-prior"}); err != nil {
		t.Fatal(err)
	}
	resp, err := delegateSend(t.Context(), flags, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
	if err != nil {
		t.Fatal(err)
	}
	result, err := validateOutboundDelegate(r, resp)
	if err != nil || !result.Duplicate || result.Entry.Operation.ID != o.ID {
		t.Fatal(result, err)
	}
}

func TestOutboundTransportUncertaintyAndNoFallback(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "old owner", true: "lost reply"}[lost], func(t *testing.T) {
			dir := shortPresenceDelegateStoreDir(t)
			_, o := outboundCLISeed(t, dir, 94)
			r := outboundRequestFixture(dir, o)
			listener, err := net.Listen("unix", sendDelegateSocketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				var req sendDelegateRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					done <- err
					return
				}
				if !lost {
					err = json.NewEncoder(conn).Encode(sendDelegateResponse{OK: true, ID: o.MessageID})
				}
				done <- err
			}()
			_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
			var failure *app.OutboundSendError
			if !errors.As(err, &failure) || failure.Code != "outcome_uncertain" {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutboundQueueDeadlineDoesNotInvokeExecutor(t *testing.T) {
	dir := shortPresenceDelegateStoreDir(t)
	_, o := outboundCLISeed(t, dir, 95)
	r := outboundRequestFixture(dir, o)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		if req.Kind == "fixture-hold" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return sendDelegateResponse{OK: true}, nil
		}
		return outboundRefusal(req, "not_dispatched"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: "fixture-hold"})
		done <- err
	}()
	<-entered
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 150 * time.Millisecond}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &r})
	var failure *app.OutboundSendError
	if !errors.As(err, &failure) || failure.Code != "not_dispatched" || calls.Load() != 1 {
		t.Fatal("queued executor crossed deadline", err, calls.Load())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOutboundWireCapsAndVersionBeforeExecutor(t *testing.T) {
	dir := shortPresenceDelegateStoreDir(t)
	_, o := outboundCLISeed(t, dir, 96)
	r := outboundRequestFixture(dir, o)
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		return sendDelegateResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, version := range []int{0, sendDelegateVersion} {
		conn, err := net.Dial("unix", sendDelegateSocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		req := sendDelegateRequest{Kind: outboundSendKind, Version: version, Outbound: &r}
		if version == sendDelegateVersion {
			req.Message = strings.Repeat("x", 20000)
		}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatal(err)
		}
		var response sendDelegateResponse
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		_, err = validateOutboundDelegate(r, response)
		var failure *app.OutboundSendError
		if !errors.As(err, &failure) || failure.Code != "outcome_uncertain" || calls.Load() != 0 {
			t.Fatal("wire/version dispatched", response, err, calls.Load())
		}
	}
}
