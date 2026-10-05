package wa

import (
	"testing"

	"github.com/openclaw/wacli/internal/store"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestContactDisplayTextContracts(t *testing.T) {
	cases := []struct{ name, display, card, want string }{
		{"multiple TEL folded duplicate", "Explicit", "BEGIN:VCARD\r\nFN:Ignored\r\nTEL;TYPE=CELL:+1 555\r\n 0003\r\nTEL;TYPE=WORK:+44 1234\r\nTEL:+1 5550003\r\nEND:VCARD", "Contact: Explicit (+1 5550003, +44 1234)"},
		{"escaped fallback FN", "", "BEGIN:VCARD\nFN:A\\\\B\\;C\\,D\\n尾\nTEL:+15550000003\nEND:VCARD", "Contact: A\\B;C,D\n尾 (+15550000003)"},
		{"display priority trim", "  Explicit  ", "FN:Other\nTEL:+15550000003", "Contact: Explicit (+15550000003)"},
		{"name only", "Fixture", "", "Contact: Fixture"},
		{"phone only", "", "TEL:+15550000003", "Contact: +15550000003"},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			card := &waProto.ContactMessage{DisplayName: proto.String(c.display), Vcard: proto.String(c.card)}
			got := ContactDisplayText(card)
			received := ParseLiveMessage(&events.Message{Message: &waProto.Message{ContactMessage: card}})
			if got != c.want || received.Text != c.want || received.Media != nil {
				t.Fatalf("got=%q received=%q want=%q", got, received.Text, c.want)
			}
		})
	}
	if ContactDisplayText(nil) != "" {
		t.Fatal("nil contract")
	}
	name := "A\\B;C,D\r\nTEL:+999\rFN:injected\n尾"
	card, err := store.NewDraftContact(name, "15550000003")
	if err != nil {
		t.Fatal(err)
	}
	wire := &waProto.ContactMessage{DisplayName: proto.String(card.DisplayName), Vcard: proto.String(card.VCard)}
	_, phones := contactDetails(wire)
	if len(phones) != 1 || phones[0] != "+15550000003" || ContactDisplayText(wire) != "Contact: "+name+" (+15550000003)" {
		t.Fatal("escaped name injected TEL or changed display")
	}
}
