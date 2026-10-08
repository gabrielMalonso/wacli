package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func statusFixtureReply(req sendDelegateRequest, state app.SyncLiveState) sendDelegateResponse {
	localReady := state == "ready"
	if localReady {
		state = app.SyncLiveUnknown
	}
	return sendDelegateResponse{OK: true, SyncStatus: &syncStatusReply{Version: syncStatusVersion, ID: req.SyncStatus.ID, StoreRef: req.SyncStatus.StoreRef, Status: app.SyncLiveStatus{State: state, OwnerReady: localReady, Initialized: localReady, Connected: "unknown", TransportConnected: "true", Authenticated: "unknown", ReadinessReason: "current_authentication_unsupported", LinkedJID: "15550000001@s.whatsapp.net", ObservedAt: time.Now().UTC()}}}
}

func TestSyncStatusReadOnlyWhileWriterBusy(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := t.TempDir()
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if req.Kind == "text" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return sendDelegateResponse{OK: true}, nil
		}
		return statusFixtureReply(req, "ready"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	conn, err := net.Dial("unix", sendDelegateSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(sendDelegateRequest{Version: 1, Kind: "text"}); err != nil {
		t.Fatal(err)
	}
	<-entered
	before := snapshotStatusStore(t, dir)
	stdout, stderr, err := runAgentTest(t, "--agent", "--read-only", "--store", dir, "sync", "status")
	if err != nil || stderr != "" {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	env := decodeAgentTest(t, stdout)
	var data syncStatusData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Ready || !data.OwnerReady || data.TransportConnected != "true" || data.Authenticated != "unknown" || data.ReadinessReason != "current_authentication_unsupported" || env.Meta.Source != "live" || env.Meta.Completeness != "unknown" || env.Meta.Freshness != "unknown" {
		t.Fatalf("%s", stdout)
	}
	if !reflect.DeepEqual(before, snapshotStatusStore(t, dir)) {
		t.Fatal("query changed store")
	}
	other := filepath.Join(dir, "absent-account")
	v := querySyncStatus(t.Context(), other)
	if v.State != "absent" {
		t.Fatalf("wrong-account=%+v", v)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("query created store: %v", err)
	}
}

func TestSyncStatusUnknownOwners(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	for _, reason := range []string{"owner_incompatible", "scope_mismatch", "invalid_reply", "timeout", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			dir := t.TempDir()
			stall := make(chan struct{})
			received := make(chan struct{})
			stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
				resp := statusFixtureReply(req, "ready")
				switch reason {
				case "owner_incompatible":
					return sendDelegateResponse{Error: "unsupported kind"}, nil
				case "scope_mismatch":
					resp.SyncStatus.StoreRef = filepath.Join(dir, "other-account")
				case "invalid_reply":
					resp.SyncStatus.Status.Initialized = false
				case "timeout", "cancelled":
					close(received)
					<-stall
				}
				return resp, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			defer close(stall)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if reason == "cancelled" {
				go func() { <-received; cancel() }()
			}
			v := querySyncStatus(ctx, dir)
			if v.State != "unknown" || v.Ready || v.Reason != reason {
				t.Fatalf("%+v", v)
			}
		})
	}
}

func TestSyncStatusRefusesOlderPositiveAuthentication(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := t.TempDir()
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		resp := statusFixtureReply(req, "ready")
		resp.SyncStatus.Version = 1
		resp.SyncStatus.Status.State = "ready"
		resp.SyncStatus.Status.Ready = true
		resp.SyncStatus.Status.Connected = "true"
		return resp, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if v := querySyncStatus(t.Context(), dir); v.Ready || v.OwnerReady || v.Reason != "owner_incompatible" {
		t.Fatalf("older unsafe authentication accepted=%+v", v)
	}
}

func TestSyncStatusAbsentStaleAndCLIValidation(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := t.TempDir()
	if v := querySyncStatus(t.Context(), dir); v.State != "absent" {
		t.Fatalf("%+v", v)
	}
	ln, err := net.Listen("unix", sendDelegateSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if v := querySyncStatus(t.Context(), dir); v.State != "absent" {
		t.Fatalf("%+v", v)
	}
	for _, duration := range []string{"0s", "-1s", "2m"} {
		_, stderr, err := runAgentTest(t, "--agent", "--store", dir, "sync", "status", "--timeout", duration)
		if err == nil || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatalf("%v %s", err, stderr)
		}
	}
}

func TestSyncStatusEarlyOwnerRefusesMutation(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := seedLocalReadStore(t)
	a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	stop, err := startSyncDelegateServer(t.Context(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	v := querySyncStatus(t.Context(), dir)
	if v.State != "initializing" || v.Ready {
		t.Fatalf("%+v", v)
	}
	other := t.TempDir()
	if err := os.Symlink(sendDelegateSocketPath(dir), sendDelegateSocketPath(other)); err != nil {
		t.Fatal(err)
	}
	if v := querySyncStatus(t.Context(), other); v.State != "unknown" || v.Ready || v.Reason != "owner_incompatible" {
		t.Fatalf("wrong owner accepted another store: %+v", v)
	}
	resp, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: "text", To: "15550000002@s.whatsapp.net", Message: "fixture"})
	if err == nil || resp.Sent {
		t.Fatalf("premature dispatch: %+v %v", resp, err)
	}
}

// Sockets are IPC endpoints, not readable file contents.
func snapshotStatusStore(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = string(data)
	}
	return result
}

type syncStatusWA struct {
	agentChatStateWA
	shutdownStarted chan struct{}
	shutdownRelease chan struct{}
	shutdownOnce    sync.Once
}

func (f *syncStatusWA) SetManualHistorySyncDownload(bool) {}
func (f *syncStatusWA) SendPresence(ctx context.Context, presence types.Presence) error {
	if presence == types.PresenceUnavailable {
		f.shutdownOnce.Do(func() { close(f.shutdownStarted) })
		select {
		case <-f.shutdownRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (f *syncStatusWA) ReconnectWithBackoff(ctx context.Context, _, _ time.Duration, opts wa.ConnectOptions) error {
	return f.Connect(ctx, opts)
}

func TestSyncStatusRealOwnerLifecycleNoSecondConnection(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "logout"}[logout], func(t *testing.T) {
			f := &syncStatusWA{shutdownStarted: make(chan struct{}), shutdownRelease: make(chan struct{})}
			bootstrapStarted, bootstrapRelease := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.fetchHook = func(ctx context.Context) ([]any, error) {
				once.Do(func() { close(bootstrapStarted) })
				select {
				case <-bootstrapRelease:
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
			ownerCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			serverStarted := make(chan func(), 1)
			done := make(chan error, 1)
			go func() {
				_, err := a.Sync(ownerCtx, app.SyncOptions{Mode: app.SyncModeFollow, PresenceMode: app.SyncPresenceModeQuiet,
					BeforeConnect: func(ctx context.Context) error {
						stop, err := startSyncDelegateServer(ctx, a, sendSpacing{})
						if err == nil {
							serverStarted <- stop
						}
						return err
					},
				})
				done <- err
			}()
			var stop func()
			select {
			case stop = <-serverStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("IPC did not start before connect")
			}
			defer stop()
			select {
			case <-bootstrapStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("bootstrap timeout")
			}
			query := func() syncStatusData {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				return querySyncStatus(ctx, dir)
			}
			v := query()
			if v.State != "initializing" || v.Ready || v.OwnerReady || v.Initialized || v.Connected != "unknown" || v.TransportConnected != "true" || v.Authenticated != "unknown" {
				t.Fatalf("bootstrap status=%+v", v)
			}
			close(bootstrapRelease)
			await := func(want app.SyncLiveState) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					v := query()
					if v.State == want && (want != "unknown" || v.OwnerReady && v.TransportConnected == "true") {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatalf("state=%+v want %s", query(), want)
			}
			await("unknown")
			for range 10 {
				if v := query(); v.Ready || !v.OwnerReady || v.Authenticated != "unknown" || v.ReadinessReason != "current_authentication_unsupported" {
					t.Fatalf("local readiness/auth gap: %+v", v)
				}
			}
			if f.opens.Load() != 1 || f.connects.Load() != 1 {
				t.Fatalf("extra WA lifetime: opens=%d connects=%d", f.opens.Load(), f.connects.Load())
			}
			f.Disconnect()
			f.emit(&events.Disconnected{})
			await("unknown")
			if f.opens.Load() != 1 || f.connects.Load() != 2 {
				t.Fatal("reconnect created another client")
			}
			if logout {
				f.emit(&events.LoggedOut{})
			} else {
				cancel()
			}
			select {
			case <-f.shutdownStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown timeout")
			}
			if logout {
				await("logged_out")
			} else {
				await("stopping")
			}
			f.emit(&events.Connected{})
			if v := query(); v.Ready || v.OwnerReady {
				t.Fatalf("late connected revived local owner: %+v", v)
			}
			close(f.shutdownRelease)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if logout {
				await("logged_out")
			} else {
				await("stopped")
			}
			stop()
			if query().State != "absent" {
				t.Fatal("stopped server remained available")
			}

		})
	}
}

func TestSyncStatusUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows IPC limitation")
	}
	dir := filepath.Join(t.TempDir(), "missing")
	v := querySyncStatus(t.Context(), dir)
	if v.State != "unknown" || v.Reason != "ipc_unsupported" || v.Ready {
		t.Fatalf("%+v", v)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("unsupported query changed filesystem")
	}
}
