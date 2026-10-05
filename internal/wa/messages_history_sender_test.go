package wa

import (
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

func TestParseHistoryMessageAuthorAssertions(t *testing.T) {
	const peer = "15550000002@s.whatsapp.net"
	const own = "15550000001@s.whatsapp.net"
	for _, tc := range []struct {
		name, chat, participant, keyParticipant, original, wantSender string
		fromMe                                                        bool
		wantAssertions                                                []string
	}{
		{name: "outgoing DM", chat: peer, fromMe: true},
		{name: "outgoing explicit own", chat: peer, fromMe: true, participant: own, wantAssertions: []string{own}},
		{name: "conflicting assertions retained", chat: peer, fromMe: true, participant: own, keyParticipant: peer, original: own, wantAssertions: []string{own, peer, own}},
		{name: "outgoing group", chat: "12345@g.us", fromMe: true},
		{name: "incoming group unknown", chat: "12345@g.us"},
		{name: "incoming group explicit", chat: "12345@g.us", participant: peer, wantSender: peer, wantAssertions: []string{peer}},
		{name: "incoming DM", chat: peer, wantSender: peer},
		{name: "remote differs from conversation", chat: own, wantSender: own},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hist := &waWeb.WebMessageInfo{
				Key:         &waCommon.MessageKey{ID: proto.String("synthetic"), FromMe: proto.Bool(tc.fromMe), RemoteJID: proto.String(peer), Participant: proto.String(tc.keyParticipant)},
				Participant: proto.String(tc.participant), OriginalSelfAuthorUserJIDString: proto.String(tc.original),
				Message: &waE2E.Message{Conversation: proto.String("synthetic text")}, Starred: proto.Bool(true),
			}
			pm := ParseHistoryMessage(tc.chat, hist)
			if pm.SenderJID != tc.wantSender || !reflect.DeepEqual(pm.SenderAssertions, tc.wantAssertions) || pm.Text != "synthetic text" || !pm.StarredKnown || !pm.Starred {
				t.Fatalf("parsed: %+v", pm)
			}
		})
	}
}

func TestParseHistoryMessageChecksAuthorAfterWrappers(t *testing.T) {
	const own = "15550000001@s.whatsapp.net"
	const peer = "15550000002@s.whatsapp.net"
	hist := &waWeb.WebMessageInfo{
		Key:     &waCommon.MessageKey{ID: proto.String("event"), RemoteJID: proto.String(peer), FromMe: proto.Bool(false)},
		Message: &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{DestinationJID: proto.String(peer), Message: &waE2E.Message{Conversation: proto.String("synthetic")}}},
	}
	pm := ParseHistoryMessage(own, hist)
	if !pm.DeviceSent || !pm.FromMe || pm.Chat.String() != peer || pm.SenderJID != "" {
		t.Fatalf("device-sent: %+v", pm)
	}
	hist.Key.FromMe = proto.Bool(true)
	hist.Participant = proto.String(own)
	hist.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("target"), Participant: proto.String(peer), FromMe: proto.Bool(true)},
		EditedMessage: &waE2E.Message{Conversation: proto.String("synthetic edit")},
	}}
	pm = ParseHistoryMessage(peer, hist)
	if !pm.Edited || pm.ID != "target" || pm.Text != "synthetic edit" || pm.SenderJID != "" || !reflect.DeepEqual(pm.SenderAssertions, []string{own, peer}) {
		t.Fatalf("protocol edit: %+v", pm)
	}
	hist.Message.ProtocolMessage.Type = waE2E.ProtocolMessage_REVOKE.Enum()
	hist.Message.ProtocolMessage.Key.FromMe = proto.Bool(false)
	pm = ParseHistoryMessage(peer, hist)
	if !pm.Revoked || pm.FromMe || pm.SenderJID != peer || !reflect.DeepEqual(pm.SenderAssertions, []string{peer}) {
		t.Fatalf("revoke target author: %+v", pm)
	}
}
