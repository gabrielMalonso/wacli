package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestMessagesContactHistoryShowAndSearch(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const chat = "15550000002@s.whatsapp.net"
	const name = "A\\B;C,D\n尾"
	const projected = "Contact: A\\B;C,D\n尾 (+15550000003)"
	ts := time.Unix(1, 0)
	if err = db.UpsertChat(chat, "dm", "", ts); err != nil {
		t.Fatal(err)
	}
	// App tests cover fake dispatch -> this text projection. CLI reads also keep
	// older name-only rows unchanged, without a session or history repair.
	for _, row := range []struct{ id, text string }{{"new-card", projected}, {"old-card", name}} {
		if err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: row.id, Text: row.text, FromMe: true, Timestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	searchMode := "like"
	if db.HasFTS() {
		searchMode = "fts5"
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"legacy", "compact", "full"} {
		for _, query := range []struct {
			name     string
			args     []string
			id, text string
			search   bool
		}{
			{"show new", []string{"messages", "show", "--chat", chat, "--id", "new-card"}, "new-card", projected, false},
			{"search phone", []string{"messages", "search", "15550000003", "--chat", chat, "--type", "text"}, "new-card", projected, true},
			{"show old", []string{"messages", "show", "--chat", chat, "--id", "old-card"}, "old-card", name, false},
		} {
			t.Run(detail+"/"+query.name, func(t *testing.T) {
				args := []string{"--store", dir, "--read-only"}
				if detail == "legacy" {
					args = append(args, "--json")
				} else {
					args = append(args, "--agent", "--detail", detail)
				}
				raw, stderr, err := runAgentTest(t, append(args, query.args...)...)
				if err != nil || stderr != "" {
					t.Fatal("contact CLI read", err, stderr)
				}
				if detail == "legacy" {
					var envelope struct {
						Success bool            `json:"success"`
						Data    json.RawMessage `json:"data"`
					}
					if err = json.Unmarshal([]byte(raw), &envelope); err != nil || !envelope.Success {
						t.Fatal("legacy envelope", err)
					}
					var m store.Message
					if query.search {
						var data struct {
							Messages []store.Message `json:"messages"`
						}
						if err = json.Unmarshal(envelope.Data, &data); err != nil || len(data.Messages) != 1 {
							t.Fatal("legacy contact search", err)
						}
						m = data.Messages[0]
					} else if err = json.Unmarshal(envelope.Data, &m); err != nil {
						t.Fatal(err)
					}
					if m.MsgID != query.id || m.Text != query.text || m.MediaType != "" {
						t.Fatalf("legacy contact text=%q media=%q", m.Text, m.MediaType)
					}
					return
				}
				envelope := decodeAgentTest(t, raw)
				if !envelope.Success || envelope.Meta.Source != "local" {
					t.Fatal("agent local contact read")
				}
				var m agentMessage
				if query.search {
					var data agentMessages
					if err = json.Unmarshal(envelope.Data, &data); err != nil || len(data.Messages) != 1 || data.SearchMode != searchMode {
						t.Fatal("agent contact search", err)
					}
					m = data.Messages[0]
				} else if err = json.Unmarshal(envelope.Data, &m); err != nil {
					t.Fatal(err)
				}
				if m.ID != query.id || m.Text != query.text || m.Type != "text" || m.Media != nil || m.TextTruncated {
					t.Fatalf("agent contact text=%q type=%q", m.Text, m.Type)
				}
				if detail == "full" && (m.Full == nil || m.Full.Content != query.text) {
					t.Fatal("full contact content")
				}
			})
		}
	}
}
