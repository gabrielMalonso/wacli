package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const historyOwnPN = "15550000001@s.whatsapp.net"
const historyOwnLID = "90001@lid"
const historyPeerPN = "15550000002@s.whatsapp.net"
const historyPeerLID = "90002@lid"

type historySenderWA struct {
	*fakeWA
	pn, lid      string
	pairErr      error
	pairCalls    int
	cryptoSender types.JID
	resolveCalls int
	unstableLID  bool
}

func newHistorySenderWA() *historySenderWA {
	f := &historySenderWA{fakeWA: newFakeWA(), pn: historyOwnPN, lid: historyOwnLID}
	for lid, pn := range map[string]string{historyOwnLID: historyOwnPN, historyPeerLID: historyPeerPN} {
		l, _ := types.ParseJID(lid)
		p, _ := types.ParseJID(pn)
		f.lids[l] = p
	}
	return f
}

func (f *historySenderWA) LinkedJID() string { return f.pn }
func (f *historySenderWA) LinkedLID() string { return f.lid }
func (f *historySenderWA) ResolveLIDToPN(ctx context.Context, jid types.JID) types.JID {
	if jid.String() == historyPeerLID {
		f.resolveCalls++
		if f.unstableLID {
			if f.resolveCalls == 1 {
				return jid
			}
			return types.NewJID("15550000003", types.DefaultUserServer)
		}
	}
	return f.fakeWA.ResolveLIDToPN(ctx, jid)
}
func (f *historySenderWA) CheckPublicPair(ctx context.Context, first, second types.JID) (wa.PublicPairResult, error) {
	f.pairCalls++
	if f.pairErr != nil {
		return wa.PublicPairUnverified, f.pairErr
	}
	return f.fakeWA.CheckPublicPair(ctx, first, second)
}

func (f *historySenderWA) ParseWebMessage(chat types.JID, hist *waWeb.WebMessageInfo) (*events.Message, error) {
	// Exercise SDK metadata parsing without initialization, keys or a connection.
	d := &wastore.Device{}
	if f.pn != "" {
		pn, _ := types.ParseJID(f.pn)
		d.ID = &pn
	}
	d.LID, _ = types.ParseJID(f.lid)
	return (&whatsmeow.Client{Store: d}).ParseWebMessage(chat, hist)
}

func (f *historySenderWA) DecryptReaction(ctx context.Context, evt *events.Message) (*waE2E.ReactionMessage, error) {
	f.cryptoSender = evt.Info.Sender
	return f.fakeWA.DecryptReaction(ctx, evt)
}

func historySenderMessage(chat string, fromMe bool) *waWeb.WebMessageInfo {
	return &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{RemoteJID: proto.String(chat), FromMe: proto.Bool(fromMe), ID: proto.String("synthetic-history")},
		MessageTimestamp: proto.Uint64(1700000000), Message: &waE2E.Message{Conversation: proto.String("synthetic text")},
	}
}

func importHistorySender(t *testing.T, a *App, chat string, messages ...*waWeb.WebMessageInfo) (int64, []error) {
	t.Helper()
	var batch []*waHistorySync.HistorySyncMsg
	for _, m := range messages {
		batch = append(batch, &waHistorySync.HistorySyncMsg{Message: m})
	}
	var stored, last atomic.Int64
	var failures []error
	a.handleHistorySync(t.Context(), SyncOptions{historyStoreError: func(_ types.JID, err error) { failures = append(failures, err) }},
		&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat), Messages: batch}}}},
		&stored, &last, func(string, string) {})
	return stored.Load(), failures
}

func TestHistorySenderParserPersistDraft(t *testing.T) {
	a := draftAppFixture(t)
	f := newHistorySenderWA()
	a.wa = f
	m := historySenderMessage(historyPeerPN, true)
	m.Starred = proto.Bool(true)
	if parsed := wa.ParseHistoryMessage(historyPeerPN, m); parsed.SenderJID != "" {
		t.Fatal("parser guessed own identity", parsed.SenderJID)
	}
	if count, failures := importHistorySender(t, a, historyPeerPN, m); count != 1 || len(failures) != 0 {
		t.Fatal(count, failures)
	}
	row, err := a.DB().GetMessage(historyPeerPN, m.GetKey().GetID())
	if err != nil || row.SenderJID != historyOwnPN || !row.FromMe || !row.Starred || row.Text != "synthetic text" {
		t.Fatalf("stored: %+v, %v", row, err)
	}
	a.wa = nil
	entry, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: historyPeerPN, Message: draftTextPointer("synthetic reply"), ReplyTo: row.MsgID}), os.Open)
	if err != nil || entry.Revision.Payload().Data().Reply.Sender.PN != historyOwnPN {
		t.Fatal("own quote", err)
	}
}

func TestHistorySenderImportMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, chat, participant, keyParticipant, original, wantSender string
		fromMe, refused                                               bool
		configure                                                     func(*historySenderWA)
	}{
		{name: "own DM", chat: historyPeerPN, fromMe: true, wantSender: historyOwnPN},
		{name: "own LID DM", chat: historyPeerLID, fromMe: true, wantSender: historyOwnPN},
		{name: "own group", chat: "12345@g.us", fromMe: true, wantSender: historyOwnPN},
		{name: "explicit own PN", chat: historyPeerPN, fromMe: true, participant: historyOwnPN, wantSender: historyOwnPN},
		{name: "explicit own LID", chat: historyPeerPN, fromMe: true, participant: historyOwnLID, wantSender: historyOwnPN},
		{name: "original own LID", chat: historyPeerPN, fromMe: true, original: historyOwnLID, wantSender: historyOwnPN},
		{name: "own unavailable", chat: historyPeerPN, fromMe: true, configure: func(f *historySenderWA) { f.pn, f.lid = "", "" }},
		{name: "own PN absent", chat: historyPeerLID, fromMe: true, configure: func(f *historySenderWA) { f.pn = "" }},
		{name: "own LID absent", chat: historyPeerLID, fromMe: true, wantSender: historyOwnPN, configure: func(f *historySenderWA) { f.lid = "" }},
		{name: "invalid own", chat: historyPeerPN, fromMe: true, refused: true, configure: func(f *historySenderWA) { f.pn = "@s.whatsapp.net" }},
		{name: "invalid own LID", chat: historyPeerPN, fromMe: true, refused: true, configure: func(f *historySenderWA) { f.lid = "not-a-number@lid" }},
		{name: "contradictory own map", chat: historyPeerPN, fromMe: true, refused: true, configure: func(f *historySenderWA) {
			l, _ := types.ParseJID(historyOwnLID)
			p, _ := types.ParseJID(historyPeerPN)
			f.lids[l] = p
		}},
		{name: "SQL error", chat: historyPeerPN, fromMe: true, refused: true, configure: func(f *historySenderWA) { f.pairErr = errors.New("synthetic SQL failure") }},
		{name: "unverified own LID", chat: historyPeerPN, fromMe: true, participant: historyOwnLID, configure: func(f *historySenderWA) { f.lids = map[types.JID]types.JID{} }},
		{name: "peer is not own", chat: historyPeerPN, fromMe: true, participant: historyPeerPN, refused: true},
		{name: "key peer is not own", chat: historyPeerPN, fromMe: true, keyParticipant: historyPeerPN, refused: true},
		{name: "original peer is not own", chat: historyPeerPN, fromMe: true, original: historyPeerPN, refused: true},
		{name: "explicit authors conflict", chat: historyPeerPN, fromMe: true, participant: historyOwnPN, keyParticipant: historyPeerPN, refused: true},
		{name: "incoming DM", chat: historyPeerPN, wantSender: historyPeerPN},
		{name: "incoming without own", chat: historyPeerPN, wantSender: historyPeerPN, configure: func(f *historySenderWA) { f.pn, f.lid = "", "" }},
		{name: "incoming explicit alias", chat: historyPeerPN, participant: historyPeerLID, wantSender: historyPeerPN},
		{name: "incoming alias SQL error", chat: historyPeerPN, participant: historyPeerLID, refused: true, configure: func(f *historySenderWA) { f.lid = ""; f.pairErr = errors.New("synthetic SQL failure") }},
		{name: "incoming contradicts own", chat: historyPeerPN, participant: historyOwnPN, refused: true},
		{name: "incoming group", chat: "12345@g.us", participant: historyPeerLID, wantSender: historyPeerPN},
		{name: "group missing author", chat: "12345@g.us"},
		{name: "group key author", chat: "12345@g.us", keyParticipant: historyPeerPN, wantSender: historyPeerPN},
		{name: "group authors conflict", chat: "12345@g.us", participant: historyPeerPN, keyParticipant: historyOwnPN, refused: true},
		{name: "group cannot be author", chat: "12345@g.us", participant: "12345@g.us", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			f := newHistorySenderWA()
			if tc.configure != nil {
				tc.configure(f)
			}
			group, _ := types.ParseJID(tc.chat)
			f.groups[group] = &types.GroupInfo{JID: group}
			a.wa = f
			m := historySenderMessage(tc.chat, tc.fromMe)
			m.Participant, m.Key.Participant, m.OriginalSelfAuthorUserJIDString = proto.String(tc.participant), proto.String(tc.keyParticipant), proto.String(tc.original)
			count, failures := importHistorySender(t, a, tc.chat, m)
			chat := canonicalJIDString(a.canonicalStoreJID(t.Context(), group))
			row, err := a.DB().GetMessage(chat, m.GetKey().GetID())
			if tc.refused {
				if count != 0 || len(failures) != 1 || !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(count, failures, err)
				}
				return
			}
			if count != 1 || len(failures) != 0 || err != nil || row.SenderJID != tc.wantSender || row.FromMe != tc.fromMe {
				t.Fatalf("count=%d failures=%v row=%+v err=%v", count, failures, row, err)
			}
		})
	}
}

func TestHistorySenderReimportRetainsProtectedRows(t *testing.T) {
	for _, tc := range []struct {
		name, oldSender, wantSender             string
		edited, deleted, unknown, contradiction bool
		oldTS                                   int64
	}{
		{name: "eligible corrects", oldSender: historyPeerPN, wantSender: historyOwnPN, oldTS: 1700000000},
		{name: "newer protected", oldSender: historyPeerPN, wantSender: historyPeerPN, oldTS: 1700000001},
		{name: "edited protected", oldSender: historyPeerPN, wantSender: historyPeerPN, oldTS: 1700000000, edited: true},
		{name: "tombstone protected", oldSender: historyPeerPN, wantSender: historyPeerPN, oldTS: 1700000000, deleted: true},
		{name: "unknown retains proven", oldSender: historyOwnPN, wantSender: historyOwnPN, oldTS: 1700000000, unknown: true},
		{name: "unknown does not certify wrong", oldSender: historyPeerPN, wantSender: historyPeerPN, oldTS: 1700000000, unknown: true},
		{name: "contradiction does not rewrite", oldSender: historyPeerPN, wantSender: historyPeerPN, oldTS: 1700000000, contradiction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := draftAppFixture(t)
			f := newHistorySenderWA()
			a.wa = f
			if err := a.DB().UpsertChat(historyPeerPN, "dm", "synthetic", time.Unix(tc.oldTS, 0)); err != nil {
				t.Fatal(err)
			}
			old := store.UpsertMessageParams{ChatJID: historyPeerPN, MsgID: "synthetic-history", SenderJID: tc.oldSender, FromMe: true, Text: "retained synthetic", Timestamp: time.Unix(tc.oldTS, 0), Edited: tc.edited, DeletedForMe: tc.deleted}
			if err := a.DB().UpsertMessage(old); err != nil {
				t.Fatal(err)
			}
			m := historySenderMessage(historyPeerPN, true)
			if tc.unknown {
				f.pn, f.lid = "", ""
			}
			if tc.contradiction {
				m.Participant = proto.String(historyPeerPN)
			}
			for i := 0; i < 2; i++ {
				count, failures := importHistorySender(t, a, historyPeerPN, m)
				if tc.unknown || tc.contradiction {
					if count != 0 || len(failures) != 1 {
						t.Fatal(count, failures)
					}
				} else if count != 1 || len(failures) != 0 {
					t.Fatal(count, failures)
				}
			}
			row, err := a.DB().GetMessage(historyPeerPN, old.MsgID)
			if err != nil || row.SenderJID != tc.wantSender || row.Edited != tc.edited || row.DeletedForMe != tc.deleted {
				t.Fatalf("retained=%+v err=%v", row, err)
			}
			if (tc.unknown || tc.contradiction) && (row.Text != old.Text || !row.Timestamp.Equal(old.Timestamp)) {
				t.Fatal("unknown replay changed retained content")
			}
			a.wa = nil
			_, err = a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: historyPeerPN, Message: draftTextPointer("synthetic reply"), ReplyTo: row.MsgID}), os.Open)
			if (tc.wantSender != historyOwnPN || tc.deleted) && err == nil {
				t.Fatal("unusable retained quote accepted")
			}
		})
	}
}

func TestHistorySenderUnknownQuoteAndNullableOwnLID(t *testing.T) {
	a := draftAppFixture(t)
	f := newHistorySenderWA()
	f.pn, f.lid = "", ""
	a.wa = f
	m := historySenderMessage(historyPeerPN, true)
	importHistorySender(t, a, historyPeerPN, m)
	a.wa = nil
	_, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: historyPeerPN, Message: draftTextPointer("synthetic reply"), ReplyTo: m.GetKey().GetID()}), os.Open)
	if err == nil || !strings.Contains(err.Error(), "known sender") {
		t.Fatal("unknown quote", err)
	}
	historyIdentityFixture(t, a, `UPDATE whatsmeow_device SET lid=NULL`)
	f.pn = historyOwnPN
	a.wa = f
	if count, failures := importHistorySender(t, a, historyPeerPN, m); count != 1 || len(failures) != 0 {
		t.Fatal(count, failures)
	}
	a.wa = nil
	if _, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: historyPeerPN, Message: draftTextPointer("synthetic reply"), ReplyTo: m.GetKey().GetID()}), os.Open); err != nil {
		t.Fatal("public PN works with nullable own LID", err)
	}
}

func TestHistorySenderOwnPairCheckedOncePerBatch(t *testing.T) {
	a := newTestApp(t)
	f := newHistorySenderWA()
	a.wa = f
	var messages []*waWeb.WebMessageInfo
	for i := 0; i < 20; i++ {
		m := historySenderMessage(historyPeerPN, true)
		m.Key.ID = proto.String(strings.Repeat("x", i+1))
		m.Participant = proto.String(historyOwnLID)
		messages = append(messages, m)
	}
	if count, failures := importHistorySender(t, a, historyPeerPN, messages...); count != 20 || len(failures) != 0 || f.pairCalls != 1 {
		t.Fatal(count, failures, f.pairCalls)
	}
}

func TestHistorySenderPreservesObservedLIDWithoutSecondLookup(t *testing.T) {
	a := newTestApp(t)
	f := newHistorySenderWA()
	f.unstableLID = true
	a.wa = f
	group := "12345@g.us"
	m := historySenderMessage(group, false)
	m.Participant = proto.String(historyPeerLID)
	if count, failures := importHistorySender(t, a, group, m); count != 1 || len(failures) != 0 {
		t.Fatal(count, failures)
	}
	row, err := a.DB().GetMessage(group, m.GetKey().GetID())
	if err != nil || row.SenderJID != historyPeerLID || f.resolveCalls != 1 {
		t.Fatalf("row=%+v err=%v lookups=%d", row, err, f.resolveCalls)
	}
}

func TestHistorySenderFinalWrappersAndCrypto(t *testing.T) {
	for _, kind := range []string{"device-sent", "contradictory-device-sent", "protocol-edit", "top-level-edit", "contradictory-edit", "direction-edit", "poll", "reaction"} {
		t.Run(kind, func(t *testing.T) {
			a := newTestApp(t)
			f := newHistorySenderWA()
			a.wa = f
			chat := historyPeerPN
			m := historySenderMessage(chat, true)
			m.Starred = proto.Bool(true)
			switch kind {
			case "device-sent", "contradictory-device-sent":
				m.Key.FromMe = proto.Bool(false)
				m.Message = &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{DestinationJID: proto.String(chat), Message: m.Message}}
				if kind == "contradictory-device-sent" {
					m.OriginalSelfAuthorUserJIDString = proto.String(historyPeerPN)
				}
			case "protocol-edit", "contradictory-edit", "direction-edit":
				m.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: m.Key.ID, FromMe: proto.Bool(true)}, EditedMessage: m.Message}}
				if kind == "contradictory-edit" {
					m.Message.ProtocolMessage.Key.Participant = proto.String(historyPeerPN)
				}
				if kind == "direction-edit" {
					m.Key.FromMe = proto.Bool(false)
				}
			case "top-level-edit":
				m.Message = &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: m.Message}}
			case "poll":
				chat = "12345@g.us"
				g, _ := types.ParseJID(chat)
				f.groups[g] = &types.GroupInfo{JID: g, AddressingMode: types.AddressingModeLID}
				m.Key.RemoteJID = proto.String(chat)
				m.OriginalSelfAuthorUserJIDString = proto.String(historyOwnLID)
				m.Message = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("synthetic poll"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("synthetic option")}}}}}}
			case "reaction":
				m.OriginalSelfAuthorUserJIDString = proto.String(historyOwnLID)
				m.Message = &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{TargetMessageKey: &waCommon.MessageKey{ID: proto.String("synthetic-target")}}}
				f.decryptedReaction = &waE2E.ReactionMessage{Text: proto.String("👍"), Key: &waCommon.MessageKey{ID: proto.String("synthetic-target")}}
			}
			count, failures := importHistorySender(t, a, chat, m)
			row, err := a.DB().GetMessage(chat, m.GetKey().GetID())
			if kind == "contradictory-edit" || kind == "direction-edit" || kind == "contradictory-device-sent" {
				if count != 0 || len(failures) != 1 || !errors.Is(err, sql.ErrNoRows) {
					t.Fatal(count, failures, err)
				}
				return
			}
			if count != 1 || len(failures) != 0 || err != nil || row.SenderJID != historyOwnPN || !row.FromMe || !row.Starred {
				t.Fatalf("row=%+v count=%d failures=%v err=%v", row, count, failures, err)
			}
			if strings.HasSuffix(kind, "edit") && !row.Edited {
				t.Fatal("edit metadata lost")
			}
			if kind == "poll" {
				poll, err := a.DB().GetPoll(chat, row.MsgID)
				if err != nil || poll.SenderJID != historyOwnLID {
					t.Fatalf("crypto poll sender=%q err=%v", poll.SenderJID, err)
				}
			}
			if kind == "reaction" && (f.cryptoSender.String() != historyOwnLID || row.ReactionEmoji != "👍") {
				t.Fatal("crypto sender changed", f.cryptoSender, row.ReactionEmoji)
			}
		})
	}
}

func TestHistorySenderUnknownLookupErrors(t *testing.T) {
	a := newTestApp(t)
	a.wa = newHistorySenderWA()
	if err := a.DB().Close(); err != nil {
		t.Fatal(err)
	}
	pm := wa.ParseHistoryMessage(historyPeerPN, historySenderMessage(historyPeerPN, true))
	if err := a.retainHistoryMessageSender(t.Context(), pm); err == nil {
		t.Fatal("SQL error treated as missing retained row")
	}
}

func TestHistorySenderRefusesContradictoryDMPollAuthor(t *testing.T) {
	a := newTestApp(t)
	f := newHistorySenderWA()
	a.wa = f
	f.decryptPollVoteFunc = func(*events.Message) (*waE2E.PollVoteMessage, error) {
		t.Fatal("contradictory author reached vote decryption")
		return nil, nil
	}
	creation := historySenderMessage(historyPeerPN, false)
	creation.Message = &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
		Name: proto.String("synthetic poll"), Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("synthetic option")}},
	}}
	vote := historySenderMessage(historyPeerPN, false)
	vote.Key.ID = proto.String("synthetic-vote")
	// The former ordering fixture claimed a third-party voter in a DM. The
	// SDK replaces that assertion with the peer; the importer must refuse it.
	vote.Participant = proto.String("15550000003@s.whatsapp.net")
	vote.Message = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{PollCreationMessageKey: creation.Key}}
	count, failures := importHistorySender(t, a, historyPeerPN, vote, creation)
	if count != 1 || len(failures) != 1 {
		t.Fatal(count, failures)
	}
	if _, err := a.DB().GetMessage(historyPeerPN, vote.GetKey().GetID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("contradictory vote imported", err)
	}
	votes, err := a.DB().ListPollVotes(historyPeerPN, creation.GetKey().GetID())
	if err != nil || len(votes) != 0 {
		t.Fatal(votes, err)
	}
}
