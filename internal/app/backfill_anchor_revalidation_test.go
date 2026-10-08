package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Local identity facts can change while an output writer is blocked. Resolver
// calls are counted separately so these tests cannot hide a network fallback.
type explicitScopeWA struct {
	*fakeWA
	scopeMu                         sync.Mutex
	pn, alias                       types.JID
	account                         string
	lookupErr, pairErr              error
	pair                            wa.PublicPairResult
	lookups, checks, remoteResolves int
}

func (f *explicitScopeWA) LinkedJID() string {
	f.scopeMu.Lock()
	defer f.scopeMu.Unlock()
	if f.account != "" {
		return f.account
	}
	return f.fakeWA.LinkedJID()
}
func (f *explicitScopeWA) LookupLocalAlias(ctx context.Context, jid types.JID) (types.JID, error) {
	f.scopeMu.Lock()
	defer f.scopeMu.Unlock()
	f.lookups++
	if jid == f.pn {
		return f.alias, f.lookupErr
	}
	return f.fakeWA.LookupLocalAlias(ctx, jid)
}
func (f *explicitScopeWA) CheckPublicPair(ctx context.Context, a, b types.JID) (wa.PublicPairResult, error) {
	f.scopeMu.Lock()
	defer f.scopeMu.Unlock()
	if a == f.pn && b == f.alias || b == f.pn && a == f.alias {
		f.checks++
		return f.pair, f.pairErr
	}
	return f.fakeWA.CheckPublicPair(ctx, a, b)
}
func (f *explicitScopeWA) ResolvePNToLID(ctx context.Context, jid types.JID) types.JID {
	f.scopeMu.Lock()
	f.remoteResolves++
	f.scopeMu.Unlock()
	return f.fakeWA.ResolvePNToLID(ctx, jid)
}

type explicitRequestWriter func([]byte) (int, error)

func (w explicitRequestWriter) Write(b []byte) (int, error) { return w(b) }

func TestExplicitBackfillRevalidatesAfterRequesting(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, change := range []string{"tombstone", "account", "scope_added", "scope_lost", "contradictory", "lookup_error", "pair_error", "cancel"} {
			t.Run(map[bool]string{false: "standalone", true: "owner"}[owner]+"/"+change, func(t *testing.T) {
				a, baseWA, _, base, opts := newExplicitBackfillTest(t)
				pn := mustTestJID(t, "15550000001@s.whatsapp.net")
				lid := mustTestJID(t, "100000000001@lid")
				f := &explicitScopeWA{fakeWA: baseWA, pn: pn, pair: wa.PublicPairVerified}
				if change == "scope_lost" {
					f.alias = lid
				}
				a.wa = f
				opts.ChatJID = pn.String()
				if err := a.db.UpsertChat(opts.ChatJID, "dm", "fixture", base); err != nil {
					t.Fatal(err)
				}
				m := storeUpsertMessage(opts.ChatJID, "selected", base, "anchor")
				m.SenderJID = pn.String()
				if err := a.db.UpsertMessage(m); err != nil {
					t.Fatal(err)
				}
				if owner {
					startBackfillFollow(t, a)
				}
				db, err := sql.Open("sqlite3", filepath.Join(a.StoreDir(), "wacli.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				failure := errors.New("fixture local identity read failed")
				mutated := false
				a.opts.Events = out.NewEventWriter(explicitRequestWriter(func(b []byte) (int, error) {
					if bytes.Contains(b, []byte(`"event":"backfill_requesting"`)) {
						// The conservative checkpoint is already visible, before the SDK call.
						pending := readAttempt(t, a, opts.ChatJID).Latest
						if !pending.DispatchPossible || pending.RequestsSent != 0 || pending.Phase != store.HistoryDispatchPossible {
							t.Errorf("pending=%+v", pending)
						}
						f.scopeMu.Lock()
						switch change {
						case "tombstone":
							if _, err := db.Exec("UPDATE messages SET deleted_for_me=1,deleted_at=1 WHERE chat_jid=? AND msg_id='selected'", opts.ChatJID); err != nil {
								t.Fatal(err)
							}
						case "account":
							f.account = "15550000009@s.whatsapp.net"
						case "scope_added":
							f.alias = lid
						case "scope_lost":
							f.alias = types.JID{}
						case "contradictory":
							f.alias = lid
							f.pair = wa.PublicPairContradictory
						case "lookup_error":
							f.lookupErr = failure
						case "pair_error":
							f.alias = lid
							f.pairErr = failure
						case "cancel":
							cancel()
						}
						f.scopeMu.Unlock()
						mutated = true
					}
					return len(b), nil
				}), true)
				calls := 0
				f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync { calls++; return nil }
				run := a.BackfillHistory
				if owner {
					run = a.BackfillHistoryConnected
				}
				res, err := run(ctx, opts)
				var typed *BackfillError
				if !mutated || !errors.As(err, &typed) || typed.History.Outcome != "not_dispatched" || typed.History.Phase != store.HistoryObserving || calls != 0 || res.RequestsSent != 0 {
					t.Fatalf("mutated=%v calls=%d res=%+v err=%v", mutated, calls, res, err)
				}
				code := "store_state"
				if change == "tombstone" {
					code = "no_local_anchor"
				}
				if change == "cancel" {
					code = "cancelled"
				}
				if typed.History.Code != code {
					t.Errorf("failure=%+v", typed.History)
				}
				if (change == "lookup_error" || change == "pair_error") && !errors.Is(err, failure) {
					t.Errorf("read error swallowed: %v", err)
				}
				final := readAttempt(t, a, opts.ChatJID).Latest
				if final.DispatchPossible || final.RequestsSent != 0 || final.ResponsesSeen != 0 || !final.CountersFinal || final.ErrorCode != code {
					t.Errorf("final=%+v", final)
				}
				f.scopeMu.Lock()
				remote := f.remoteResolves
				f.scopeMu.Unlock()
				if remote != 0 || f.connectCalls != 1 {
					t.Errorf("remote=%d connects=%d", remote, f.connectCalls)
				}
			})
		}
	}
}

func TestExplicitBackfillScopeLocalOnly(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, mode := range []string{"missing", "verified", "contradictory", "unverified", "lookup_error", "pair_error"} {
			t.Run(map[bool]string{false: "standalone", true: "owner"}[owner]+"/"+mode, func(t *testing.T) {
				a, baseWA, _, base, opts := newExplicitBackfillTest(t)
				pn := mustTestJID(t, "15550000001@s.whatsapp.net")
				lid := mustTestJID(t, "100000000001@lid")
				f := &explicitScopeWA{fakeWA: baseWA, pn: pn, pair: wa.PublicPairVerified}
				failure := errors.New("fixture SQL read failure")
				switch mode {
				case "verified":
					f.alias = lid
				case "contradictory":
					f.alias = lid
					f.pair = wa.PublicPairContradictory
				case "unverified":
					f.alias = lid
					f.pair = wa.PublicPairUnverified
				case "lookup_error":
					f.lookupErr = failure
				case "pair_error":
					f.alias = lid
					f.pairErr = failure
				}
				a.wa = f
				opts.ChatJID = pn.String()
				if err := a.db.UpsertChat(opts.ChatJID, "dm", "fixture", base); err != nil {
					t.Fatal(err)
				}
				m := storeUpsertMessage(opts.ChatJID, "selected", base, "anchor")
				m.SenderJID = pn.String()
				if err := a.db.UpsertMessage(m); err != nil {
					t.Fatal(err)
				}
				if owner {
					startBackfillFollow(t, a)
				}
				calls := 0
				f.onDemandHistory = func(info types.MessageInfo, _ int) *events.HistorySync {
					calls++
					if info.Chat != pn {
						t.Errorf("wire chat changed: %s", info.Chat)
					}
					responseChat := pn.String()
					if mode == "verified" {
						responseChat = lid.String()
					}
					hs := backfillTestResponse(responseChat, "empty", base)
					hs.Data.Conversations[0].Messages = nil
					return hs
				}
				run := a.BackfillHistory
				if owner {
					run = a.BackfillHistoryConnected
				}
				res, err := run(t.Context(), opts)
				valid := mode == "missing" || mode == "verified"
				if valid {
					if err != nil || calls != 1 || res.StopReason != BackfillStopEmptyResponse {
						t.Fatalf("calls=%d res=%+v err=%v", calls, res, err)
					}
					wantAlias := ""
					if mode == "verified" {
						wantAlias = lid.String()
					}
					if res.Evidence.WindowAliasJID != wantAlias {
						t.Errorf("scope=%+v", res.Evidence)
					}
				} else {
					var typed *BackfillError
					if !errors.As(err, &typed) || typed.History.Code != "store_state" || typed.History.Outcome != "not_dispatched" || calls != 0 {
						t.Fatalf("calls=%d err=%v", calls, err)
					}
					if (mode == "lookup_error" || mode == "pair_error") && !errors.Is(err, failure) {
						t.Errorf("read error swallowed: %v", err)
					}
				}
				f.scopeMu.Lock()
				remote, checks := f.remoteResolves, f.checks
				f.scopeMu.Unlock()
				if remote != 0 {
					t.Errorf("remote resolver called %d", remote)
				}
				if mode != "missing" && mode != "lookup_error" && checks == 0 {
					t.Error("candidate not corroborated")
				}
				wantConnect := 0
				if owner || valid {
					wantConnect = 1
				}
				if f.connectCalls != wantConnect {
					t.Errorf("connects=%d want=%d", f.connectCalls, wantConnect)
				}
			})
		}
	}
}

func TestExplicitBackfillScopeFailureAfterCallRemainsUncertain(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "owner"}[owner], func(t *testing.T) {
			a, baseWA, chat, base, opts := newExplicitBackfillTest(t)
			f := &explicitScopeWA{fakeWA: baseWA}
			a.wa = f
			if owner {
				startBackfillFollow(t, a)
			}
			f.onDemandHistory = func(types.MessageInfo, int) *events.HistorySync {
				f.scopeMu.Lock()
				f.account = "15550000009@s.whatsapp.net"
				f.scopeMu.Unlock()
				return backfillTestResponse(chat, "gap", base.Add(-1e9))
			}
			run := a.BackfillHistory
			if owner {
				run = a.BackfillHistoryConnected
			}
			_, err := run(t.Context(), opts)
			var typed *BackfillError
			if !errors.As(err, &typed) || typed.History.Outcome != "uncertain" || typed.History.Code != "backfill_outcome_uncertain" {
				t.Fatalf("failure=%v", err)
			}
			final := readAttempt(t, a, chat).Latest
			if !final.DispatchPossible || final.RequestsSent != 1 || !final.CountersFinal {
				t.Fatalf("prior invocation=%+v", final)
			}
		})
	}
}
