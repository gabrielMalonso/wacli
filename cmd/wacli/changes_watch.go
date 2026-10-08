package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newChangesWatchCmd(flags *rootFlags) *cobra.Command {
	var limit int
	var interval time.Duration
	cmd := &cobra.Command{
		Use: "watch", Short: "Continuously read retained local changes as NDJSON pages",
		Long: "Emit NDJSON pages from the existing archive, starting at the retained beginning without --cursor.\nThe first page is emitted even when empty; unchanged empty polls are silent.\nSave next_cursor only after processing a complete frame. No WhatsApp connection or sync is started.\nThe global --timeout defaults to 5m; use 0 to wait until cancellation, or 100ms..24h.\nSIGINT/SIGTERM, timeout and output failures exit nonzero without a final checkpoint.",
		Args: cobra.NoArgs,
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum changes per frame (1-200; agent default 20)")
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval after draining backlog (100ms-1m)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Default stdout is also NDJSON, and legacy failures use the JSON envelope.
		flags.asJSON = true
		if limit < 1 || limit > 200 || interval < 100*time.Millisecond || interval > time.Minute {
			return agentUsageError(fmt.Errorf("changes watch requires --limit 1..200 and --interval 100ms..1m"))
		}
		if flags.timeout != 0 && (flags.timeout < 100*time.Millisecond || flags.timeout > 24*time.Hour) {
			return agentUsageError(fmt.Errorf("changes watch requires --timeout 0 or 100ms..24h"))
		}
		if cmd.Flags().Changed("cursor") {
			if err := store.ValidateChangesCursor(flags.cursor); err != nil {
				return changeReadError(err)
			}
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := withTimeout(signalCtx, flags)
		defer cancel()
		db, err := store.OpenChangesReadOnly(ctx, filepath.Join(flags.storeDir, "wacli.db"))
		if err != nil {
			if ctx.Err() != nil {
				return changesWatchContextError(ctx.Err())
			}
			return agentStoreError(err)
		}
		defer db.Close()
		output, closeOutput, err := changesWatchOutput(ctx, os.Stdout)
		if err != nil {
			return &out.AgentError{Code: "output_failed", Message: "Change watch output is unavailable.", ExitCode: 1, Cause: err}
		}
		defer closeOutput()
		scope := *flags.agentAccount.StoreRef + "\x00" + flags.agentAccount.Name
		return watchChanges(ctx, db, scope, limit, flags.cursor, interval, func(page store.ChangesPage) error {
			if err := ctx.Err(); err != nil {
				return changesWatchContextError(err)
			}
			err := writeChangesFrame(output, flags, limit, page)
			if err != nil && ctx.Err() != nil {
				return changesWatchContextError(ctx.Err())
			}
			// The file deadline can fire just before the context timer is scheduled.
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return changesWatchContextError(context.DeadlineExceeded)
			}
			return err
		})
	}
	return cmd
}

// Deadlines also wake a pollable stdout pipe/socket when its consumer stalls.
// Regular-file IO and handles without deadline support retain OS semantics.
func changesWatchOutput(ctx context.Context, stdout *os.File) (io.Writer, func(), error) {
	file, closeFile, err := changesWatchOutputFile(stdout)
	if err != nil {
		return nil, nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = file.SetWriteDeadline(deadline)
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = file.SetWriteDeadline(time.Now())
		close(done)
	})
	return file, func() {
		if !stop() {
			<-done
		}
		_ = file.SetWriteDeadline(time.Time{})
		closeFile()
	}, nil
}

func writeChangesFrame(w io.Writer, flags *rootFlags, limit int, page store.ChangesPage) error {
	var err error
	if flags.agent {
		meta := agentMeta(flags)
		meta.Limit = limit
		meta.Page = &out.AgentPage{Returned: len(page.Changes), HasMore: page.HasMore, NextCursor: &page.NextCursor}
		// Strict output policy: unlike finite queries, EPIPE must stop the reader.
		err = out.WriteAgentActionJSON(w, flags.agentAccount, meta, struct {
			Changes      []store.Change `json:"changes"`
			IntroducedAt time.Time      `json:"introduced_at"`
		}{page.Changes, page.IntroducedAt})
	} else {
		err = out.WriteActionJSON(w, page)
	}
	if err != nil {
		var typed *out.AgentError
		if errors.As(err, &typed) {
			return err
		}
		return &out.AgentError{Code: "output_failed", Message: "Change frame output failed; resume from the last completely processed frame.", ExitCode: 1, Cause: err}
	}
	return nil
}

// Each ListChanges call releases its snapshot before output or waiting. A commit
// between an empty read and the wait is seen on the next poll with the same cursor.
func watchChanges(ctx context.Context, db *store.DB, scope string, limit int, cursor string, interval time.Duration, emit func(store.ChangesPage) error) error {
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return changesWatchContextError(err)
		}
		page, err := db.ListChangesWithShortWait(ctx, scope, limit, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return changesWatchContextError(ctx.Err())
			}
			return changeReadError(err)
		}
		if err := ctx.Err(); err != nil {
			return changesWatchContextError(err)
		}
		if first || len(page.Changes) > 0 || page.NextCursor != cursor {
			if err = emit(page); err != nil {
				return err
			}
			cursor = page.NextCursor
			first = false
		}
		if page.HasMore {
			continue
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return changesWatchContextError(ctx.Err())
		case <-timer.C:
		}
	}
}

func changesWatchContextError(err error) error {
	code, message := "cancelled", "Change watch cancelled."
	if errors.Is(err, context.DeadlineExceeded) {
		code, message = "timeout", "Change watch timeout reached."
	}
	return &out.AgentError{Code: code, Message: message, ExitCode: 1, Cause: err}
}
