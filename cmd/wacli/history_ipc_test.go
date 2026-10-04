package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
)

func fixtureBackfillRequest(chat string) sendDelegateRequest {
	return sendDelegateRequest{Version: sendDelegateVersion, Kind: historyBackfillKind,
		Backfill: &backfillDelegateOptions{ChatJID: chat, Count: 50, Requests: 1, WaitMS: 20, IdleMS: 1}}
}

func TestHistoryBackfillDelegatesThroughLockedFixtureStore(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	requests := make(chan sendDelegateRequest, 1)
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		requests <- req
		return sendDelegateResponse{OK: true, Backfill: &app.BackfillResult{
			ChatJID: req.Backfill.ChatJID, RequestsSent: 2, ResponsesSeen: 1,
			MessagesAdded: 3, MessagesSynced: 5, StopReason: app.BackfillStopPrimaryNoMore,
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	stdout, stderr, err := runPresenceDelegateHelper(t, []string{
		"--store", dir, "--json", "--timeout", "2s", "history", "backfill", "--chat", "123@g.us",
		"--count", "70", "--requests", "2", "--wait", "80ms", "--idle-exit", "20ms",
	})
	if err != nil {
		t.Fatalf("CLI: %v, stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"messages_added":3`) || !strings.Contains(stdout, `"messages_synced":5`) || !strings.Contains(stdout, `"primary_no_more_messages"`) {
		t.Fatalf("legacy result: %s", stdout)
	}
	req := <-requests
	if req.Version != 1 || req.Kind != historyBackfillKind || req.Backfill == nil || req.Backfill.Count != 70 || req.Backfill.Requests != 2 || req.Backfill.WaitMS != 80 || req.Backfill.IdleMS != 20 || req.DeadlineUnixMS <= 0 {
		t.Fatalf("request: %+v", req)
	}
	for _, name := range []string{"wacli.db", "session.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("client opened %s: %v", name, err)
		}
	}
}

func TestHistoryBackfillGuardsBeforeDelegation(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	for _, args := range [][]string{
		{"--read-only", "history", "backfill", "--chat", "123@g.us"},
		{"--agent", "history", "backfill", "--chat", "123@g.us"},
		{"history", "backfill", "--chat", "123@g.us", "--count", "501"},
		{"history", "backfill", "--chat", "123@g.us", "--wait", "6m"},
		{"history", "backfill", "--chat", "123@g.us", "--requests", "101"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			dir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			var calls atomic.Int64
			stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
				calls.Add(1)
				return sendDelegateResponse{OK: true}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			_, _, err = runPresenceDelegateHelper(t, append([]string{"--store", dir}, args...))
			if err == nil || calls.Load() != 0 {
				t.Fatalf("guard: err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestHistoryBackfillSerializesAndExpiresQueuedRequests(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls, active, peak atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		n := active.Add(1)
		defer active.Add(-1)
		peak.Store(max(peak.Load(), n))
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return sendDelegateResponse{}, ctx.Err()
			}
		}
		return sendDelegateResponse{OK: true, Backfill: &app.BackfillResult{ChatJID: req.Backfill.ChatJID}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	flags := &rootFlags{storeDir: dir, timeout: time.Second}
	first := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), flags, fixtureBackfillRequest("first@g.us"))
		first <- err
	}()
	<-started
	_, err = delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 50 * time.Millisecond}, fixtureBackfillRequest("expired@g.us"))
	if err == nil || !strings.Contains(err.Error(), "before dispatch") || !strings.Contains(err.Error(), "no history was requested") {
		t.Fatalf("queue expiry: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), flags, fixtureBackfillRequest("second@g.us"))
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("concurrent request bypassed slot: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || peak.Load() != 1 {
		t.Fatalf("dispatches=%d peak=%d", calls.Load(), peak.Load())
	}
}

func TestHistoryBackfillDoesNotUseOrRecordSendPacing(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	pacer := newSendPacer(sendSpacing{min: time.Hour, max: time.Hour})
	pacer.record()
	last := pacer.last
	done := make(chan struct{})
	go func() {
		handleSendDelegateConn(context.Background(), server, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			return sendDelegateResponse{OK: true}, nil
		}, slot, pacer)
		close(done)
	}()
	req := fixtureBackfillRequest("123@g.us")
	req.TimeoutMS = 500
	if err := json.NewEncoder(client).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp sendDelegateResponse
	if err := json.NewDecoder(client).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	<-done
	if !resp.OK || pacer.last != last {
		t.Fatalf("backfill changed pacing: resp=%+v last=%v", resp, pacer.last)
	}
}

func TestHistoryBackfillServerValidatesBeforeDispatch(t *testing.T) {
	for _, mutate := range []func(*sendDelegateRequest){
		func(r *sendDelegateRequest) { r.Backfill = nil },
		func(r *sendDelegateRequest) { r.Backfill.WaitMS = 1 << 62 },
		func(r *sendDelegateRequest) { r.Backfill.Count = 501 },
		func(r *sendDelegateRequest) { r.Version++ },
	} {
		server, client := net.Pipe()
		_ = client.SetDeadline(time.Now().Add(time.Second))
		req := fixtureBackfillRequest("123@g.us")
		mutate(&req)
		go handleSendDelegateConn(context.Background(), server, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			t.Error("invalid backfill executed")
			return sendDelegateResponse{}, nil
		}, make(chan struct{}), nil)
		if err := json.NewEncoder(client).Encode(req); err != nil {
			t.Fatal(err)
		}
		var resp sendDelegateResponse
		if err := json.NewDecoder(client).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		if resp.OK || !strings.Contains(resp.Error, "before dispatch") {
			t.Fatalf("invalid request: %+v", resp)
		}
	}
}

func TestHistoryBackfillUnsupportedAndLostResponseNeverOpenDirectWriter(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost_%v", lost), func(t *testing.T) {
			dir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			ln, err := net.Listen("unix", sendDelegateSocketPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req sendDelegateRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				if !lost {
					_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: `unsupported send kind "history_backfill"`})
				}
			}()
			_, stderr, err := runPresenceDelegateHelper(t, []string{"--store", dir, "--timeout", "1s", "history", "backfill", "--chat", "123@g.us"})
			<-done
			want := "unsupported send kind"
			if lost {
				want = "history may already have been persisted"
			}
			if err == nil || !strings.Contains(stderr, want) {
				t.Fatalf("error=%v stderr=%s", err, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "wacli.db")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fallback writer opened: %v", err)
			}
		})
	}
}

func TestHistoryBackfillUsesOnlySelectedAccountSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG config fixture requires Linux")
	}
	configDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", configDir)
	one, two := shortPresenceDelegateStoreDir(t), shortPresenceDelegateStoreDir(t)
	if err := config.SaveAccountsConfig(config.DefaultConfigPath(), &config.AccountsConfig{DefaultAccount: "one", Accounts: map[string]config.AccountEntry{
		"one": {Store: one}, "two": {Store: two},
	}}); err != nil {
		t.Fatal(err)
	}
	var selected atomic.Int64
	var stops []func()
	for i, dir := range []string{one, two} {
		lk, err := lock.Acquire(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lk.Release()
		stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
			selected.Store(int64(i + 1))
			return sendDelegateResponse{OK: true, Backfill: &app.BackfillResult{ChatJID: req.Backfill.ChatJID}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		stops = append(stops, stop)
	}
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	for _, tc := range []struct {
		args []string
		want int64
	}{
		{[]string{"--account", "two"}, 2}, {nil, 1}, {[]string{"--store", two}, 2},
	} {
		args := append(tc.args, "--timeout", "1s", "history", "backfill", "--chat", "123@g.us")
		_, stderr, err := runPresenceDelegateHelper(t, args)
		if err != nil || selected.Load() != tc.want {
			t.Fatalf("account route=%d want=%d err=%v stderr=%s", selected.Load(), tc.want, err, stderr)
		}
	}
}

func TestHistoryBackfillDialFailurePreservesLockError(t *testing.T) {
	orig := fmt.Errorf("fixture lock: %w", lock.ErrLocked)
	_, delegated, err := tryDelegateSend(context.Background(), &rootFlags{storeDir: t.TempDir()}, orig, fixtureBackfillRequest("123@g.us"))
	if delegated || err != orig {
		t.Fatalf("dial failure=%v delegated=%v", err, delegated)
	}
}

func TestHistoryBackfillTimeoutAfterDispatchIsUncertainAndOwnerContinues(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return sendDelegateResponse{}, ctx.Err()
		}
		return sendDelegateResponse{OK: true, Backfill: &app.BackfillResult{ChatJID: req.Backfill.ChatJID}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, err = delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 60 * time.Millisecond}, fixtureBackfillRequest("first@g.us"))
	if err == nil || !strings.Contains(err.Error(), "after dispatch") || !strings.Contains(err.Error(), "persisted history may remain") {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: time.Second}, fixtureBackfillRequest("next@g.us")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("owner calls=%d", calls.Load())
	}
}

func TestHistoryBackfillServerStopCancelsOperationAndIdleConnections(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	started := make(chan struct{})
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, _ sendDelegateRequest) (sendDelegateResponse, error) {
		close(started)
		<-ctx.Done()
		return sendDelegateResponse{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	idle, err := net.Dial("unix", sendDelegateSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	request := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: time.Second}, fixtureBackfillRequest("123@g.us"))
		request <- err
	}()
	<-started
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server stop waited on idle connection/operation")
	}
	if err := <-request; err == nil {
		t.Fatal("stopped owner reported success")
	}
}
