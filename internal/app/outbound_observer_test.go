package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func outboundEventFixture(t *testing.T, group, dispatch bool, configure ...func(*outboundLifecycleFake)) (*App, *outboundLifecycleFake, store.OutboundOperation, *bytes.Buffer) {
	t.Helper()
	a, _, r, adapter, rev := outboundAppFixture(t, store.DraftTextKind)
	var log bytes.Buffer
	a.opts.Events = out.NewEventWriter(&log, true)
	if group {
		e, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: "120363000001@g.us", Message: draftTextPointer("group fixture")}), nil)
		if err != nil {
			t.Fatal(err)
		}
		rev = e.Revision
		r.DraftID, r.RevisionID, r.Hash = rev.DraftID(), rev.ID(), rev.Payload().Hash()
	}
	id, _ := store.NewDraftID()
	o, err := a.DB().Outbound().Reserve(t.Context(), store.OutboundReservation{Version: 1, ID: id, DraftID: r.DraftID, RevisionID: r.RevisionID, Hash: r.Hash, Key: r.Key, Account: rev.Payload().Data().Account, MessageID: "3EB0EVENTFIXTURE", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if dispatch {
		for _, phase := range []store.OutboundPhase{store.OutboundPreparing, store.OutboundDispatchPossible} {
			o, err = a.DB().Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: store.OutboundPending, At: outboundNow(o)})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: adapter}
	for _, configure := range configure {
		configure(f)
	}
	a.opts.WAFactory = func(opts wa.Options) (WAClient, error) {
		if opts.KeyStateStore != a.DB() {
			t.Fatal("another writer")
		}
		return f, nil
	}
	if err := a.OpenWA(); err != nil {
		t.Fatal(err)
	}
	return a, f, o, &log
}

func outboundFixtureReceipt(o store.OutboundOperation, kind types.ReceiptType) *events.Receipt {
	chat, _ := types.ParseJID(o.Recipient.JID)
	actor, _ := types.ParseJID(o.Recipient.LID)
	if chat.Server == types.GroupServer {
		actor = types.NewJID("90003", types.HiddenUserServer)
	}
	actor.Device = 7
	return &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsGroup: chat.Server == types.GroupServer}, MessageIDs: []string{o.MessageID}, Timestamp: time.Unix(1700000000, 0), Type: kind}
}

func outboundEventEntry(t *testing.T, a *App, o store.OutboundOperation) store.OutboundEntry {
	t.Helper()
	e, err := a.DB().Outbound().Read(t.Context(), o.ID, "", "", 200, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestOutboundEventReceiptsMonotonicAndScopes(t *testing.T) {
	a, f, o, _ := outboundEventFixture(t, false, true)
	read := outboundFixtureReceipt(o, types.ReceiptTypeRead)
	read.SenderAlt, _ = types.ParseJID(o.Recipient.PN)
	read.MessageSender, _ = types.ParseJID(o.Account.LID)
	f.emit(read)
	f.emit(read)
	e := outboundEventEntry(t, a, o)
	if len(e.Observations.Items) != 1 || e.Operation.Generation != o.Generation+1 || e.Operation.Result != store.OutboundPending || e.Operation.EvidenceStatus(e.Evidence) != "read" || e.Observations.Items[0].Device != 7 {
		t.Fatal(e)
	}
	o, err := a.DB().Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: e.Operation.Generation, Phase: store.OutboundFinalized, Result: store.OutboundUncertain, ErrorCode: "transport_error", At: outboundNow(e.Operation)})
	if err != nil {
		t.Fatal(err)
	}
	delivery := outboundFixtureReceipt(o, types.ReceiptTypeDelivered)
	delivery.Timestamp = read.Timestamp.Add(-time.Hour)
	f.emit(delivery)
	f.emit(outboundFixtureReceipt(o, types.ReceiptTypeServerError))
	e = outboundEventEntry(t, a, o)
	if e.Operation.Result != store.OutboundUncertain || e.Operation.ErrorCode != "transport_error" || !e.Evidence.ServerError || e.Operation.EvidenceStatus(e.Evidence) != "read" || len(e.Observations.Items) != 3 {
		t.Fatal(e)
	}
	for _, f := range e.Observations.Items {
		if f.Fact == store.OutboundAck || f.EventAt == nil || !f.ObservedAt.After(*f.EventAt) {
			t.Fatal("fabricated ACK/time", f)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*events.Receipt)
	}{
		{"wrong chat", func(v *events.Receipt) { v.Chat = types.NewJID("90009", types.HiddenUserServer) }},
		{"wrong actor", func(v *events.Receipt) { v.Sender = types.NewJID("90009", types.HiddenUserServer) }},
		{"own actor", func(v *events.Receipt) { v.Sender, _ = types.ParseJID(o.Account.PN) }},
		{"own alias", func(v *events.Receipt) { v.SenderAlt, _ = types.ParseJID(o.Account.PN) }},
		{"contradictory alias", func(v *events.Receipt) { v.SenderAlt = types.NewJID("15550000009", types.DefaultUserServer) }},
		{"wrong author", func(v *events.Receipt) { v.MessageSender, _ = types.ParseJID(o.Recipient.PN) }},
		{"own flag", func(v *events.Receipt) { v.IsFromMe = true }},
		{"wrong group flag", func(v *events.Receipt) { v.IsGroup = true }},
		{"foreign integration", func(v *events.Receipt) { v.Sender.Integrator = 1 }},
		{"wrong id", func(v *events.Receipt) { v.MessageIDs = []string{"OTHER"} }},
		{"invalid id", func(v *events.Receipt) { v.MessageIDs = []string{"raw secret / SQL"} }},
		{"read-self", func(v *events.Receipt) { v.Type = types.ReceiptTypeReadSelf }},
		{"sender", func(v *events.Receipt) { v.Type = types.ReceiptTypeSender }},
		{"retry", func(v *events.Receipt) { v.Type = types.ReceiptTypeRetry }},
		{"played", func(v *events.Receipt) { v.Type = types.ReceiptTypePlayed }},
		{"unknown", func(v *events.Receipt) { v.Type = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := outboundFixtureReceipt(o, types.ReceiptTypeRead)
			v.Timestamp = v.Timestamp.Add(time.Second)
			tc.edit(v)
			f.emit(v)
			if after := outboundEventEntry(t, a, o); after.Operation.Generation != e.Operation.Generation {
				t.Fatal("invalid receipt promoted", after)
			}
		})
	}
}

func TestOutboundEventGroupParticipantAliases(t *testing.T) {
	a, f, o, _ := outboundEventFixture(t, true, true)
	first := outboundFixtureReceipt(o, types.ReceiptTypeRead)
	// These fields can be inherited from the grouped parser. Unknown alias stays
	// unasserted; the actual sender remains a separate participant scope.
	first.IsFromMe = true
	first.SenderAlt = types.NewJID("15550000003", types.DefaultUserServer)
	second := *first
	second.Sender = types.NewJID("90004", types.HiddenUserServer)
	second.Sender.Device = 9
	own, _ := types.ParseJID(o.Account.PN)
	ownLID, _ := types.ParseJID(o.Account.LID)
	sdk := whatsmeow.NewClient(&wmstore.Device{ID: &own, LID: ownLID}, nil)
	sdk.AddEventHandler(func(evt any) { f.emit(evt) })
	node := &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{"from": first.Chat, "type": "read", "participant_pn": first.SenderAlt, "t": fmt.Sprint(first.Timestamp.Unix())}, Content: []waBinary.Node{{Tag: "participants", Attrs: waBinary.Attrs{"key": o.MessageID}, Content: []waBinary.Node{
		{Tag: "user", Attrs: waBinary.Attrs{"jid": first.Sender, "t": fmt.Sprint(first.Timestamp.Unix())}},
		{Tag: "user", Attrs: waBinary.Attrs{"jid": second.Sender, "t": fmt.Sprint(first.Timestamp.Unix())}},
	}}}}
	partial, participants, err := sdk.DangerousInternals().ParseReceipt(node)
	if err != nil || len(participants) != 1 {
		t.Fatal(partial, participants, err)
	}
	sdk.DangerousInternals().HandleGroupedReceipt(*partial, &participants[0])
	// Independently exercise a stale own flag with the actual participant intact.
	f.emit(first)
	e := outboundEventEntry(t, a, o)
	if *e.Evidence.ReadParticipants != 2 || *e.Evidence.DeliveredParticipants != 2 || e.Evidence.Read != "unknown" || e.Evidence.Delivered != "unknown" || e.Evidence.Accepted != "observed" {
		t.Fatal(e)
	}
	for _, fact := range e.Observations.Items {
		if fact.ActorAlias != "" {
			t.Fatal("inherited alias merged actors", fact)
		}
	}
	f.lids[first.Sender.ToNonAD()] = first.SenderAlt
	f.emit(first)
	// A verified PN form resolves only the first participant, never the second.
	pn := *first
	pn.Sender, pn.SenderAlt = first.SenderAlt, first.Sender.ToNonAD()
	pn.IsFromMe = false
	f.emit(&pn)
	f.emit(&second) // positive conflicting map rejects the inherited association
	e = outboundEventEntry(t, a, o)
	if *e.Evidence.ReadParticipants != 2 || len(e.Observations.Items) != 4 {
		t.Fatal(e)
	}
	for _, actor := range []string{o.Account.PN, o.Account.LID} {
		own := *first
		own.Sender, _ = types.ParseJID(actor)
		own.IsFromMe = false
		f.emit(&own)
	}
	wrongAuthor := *first
	wrongAuthor.MessageSender = second.Sender
	f.emit(&wrongAuthor)
	if after := outboundEventEntry(t, a, o); after.Operation.Generation != e.Operation.Generation {
		t.Fatal("own/wrong author promoted", after)
	}
}

func outboundHistoryMessage(o store.OutboundOperation) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(o.MessageID), RemoteJID: proto.String(o.Recipient.JID), FromMe: proto.Bool(true)}, Message: &waE2E.Message{Conversation: proto.String("echo fixture")}, MessageTimestamp: proto.Uint64(1700000000)}
}

func TestOutboundEventEchoOriginsAndControls(t *testing.T) {
	a, f, o, _ := outboundEventFixture(t, false, true)
	chat, _ := types.ParseJID(o.Recipient.JID)
	actor, _ := types.ParseJID(o.Account.LID)
	actor.Device = 2
	live := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: o.MessageID, Timestamp: time.Unix(1700000000, 0)}, Message: &waE2E.Message{Conversation: proto.String("echo")}}
	f.emit(live)
	f.emit(live)
	hist := outboundHistoryMessage(o)
	// Native SDK history-shaped events and ordinary history use the same fact source.
	web := *live
	web.SourceWebMsg = hist
	f.emit(&web)
	e := outboundEventEntry(t, a, o)
	if !e.Evidence.OwnEcho || e.Evidence.Accepted != "unknown" || e.Evidence.Delivered != "unknown" || e.Evidence.Read != "unknown" || len(e.Observations.Items) != 2 {
		t.Fatal(e)
	}
	for _, raw := range []*waE2E.Message{
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), EditedMessage: live.Message}},
		{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}},
		{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}},
		{EncReactionMessage: &waE2E.EncReactionMessage{}},
		{EditedMessage: &waE2E.FutureProofMessage{Message: live.Message}},
	} {
		control := *live
		control.RawMessage = raw
		f.emit(&control)
		badHist := proto.Clone(hist).(*waWeb.WebMessageInfo)
		badHist.Message = raw
		web.SourceWebMsg = badHist
		f.emit(&web)
	}
	for _, edit := range []func(*waWeb.WebMessageInfo){
		func(v *waWeb.WebMessageInfo) { v.Key.RemoteJID = proto.String("90009@lid") },
		func(v *waWeb.WebMessageInfo) { v.Key.FromMe = proto.Bool(false) },
		func(v *waWeb.WebMessageInfo) { v.Key.FromMe = nil },
		func(v *waWeb.WebMessageInfo) { v.OriginalSelfAuthorUserJIDString = proto.String(o.Recipient.PN) },
	} {
		bad := proto.Clone(hist).(*waWeb.WebMessageInfo)
		edit(bad)
		web.SourceWebMsg = bad
		f.emit(&web)
	}
	if after := outboundEventEntry(t, a, o); after.Operation.Generation != e.Operation.Generation {
		t.Fatal("control or contradictory key promoted", after)
	}
	for _, dispatch := range []bool{false, true} {
		t.Run(fmt.Sprint("projection/dispatch=", dispatch), func(t *testing.T) {
			a, f, o, _ := outboundEventFixture(t, false, dispatch)
			p := store.DraftPayloadData{Text: &store.DraftText{Text: "local projection"}}
			if err := a.persistOutboundHistory(p, o, whatsmeow.SendResponse{Timestamp: time.Now().UTC(), Sender: actor}, nil); err != nil {
				t.Fatal(err)
			}
			if outboundEventEntry(t, a, o).Evidence.OwnEcho {
				t.Fatal("local projection became echo")
			}
			f.emit(live)
			f.emit(outboundFixtureReceipt(o, types.ReceiptTypeRead))
			e := outboundEventEntry(t, a, o)
			if e.Evidence.OwnEcho != dispatch || (e.Evidence.Read == "observed") != dispatch {
				t.Fatal(e)
			}
		})
	}
}

func TestOutboundEventRealAppBeforeAndAfterACK(t *testing.T) {
	for _, before := range []types.ReceiptType{types.ReceiptTypeRead, types.ReceiptTypeDelivered} {
		for _, outcome := range []store.OutboundResult{store.OutboundAccepted, store.OutboundUncertain, store.OutboundRejected} {
			t.Run(fmt.Sprint(before, "/", outcome), func(t *testing.T) {
				a, _, r, adapter, _ := outboundAppFixture(t, store.DraftTextKind)
				f := &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: adapter}
				a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
				adapter.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
					o, err := a.DB().Outbound().ByMessage(ctx, r.OwnPN, id)
					if err != nil {
						return whatsmeow.SendResponse{}, err
					}
					f.emit(outboundFixtureReceipt(o, before))
					if outcome == store.OutboundUncertain {
						return whatsmeow.SendResponse{}, errors.New("synthetic uncertain result")
					}
					var sendErr error
					if outcome == store.OutboundRejected {
						sendErr = whatsmeow.ErrServerReturnedError
					}
					return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("90001", types.HiddenUserServer), Timestamp: time.Now().UTC()}, sendErr
				}
				result, err := a.SendOutbound(t.Context(), r, nil)
				if (err != nil) != (outcome != store.OutboundAccepted) || result.Entry.Evidence.Delivered != "observed" || (result.Entry.Evidence.Read == "observed") != (before == types.ReceiptTypeRead) || result.Persistence != "confirmed" || adapter.sends != 1 {
					t.Fatal(result, err)
				}
				o := result.Entry.Operation
				f.emit(outboundFixtureReceipt(o, types.ReceiptTypeDelivered))
				f.emit(outboundFixtureReceipt(o, types.ReceiptTypeRead))
				f.emit(outboundFixtureReceipt(o, types.ReceiptTypeServerError))
				entry := outboundEventEntry(t, a, o)
				if entry.Operation.Result != outcome || entry.Evidence.OwnEcho || !entry.Evidence.ServerError || entry.Operation.EvidenceStatus(entry.Evidence) != "read" {
					t.Fatal(entry)
				}
				again, err := a.SendOutbound(t.Context(), r, nil)
				if err != nil || !again.Duplicate || adapter.sends != 1 || f.connectCalls != 1 {
					t.Fatal(again, err, adapter.sends)
				}
			})
		}
	}
}

func TestOutboundEventSyncNativeAndManualHistory(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			a, f, o, _ := outboundEventFixture(t, false, true)
			hs := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(o.Recipient.JID), Messages: []*waHistorySync.HistorySyncMsg{{Message: outboundHistoryMessage(o)}}}}}
			var event any = &events.HistorySync{Data: hs}
			if manual {
				f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) { return hs, nil }
				event = &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}}}}
			}
			f.connectEvents = []any{event, outboundFixtureReceipt(o, types.ReceiptTypeDelivered)}
			res, err := a.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: 15 * time.Millisecond, PresenceMode: SyncPresenceModeQuiet})
			if err != nil || !manual && res.MessagesStored != 1 {
				t.Fatal(res, err)
			}
			entry := outboundEventEntry(t, a, o)
			if !entry.Evidence.OwnEcho || entry.Evidence.Delivered != "observed" || entry.Operation.Result != store.OutboundPending || len(entry.Observations.Items) != 2 || entry.Observations.Items[0].Source != store.OutboundHistoryEcho {
				t.Fatal(entry)
			}
			if _, err := a.DB().GetMessage(o.Recipient.JID, o.MessageID); err != nil {
				t.Fatal("legacy history missing", err)
			}
			ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			page, err := ro.Outbound().List(t.Context(), a.StoreDir(), o.Account.PN, 1, "")
			if err != nil || len(page.Items) != 1 || !page.Items[0].Evidence.OwnEcho || page.Items[0].Evidence.Delivered != "observed" {
				t.Fatal(page, err)
			}
		})
	}
}

func TestOutboundEventAccountCaptureAndPairConflict(t *testing.T) {
	for _, initialAbsent := range []bool{false, true} {
		t.Run(fmt.Sprint(initialAbsent), func(t *testing.T) {
			a, f, o, log := outboundEventFixture(t, false, true, func(f *outboundLifecycleFake) {
				if initialAbsent {
					f.adapter.account = store.DraftIdentity{}
				}
			})
			own, _ := types.ParseJID(o.Account.PN)
			lid, _ := types.ParseJID(o.Account.LID)
			f.emit(&events.PairSuccess{ID: own, LID: lid})
			f.emit(outboundFixtureReceipt(o, types.ReceiptTypeDelivered))
			e := outboundEventEntry(t, a, o)
			if e.Evidence.Delivered != "observed" {
				t.Fatal(e)
			}
			// A mutable client's new identity cannot silently rebind this observer.
			f.adapter.account = store.DraftIdentity{PN: "15550000009@s.whatsapp.net", LID: "90009@lid"}
			f.emit(&events.PairSuccess{ID: own, LID: types.NewJID("90009", types.HiddenUserServer)})
			f.emit(&events.PairSuccess{ID: own, LID: lid})
			f.emit(outboundFixtureReceipt(o, types.ReceiptTypeRead))
			if after := outboundEventEntry(t, a, o); after.Evidence.Read != "unknown" || after.Operation.Generation != e.Operation.Generation || !strings.Contains(log.String(), "outbound_evidence_incomplete") {
				t.Fatal(after, log.String())
			}
		})
	}
}

func TestOutboundEventBoundsFailuresAndLateCallbacks(t *testing.T) {
	a, f, o, log := outboundEventFixture(t, false, true)
	v := outboundFixtureReceipt(o, types.ReceiptTypeRead)
	v.MessageIDs = make([]string, 203)
	for i := range v.MessageIDs {
		v.MessageIDs[i] = fmt.Sprint("NO-MATCH-", i)
	}
	v.MessageIDs[201] = o.MessageID
	f.emit(v)
	if outboundEventEntry(t, a, o).Evidence.Read != "unknown" {
		t.Fatal("cap bypassed")
	}
	var warning struct {
		Data struct{ Examined, Skipped int }
	}
	if err := json.Unmarshal(bytes.TrimSpace(log.Bytes()), &warning); err != nil || warning.Data.Examined != 200 || warning.Data.Skipped != 3 {
		t.Fatal(warning, err, log.String())
	}
	log.Reset()
	looked, released := make(chan struct{}), make(chan struct{})
	realLookup := a.outboundEvents.archive.ByMessage
	a.outboundEvents.archive.ByMessage = func(ctx context.Context, account, id string) (store.OutboundOperation, error) {
		close(looked)
		<-released
		return realLookup(ctx, account, id)
	}
	f.mu.Lock()
	late := f.handlers[a.sessionHandler]
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { f.emit(outboundFixtureReceipt(o, types.ReceiptTypeRead)); close(done) }()
	<-looked
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	<-a.outboundEvents.ctx.Done()
	select {
	case <-closed:
		t.Fatal("Close released DB while lookup was in flight")
	default:
	}
	close(released)
	<-done
	<-closed
	if !strings.Contains(log.String(), "persistence_failures") || strings.Contains(log.String(), "SQL") {
		t.Fatal(log.String())
	}
	late(outboundFixtureReceipt(o, types.ReceiptTypeRead))
	f.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	if err := a.OpenWA(); err == nil {
		t.Fatal("closed app reopened")
	}
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	e, err := ro.Outbound().Read(t.Context(), o.ID, "", "", 20, "")
	if err != nil || e.Evidence.Read != "unknown" || len(e.Observations.Items) != 0 {
		t.Fatal("late write", e, err)
	}
}

func TestOutboundEventPersistenceErrorAndMissingTimestamp(t *testing.T) {
	a, f, o, log := outboundEventFixture(t, false, true)
	a.outboundEvents.archive.Observe = func(context.Context, string, store.DraftIdentity, string, store.OutboundObservation) (store.OutboundOperation, error) {
		return store.OutboundOperation{}, errors.New("SQL raw secret path")
	}
	v := outboundFixtureReceipt(o, types.ReceiptTypeDelivered)
	f.emit(v)
	if outboundEventEntry(t, a, o).Evidence.Delivered != "unknown" || strings.Contains(log.String(), "raw secret") || strings.Count(log.String(), "\n") != 1 {
		t.Fatal(log.String())
	}
	a.outboundEvents.archive.Observe = a.DB().Outbound().Observe
	v.Timestamp = time.Time{}
	f.emit(v)
	e := outboundEventEntry(t, a, o)
	if len(e.Observations.Items) != 1 || e.Observations.Items[0].EventAt != nil {
		t.Fatal(e)
	}
}

func TestOutboundEventHistoryBudgetPreservesLegacyStorage(t *testing.T) {
	a, _, o, log := outboundEventFixture(t, false, true)
	b := a.outboundEvents.batch(t.Context())
	b.cancel() // an expired shared event budget, never renewed for each message
	var stored, last atomic.Int64
	hs := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(o.Recipient.JID), Messages: []*waHistorySync.HistorySyncMsg{{Message: outboundHistoryMessage(o)}, {Message: outboundHistoryMessage(o)}}}}}}
	a.handleHistorySync(t.Context(), SyncOptions{outboundHistory: b}, hs, &stored, &last, func(string, string) {})
	b.finish()
	if stored.Load() != 2 || outboundEventEntry(t, a, o).Evidence.OwnEcho || strings.Count(log.String(), "outbound_evidence_incomplete") != 1 {
		t.Fatal(stored.Load(), log.String())
	}
}

func TestOutboundEventSyncCloseDuringManualHistory(t *testing.T) {
	a, f, o, _ := outboundEventFixture(t, false, true)
	connected := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	syncDone := make(chan error, 1)
	go func() {
		_, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, PresenceMode: SyncPresenceModeQuiet, AfterConnect: func(context.Context) error { close(connected); return nil }})
		syncDone <- err
	}()
	<-connected
	started, release := make(chan struct{}), make(chan struct{})
	f.downloadHistory = func(*waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
		close(started)
		<-release
		return &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(o.Recipient.JID), Messages: []*waHistorySync.HistorySyncMsg{{Message: outboundHistoryMessage(o)}}}}}, nil
	}
	f.mu.Lock()
	var copiedSync func(any)
	for id, handler := range f.handlers {
		if id != a.sessionHandler {
			copiedSync = handler
		}
	}
	f.mu.Unlock()
	if copiedSync == nil {
		t.Fatal("production Sync handler missing")
	}
	notif := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{HistorySyncNotification: &waE2E.HistorySyncNotification{SyncType: waE2E.HistorySyncType_ON_DEMAND.Enum()}}}}
	eventDone := make(chan struct{})
	go func() { f.emit(notif); close(eventDone) }()
	<-started
	closeDone := make(chan struct{})
	go func() { a.Close(); close(closeDone) }()
	<-a.outboundEvents.ctx.Done()
	if _, err := a.DB().Outbound().ByMessage(t.Context(), o.Account.PN, o.MessageID); err != nil {
		t.Fatal("DB closed before legacy callback drain", err)
	}
	select {
	case <-closeDone:
		t.Fatal("Close did not wait for admitted history callback")
	default:
	}
	close(release)
	<-eventDone
	<-closeDone
	// A copied legacy Sync callback is also gated after the DB is released.
	copiedSync(notif)
	copiedSync(&events.HistorySync{Data: &waHistorySync.HistorySync{}})
	copiedSync(outboundFixtureReceipt(o, types.ReceiptTypeRead))
	cancel()
	<-syncDone
	ro, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	entry, err := ro.Outbound().Read(t.Context(), o.ID, "", "", 200, "")
	if err != nil || entry.Evidence.OwnEcho || entry.Evidence.Read != "unknown" {
		t.Fatal("evidence written after cancellation/close", entry, err)
	}
}

func TestOutboundEventTotalBudgetAndLocalPairError(t *testing.T) {
	t.Run("one budget for batch", func(t *testing.T) {
		a, f, o, log := outboundEventFixture(t, false, true)
		var lookups int
		a.outboundEvents.archive.ByMessage = func(ctx context.Context, _, _ string) (store.OutboundOperation, error) {
			lookups++
			<-ctx.Done()
			return store.OutboundOperation{}, ctx.Err()
		}
		v := outboundFixtureReceipt(o, types.ReceiptTypeRead)
		v.MessageIDs = []string{o.MessageID, "NEXT-ID", "LAST-ID"}
		start := time.Now()
		f.emit(v)
		elapsed := time.Since(start)
		if lookups != 1 || elapsed > 4*time.Second || !strings.Contains(log.String(), `"skipped":2`) {
			t.Fatal(lookups, elapsed, log.String())
		}
	})
	t.Run("local SQL failure is not unknown alias", func(t *testing.T) {
		a, f, o, log := outboundEventFixture(t, true, true)
		failed := &outboundPairFailureWA{outboundLifecycleFake: f}
		a.outboundEvents.client = failed
		v := outboundFixtureReceipt(o, types.ReceiptTypeRead)
		v.SenderAlt = types.NewJID("15550000003", types.DefaultUserServer)
		f.emit(v)
		if *outboundEventEntry(t, a, o).Evidence.ReadParticipants != 0 || !strings.Contains(log.String(), `"persistence_failures":1`) || strings.Contains(log.String(), "private SQL") {
			t.Fatal(log.String())
		}
	})
}

type outboundPairFailureWA struct{ *outboundLifecycleFake }

func (*outboundPairFailureWA) CheckPublicPair(context.Context, types.JID, types.JID) (wa.PublicPairResult, error) {
	return wa.PublicPairUnverified, errors.New("private SQL failure")
}

func TestOutboundEventBurstMeasurement(t *testing.T) {
	a, f, first, _ := outboundEventFixture(t, false, true)
	match := outboundFixtureReceipt(first, types.ReceiptTypeDelivered)
	miss := outboundFixtureReceipt(first, types.ReceiptTypeDelivered)
	match.MessageIDs, miss.MessageIDs = nil, nil
	for i := range outboundEventIDs {
		id, _ := store.NewDraftID()
		message := fmt.Sprint("BURST-", i)
		o, err := a.DB().Outbound().Reserve(t.Context(), store.OutboundReservation{Version: 1, ID: id, DraftID: first.DraftID, RevisionID: first.RevisionID, Hash: first.Hash, Account: first.Account, Key: message, MessageID: message, CreatedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []store.OutboundPhase{store.OutboundPreparing, store.OutboundDispatchPossible} {
			o, err = a.DB().Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: store.OutboundPending, At: outboundNow(o)})
			if err != nil {
				t.Fatal(err)
			}
		}
		match.MessageIDs = append(match.MessageIDs, message)
		miss.MessageIDs = append(miss.MessageIDs, "MISSING-"+message)
	}
	start := time.Now()
	f.emit(miss)
	noMatch := time.Since(start)
	start = time.Now()
	f.emit(match)
	matched := time.Since(start)
	retained := 0
	for _, id := range match.MessageIDs {
		o, err := a.DB().Outbound().ByMessage(t.Context(), first.Account.PN, id)
		if err != nil {
			t.Fatal(err)
		}
		if outboundEventEntry(t, a, o).Evidence.Delivered == "observed" {
			retained++
		}
	}
	t.Logf("200 indexed misses: %s; 200 matching FULL observations: %s; retained=%d/200 (environment-specific, best effort)", noMatch, matched, retained)
}

func TestOutboundEventHistoryCandidateCapAndServerErrorOnly(t *testing.T) {
	a, f, o, log := outboundEventFixture(t, false, true)
	f.emit(outboundFixtureReceipt(o, types.ReceiptTypeServerError))
	e := outboundEventEntry(t, a, o)
	if !e.Evidence.ServerError || e.Evidence.Accepted != "unknown" || e.Operation.Result != store.OutboundPending || e.Operation.EvidenceStatus(e.Evidence) != "uncertain" {
		t.Fatal("server error changed attempt or proved acceptance", e)
	}
	log.Reset()
	lookups := 0
	a.outboundEvents.archive.ByMessage = func(context.Context, string, string) (store.OutboundOperation, error) {
		lookups++
		return store.OutboundOperation{}, &store.OutboundError{Code: "not_found"}
	}
	messages := make([]*waHistorySync.HistorySyncMsg, 205)
	for i := range messages {
		messages[i] = &waHistorySync.HistorySyncMsg{Message: outboundHistoryMessage(o)}
	}
	var stored, last atomic.Int64
	a.handleHistorySync(t.Context(), SyncOptions{}, &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(o.Recipient.JID), Messages: messages}}}}, &stored, &last, func(string, string) {})
	if lookups > 200 || lookups == 0 || stored.Load() != 205 || strings.Count(log.String(), "outbound_evidence_incomplete") != 1 || outboundEventEntry(t, a, o).Evidence.OwnEcho {
		t.Fatal(lookups, stored.Load(), log.String())
	}
}

func TestOutboundEventFrozenAccountWrongAndUnknownOwnLID(t *testing.T) {
	t.Run("different client account", func(t *testing.T) {
		a, f, o, _ := outboundEventFixture(t, false, true, func(f *outboundLifecycleFake) {
			f.adapter.account = store.DraftIdentity{PN: "15550000009@s.whatsapp.net", LID: "90009@lid"}
		})
		f.emit(outboundFixtureReceipt(o, types.ReceiptTypeRead))
		if e := outboundEventEntry(t, a, o); len(e.Observations.Items) != 0 {
			t.Fatal("attributed by archive path", e)
		}
	})
	t.Run("group own LID unknown", func(t *testing.T) {
		a := newTestApp(t)
		account := store.DraftIdentity{PN: "15550000001@s.whatsapp.net"}
		payload, err := store.NewDraftPayload(store.DraftPayloadData{Account: account, Recipient: store.DraftRecipient{JID: "120363000001@g.us"}, Kind: store.DraftTextKind, Text: &store.DraftText{Text: "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		draftID, _ := store.NewDraftID()
		revisionID, _ := store.NewDraftID()
		revision, err := store.NewDraftRevision(draftID, revisionID, time.Now().UTC(), payload, store.DraftReviewSnapshot{RequestedRaw: "120363000001@g.us"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.DB().WriteDraft(t.Context(), revision, ""); err != nil {
			t.Fatal(err)
		}
		id, _ := store.NewDraftID()
		o, err := a.DB().Outbound().Reserve(t.Context(), store.OutboundReservation{Version: 1, ID: id, DraftID: draftID, RevisionID: revisionID, Hash: payload.Hash(), Key: "own-lid-unknown", MessageID: "OWN-LID-UNKNOWN", Account: account, CreatedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []store.OutboundPhase{store.OutboundPreparing, store.OutboundDispatchPossible} {
			o, err = a.DB().Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: store.OutboundPending, At: outboundNow(o)})
			if err != nil {
				t.Fatal(err)
			}
		}
		f := &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: &outboundFake{account: account}}
		a.opts.WAFactory = func(wa.Options) (WAClient, error) { return f, nil }
		if err := a.OpenWA(); err != nil {
			t.Fatal(err)
		}
		v := outboundFixtureReceipt(o, types.ReceiptTypeRead)
		f.emit(v)
		if e := outboundEventEntry(t, a, o); *e.Evidence.ReadParticipants != 0 {
			t.Fatal("ambiguous LID actor promoted", e)
		}
		v.Sender = types.NewJID("15550000003", types.DefaultUserServer)
		f.emit(v)
		if e := outboundEventEntry(t, a, o); *e.Evidence.ReadParticipants != 1 {
			t.Fatal("known non-own PN discarded", e)
		}
	})
}
