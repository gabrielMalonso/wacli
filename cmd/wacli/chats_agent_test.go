package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Production App/WAFactory, builders and transport; only the SDK boundary is
// fake. Unexpected auth, discovery, receipts or legacy sends fail the fixture.
type agentChatStateWA struct {
	outboundOwnerWA
	handlers       map[uint32]func(any)
	next           uint32
	states         atomic.Int64
	fetches        atomic.Int64
	stateHook      func(context.Context, appstate.PatchInfo, func()) ([]any, error)
	fetchHook      func(context.Context) ([]any, error)
	disconnectHook func()
	chatSettings   map[types.JID]types.LocalChatSettings
}

func (f *agentChatStateWA) IsAuthed() bool { return true }
func (f *agentChatStateWA) AddEventHandler(h func(any)) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers == nil {
		f.handlers = make(map[uint32]func(any))
	}
	f.next++
	f.handlers[f.next] = h
	return f.next
}
func (f *agentChatStateWA) RemoveEventHandler(id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.handlers, id)
	f.removed.Add(1)
}
func (f *agentChatStateWA) emit(evt any) {
	f.applyChatSettings(evt)
	f.mu.Lock()
	handlers := make([]func(any), 0, len(f.handlers))
	for _, h := range f.handlers {
		handlers = append(handlers, h)
	}
	f.mu.Unlock()
	for _, h := range handlers {
		h(evt)
	}
}

func (f *agentChatStateWA) GetChatSettings(ctx context.Context, jid types.JID) (types.LocalChatSettings, error) {
	if err := ctx.Err(); err != nil {
		return types.LocalChatSettings{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatSettings[jid], nil
}

func (f *agentChatStateWA) applyChatSettings(evt any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatSettings == nil {
		f.chatSettings = make(map[types.JID]types.LocalChatSettings)
	}
	switch v := evt.(type) {
	case *events.Archive:
		if v != nil && v.Action != nil {
			settings := f.chatSettings[v.JID]
			settings.Found, settings.Archived = true, v.Action.GetArchived()
			f.chatSettings[v.JID] = settings
		}
	case *events.Pin:
		if v != nil && v.Action != nil {
			settings := f.chatSettings[v.JID]
			settings.Found, settings.Pinned = true, v.Action.GetPinned()
			f.chatSettings[v.JID] = settings
		}
	}
}

func (f *agentChatStateWA) applyChatStatePatch(p appstate.PatchInfo) {
	for _, mutation := range p.Mutations {
		jid, _ := types.ParseJID(mutation.Index[1])
		if action := mutation.Value.GetArchiveChatAction(); action != nil {
			f.applyChatSettings(&events.Archive{JID: jid, Action: action})
		}
		if action := mutation.Value.GetPinAction(); action != nil {
			f.applyChatSettings(&events.Pin{JID: jid, Action: action})
		}
	}
}
func (f *agentChatStateWA) Connect(_ context.Context, opts wa.ConnectOptions) error {
	if opts.AllowQR || opts.OnQRCode != nil {
		return errors.New("fixture forbids auth")
	}
	if f.IsConnected() {
		return nil
	}
	f.connects.Add(1)
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	f.emit(&events.Connected{})
	return nil
}
func (f *agentChatStateWA) Disconnect() {
	f.outboundOwnerWA.Disconnect()
	if f.disconnectHook != nil {
		f.disconnectHook()
	}
}

// EnsureAuthed retains its established history migration before the action's
// strict snapshot. No best-effort resolver is used to choose the mutation target.
func (f *agentChatStateWA) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID { return jid }
func (f *agentChatStateWA) FetchAppStateEvents(ctx context.Context, _ string, _, _ bool) ([]any, error) {
	f.fetches.Add(1)
	if f.fetchHook != nil {
		return f.fetchHook(ctx)
	}
	return nil, nil
}
func (f *agentChatStateWA) patch(ctx context.Context, p appstate.PatchInfo, boundary func()) ([]any, error) {
	f.states.Add(1)
	if f.stateHook != nil {
		events, err := f.stateHook(ctx, p, boundary)
		if err == nil {
			f.applyChatStatePatch(p)
		}
		for _, evt := range events {
			f.applyChatSettings(evt)
		}
		return events, err
	}
	boundary()
	f.applyChatStatePatch(p)
	return nil, nil
}
func (f *agentChatStateWA) ArchiveChat(ctx context.Context, jid types.JID, archive bool, ts time.Time, key *waCommon.MessageKey, boundary func()) ([]any, error) {
	return f.patch(ctx, appstate.BuildArchive(jid, archive, ts, key), boundary)
}
func (f *agentChatStateWA) MarkChatAsRead(ctx context.Context, jid types.JID, read bool, ts time.Time, key *waCommon.MessageKey, boundary func()) ([]any, error) {
	return f.patch(ctx, appstate.BuildMarkChatAsRead(jid, read, ts, key), boundary)
}

func agentChatStateOwnerFixture(t *testing.T, connect bool) (string, *app.App, *agentChatStateWA, app.ChatStateRequest) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	f := &agentChatStateWA{}
	dir, a := draftOwnerFixtureOptions(t, app.Options{WAFactory: func(opts wa.Options) (app.WAClient, error) {
		f.opens.Add(1)
		if opts.StorePath == "" || opts.KeyStateStore == nil {
			t.Error("missing factory scope")
		}
		return f, nil
	}})
	// The strict mirror is PN-scoped. Retain the original alias-only read
	// fixture too, without relying on history migration to supply this anchor.
	if err := a.DB().UpsertChat(localReadPN, "dm", "Synthetic", time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: localReadPN, MsgID: "anchor", Timestamp: time.Unix(1700000000, 0), Text: "old"}); err != nil {
		t.Fatal(err)
	}
	if connect {
		if err := a.OpenWA(); err != nil {
			t.Fatal(err)
		}
		if err := a.Connect(t.Context(), false, nil); err != nil {
			t.Fatal(err)
		}
	}
	return dir, a, f, app.ChatStateRequest{Version: 1, StoreRef: dir, Requested: localReadPN, Action: app.ChatStateArchive}
}

func chatStateCLIArgs(r app.ChatStateRequest) []string {
	return []string{"--agent", "--store", r.StoreRef, "--timeout", "2s", "chats", string(r.Action), "--chat", r.Requested}
}

func TestAgentChatStatePreflightBeforeEffects(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, action := range []string{"mark-read", "mark-unread", "archive", "unarchive"} {
		for _, extra := range [][]string{{"--chat", "PRIVATE_NAME"}, {"--chat", localReadPN, "--pick", "0"}, {"--chat", "123@newsletter"}, {"--read-only"}, {"--chat", localReadPN, "--timeout", "PRIVATE_TIMEOUT"}, {"--PRIVATE_FLAG=x"}, {"--chat", localReadPN, "PRIVATE_POSITIONAL"}, {}} {
			t.Run(action+"/"+strings.Join(extra, "/"), func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "missing")
				args := append([]string{"--agent", "--store", dir, "chats", action}, extra...)
				stdout, stderr, err := runAgentTest(t, args...)
				if err == nil || stdout != "" || decodeAgentTest(t, stderr).Meta.Source != "live" || commandExitCode(err) != 2 {
					t.Fatal(stdout, stderr, err)
				}
				envelope := decodeAgentTest(t, stderr)
				if strings.Contains(envelope.Error.Message+envelope.Error.Recovery, "PRIVATE_") || envelope.Error.Code == "invalid_arguments" && envelope.Error.Message != "Invalid explicit chat state arguments." {
					t.Fatal("unsanitized preflight", stderr)
				}
				failure := envelope.Error.ChatState
				if failure == nil || failure.Action != action || failure.Outcome != "not_dispatched" || failure.LocalMirror != "unknown" {
					t.Fatal("missing preflight invocation knowledge", stderr)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("preflight effect", err)
				}
			})
		}
	}
	for _, action := range []string{"pin", "unpin", "mute", "unmute", "cleanup"} {
		_, stderr, err := runAgentTest(t, "--agent", "chats", action)
		if err == nil || decodeAgentTest(t, stderr).Error.Code != "unsupported_command" {
			t.Fatal(action, stderr, err)
		}
	}
	t.Setenv("WACLI_READONLY", "1")
	dir := filepath.Join(t.TempDir(), "missing")
	_, stderr, err := runAgentTest(t, "--agent", "--store", dir, "chats", "archive", "--chat", localReadPN)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "read_only" || decodeAgentTest(t, stderr).Meta.Source != "live" {
		t.Fatal(stderr, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("env policy effect")
	}
}

func TestAgentChatStateUsageSanitizationPreservesCauseAndCorrelation(t *testing.T) {
	cause := errors.New("PRIVATE_CALLER_VALUE")
	request := &app.ChatStateRequest{Version: 1, Requested: localReadPN, Action: app.ChatStateAction("archive")}
	correlation := &out.AgentChatStateError{Requested: request.Requested, Action: string(request.Action), Outcome: "not_dispatched", LocalMirror: "unknown"}
	for _, existingCorrelation := range []*out.AgentChatStateError{nil, correlation} {
		original := agentUsageError(cause)
		original.ChatState = existingCorrelation
		classified := classifyChatStateAgentError(original, request)
		if classified.Message != "Invalid explicit chat state arguments." || classified.Code != "invalid_arguments" || classified.ExitCode != 2 || !errors.Is(classified, cause) || classified.ChatState == nil || *classified.ChatState != *correlation {
			t.Fatalf("changed error knowledge: %+v", classified)
		}
		if original.Message != cause.Error() || original.ChatState != existingCorrelation {
			t.Fatal("classifier mutated its cause")
		}
	}
}

func exerciseAgentChatStateCLI(t *testing.T, binary string) {
	t.Helper()
	dir, a, f, r := agentChatStateOwnerFixture(t, true)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); _ = lk.Release() })
	stop, err := startSendDelegateServer(t.Context(), a, sendSpacing{min: 10 * time.Second, max: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	for _, invalid := range [][]string{{"--read-only"}, {"--pick", "1"}} {
		_, stderr, err := runDraftBinary(t, binary, append(chatStateCLIArgs(r), invalid...), false)
		if err == nil || decodeAgentTest(t, stderr).Meta.Source != "live" || f.states.Load() != 0 || f.fetches.Load() != 0 {
			t.Fatal("invalid invocation reached owner", stderr, err)
		}
	}
	t.Setenv("WACLI_READONLY", "1")
	_, stderr, err := runDraftBinary(t, binary, chatStateCLIArgs(r), true)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "read_only" || f.fetches.Load() != 0 {
		t.Fatal(stderr, err)
	}
	t.Setenv("WACLI_READONLY", "0")
	for _, action := range []app.ChatStateAction{app.ChatStateMarkRead, app.ChatStateMarkUnread, app.ChatStateArchive, app.ChatStateUnarchive} {
		r.Action = action
		if action == app.ChatStateMarkUnread || action == app.ChatStateMarkRead {
			r.Requested = localReadLID
		} else {
			r.Requested = "+15550000001"
		}
		stdout, stderr, err := runDraftBinary(t, binary, chatStateCLIArgs(r), false)
		if err != nil || stderr != "" {
			t.Fatal(stdout, stderr, err)
		}
		e := decodeAgentTest(t, stdout)
		var result app.ChatStateResult
		if err := json.Unmarshal(e.Data, &result); err != nil {
			t.Fatal(err)
		}
		expected := r
		expected.Requested, _ = store.NormalizeDraftTarget(r.Requested)
		if app.ValidateChatStateResult(expected, result) != nil || e.Meta.Source != "live" || result.LocalMirror != app.ChatStateMirrorPersisted || result.Observation.Target.JID != localReadPN {
			t.Fatal(stdout)
		}
	}
	if f.states.Load() != 4 || f.opens.Load() != 1 || f.connects.Load() != 1 {
		t.Fatal("new connection/retry", f.states.Load(), f.opens.Load(), f.connects.Load())
	}
	// Local context reading is independent of the writer and does not clear unread.
	_, stderr, err = runDraftBinary(t, binary, []string{"--agent", "--store", dir, "messages", "context", "--chat", localReadPN, "--id", "m1"}, true)
	if err != nil || stderr != "" {
		t.Fatal(stderr, err)
	}
	chat, err := a.DB().GetChat(localReadPN)
	if err != nil || !chat.Unread || chat.Archived || chat.Pinned || f.states.Load() != 4 {
		t.Fatalf("chat=%+v err=%v", chat, err)
	}
}

func TestAgentChatStateCLIRealOwner(t *testing.T) { exerciseAgentChatStateCLI(t, "") }
func TestAgentChatStateProductionBinaryOwner(t *testing.T) {
	binary := os.Getenv("WACLI_CHAT_STATE_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_CHAT_STATE_E2E_BINARY to a freshly built local binary")
	}
	exerciseAgentChatStateCLI(t, binary)
}

func TestAgentChatStateStandaloneLifecycle(t *testing.T) {
	_, a, f, r := agentChatStateOwnerFixture(t, false)
	r.Action = app.ChatStateMarkRead
	f.disconnectHook = func() {
		f.emit(&events.Archive{JID: types.NewJID("15550000002", types.DefaultUserServer), Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
	}
	result, err := connectAndApplyAgentChatState(t.Context(), a, r)
	if err != nil || result.Outcome != app.ChatStateSDKCompleted || f.states.Load() != 1 || f.opens.Load() != 1 || f.connects.Load() != 1 {
		t.Fatal(result, err)
	}
	a.Close()
	f.mu.Lock()
	remaining := len(f.handlers)
	f.mu.Unlock()
	if remaining != 0 || f.closed.Load() != 1 || f.IsConnected() {
		t.Fatal("client/handlers not drained")
	}
	reader, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	chat, err := reader.GetChat("15550000002@s.whatsapp.net")
	if err != nil || !chat.Archived {
		t.Fatal("handler removed before disconnect persistence", chat, err)
	}
}

func TestAgentChatStateOwnerPolicyAndUnavailable(t *testing.T) {
	for _, mode := range []string{"readonly", "disconnected", "lock without socket"} {
		t.Run(mode, func(t *testing.T) {
			dir, a, f, r := agentChatStateOwnerFixture(t, mode != "readonly")
			r.Action = app.ChatStateMarkRead
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close(); _ = lk.Release() })
			owner := a
			if mode == "readonly" {
				owner, err = app.New(app.Options{StoreDir: dir, ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(owner.Close)
			} else if mode == "disconnected" {
				f.Disconnect()
			}
			if mode != "lock without socket" {
				stop, err := startSendDelegateServer(t.Context(), owner, sendSpacing{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(stop)
			}
			stdout, stderr, err := runAgentTest(t, chatStateCLIArgs(r)...)
			want := map[string]string{"readonly": "read_only", "disconnected": "not_dispatched", "lock without socket": "store_locked"}[mode]
			if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != want || f.states.Load() != 0 || f.fetches.Load() != 0 {
				t.Fatal(stdout, stderr, err)
			}
		})
	}
}

func TestAgentChatStateIPCOldUntypedCrossScopeAndCaps(t *testing.T) {
	for _, mode := range []string{"old error", "untyped success", "cross action", "cross store", "cross target", "bad capability", "bad result", "oversized reply", "EOF"} {
		t.Run(mode, func(t *testing.T) {
			dir, a, _, r := agentChatStateOwnerFixture(t, true)
			r.Action = app.ChatStateMarkRead
			var calls atomic.Int64
			execute := func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
				calls.Add(1)
				if mode == "old error" {
					return sendDelegateResponse{Error: `unsupported send kind "agent_chat_state" SECRET_CAUSE`}, nil
				}
				if mode == "untyped success" {
					return sendDelegateResponse{OK: true, Chat: r.Requested, Action: string(r.Action)}, nil
				}
				resp, err := executeDelegatedSend(ctx, a, req)
				if err != nil || resp.AgentChatState.Result == nil {
					t.Error(resp, err)
					return resp, err
				}
				switch mode {
				case "cross action":
					resp.AgentChatState.Result.Request.Action = app.ChatStateMarkUnread
				case "cross store":
					resp.AgentChatState.Result.Request.StoreRef += "/other"
				case "cross target":
					resp.AgentChatState.Result.Request.Requested = "15550000002@s.whatsapp.net"
				case "bad capability":
					resp.AgentChatState.Capability = "old"
				case "bad result":
					resp.AgentChatState.Result.LocalMirror = app.ChatStateMirrorUnknown
				case "oversized reply":
					resp.Error = strings.Repeat("x", agentChatStateMaxFrame)
				}
				return resp, nil
			}
			if mode == "EOF" {
				listener, err := net.Listen("unix", sendDelegateSocketPath(dir))
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan struct{})
				go func() {
					defer close(done)
					c, err := listener.Accept()
					if err == nil {
						var req sendDelegateRequest
						_ = json.NewDecoder(c).Decode(&req)
						c.Close()
					}
				}()
				defer func() { <-done }()
			} else {
				stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, execute)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
			}
			_, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &r})
			typed := classifyChatStateAgentError(err, &r)
			if err == nil || typed.Code != "chat_state_outcome_uncertain" || typed.ChatState.Outcome != "uncertain" || strings.Contains(typed.Message, "SECRET") || calls.Load() > 1 {
				t.Fatal(typed, err, calls.Load())
			}
		})
	}
	for _, mode := range []string{"version", "action", "legacy kind", "outbound kind", "extra legacy field", "scope store", "raw cap", "unknown field", "two JSON objects"} {
		t.Run("request/"+mode, func(t *testing.T) {
			_, a, f, r := agentChatStateOwnerFixture(t, true)
			r.Action = app.ChatStateMarkRead
			s, c := net.Pipe()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			req := sendDelegateRequest{AgentChatState: &r, Kind: agentChatStateKind, Version: 1}
			switch mode {
			case "version":
				req.Version = 2
			case "action":
				r.Action = "pin"
			case "legacy kind":
				req.Kind = chatStateKind
				req.To, req.ChatStateAction = r.Requested, "archive"
			case "outbound kind":
				req.Kind = outboundSendKind
			case "extra legacy field":
				req.To = r.Requested
			case "scope store":
				r.StoreRef += "/other"
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "raw cap":
				raw = append(raw, []byte(strings.Repeat(" ", agentChatStateMaxFrame))...)
			case "unknown field":
				raw = []byte(strings.Replace(string(raw), `"version":1`, `"unknown":"SECRET","version":1`, 1))
			case "two JSON objects":
				raw = append(raw, []byte(`{}`)...)
			}
			raw = append(raw, '\n')
			done := make(chan struct{})
			var invocations atomic.Int64
			go func() {
				defer close(done)
				handleSendDelegateConn(t.Context(), s, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
					invocations.Add(1)
					return executeDelegatedSend(ctx, a, req)
				}, make(chan struct{}, 1), newSendPacer(sendSpacing{}))
			}()
			_, _ = c.Write(raw)
			var resp sendDelegateResponse
			_ = json.NewDecoder(c).Decode(&resp)
			c.Close()
			<-done
			if resp.OK || f.states.Load() != 0 || f.fetches.Load() != 0 {
				t.Fatal("invalid frame executed", mode, resp)
			}
			if mode != "scope store" && invocations.Load() != 0 {
				t.Fatal("invalid typed frame reached an executor", mode)
			}
		})
	}
}

func TestAgentChatStateOutputFailureRetainsKnowledge(t *testing.T) {
	_, a, _, r := agentChatStateOwnerFixture(t, true)
	r.Action = app.ChatStateMarkRead
	result, err := a.ApplyAgentChatState(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{agent: true, agentCapability: agentChatState, agentAccount: out.AgentAccount{StoreRef: &r.StoreRef}}
	err = writeAgentChatStateResult(outboundBrokenWriter{}, flags, result)
	failure := classifyChatStateAgentError(err, &r)
	if err == nil || failure.Code != "chat_state_output_unconfirmed" || failure.ChatState.Outcome != "sdk_completed" || failure.ChatState.LocalMirror != "persisted" || failure.ChatState.TargetJID != localReadPN || failure.ChatState.OwnPN != result.Observation.Account.PN {
		t.Fatal(failure, err)
	}
}

func TestAgentChatStateLostReplyLateMirrorNoReplay(t *testing.T) {
	dir, a, f, r := agentChatStateOwnerFixture(t, true)
	r.Action = app.ChatStateMarkRead
	started, release := make(chan struct{}), make(chan struct{})
	f.stateHook = func(_ context.Context, _ appstate.PatchInfo, boundary func()) ([]any, error) {
		boundary()
		close(started)
		<-release
		return nil, nil
	}
	stop, err := startSendDelegateServer(t.Context(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 100 * time.Millisecond}, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &r})
		done <- err
	}()
	<-started
	err = <-done
	if failure := classifyChatStateAgentError(err, &r); err == nil || failure.ChatState.Outcome != "uncertain" {
		t.Fatal(failure, err)
	}
	close(release)
	stop() // Drain owner execution instead of polling the local mirror.
	chat, err := a.DB().GetChat(localReadPN)
	if err != nil || chat.Unread || f.states.Load() != 1 {
		t.Fatal("late mirror/replay", chat, err, f.states.Load())
	}
}

func TestAgentChatStateIndependentSendAndSerializedState(t *testing.T) {
	dir, a, f, r := agentChatStateOwnerFixture(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	f.fetchHook = func(ctx context.Context) ([]any, error) {
		close(started)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	stop, err := startSendDelegateServer(t.Context(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 2 * time.Second}, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &r})
		done <- err
	}()
	<-started
	second := r
	second.Requested = "15550000002@s.whatsapp.net"
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: 60 * time.Millisecond}, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &second})
	if failure := classifyChatStateAgentError(err, &second); err == nil || failure.Code != "not_dispatched" {
		t.Fatal("app-state serialization", failure, err)
	}
	payload, err := store.NewDraftPayload(store.DraftPayloadData{Account: store.DraftIdentity{PN: f.LinkedJID(), LID: f.LinkedLID()}, Recipient: store.DraftRecipient{JID: localReadPN, PN: localReadPN, LID: localReadLID}, Kind: store.DraftTextKind, Text: &store.DraftText{Text: "independent send"}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := store.NewDraftRevision(strings.Repeat("a", 32), strings.Repeat("b", 32), time.Now().UTC(), payload, store.DraftReviewSnapshot{RequestedRaw: localReadPN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), rev, ""); err != nil {
		t.Fatal(err)
	}
	f.onSend = func(_ context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		if msg.GetConversation() != "independent send" {
			t.Error(msg)
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	outbound := app.OutboundSendRequest{Version: 1, StoreRef: dir, RequestID: strings.Repeat("c", 32), OwnPN: f.LinkedJID(), DraftID: rev.DraftID(), RevisionID: rev.ID(), Hash: payload.Hash(), Key: "independent-send"}
	resp, err := delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: outboundSendKind, Outbound: &outbound})
	if err != nil || !resp.OK || f.sends.Load() != 1 || f.states.Load() != 0 {
		t.Fatal("send blocked by recovery", resp, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.states.Load() != 1 || f.connects.Load() != 1 {
		t.Fatal("state ran late or opened another connection", f.states.Load(), f.connects.Load())
	}
}

// Use only fixture SQL, never a linked-device store, to inject a mirror failure.
func TestAgentChatStateIPCSDKCompletedMirrorFailureSanitized(t *testing.T) {
	dir, a, f, r := agentChatStateOwnerFixture(t, true)
	r.Action = app.ChatStateMarkRead
	if err := a.DB().SetChatUnreadCount(r.Requested, 1); err != nil {
		t.Fatal(err)
	}
	f.stateHook = func(_ context.Context, _ appstate.PatchInfo, boundary func()) ([]any, error) {
		boundary()
		db, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
		if err != nil {
			return nil, err
		}
		defer db.Close()
		_, err = db.Exec(`CREATE TRIGGER fail_mirror BEFORE UPDATE OF unread_count ON chats BEGIN SELECT RAISE(ABORT,'SECRET_SQL_CAUSE'); END`)
		return nil, err
	}
	stop, err := startSendDelegateServer(t.Context(), a, sendSpacing{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, err = delegateSend(t.Context(), &rootFlags{storeDir: dir, timeout: time.Second}, sendDelegateRequest{Kind: agentChatStateKind, AgentChatState: &r})
	failure := classifyChatStateAgentError(err, &r)
	var raw strings.Builder
	if err := out.WriteAgentError(&raw, out.AgentAccount{StoreRef: &dir}, out.AgentMeta{Source: "live"}, failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "chat_state_local_mirror_unconfirmed" || failure.ChatState.Outcome != "sdk_completed" || strings.Contains(raw.String(), "SECRET") || f.states.Load() != 1 {
		t.Fatal(raw.String(), err)
	}
}

func TestAgentMarkReadAdditionalPreflight(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, extra := range [][]string{{"--receipts"}, {"--receipts=false"}, {"--timeout", "0"}, {"--timeout", "-1s"}, {"--timeout", "5m1s"}, {"--events"}} {
		t.Run(strings.Join(extra, "/"), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "missing")
			args := append([]string{"--agent", "--store", dir, "chats", "mark-read", "--chat", localReadPN}, extra...)
			stdout, stderr, err := runAgentTest(t, args...)
			e := decodeAgentTest(t, stderr)
			if err == nil || stdout != "" || commandExitCode(err) != 2 || e.Meta.Source != "live" || e.Error.Code != "invalid_arguments" || e.Error.ChatState == nil || e.Error.ChatState.Action != "mark-read" || e.Error.ChatState.Outcome != "not_dispatched" {
				t.Fatal(stdout, stderr, err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("preflight touched store", err)
			}
		})
	}
	t.Setenv("WACLI_READONLY", "1")
	dir := filepath.Join(t.TempDir(), "missing")
	_, stderr, err := runAgentTest(t, "--agent", "--store", dir, "chats", "mark-read", "--chat", localReadPN)
	e := decodeAgentTest(t, stderr)
	if err == nil || e.Error.Code != "read_only" || e.Error.ChatState.Action != "mark-read" || e.Meta.Source != "live" {
		t.Fatal(stderr, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("env policy touched store", err)
	}
}

func TestAgentMarkReadRunnerDefendsReceiptsAndTimeout(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, mode := range []string{"receipts", "zero timeout", "long timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "missing")
			flags := &rootFlags{agent: true, storeDir: dir, timeout: time.Second}
			opts := chatStateOptions{chat: localReadPN}
			switch mode {
			case "receipts":
				opts.receipts = true
			case "zero timeout":
				flags.timeout = 0
			case "long timeout":
				flags.timeout = 6 * time.Minute
			}
			err := runAgentChatState(flags, opts, app.ChatStateMarkRead)
			if err == nil || classifyChatStateAgentError(err, nil).Code != "invalid_arguments" {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("runner touched store", err)
			}
		})
	}
}

func TestAgentMarkReadProductionBinaryStandalone(t *testing.T) {
	binary := os.Getenv("WACLI_CHAT_STATE_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_CHAT_STATE_E2E_BINARY to a freshly built local binary")
	}
	for _, mode := range []string{"readonly", "receipts=false", "missing authentication"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "synthetic-unpaired")
			args := []string{"--agent", "--store", dir, "chats", "mark-read", "--chat", localReadPN}
			switch mode {
			case "readonly":
				args = append(args, "--read-only")
			case "receipts=false":
				args = append(args, "--receipts=false")
			}
			stdout, stderr, err := runDraftBinary(t, binary, args, false)
			e := decodeAgentTest(t, stderr)
			want := map[string]string{"readonly": "read_only", "receipts=false": "invalid_arguments", "missing authentication": "identity_unavailable"}[mode]
			if err == nil || stdout != "" || e.Meta.Source != "live" || e.Error.Code != want || e.Error.ChatState.Action != "mark-read" || e.Error.ChatState.Outcome != "not_dispatched" || e.Error.ChatState.LocalMirror != "unknown" {
				t.Fatal(stdout, stderr, err)
			}
			if mode != "missing authentication" {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("preflight touched store", err)
				}
			}
		})
	}
}

// Copy only this helper into historical sources: their actual decoder and
// executor reject the new request before an App or WhatsApp client is needed.
func TestAgentChatStateHistoricalOwnerHelper(t *testing.T) {
	dir := os.Getenv("WACLI_CHAT_STATE_HISTORICAL_STORE")
	if dir == "" {
		t.Skip("historical owner subprocess helper")
	}
	var calls atomic.Int64
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		calls.Add(1)
		return executeDelegatedSend(ctx, nil, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := os.Stdout.WriteString("R23_HISTORICAL_READY\n"); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if n, err := os.Stdin.Read(signal[:]); err != nil || n != 1 {
		t.Fatal("missing client completion signal", n, err)
	}
	stop() // Drain the historical handler before asserting its invocation count.
	if got := strconv.FormatInt(calls.Load(), 10); got != os.Getenv("WACLI_CHAT_STATE_HISTORICAL_CALLS") {
		t.Fatal("historical executor invocation count", got)
	}
}

func TestAgentMarkReadProductionBinaryHistoricalOwners(t *testing.T) {
	binary := os.Getenv("WACLI_CHAT_STATE_E2E_BINARY")
	raw := os.Getenv("WACLI_CHAT_STATE_HISTORICAL_OWNERS")
	if binary == "" || raw == "" {
		t.Skip("set the production CLI and typed/untyped historical test binaries")
	}
	var owners map[string]string
	if err := json.Unmarshal([]byte(raw), &owners); err != nil || len(owners) != 2 || owners["typed"] == "" || owners["untyped"] == "" {
		t.Fatal("invalid historical fixture executables", err)
	}
	for _, kind := range []string{"typed", "untyped"} {
		t.Run(kind, func(t *testing.T) {
			dir := shortPresenceDelegateStoreDir(t)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lk.Release() })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			owner := exec.CommandContext(ctx, owners[kind], "-test.v", "-test.timeout=8s", "-test.run=^TestAgentChatStateHistoricalOwnerHelper$")
			wantCalls := "0"
			if kind == "untyped" {
				wantCalls = "1"
			}
			owner.Env = append(os.Environ(), "WACLI_CHAT_STATE_HISTORICAL_STORE="+dir, "WACLI_CHAT_STATE_HISTORICAL_CALLS="+wantCalls)
			stdin, err := owner.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var ownerErr strings.Builder
			owner.Stderr = &ownerErr
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			ready, drained := make(chan struct{}), make(chan struct{})
			var ownerOut strings.Builder
			go func() {
				defer close(drained)
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					line := scanner.Text()
					ownerOut.WriteString(line + "\n")
					if line == "R23_HISTORICAL_READY" {
						close(ready)
					}
				}
			}()
			t.Cleanup(func() {
				_, _ = stdin.Write([]byte{1})
				_ = stdin.Close()
				<-drained
				if err := owner.Wait(); err != nil {
					t.Error("historical helper failed", err, ownerOut.String(), ownerErr.String())
				}
				t.Log("historical production handler:", ownerOut.String())
			})
			select {
			case <-ready:
			case <-drained:
				t.Fatal("historical owner did not become ready", ownerOut.String())
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			stdoutText, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "--timeout", "2s", "chats", "mark-read", "--chat", localReadPN}, false)
			e := decodeAgentTest(t, stderr)
			wantCode, wantOutcome, wantExit := "invalid_arguments", "not_dispatched", 2
			if kind == "untyped" {
				wantCode, wantOutcome, wantExit = "chat_state_outcome_uncertain", "uncertain", 1
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != wantExit || stdoutText != "" || e.Meta.Source != "live" || e.Error.Code != wantCode || e.Error.ChatState == nil || e.Error.ChatState.Requested != localReadPN || e.Error.ChatState.Action != "mark-read" || e.Error.ChatState.Outcome != wantOutcome || e.Error.ChatState.LocalMirror != "unknown" {
				t.Fatal(stdoutText, stderr, err)
			}
			for _, name := range []string{"session.db", "wacli.db"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatal("client attempted standalone fallback", name, err)
				}
			}
			t.Log("new production mark-read client:", stderr)
		})
	}
}
