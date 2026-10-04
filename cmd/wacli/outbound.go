package main

import (
	"context"
	"fmt"

	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newOutboundCmd(flags *rootFlags) *cobra.Command {
	c := &cobra.Command{Use: "outbound", Short: "Inspect retained outbound operations offline (sending is not available)"}
	c.AddCommand(newOutboundShowCmd(flags), newOutboundListCmd(flags))
	return c
}
func newOutboundShowCmd(flags *rootFlags) *cobra.Command {
	var key, account string
	var limit int
	c := &cobra.Command{Use: "show [OPERATION_ID]", Short: "Inspect one operation and a bounded page of retained facts", Args: cobra.MaximumNArgs(1)}
	c.Flags().StringVar(&key, "key", "", "literal retained idempotency key (requires --account-jid)")
	c.Flags().StringVar(&account, "account-jid", "", "frozen own PN identity for a key lookup; never opens a session")
	c.Flags().IntVar(&limit, "limit", 20, "maximum observations in this page (1 to 200)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		id := ""
		if len(args) == 1 {
			id = args[0]
		}
		if id != "" {
			if cmd.Flags().Changed("key") || cmd.Flags().Changed("account-jid") || store.ValidateDraftID(id) != nil {
				return agentUsageError(fmt.Errorf("use an operation ID or --key with --account-jid"))
			}
		} else if store.ValidateOutboundKey(key) != nil || store.ValidateOutboundAccount(account) != nil {
			return agentUsageError(fmt.Errorf("use an operation ID or a bounded literal --key with canonical own PN --account-jid"))
		}
		if err := validateOutboundQuery(limit, flags.cursor, cmd.Flags().Changed("cursor")); err != nil {
			return err
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		a, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyOutboundError(err)
		}
		defer closeApp(a, lk)
		entry, err := a.DB().Outbound().Read(ctx, id, key, account, limit, flags.cursor)
		if err != nil {
			return classifyOutboundError(err)
		}
		return writeOutboundEntry(flags, entry)
	}
	return c
}
func newOutboundListCmd(flags *rootFlags) *cobra.Command {
	var account string
	var limit int
	c := &cobra.Command{Use: "list", Short: "List retained operations from the selected archive without connecting", Args: cobra.NoArgs}
	c.Flags().StringVar(&account, "account-jid", "", "optional frozen own PN identity filter")
	c.Flags().IntVar(&limit, "limit", 20, "maximum operations (1 to 200)")
	c.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("account-jid") && store.ValidateOutboundAccount(account) != nil {
			return agentUsageError(fmt.Errorf("--account-jid must be a canonical own PN JID"))
		}
		if err := validateOutboundQuery(limit, flags.cursor, cmd.Flags().Changed("cursor")); err != nil {
			return err
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		a, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyOutboundError(err)
		}
		defer closeApp(a, lk)
		page, err := a.DB().Outbound().List(ctx, a.StoreDir(), account, limit, flags.cursor)
		if err != nil {
			return classifyOutboundError(err)
		}
		return writeOutboundList(flags, page)
	}
	return c
}
func validateOutboundQuery(limit int, cursor string, cursorSet bool) error {
	if limit < 1 || limit > 200 {
		return agentUsageError(fmt.Errorf("--limit must be 1 to 200"))
	}
	if cursorSet {
		if err := store.ValidateOutboundCursor(cursor); err != nil {
			return classifyOutboundError(err)
		}
	}
	return nil
}
