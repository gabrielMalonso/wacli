package store

import "strings"

// NewDraftContact creates one explicit vCard 3.0 contact. PN input is never
// inferred from a LID. Its UTF-8 bytes and CRLF framing are frozen in the hash;
// compatibility with remote WhatsApp clients still requires future validation.
func NewDraftContact(displayName, phone string) (DraftContact, error) {
	if err := checkDraftString("contact.display_name", displayName, true); err != nil {
		return DraftContact{}, err
	}
	if strings.IndexFunc(displayName, func(r rune) bool { return r < 32 && r != '\n' && r != '\r' && r != '\t' || r == 127 }) >= 0 {
		return DraftContact{}, invalidDraft("contact.display_name", "unsupported control character")
	}
	if err := checkDraftString("contact.phone", phone, true); err != nil {
		return DraftContact{}, err
	}
	phone, err := normalizeDraftPhone(phone)
	if err != nil {
		return DraftContact{}, err
	}
	// A CRLF pair is one text newline. Lone CR is also escaped so a name
	// cannot inject another property line. The display name itself stays exact.
	name := strings.ReplaceAll(displayName, "\r\n", "\n")
	name = strings.ReplaceAll(name, "\r", "\n")
	name = strings.NewReplacer("\\", "\\\\", "\n", "\\n", ";", "\\;", ",", "\\,").Replace(name)
	card := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:;" + name + ";;;\r\nFN:" + name +
		"\r\nTEL;TYPE=CELL;waid=" + phone + ":+" + phone + "\r\nEND:VCARD\r\n"
	if err := checkDraftString("contact.vcard", card, true); err != nil {
		return DraftContact{}, err
	}
	return DraftContact{DisplayName: displayName, Phone: phone, VCard: card}, nil
}
