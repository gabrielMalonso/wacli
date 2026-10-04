package main

import (
	"os"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

const agentTextRunes = 320

// Cut at Unicode code point boundaries; IDs are never passed through this.
func agentText(text, detail string) (string, bool) {
	if detail == "full" {
		return text, false
	}
	count := 0
	for offset := range text {
		if count == agentTextRunes {
			return text[:offset], true
		}
		count++
	}
	return text, false
}
func agentTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

type agentMessage struct {
	ID              string            `json:"id"`
	ChatJID         string            `json:"chat_jid"`
	SenderJID       string            `json:"sender_jid"`
	FromMe          bool              `json:"from_me"`
	Timestamp       *time.Time        `json:"timestamp"`
	Text            string            `json:"text"`
	TextTruncated   bool              `json:"text_truncated"`
	Type            string            `json:"type"`
	Media           *agentMedia       `json:"media,omitempty"`
	Quote           *agentQuote       `json:"quote,omitempty"`
	Reaction        *agentReaction    `json:"reaction,omitempty"`
	Edited          bool              `json:"edited,omitempty"`
	Revoked         bool              `json:"revoked,omitempty"`
	DeletedForMe    bool              `json:"deleted_for_me,omitempty"`
	DeletedAt       *time.Time        `json:"deleted_at,omitempty"`
	DeletionReason  string            `json:"deletion_reason,omitempty"`
	PayloadPurgedAt *time.Time        `json:"payload_purged_at,omitempty"`
	Forwarded       bool              `json:"forwarded,omitempty"`
	Full            *agentMessageFull `json:"full,omitempty"`
	FieldsTruncated []string          `json:"fields_truncated,omitempty"`
}
type agentMedia struct {
	Type     string `json:"type"`
	Filename string `json:"filename,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}
type agentQuote struct {
	ID        string `json:"id"`
	SenderJID string `json:"sender_jid,omitempty"`
}
type agentReaction struct {
	ID    string `json:"id"`
	Emoji string `json:"emoji"`
}
type agentButton struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"`
}
type agentMessageFull struct {
	Content         string        `json:"content"`
	Caption         string        `json:"caption,omitempty"`
	ChatName        string        `json:"chat_name,omitempty"`
	SenderName      string        `json:"sender_name,omitempty"`
	ForwardingScore uint32        `json:"forwarding_score,omitempty"`
	Starred         bool          `json:"starred"`
	StarredAt       *time.Time    `json:"starred_at,omitempty"`
	Downloaded      bool          `json:"downloaded"`
	DownloadedAt    *time.Time    `json:"downloaded_at,omitempty"`
	Buttons         []agentButton `json:"buttons,omitempty"`
}

func agentMessageDTO(m store.Message, detail string) agentMessage {
	text, cut := agentText(messageText(m), detail)
	typ := m.MediaType
	if typ == "" {
		typ = "text"
	}
	if m.ReactionToID != "" {
		typ = "reaction"
	}
	d := agentMessage{ID: m.MsgID, ChatJID: m.ChatJID, SenderJID: m.SenderJID, FromMe: m.FromMe, Timestamp: agentTime(m.Timestamp), Text: text, TextTruncated: cut, Type: typ, Edited: m.Edited, Revoked: m.Revoked, DeletedForMe: m.DeletedForMe, DeletedAt: m.DeletedAt, DeletionReason: m.DeletionReason, PayloadPurgedAt: m.PayloadPurgedAt, Forwarded: m.IsForwarded}
	if m.MediaType != "" {
		filename, truncated := agentText(m.Filename, detail)
		d.Media = &agentMedia{m.MediaType, filename, m.MimeType}
		if truncated {
			d.FieldsTruncated = append(d.FieldsTruncated, "media.filename")
		}
	}
	if m.QuotedMsgID != "" || m.QuotedSenderJID != "" {
		d.Quote = &agentQuote{m.QuotedMsgID, m.QuotedSenderJID}
	}
	if m.ReactionToID != "" {
		d.Reaction = &agentReaction{m.ReactionToID, m.ReactionEmoji}
	}
	if detail == "full" {
		full := &agentMessageFull{Content: m.Text, Caption: m.MediaCaption, ChatName: m.ChatName, SenderName: m.SenderName, ForwardingScore: m.ForwardingScore, Starred: m.Starred, StarredAt: agentTime(m.StarredAt), Downloaded: m.LocalPath != "", DownloadedAt: agentTime(m.DownloadedAt)}
		for _, b := range m.Buttons {
			full.Buttons = append(full.Buttons, agentButton{b.Type, b.DisplayText, b.ID, b.Description})
		}
		d.Full = full
	}
	return d
}

type agentMessages struct {
	Messages   []agentMessage `json:"messages"`
	SearchMode string         `json:"search_mode,omitempty"`
	SelectedID string         `json:"selected_id,omitempty"`
}

func writeAgentMessages(flags *rootFlags, msgs []store.Message, limit int, searchMode, selected string, before, after *int) error {
	data := agentMessages{Messages: make([]agentMessage, 0, len(msgs)), SearchMode: searchMode, SelectedID: selected}
	for _, m := range msgs {
		data.Messages = append(data.Messages, agentMessageDTO(m, flags.detail))
	}
	meta := agentMeta(flags)
	meta.Limit = limit
	meta.Before = before
	meta.After = after
	meta.Excluded = []string{"tombstones"}
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}

type agentChat struct {
	JID             string         `json:"jid"`
	Kind            string         `json:"kind"`
	Name            string         `json:"name,omitempty"`
	LastMessageAt   *time.Time     `json:"last_message_at"`
	Unread          bool           `json:"unread"`
	UnreadCount     int            `json:"unread_count"`
	Full            *agentChatFull `json:"full,omitempty"`
	FieldsTruncated []string       `json:"fields_truncated,omitempty"`
}
type agentChatFull struct {
	Archived   bool  `json:"archived"`
	Pinned     bool  `json:"pinned"`
	MutedUntil int64 `json:"muted_until"`
}

func agentChatDTO(c store.Chat, detail string) agentChat {
	name, cut := agentText(c.Name, detail)
	d := agentChat{JID: c.JID, Kind: c.Kind, Name: name, LastMessageAt: agentTime(c.LastMessageTS), Unread: c.Unread, UnreadCount: c.UnreadCount}
	if cut {
		d.FieldsTruncated = []string{"name"}
	}
	if detail == "full" {
		d.Full = &agentChatFull{c.Archived, c.Pinned, c.MutedUntil}
	}
	return d
}

type agentChats struct {
	Chats []agentChat `json:"chats"`
}

func writeAgentChats(flags *rootFlags, chats []store.Chat, limit int) error {
	data := agentChats{Chats: make([]agentChat, 0, len(chats))}
	for _, c := range chats {
		data.Chats = append(data.Chats, agentChatDTO(c, flags.detail))
	}
	meta := agentMeta(flags)
	meta.Limit = limit
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}

type agentContact struct {
	JID             string            `json:"jid"`
	Phone           string            `json:"phone,omitempty"`
	Name            string            `json:"name,omitempty"`
	Alias           string            `json:"alias,omitempty"`
	Full            *agentContactFull `json:"full,omitempty"`
	FieldsTruncated []string          `json:"fields_truncated,omitempty"`
}
type agentContactFull struct {
	SystemName string     `json:"system_name,omitempty"`
	Tags       []string   `json:"tags,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

func agentContactDTO(c store.Contact, detail string) agentContact {
	name, cutName := agentText(c.Name, detail)
	alias, cutAlias := agentText(c.Alias, detail)
	d := agentContact{JID: c.JID, Phone: c.Phone, Name: name, Alias: alias}
	if cutName {
		d.FieldsTruncated = append(d.FieldsTruncated, "name")
	}
	if cutAlias {
		d.FieldsTruncated = append(d.FieldsTruncated, "alias")
	}
	if detail == "full" {
		d.Full = &agentContactFull{c.SystemName, c.Tags, agentTime(c.UpdatedAt)}
	}
	return d
}

type agentContacts struct {
	Contacts []agentContact `json:"contacts"`
}

func writeAgentContacts(flags *rootFlags, cs []store.Contact, limit int) error {
	data := agentContacts{Contacts: make([]agentContact, 0, len(cs))}
	for _, c := range cs {
		data.Contacts = append(data.Contacts, agentContactDTO(c, flags.detail))
	}
	meta := agentMeta(flags)
	meta.Limit = limit
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}

type agentResolution struct {
	Input           string   `json:"input"`
	JID             string   `json:"jid,omitempty"`
	Phone           string   `json:"phone,omitempty"`
	LID             string   `json:"lid,omitempty"`
	Name            string   `json:"name,omitempty"`
	Resolved        bool     `json:"resolved"`
	FieldsTruncated []string `json:"fields_truncated,omitempty"`
}
type agentResolutions struct {
	Resolutions []agentResolution `json:"resolutions"`
}

func writeAgentResolutions(flags *rootFlags, rs []contactResolution) error {
	data := agentResolutions{Resolutions: make([]agentResolution, 0, len(rs))}
	for _, r := range rs {
		name, cut := agentText(r.Name, flags.detail)
		d := agentResolution{Input: r.Input, JID: r.JID, Phone: r.Phone, LID: r.LID, Name: name, Resolved: r.Resolved}
		if cut {
			d.FieldsTruncated = []string{"name"}
		}
		data.Resolutions = append(data.Resolutions, d)
	}
	meta := agentMeta(flags)
	meta.Limit = agentMaxResults
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}

type agentCoverage struct {
	ChatJID         string     `json:"chat_jid"`
	Kind            string     `json:"kind"`
	Name            string     `json:"name,omitempty"`
	MessageCount    int64      `json:"message_count"`
	OldestAt        *time.Time `json:"oldest_message_at"`
	NewestAt        *time.Time `json:"newest_message_at"`
	Status          string     `json:"status"`
	BlockedReason   string     `json:"blocked_reason,omitempty"`
	FieldsTruncated []string   `json:"fields_truncated,omitempty"`
}
type agentCoverageData struct {
	Coverage []agentCoverage `json:"coverage"`
}

func writeAgentCoverage(flags *rootFlags, cs []store.HistoryCoverage, limit int) error {
	data := agentCoverageData{Coverage: make([]agentCoverage, 0, len(cs))}
	for _, c := range cs {
		name, cut := agentText(c.Name, flags.detail)
		d := agentCoverage{ChatJID: c.ChatJID, Kind: c.Kind, Name: name, MessageCount: c.MessageCount, OldestAt: agentTime(c.OldestTS), NewestAt: agentTime(c.NewestTS), Status: c.Status, BlockedReason: c.BlockedReason}
		if cut {
			d.FieldsTruncated = []string{"name"}
		}
		data.Coverage = append(data.Coverage, d)
	}
	meta := agentMeta(flags)
	meta.Limit = limit
	return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, data)
}
