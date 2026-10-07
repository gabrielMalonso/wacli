package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newChangesCmd(flags *rootFlags) *cobra.Command {
	parent := &cobra.Command{Use: "changes", Short: "Read durable local archive changes"}
	var limit int
	cmd := &cobra.Command{Use: "list", Short: "Read a page of retained changes (start at the beginning without --cursor)", Args: cobra.NoArgs}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum changes (1-200)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if limit < 1 || limit > 200 {
			return agentUsageError(fmt.Errorf("--limit must be between 1 and 200"))
		}
		if cmd.Flags().Changed("cursor") {
			if err := store.ValidateChangesCursor(flags.cursor); err != nil {
				return changeReadError(err)
			}
		}
		// Freeze the same public selection used by agent envelopes, also for tables/legacy JSON.
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		db, err := store.OpenReadOnly(filepath.Join(flags.storeDir, "wacli.db"))
		if err != nil {
			return agentStoreError(err)
		}
		defer db.Close()
		page, err := db.ListChanges(ctx, *flags.agentAccount.StoreRef+"\x00"+flags.agentAccount.Name, limit, flags.cursor)
		if err != nil {
			return changeReadError(err)
		}
		if flags.agent {
			meta := agentMeta(flags)
			meta.Limit = limit
			meta.Page = &out.AgentPage{Returned: len(page.Changes), HasMore: page.HasMore, NextCursor: &page.NextCursor}
			return out.WriteAgentJSON(os.Stdout, flags.agentAccount, meta, struct {
				Changes      []store.Change `json:"changes"`
				IntroducedAt time.Time      `json:"introduced_at"`
			}{page.Changes, page.IntroducedAt})
		}
		if flags.asJSON {
			return out.WriteJSON(os.Stdout, page)
		}
		tw := newTableWriter(os.Stdout)
		fmt.Fprintln(tw, "EVENT ID\tKIND\tCHAT\tMESSAGE ID")
		for _, c := range page.Changes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sanitize(c.EventID), sanitize(c.Kind), sanitize(c.ChatJID), sanitize(c.ID))
		}
		if err = tw.Flush(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "has_more: %t\nnext_cursor: %s\n", page.HasMore, page.NextCursor)
		return err
	}
	parent.AddCommand(cmd)
	return parent
}

func changeReadError(err error) error {
	var cursor *store.ChangesCursorError
	if errors.As(err, &cursor) {
		code := "invalid_cursor"
		if cursor.Expired {
			code = "cursor_expired"
		}
		return &out.AgentError{Code: code, Message: cursor.Error(), ExitCode: 2, Cause: err, Recovery: "Inspect archive continuity; restart without --cursor only when replaying the retained feed is intended."}
	}
	return agentStoreError(err)
}
