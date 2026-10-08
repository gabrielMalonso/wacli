package app

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func newExplicitBackfillTest(t *testing.T) (*App, *fakeWA, string, time.Time, BackfillOptions) {
	t.Helper()
	a, f, chat, base := newBackfillRetryTest(t)
	for _, m := range []store.UpsertMessageParams{
		storeUpsertMessage(chat, "global-oldest", base.Add(-365*24*time.Hour), "old"),
		storeUpsertMessage(chat, "selected", base, "anchor"),
	} {
		m.SenderJID = "15550000001@s.whatsapp.net"
		if err := a.db.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	opts := backfillRetryOptions(chat)
	opts.BeforeID = "selected"
	opts.WaitPerRequest = time.Second
	return a, f, chat, base, opts
}

func TestExplicitBackfillRecoversWindowWithoutMovingGlobalOldest(t *testing.T) {
	for _, connected := range []bool{false, true} {
		for _, fromMe := range []bool{false, true} {
			t.Run(map[bool]string{false: "standalone", true: "owner"}[connected]+map[bool]string{false: "_incoming", true: "_own"}[fromMe], func(t *testing.T) {
				a, f, chat, base, opts := newExplicitBackfillTest(t)
				sender := "15550000001@s.whatsapp.net"
				if fromMe {
					sender = f.LinkedJID()
					m := storeUpsertMessage(chat, "selected", base, "anchor")
					m.FromMe = true
					m.SenderJID = sender
					if err := a.db.UpsertMessage(m); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				f.onDemandHistory = func(info types.MessageInfo, count int) *events.HistorySync {
					calls++
					want := types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("123", types.GroupServer), Sender: mustTestJID(t, sender), IsFromMe: fromMe, IsGroup: true}, ID: "selected", Timestamp: base}
					if !reflect.DeepEqual(info, want) || count != 50 {
						t.Errorf("SDK input=%+v want=%+v count=%d", info, want, count)
					}
					// Verify the pinned SDK wire fields, including its seconds-in-MS field.
					req := (&whatsmeow.Client{}).BuildHistorySyncRequest(&info, count).GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
					if req.GetOldestMsgID() != "selected" || req.GetOldestMsgFromMe() != fromMe || req.GetChatJID() != chat || req.GetOldestMsgTimestampMS() != base.Unix() {
						t.Errorf("wrong SDK request: %+v", req)
					}
					return backfillTestResponse(chat, "gap", base.Add(-time.Minute))
				}
				run := a.BackfillHistory
				if connected {
					startBackfillFollow(t, a)
					run = a.BackfillHistoryConnected
				}
				res, err := run(context.Background(), opts)
				if err != nil || res.StopReason != BackfillStopRequestedBatchLimit || res.MessagesAdded != 1 || res.MessagesAddedBefore == nil || *res.MessagesAddedBefore != 1 || res.BeforeID != "selected" || calls != 1 {
					t.Fatalf("result=%+v calls=%d err=%v", res, calls, err)
				}
				oldest, err := a.db.GetOldestMessageInfo(chat)
				if err != nil || oldest.MsgID != "global-oldest" {
					t.Fatalf("oldest=%+v err=%v", oldest, err)
				}
				if res.Evidence == nil || res.Evidence.FirstAnchorID != "selected" || res.Evidence.LastAnchorID != "selected" || res.Evidence.PreparedRequestChatJID != chat {
					t.Fatalf("evidence=%+v", res.Evidence)
				}
				if connected {
					assertBackfillOwnerHealthy(t, a, f, chat, base)
				}
			})
		}
	}
}

func mustTestJID(t *testing.T, raw string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return jid
}

func TestExplicitBackfillInvalidAnchorsNeverConnectOrDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, sql    string
		before, chat string
		readonly     bool
	}{
		{name: "missing", before: "missing"},
		{name: "wrong chat", chat: "124@g.us"},
		{name: "invalid from_me", sql: "UPDATE messages SET from_me=2 WHERE msg_id='selected'"},
		{name: "invalid time", sql: "UPDATE messages SET ts=0 WHERE msg_id='selected'"},
		{name: "negative time", sql: "UPDATE messages SET ts=-1 WHERE msg_id='selected'"},
		{name: "missing sender", sql: "UPDATE messages SET sender_jid='' WHERE msg_id='selected'"},
		{name: "invalid sender", sql: "UPDATE messages SET sender_jid='bogus@lid' WHERE msg_id='selected'"},
		{name: "wrong account from_me", sql: "UPDATE messages SET from_me=1 WHERE msg_id='selected'"},
		{name: "incoming own sender", sql: "UPDATE messages SET sender_jid='1234567890@s.whatsapp.net' WHERE msg_id='selected'"},
		{name: "revoked", sql: "UPDATE messages SET revoked=1 WHERE msg_id='selected'"},
		{name: "deleted", sql: "UPDATE messages SET deleted_for_me=1,deleted_at=1 WHERE msg_id='selected'"},
		{name: "purged", sql: "UPDATE messages SET payload_purged_at=1 WHERE msg_id='selected'"},
		{name: "invalid chat", chat: "oops@invalid"},
		{name: "readonly", readonly: true},
	} {
		for _, connected := range []bool{false, true} {
			if connected && tc.readonly {
				continue
			}
			t.Run(tc.name+map[bool]string{false: "_standalone", true: "_owner"}[connected], func(t *testing.T) {
				a, f, _, _, opts := newExplicitBackfillTest(t)
				if connected {
					startBackfillFollow(t, a)
				}
				a.opts.ReadOnly = tc.readonly
				if tc.before != "" {
					opts.BeforeID = tc.before
				}
				if tc.chat != "" {
					opts.ChatJID = tc.chat
				}
				if tc.sql != "" {
					db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.Exec(tc.sql); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { calls++; return nil }
				run := a.BackfillHistory
				if connected {
					run = a.BackfillHistoryConnected
				}
				_, err := run(context.Background(), opts)
				wantConnect := 0
				if connected {
					wantConnect = 1
				}
				if err == nil || calls != 0 || f.connectCalls != wantConnect {
					t.Fatalf("err=%v dispatch=%d connects=%d", err, calls, f.connectCalls)
				}
			})
		}
	}
}

func TestExplicitBackfillResponseLimits(t *testing.T) {
	for _, scenario := range []string{"duplicate", "newer", "same second", "empty", "primary", "timeout", "cancel", "transport", "out_of_order"} {
		t.Run(scenario, func(t *testing.T) {
			a, f, chat, base, opts := newExplicitBackfillTest(t)
			startBackfillFollow(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts.WaitPerRequest = 10 * time.Millisecond
			if scenario == "out_of_order" {
				opts.IdleExit = 20 * time.Millisecond
			}
			calls := 0
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				calls++
				id, ts := "new", base.Add(-time.Minute)
				switch scenario {
				case "timeout":
					return nil
				case "cancel":
					cancel()
					return nil
				case "duplicate":
					id = "global-oldest"
					ts = base.Add(-365 * 24 * time.Hour)
				case "newer":
					ts = base.Add(time.Minute)
				case "same second":
					ts = base
				case "out_of_order":
					go func() {
						time.Sleep(5 * time.Millisecond)
						f.emit(backfillTestResponse(chat, "gap-after", base.Add(-time.Minute)))
					}()
					return backfillTestResponse(chat, "later-first", base.Add(time.Minute))
				}
				hs := backfillTestResponse(chat, id, ts)
				if scenario == "empty" || scenario == "primary" {
					hs.Data.Conversations[0].Messages = nil
				}
				if scenario == "primary" {
					hs.Data.Conversations[0].EndOfHistoryTransferType = waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY.Enum()
				}
				return hs
			}
			if scenario == "transport" {
				f.onDemandErr = errors.New("offline transport fixture")
			}
			res, err := a.BackfillHistoryConnected(ctx, opts)
			if scenario == "timeout" || scenario == "cancel" || scenario == "transport" {
				var typed *BackfillError
				if !errors.As(err, &typed) || typed.History.Outcome != "uncertain" {
					t.Fatalf("result=%+v err=%v", res, err)
				}
			} else {
				want := BackfillStopNoProgress
				added := int64(0)
				switch scenario {
				case "empty":
					want = BackfillStopEmptyResponse
				case "primary":
					want = BackfillStopPrimaryNoMore
				case "out_of_order":
					want = BackfillStopRequestedBatchLimit
					added = 1
				}
				if err != nil || res.StopReason != want || res.MessagesAddedBefore == nil || *res.MessagesAddedBefore != added {
					t.Fatalf("result=%+v err=%v", res, err)
				}
			}
			if calls > 1 {
				t.Fatalf("explicit anchor retried %d times", calls)
			}
		})
	}
}

func TestExplicitBackfillPreservesPNLIDIdentityWithoutRetry(t *testing.T) {
	for _, raw := range []string{"15550000001@s.whatsapp.net", "100000000001@lid"} {
		t.Run(raw, func(t *testing.T) {
			a, f, _, base, opts := newExplicitBackfillTest(t)
			opts.ChatJID = raw
			if err := a.db.UpsertChat(raw, "dm", "fixture", base); err != nil {
				t.Fatal(err)
			}
			m := storeUpsertMessage(raw, "selected", base, "anchor")
			if err := a.db.UpsertMessage(m); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(raw, "@s.whatsapp.net") {
				// Even a known alias must not substitute the requested PN on the wire.
				f.lids[types.NewJID("100000000001", types.HiddenUserServer)] = mustTestJID(t, raw)
			}
			startBackfillFollow(t, a)
			calls := 0
			f.onDemandHistory = func(info types.MessageInfo, _ int) *events.HistorySync {
				calls++
				if info.Chat.String() != raw || info.Sender.String() != raw {
					t.Errorf("identity substituted: %+v", info)
				}
				return backfillTestResponse(raw, "gap", base.Add(-time.Minute))
			}
			res, err := a.BackfillHistoryConnected(t.Context(), opts)
			if err != nil || calls != 1 || res.StopReason != BackfillStopRequestedBatchLimit {
				t.Fatalf("res=%+v calls=%d err=%v", res, calls, err)
			}
		})
	}
}

func TestExplicitBackfillStoreFailureNeverConnects(t *testing.T) {
	a, f, _, _, opts := newExplicitBackfillTest(t)
	fixture, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	// A corrupt fixture schema must fail, rather than look like an absent row.
	if _, err := fixture.Exec(`ALTER TABLE messages RENAME TO fixture_hidden_messages`); err != nil {
		t.Fatal(err)
	}
	_, err = a.BackfillHistory(t.Context(), opts)
	var typed *BackfillError
	if !errors.As(err, &typed) || typed.History.Code != "store_state" || typed.History.Outcome != "not_dispatched" || f.connectCalls != 0 {
		t.Fatalf("error=%v connects=%d", err, f.connectCalls)
	}
}
