package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

func newMessagesListCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var sender string
	var limit int
	var afterStr string
	var beforeStr string
	var fromMe bool
	var fromThem bool
	var asc bool
	var forwarded bool
	var starred bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			if fromMe && fromThem {
				return fmt.Errorf("--from-me and --from-them are mutually exclusive")
			}

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			after, before, err := messageTimeBounds(afterStr, beforeStr)
			if err != nil {
				return err
			}

			var fromMeFilter *bool
			switch {
			case fromMe:
				v := true
				fromMeFilter = &v
			case fromThem:
				v := false
				fromMeFilter = &v
			}

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}

			params := store.ListMessagesParams{
				ChatJIDs:  chatJIDs,
				SenderJID: sender,
				Limit:     limit,
				After:     after,
				Before:    before,
				FromMe:    fromMeFilter,
				Asc:       asc,
				Forwarded: forwarded,
				Starred:   starred,
			}
			if flags.agent {
				chatIdentity := ""
				if strings.TrimSpace(chat) != "" {
					jid, err := wa.ParseUserOrJID(strings.TrimSpace(chat))
					if err != nil {
						return err
					}
					chatIdentity = canonicalMessageFilterJID(jid).String()
				}
				if strings.TrimSpace(sender) != "" {
					jid, err := wa.ParseUserOrJID(strings.TrimSpace(sender))
					if err != nil {
						return err
					}
					params.SenderJID = canonicalMessageFilterJID(jid).String()
				}
				page, err := a.DB().ListMessagesPage(store.ListMessagesPageParams{ListMessagesParams: params, StoreRef: *flags.agentAccount.StoreRef, ChatIdentity: chatIdentity, Cursor: flags.cursor})
				if err != nil {
					return err
				}
				page.Messages = resolveMessageSenderNames(ctx, a, page.Messages)
				return writeAgentMessagePage(flags, page, limit)
			}
			msgs, err := a.DB().ListMessages(params)
			if err != nil {
				return err
			}
			msgs = resolveMessageSenderNames(ctx, a, msgs)

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"messages": msgs,
					"fts":      a.DB().HasFTS(),
				})
			}

			return writeMessagesList(os.Stdout, msgs, fullTableOutput(flags.fullOutput))
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "filter by chat JID")
	cmd.Flags().StringVar(&sender, "sender", "", "filter by sender JID")
	cmd.Flags().IntVar(&limit, "limit", 50, "max number of messages to return")
	cmd.Flags().StringVar(&afterStr, "after", "", "only messages after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&beforeStr, "before", "", "only messages before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().BoolVar(&fromMe, "from-me", false, "only messages sent by me")
	cmd.Flags().BoolVar(&fromThem, "from-them", false, "only messages received (not sent by me)")
	cmd.Flags().BoolVar(&asc, "asc", false, "show oldest messages first (default: newest first)")
	cmd.Flags().BoolVar(&forwarded, "forwarded", false, "only forwarded messages")
	cmd.Flags().BoolVar(&starred, "starred", false, "only starred messages")
	return cmd
}

func newMessagesSearchCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var from string
	var limit int
	var afterStr string
	var beforeStr string
	var hasMedia bool
	var msgType string
	var forwarded bool
	var starred bool
	var sortBy string
	var asc bool

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search messages (FTS5 if available; otherwise LIKE)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !flags.agent && (cmd.Flags().Changed("sort") || cmd.Flags().Changed("asc")) {
				return fmt.Errorf("search --sort and --asc require --agent")
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			after, before, err := messageTimeBounds(afterStr, beforeStr)
			if err != nil {
				return err
			}

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}

			params := store.SearchMessagesParams{
				Query:     args[0],
				ChatJIDs:  chatJIDs,
				From:      from,
				Limit:     limit,
				After:     after,
				Before:    before,
				HasMedia:  hasMedia,
				Type:      msgType,
				Forwarded: forwarded,
				Starred:   starred,
			}
			if flags.agent && strings.TrimSpace(from) != "" {
				jid, err := wa.ParseUserOrJID(strings.TrimSpace(from))
				if err != nil {
					return err
				}
				params.From = canonicalMessageFilterJID(jid).String()
			}
			if flags.agent && sortBy == "time" {
				chatIdentity := ""
				if strings.TrimSpace(chat) != "" {
					jid, err := wa.ParseUserOrJID(strings.TrimSpace(chat))
					if err != nil {
						return err
					}
					chatIdentity = canonicalMessageFilterJID(jid).String()
				}
				page, err := a.DB().SearchMessagesPage(store.SearchMessagesPageParams{SearchMessagesParams: params, StoreRef: *flags.agentAccount.StoreRef, ChatIdentity: chatIdentity, Asc: asc, Cursor: flags.cursor})
				if err != nil {
					return err
				}
				page.Messages = resolveMessageSenderNames(ctx, a, page.Messages)
				return writeAgentSearch(flags, page.Messages, limit, a.DB().HasFTS(), true, asc, &page)
			}
			msgs, err := a.DB().SearchMessages(params)
			if err != nil {
				return err
			}
			msgs = resolveMessageSenderNames(ctx, a, msgs)

			if flags.agent {
				return writeAgentSearch(flags, msgs, limit, a.DB().HasFTS(), false, false, nil)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"messages": msgs,
					"fts":      a.DB().HasFTS(),
				})
			}

			if err := writeMessagesSearch(os.Stdout, msgs, fullTableOutput(flags.fullOutput)); err != nil {
				return err
			}
			if !a.DB().HasFTS() {
				fmt.Fprintln(os.Stderr, "Note: FTS5 not enabled; search is using LIKE (slow).")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&sortBy, "sort", "relevance", "agent search order (relevance|time); time enables pagination")
	cmd.Flags().BoolVar(&asc, "asc", false, "agent temporal search: oldest first")
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID")
	cmd.Flags().StringVar(&from, "from", "", "sender JID")
	cmd.Flags().IntVar(&limit, "limit", 50, "limit results")
	cmd.Flags().StringVar(&afterStr, "after", "", "only messages after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&beforeStr, "before", "", "only messages before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().BoolVar(&hasMedia, "has-media", false, "only messages with media")
	cmd.Flags().StringVar(&msgType, "type", "", "message type filter (text|image|video|audio|document)")
	cmd.Flags().BoolVar(&forwarded, "forwarded", false, "only forwarded messages")
	cmd.Flags().BoolVar(&starred, "starred", false, "only starred messages")
	return cmd
}

func newMessagesStarredCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var limit int
	var afterStr string
	var beforeStr string
	var asc bool

	cmd := &cobra.Command{
		Use:   "starred",
		Short: "List starred messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			after, before, err := messageTimeBounds(afterStr, beforeStr)
			if err != nil {
				return err
			}

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}
			msgs, err := a.DB().ListStarredMessages(store.ListStarredMessagesParams{
				ChatJIDs: chatJIDs,
				Limit:    limit,
				After:    after,
				Before:   before,
				Asc:      asc,
			})
			if err != nil {
				return err
			}
			msgs = resolveMessageSenderNames(ctx, a, msgs)

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"messages": msgs,
					"fts":      a.DB().HasFTS(),
				})
			}
			return writeMessagesStarred(os.Stdout, msgs, fullTableOutput(flags.fullOutput))
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "filter by chat JID")
	cmd.Flags().IntVar(&limit, "limit", 50, "max number of messages to return")
	cmd.Flags().StringVar(&afterStr, "after", "", "only messages with stored star time after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&beforeStr, "before", "", "only messages with stored star time before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().BoolVar(&asc, "asc", false, "show oldest starred messages first (default: newest starred first)")
	return cmd
}

func newMessagesShowCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show one message",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chat == "" || id == "" {
				return fmt.Errorf("--chat and --id are required")
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}
			m, err := getMessageByChatFilter(a.DB(), chatJIDs, id)
			if err != nil {
				return err
			}
			m = resolveMessageSenderNames(ctx, a, []store.Message{m})[0]

			if flags.agent {
				d := agentMessageDTO(m, flags.detail)
				meta := agentMeta(flags)
				if d.TextTruncated || len(d.FieldsTruncated) > 0 {
					meta.Recovery = agentMessageRecovery
				}
				return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, d)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, m)
			}

			return writeMessageShow(os.Stdout, m)
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "chat JID")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	return cmd
}

func newMessagesContextCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var before int
	var after int

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Show message context around a message ID",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chat == "" || id == "" {
				return fmt.Errorf("--chat and --id are required")
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}
			msgs, err := getMessageContextByChatFilter(a.DB(), chatJIDs, id, before, after)
			if err != nil {
				return err
			}
			msgs = resolveMessageSenderNames(ctx, a, msgs)

			if flags.agent {
				return writeAgentMessages(flags, msgs, before+after+1, "", id, &before, &after)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, msgs)
			}

			return writeMessageContext(os.Stdout, msgs, id, fullTableOutput(flags.fullOutput))
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	cmd.Flags().IntVar(&before, "before", 5, "messages before")
	cmd.Flags().IntVar(&after, "after", 5, "messages after")
	return cmd
}

func newMessagesExportCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var limit int
	var afterStr string
	var beforeStr string
	var output string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export messages as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			after, before, err := messageTimeBounds(afterStr, beforeStr)
			if err != nil {
				return err
			}

			chatJIDs, err := messageChatJIDFilter(ctx, a, chat)
			if err != nil {
				return err
			}

			msgs, err := a.DB().ListMessages(store.ListMessagesParams{
				ChatJIDs: chatJIDs,
				Limit:    limit,
				After:    after,
				Before:   before,
				Asc:      true,
			})
			if err != nil {
				return err
			}
			msgs = resolveMessageSenderNames(ctx, a, msgs)

			dst := os.Stdout
			if output != "" {
				f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				dst = f
			}

			return out.WriteJSON(dst, map[string]any{
				"messages": msgs,
				"fts":      a.DB().HasFTS(),
			})
		},
	}

	cmd.Flags().StringVar(&chat, "chat", "", "filter by chat JID")
	cmd.Flags().IntVar(&limit, "limit", 1000, "max number of messages to export")
	cmd.Flags().StringVar(&afterStr, "after", "", "only messages after time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&beforeStr, "before", "", "only messages before time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&output, "output", "", "write JSON export to file instead of stdout")
	return cmd
}

func messageTimeBounds(afterStr, beforeStr string) (after, before *time.Time, err error) {
	if afterStr != "" {
		t, err := parseTime(afterStr)
		if err != nil {
			return nil, nil, err
		}
		after = &t
	}
	if beforeStr != "" {
		t, err := parseTime(beforeStr)
		if err != nil {
			return nil, nil, err
		}
		before = &t
	}
	return after, before, nil
}
