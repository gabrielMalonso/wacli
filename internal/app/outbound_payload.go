package app

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func outboundTarget(r store.DraftRecipient) (types.JID, error) {
	target := r.JID
	if r.PN != "" && r.LID == "" {
		return types.JID{}, fmt.Errorf("PN requires a frozen LID")
	}
	if r.LID != "" {
		target = r.LID
	}
	return ParseHistoryJID(target)
}

// Content is constructed only from the exact validated revision. No preview,
// ephemeral defaults, self opt-in, discovery or reloaded quote text is applied.
func prepareOutboundPayload(p store.DraftPayloadData, uploaded *whatsmeow.UploadResponse) (types.JID, *waE2E.Message, error) {
	to, err := outboundTarget(p.Recipient)
	if err != nil {
		return to, nil, err
	}
	var ci *waE2E.ContextInfo
	if p.Reply != nil || p.Text != nil && len(p.Text.Mentions) > 0 {
		ci = &waE2E.ContextInfo{}
	}
	if p.Reply != nil {
		sender, err := outboundTarget(p.Reply.Sender)
		if err != nil {
			return to, nil, err
		}
		ci.StanzaID = proto.String(p.Reply.ID)
		ci.Participant = proto.String(sender.String())
		ci.RemoteJID = proto.String(to.String())
		ci.QuotedMessage = &waE2E.Message{Conversation: proto.String(p.Reply.Text)}
	}
	msg := &waE2E.Message{}
	switch p.Kind {
	case store.DraftTextKind:
		for _, mention := range p.Text.Mentions {
			jid, err := outboundTarget(mention)
			if err != nil {
				return to, nil, err
			}
			ci.MentionedJID = append(ci.MentionedJID, jid.String())
		}
		if ci == nil {
			msg.Conversation = proto.String(p.Text.Text)
		} else {
			msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: proto.String(p.Text.Text), ContextInfo: ci}
		}
	case store.DraftContactKind:
		msg.ContactMessage = &waE2E.ContactMessage{DisplayName: proto.String(p.Contact.DisplayName), Vcard: proto.String(p.Contact.VCard)}
	case store.DraftImageKind:
		up := whatsmeow.UploadResponse{}
		if uploaded != nil {
			up = *uploaded
		}
		value := p.Image
		msg.ImageMessage = wa.BuildImageMessage(up, value.Caption, wa.ImageMetadata{MIME: value.MIME, Width: value.Width, Height: value.Height, JPEGThumbnail: value.JPEGThumbnail})
		msg.ImageMessage.ContextInfo = ci
	case store.DraftVoiceKind:
		msg.AudioMessage = &waE2E.AudioMessage{Mimetype: proto.String(p.Voice.MIME), PTT: proto.Bool(true), ContextInfo: ci}
		if uploaded != nil {
			m := msg.AudioMessage
			m.URL, m.DirectPath, m.FileLength = proto.String(uploaded.URL), proto.String(uploaded.DirectPath), proto.Uint64(uploaded.FileLength)
			m.MediaKey, m.FileSHA256, m.FileEncSHA256 = bytes.Clone(uploaded.MediaKey), bytes.Clone(uploaded.FileSHA256), bytes.Clone(uploaded.FileEncSHA256)
		}
	case store.DraftDocumentKind:
		msg.DocumentMessage = &waE2E.DocumentMessage{FileName: proto.String(p.Document.Filename), Mimetype: proto.String(p.Document.MIME), Caption: proto.String(p.Document.Caption), ContextInfo: ci}
		if uploaded != nil {
			m := msg.DocumentMessage
			m.URL, m.DirectPath, m.FileLength = proto.String(uploaded.URL), proto.String(uploaded.DirectPath), proto.Uint64(uploaded.FileLength)
			m.MediaKey, m.FileSHA256, m.FileEncSHA256 = uploaded.MediaKey, uploaded.FileSHA256, uploaded.FileEncSHA256
		}
	default:
		return to, nil, fmt.Errorf("unsupported outbound payload")
	}
	return to, msg, nil
}

func validateOutboundIdentities(p store.DraftPayloadData, identities []HistoryIdentity) error {
	byInput := make(map[string]HistoryIdentity, len(identities))
	for _, i := range identities {
		if i.AccountJID != p.Account.PN || i.AccountAliasJID != p.Account.LID {
			return fmt.Errorf("own identity changed or unavailable")
		}
		byInput[i.InputJID] = i
	}
	recipients := []store.DraftRecipient{p.Recipient}
	if p.Text != nil {
		recipients = append(recipients, p.Text.Mentions...)
	}
	if p.Reply != nil {
		recipients = append(recipients, p.Reply.Sender)
	}
	for _, frozen := range recipients {
		i, ok := byInput[frozen.JID]
		if !ok || store.DraftRecipientFromPublic(i.ChatJID, i.AliasJID) != frozen {
			return fmt.Errorf("frozen identity relation changed or unavailable")
		}
		if _, err := outboundTarget(frozen); err != nil {
			return err
		}
	}
	if !strings.HasSuffix(p.Recipient.JID, "@g.us") && p.Account.LID == "" {
		return fmt.Errorf("own LID unavailable")
	}
	return nil
}

func outboundIdentityInputs(p store.DraftPayloadData) []string {
	inputs := []string{p.Recipient.JID}
	if p.Text != nil {
		for _, r := range p.Text.Mentions {
			inputs = append(inputs, r.JID)
		}
	}
	if p.Reply != nil {
		inputs = append(inputs, p.Reply.Sender.JID)
	}
	return inputs
}
