package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/app"
)

// A distinct kind makes older owners reject this request explicitly.
const historyBackfillKind = "history_backfill"

type backfillDelegateOptions struct {
	ChatJID  string `json:"chat_jid"`
	Count    int    `json:"count"`
	Requests int    `json:"requests"`
	WaitMS   int64  `json:"wait_ms"`
	IdleMS   int64  `json:"idle_ms"`
}

func (o backfillDelegateOptions) options() (app.BackfillOptions, error) {
	// Validate before multiplying durations to prevent overflow in untrusted IPC.
	if o.WaitMS < 0 || o.IdleMS < 0 || o.WaitMS > int64(5*time.Minute/time.Millisecond) || o.IdleMS > int64(5*time.Minute/time.Millisecond) {
		return app.BackfillOptions{}, fmt.Errorf("backfill wait and idle must be between 0 and 5m")
	}
	return app.PrepareBackfillOptions(app.BackfillOptions{
		ChatJID: o.ChatJID, Count: o.Count, Requests: o.Requests,
		WaitPerRequest: time.Duration(o.WaitMS) * time.Millisecond,
		IdleExit:       time.Duration(o.IdleMS) * time.Millisecond,
	})
}

func delegateHistoryBackfill(ctx context.Context, flags *rootFlags, lockErr error, opts app.BackfillOptions) error {
	resp, _, err := tryDelegateSend(ctx, flags, lockErr, sendDelegateRequest{
		Kind: historyBackfillKind,
		Backfill: &backfillDelegateOptions{
			ChatJID: opts.ChatJID, Count: opts.Count, Requests: opts.Requests,
			WaitMS: max(1, durationMillis(opts.WaitPerRequest)), IdleMS: max(1, durationMillis(opts.IdleExit)),
		},
	})
	if err != nil {
		return err
	}
	if resp.Backfill == nil {
		return fmt.Errorf("running sync returned no backfill result; history may already have been persisted; check before retrying")
	}
	return writeBackfillResult(os.Stdout, *resp.Backfill, flags.asJSON)
}

func executeDelegatedBackfill(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.Backfill == nil {
		return sendDelegateResponse{}, fmt.Errorf("missing history backfill options")
	}
	opts, err := req.Backfill.options()
	if err != nil {
		return sendDelegateResponse{}, err
	}
	res, err := a.BackfillHistoryConnected(ctx, opts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Backfill: &res}, nil
}

func delegateTransportError(kind string, err error) error {
	if kind == historyBackfillKind {
		return fmt.Errorf("no reliable reply from running sync after attempting history backfill dispatch; history may already have been persisted; check before retrying: %w", err)
	}
	return err
}
