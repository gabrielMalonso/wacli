package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

type fakeChatResolver struct {
	lidToPN map[types.JID]types.JID
	names   map[types.JID]string
}

func (f fakeChatResolver) ResolveChatName(ctx context.Context, chat types.JID, pushName string) string {
	if name, ok := f.names[chat.ToNonAD()]; ok {
		return name
	}
	return chat.String()
}

func (f fakeChatResolver) ResolveLIDToPN(ctx context.Context, jid types.JID) types.JID {
	if pn, ok := f.lidToPN[jid.ToNonAD()]; ok {
		pn.Device = jid.Device
		return pn
	}
	return jid
}

func (f fakeChatResolver) ResolvePNToLID(ctx context.Context, jid types.JID) types.JID {
	for lid, pn := range f.lidToPN {
		if pn == jid.ToNonAD() {
			lid.Device = jid.Device
			return lid
		}
	}
	return jid
}

func TestResolveStoredChatsMapsLIDRows(t *testing.T) {
	lid := mustParseJID(t, "999123456789@lid")
	pn := mustParseJID(t, "15551234567@s.whatsapp.net")
	resolver := fakeChatResolver{
		lidToPN: map[types.JID]types.JID{lid: pn},
		names:   map[types.JID]string{pn: "Alice"},
	}

	got := resolveStoredChatsWith(context.Background(), resolver, []store.Chat{{
		JID:           lid.String(),
		Kind:          "unknown",
		Name:          lid.String(),
		LastMessageTS: time.Unix(10, 0),
	}})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].JID != pn.String() || got[0].Kind != "dm" || got[0].Name != "Alice" {
		t.Fatalf("resolved chat = %+v", got[0])
	}
}

func TestResolveStoredChatsMergesMappedDuplicates(t *testing.T) {
	lid := mustParseJID(t, "999123456789@lid")
	pn := mustParseJID(t, "15551234567@s.whatsapp.net")
	resolver := fakeChatResolver{
		lidToPN: map[types.JID]types.JID{lid: pn},
		names:   map[types.JID]string{pn: "Alice"},
	}
	old := time.Unix(10, 0)
	newer := time.Unix(20, 0)

	got := resolveStoredChatsWith(context.Background(), resolver, []store.Chat{
		{JID: lid.String(), Kind: "unknown", Name: lid.String(), LastMessageTS: newer, Unread: true, UnreadCount: 2},
		{JID: pn.String(), Kind: "dm", Name: "", LastMessageTS: old, Unread: true, UnreadCount: 1},
	})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].JID != pn.String() || got[0].Name != "Alice" || !got[0].LastMessageTS.Equal(newer) || !got[0].Unread || got[0].UnreadCount != 3 {
		t.Fatalf("merged chat = %+v", got[0])
	}
}

func TestChatFlagsString(t *testing.T) {
	got := chatFlagsString(store.Chat{Pinned: true, Archived: true, MutedUntil: -1, Unread: true})
	if got != "pinned,archived,muted,unread" {
		t.Fatalf("flags = %q", got)
	}
	got = chatFlagsString(store.Chat{Unread: true, UnreadCount: 1})
	if got != "unread:1" {
		t.Fatalf("flags = %q", got)
	}
	got = chatFlagsString(store.Chat{Unread: true, UnreadCount: 3})
	if got != "unread:3" {
		t.Fatalf("flags = %q", got)
	}
	if err := validateBoolFilter("archived", true, true); err == nil {
		t.Fatal("expected mutually exclusive filter error")
	}
	if err := validateBoolFilter("archived", true, false); err != nil {
		t.Fatalf("unexpected filter error: %v", err)
	}
}

func TestResolveStoredChatsPreservesPinnedOrderAndStableTies(t *testing.T) {
	lid := mustParseJID(t, "999123456789@lid")
	pn := mustParseJID(t, "15551234567@s.whatsapp.net")
	resolver := fakeChatResolver{lidToPN: map[types.JID]types.JID{lid: pn}}
	old, recent := time.Unix(10, 0), time.Unix(20, 0)
	for _, fusion := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate", true: "fused"}[fusion], func(t *testing.T) {
			input := []store.Chat{
				{JID: pn.String(), Pinned: true, Archived: true, MutedUntil: -1, LastMessageTS: old},
				{JID: "pinned-tie@s.whatsapp.net", Pinned: true, LastMessageTS: old},
				{JID: "recent@s.whatsapp.net", LastMessageTS: recent},
				{JID: lid.String(), LastMessageTS: recent},
				{JID: "last@s.whatsapp.net", LastMessageTS: old},
			}
			if !fusion {
				input[0].JID = "other-pinned@s.whatsapp.net"
			}
			got := resolveStoredChatsWith(context.Background(), resolver, input)
			var ids []string
			for _, c := range got {
				ids = append(ids, c.JID)
			}
			want := []string{input[0].JID, "pinned-tie@s.whatsapp.net", "recent@s.whatsapp.net", pn.String(), "last@s.whatsapp.net"}
			if fusion {
				want = []string{pn.String(), "pinned-tie@s.whatsapp.net", "recent@s.whatsapp.net", "last@s.whatsapp.net"}
			}
			if !reflect.DeepEqual(ids, want) || !got[0].Pinned || !got[0].Archived || got[0].MutedUntil != -1 {
				t.Fatalf("resolved order/flags: %+v want=%v", got, want)
			}
			if fusion && !got[0].LastMessageTS.Equal(recent) {
				t.Fatal("fusion should still use newest activity")
			}
		})
	}
}
