package app

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
)

const chatStatePeerPN = "15550000001@s.whatsapp.net"
const chatStatePeerLID = "300@lid"

type chatStateAgentWA struct {
	*fakeWA
	patchMu   sync.Mutex
	patches   []appstate.PatchInfo
	resolves  atomic.Int64
	sendHook  func(context.Context, func()) ([]any, error)
	fetchHook func(context.Context, string, bool, bool) ([]any, error)
}

func (f *chatStateAgentWA) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID {
	f.resolves.Add(1)
	return jid
}
func (f *chatStateAgentWA) ResolvePNToLID(_ context.Context, jid types.JID) types.JID {
	f.resolves.Add(1)
	return jid
}
func (f *chatStateAgentWA) sendPatch(ctx context.Context, patch appstate.PatchInfo, boundary func()) ([]any, error) {
	f.patchMu.Lock()
	f.patches = append(f.patches, patch)
	f.patchMu.Unlock()
	if f.sendHook != nil {
		return f.sendHook(ctx, boundary)
	}
	boundary()
	return nil, nil
}
func (f *chatStateAgentWA) ArchiveChat(ctx context.Context, jid types.JID, archive bool, ts time.Time, key *waCommon.MessageKey, boundary func()) ([]any, error) {
	return f.sendPatch(ctx, appstate.BuildArchive(jid, archive, ts, key), boundary)
}
func (f *chatStateAgentWA) MarkChatAsRead(ctx context.Context, jid types.JID, read bool, ts time.Time, key *waCommon.MessageKey, boundary func()) ([]any, error) {
	return f.sendPatch(ctx, appstate.BuildMarkChatAsRead(jid, read, ts, key), boundary)
}
func (f *chatStateAgentWA) FetchAppStateEvents(ctx context.Context, name string, full, only bool) ([]any, error) {
	if f.fetchHook != nil {
		return f.fetchHook(ctx, name, full, only)
	}
	return f.fakeWA.FetchAppStateEvents(ctx, name, full, only)
}

func chatStateAgentFixture(t *testing.T) (*App, *chatStateAgentWA, ChatStateRequest) {
	t.Helper()
	f := &chatStateAgentWA{fakeWA: newFakeWA()}
	f.linkedLID = "800@lid"
	dir := t.TempDir()
	opens := 0
	a, err := New(Options{StoreDir: dir, WAFactory: func(opts wa.Options) (WAClient, error) {
		opens++
		if opts.StorePath != filepath.Join(dir, "session.db") || opts.KeyStateStore == nil {
			t.Error("missing factory store scope")
		}
		return f, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if opens != 1 || len(f.handlers) != 0 || f.connected || f.connectCalls != 1 {
			t.Error("client lifetime was not released", opens, len(f.handlers), f.connected, f.connectCalls)
		}
	})
	historyIdentityFixture(t, a, `CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT); INSERT INTO whatsmeow_device VALUES('1234567890:2@s.whatsapp.net','800@lid'); CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE)`)
	if err := a.EnsureAuthed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Connect(t.Context(), false, nil); err != nil {
		t.Fatal(err)
	}
	return a, f, ChatStateRequest{Version: 1, StoreRef: dir, Requested: chatStatePeerPN, Action: ChatStateArchive}
}

func assertChatStateFailure(t *testing.T, err error, code string, outcome ChatStateOutcome) *ChatStateError {
	t.Helper()
	var failure *ChatStateError
	if !errors.As(err, &failure) || failure.Code != code || failure.Result.Outcome != outcome {
		t.Fatalf("failure = %+v, want %s/%s", err, code, outcome)
	}
	return failure
}

func TestAgentChatStateExactTargetsAndSDKShapes(t *testing.T) {
	for _, target := range []struct{ name, requested, stored, mapping string }{
		{"PN only", chatStatePeerPN, chatStatePeerPN, ""},
		{"LID only", chatStatePeerLID, chatStatePeerLID, ""},
		{"mapped PN", chatStatePeerPN, chatStatePeerPN, "INSERT INTO whatsmeow_lid_map VALUES('300','15550000001')"},
		{"mapped LID", chatStatePeerLID, chatStatePeerPN, "INSERT INTO whatsmeow_lid_map VALUES('300','15550000001')"},
		{"group", "123456-789@g.us", "123456-789@g.us", ""},
	} {
		for _, action := range []ChatStateAction{ChatStateMarkUnread, ChatStateArchive, ChatStateUnarchive} {
			t.Run(target.name+"/"+string(action), func(t *testing.T) {
				a, f, r := chatStateAgentFixture(t)
				if target.mapping != "" {
					historyIdentityFixture(t, a, target.mapping)
				}
				r.Requested, r.Action = target.requested, action
				if err := a.DB().SetChatUnreadCount(target.stored, 3); err != nil {
					t.Fatal(err)
				}
				if err := a.DB().SetChatPinned(target.stored, true); err != nil {
					t.Fatal(err)
				}
				if action == ChatStateUnarchive {
					if err := a.DB().SetChatArchived(target.stored, true); err != nil {
						t.Fatal(err)
					}
				}
				result, err := a.ApplyAgentChatState(t.Context(), r)
				if err != nil || ValidateChatStateResult(r, result) != nil || result.Outcome != ChatStateSDKCompleted || result.LocalMirror != ChatStateMirrorPersisted || result.Observation.Target.JID != target.stored || result.Observation.Account.PN != f.LinkedJID() {
					t.Fatal(result, err)
				}
				if len(f.patches) != 1 || f.resolves.Load() != 0 || len(f.readReceiptCalls) != 0 {
					t.Fatal("discovery/retry/receipt", f.patches, f.resolves.Load())
				}
				p := f.patches[0]
				if p.Type != appstate.WAPatchRegularLow || p.Mutations[0].Index[1] != r.Requested {
					t.Fatal("SDK target was retargeted", p)
				}
				if action == ChatStateMarkUnread {
					if p.Mutations[0].Value.GetMarkChatAsReadAction().GetRead() {
						t.Fatal("read patch")
					}
				} else if p.Mutations[0].Value.GetArchiveChatAction().GetArchived() != (action == ChatStateArchive) || (len(p.Mutations) == 2) != (action == ChatStateArchive) {
					t.Fatal("archive/unpin shape", p)
				}
				chat, err := a.DB().GetChat(target.stored)
				if err != nil || !chat.Unread || chat.UnreadCount != 3 || chat.Archived != (action == ChatStateArchive) || chat.Pinned != (action == ChatStateMarkUnread) {
					t.Fatal(chat, err)
				}
				required, err := a.DB().AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
				if err != nil || !required {
					t.Fatal("recovery debt lost", required, err)
				}
			})
		}
	}
}

func TestAgentChatStateObservesAfterRecoveryAndFreezesMirror(t *testing.T) {
	a, f, r := chatStateAgentFixture(t)
	r.Requested = chatStatePeerLID
	historyIdentityFixture(t, a, "INSERT INTO whatsmeow_lid_map VALUES('300','15550000001')")
	f.fetchHook = func(context.Context, string, bool, bool) ([]any, error) {
		historyIdentityFixture(t, a, "UPDATE whatsmeow_lid_map SET pn='15550000002' WHERE lid='300'")
		return nil, nil
	}
	f.sendHook = func(_ context.Context, boundary func()) ([]any, error) {
		boundary()
		historyIdentityFixture(t, a, "UPDATE whatsmeow_lid_map SET pn='15550000003' WHERE lid='300'")
		return nil, nil
	}
	result, err := a.ApplyAgentChatState(t.Context(), r)
	if err != nil || result.Observation.Target.PN != "15550000002@s.whatsapp.net" || f.patches[0].Mutations[0].Index[1] != chatStatePeerLID {
		t.Fatal(result, err)
	}
	chat, err := a.DB().GetChat(result.Observation.Target.JID)
	if err != nil || !chat.Archived || f.resolves.Load() != 0 {
		t.Fatal(chat, err)
	}
	if _, err := a.DB().GetChat("15550000003@s.whatsapp.net"); err == nil {
		t.Fatal("mirror resolved twice")
	}
}

func TestAgentChatStateIdentityFailuresBeforeMutation(t *testing.T) {
	for _, scenario := range []struct{ name, query, requested string }{
		{"SQL error", "DROP TABLE whatsmeow_lid_map", chatStatePeerPN},
		{"account changed", "UPDATE whatsmeow_device SET jid='15550000009@s.whatsapp.net'", chatStatePeerPN},
		{"own LID changed", "UPDATE whatsmeow_device SET lid='801@lid'", chatStatePeerPN},
		{"malformed own LID", "UPDATE whatsmeow_device SET lid='bad alias'", chatStatePeerPN},
		{"own PN map contradicts", "INSERT INTO whatsmeow_lid_map VALUES('801','1234567890')", "1234567890@s.whatsapp.net"},
		{"own LID map contradicts", "INSERT INTO whatsmeow_lid_map VALUES('800','15550000002')", "800@lid"},
		{"ambiguous account", "INSERT INTO whatsmeow_device VALUES('15550000009@s.whatsapp.net',NULL)", chatStatePeerPN},
		{"contradictory reverse", "DROP TABLE whatsmeow_lid_map; CREATE TABLE whatsmeow_lid_map(lid TEXT,pn TEXT); INSERT INTO whatsmeow_lid_map VALUES('301','15550000001'),('300','15550000001')", chatStatePeerLID},
		{"malformed map", "INSERT INTO whatsmeow_lid_map VALUES('bad alias','15550000001')", chatStatePeerPN},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a, f, r := chatStateAgentFixture(t)
			historyIdentityFixture(t, a, scenario.query)
			r.Requested = scenario.requested
			_, err := a.ApplyAgentChatState(t.Context(), r)
			assertChatStateFailure(t, err, "identity_unavailable", ChatStateNotDispatched)
			if len(f.patches) != 0 || f.resolves.Load() != 0 {
				t.Fatal("mutation or fallback on identity failure")
			}
		})
	}
}

func TestAgentChatStateUncertaintyAndMirrorFailure(t *testing.T) {
	for _, mode := range []string{"before boundary error", "boundary timeout", "post fetch error", "SDK nil mirror failure", "SDK nil after cancellation"} {
		t.Run(mode, func(t *testing.T) {
			a, f, r := chatStateAgentFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := a.DB().SetChatUnreadCount(r.Requested, 0); err != nil {
				t.Fatal(err)
			}
			f.sendHook = func(ctx context.Context, boundary func()) ([]any, error) {
				if mode == "before boundary error" {
					return nil, errors.New("SDK preparation error")
				}
				boundary()
				switch mode {
				case "boundary timeout":
					cancel()
					return nil, ctx.Err()
				case "post fetch error":
					return nil, errors.New("post-send app-state fetch failed")
				case "SDK nil mirror failure":
					db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					_, err = db.Exec(`CREATE TRIGGER fail_archive BEFORE UPDATE OF archived ON chats BEGIN SELECT RAISE(ABORT,'SECRET_MIRROR_CAUSE'); END`)
					if err != nil {
						t.Fatal(err)
					}
				case "SDK nil after cancellation":
					cancel()
				}
				return nil, nil
			}
			result, err := a.ApplyAgentChatState(ctx, r)
			if mode == "SDK nil after cancellation" {
				if err != nil || result.Outcome != ChatStateSDKCompleted || result.LocalMirror != ChatStateMirrorPersisted {
					t.Fatal(result, err)
				}
			} else if mode == "SDK nil mirror failure" {
				failure := assertChatStateFailure(t, err, "chat_state_local_mirror_unconfirmed", ChatStateSDKCompleted)
				if failure.Result.LocalMirror != ChatStateMirrorUnconfirmed || failure.Result.Observation == nil {
					t.Fatal(failure)
				}
			} else {
				assertChatStateFailure(t, err, "chat_state_outcome_uncertain", ChatStateUncertain)
			}
			if len(f.patches) != 1 {
				t.Fatal("application repeated mutation", f.patches)
			}
			required, err := a.DB().AppStateRecoveryRequired(string(appstate.WAPatchRegularLow))
			if err != nil || !required {
				t.Fatal("lost debt", required, err)
			}
		})
	}
}

func TestAgentChatStateSerializationAndPreflight(t *testing.T) {
	a, f, r := chatStateAgentFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.fetchHook = func(ctx context.Context, _ string, _ bool, _ bool) ([]any, error) {
		close(started)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { _, err := a.ApplyAgentChatState(t.Context(), r); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err := a.ApplyAgentChatState(ctx, r)
	assertChatStateFailure(t, err, "not_dispatched", ChatStateNotDispatched)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 1 {
		t.Fatal("expired waiter invoked SDK")
	}
	for _, mode := range []string{"invalid", "store", "disconnected", "readonly"} {
		bad := r
		switch mode {
		case "invalid":
			bad.Action = "mark-read"
		case "store":
			bad.StoreRef = filepath.Join(a.StoreDir(), "other")
		case "disconnected":
			f.Disconnect()
		case "readonly":
			a.opts.ReadOnly = true
		}
		_, err := a.ApplyAgentChatState(t.Context(), bad)
		if err == nil || len(f.patches) != 1 {
			t.Fatal(mode, err)
		}
	}
}

func TestAgentChatStateMarkerDoesNotInventCount(t *testing.T) {
	a, _, r := chatStateAgentFixture(t)
	r.Action = ChatStateMarkUnread
	if _, err := a.ApplyAgentChatState(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	chat, err := a.DB().GetChat(r.Requested)
	if err != nil || !chat.Unread || chat.UnreadCount != 0 {
		t.Fatal(chat, err)
	}
}

func TestAgentChatStateUnknownOwnAliasAndLegacyPolicy(t *testing.T) {
	a, f, r := chatStateAgentFixture(t)
	historyIdentityFixture(t, a, `UPDATE whatsmeow_device SET lid=NULL`)
	r.Requested = chatStatePeerLID
	result, err := a.ApplyAgentChatState(t.Context(), r)
	if err != nil || result.Observation.Account.LID != "" || result.Observation.Target.JID != r.Requested || f.resolves.Load() != 0 {
		t.Fatal(result, err)
	}
	historyIdentityFixture(t, a, `UPDATE whatsmeow_device SET lid='malformed'`)
	identities, err := a.ReadHistoryIdentities(t.Context(), []string{r.Requested})
	if err != nil || len(identities) != 1 || identities[0].AccountAliasJID != "" {
		t.Fatal("legacy permissive account read changed", identities, err)
	}
	_, err = a.ApplyAgentChatState(t.Context(), r)
	assertChatStateFailure(t, err, "identity_unavailable", ChatStateNotDispatched)
	if len(f.patches) != 1 {
		t.Fatal("invalid device alias dispatched")
	}
}
