package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// historyRuntime exists only after follow has connected and migrated aliases.
// Its cancellation is independent of the operation and never closes the socket.
type historyRuntime struct {
	ctx            context.Context
	cancel         context.CancelFunc
	messagesStored *atomic.Int64
}

type historyObserver struct {
	mu       sync.Mutex
	active   bool
	inFlight int
	last     time.Time
	after    func(*events.HistorySync)
	storeErr func(types.JID, error)
}

func newHistoryObserver(after func(*events.HistorySync), storeErr func(types.JID, error)) *historyObserver {
	return &historyObserver{active: true, last: time.Now(), after: after, storeErr: storeErr}
}

func (a *App) startHistoryRuntime(ctx context.Context, stored *atomic.Int64) *historyRuntime {
	ctx, cancel := context.WithCancel(ctx)
	r := &historyRuntime{ctx: ctx, cancel: cancel, messagesStored: stored}
	a.historyMu.Lock()
	a.historyRuntime = r
	a.historyMu.Unlock()
	return r
}

func (a *App) stopHistoryRuntime(r *historyRuntime) {
	r.cancel()
	a.historyMu.Lock()
	if a.historyRuntime == r {
		a.historyRuntime = nil
	}
	a.historyMu.Unlock()
}

func (a *App) registerHistoryObserver(r *historyRuntime, o *historyObserver) error {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	if a.historyRuntime != r || r.ctx.Err() != nil {
		return fmt.Errorf("sync follow owner stopped before backfill dispatch")
	}
	if a.historyObserver != nil {
		return fmt.Errorf("history backfill is already running")
	}
	a.historyObserver = o
	return nil
}

func (a *App) removeHistoryObserver(o *historyObserver) {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	o.mu.Lock()
	o.active = false
	o.mu.Unlock()
	if a.historyObserver == o {
		a.historyObserver = nil
	}
}

// Capture once, before any download or persistence. A callback already in flight
// retains this operation's observer even if a later operation has registered.
func (a *App) historyEventOptions(opts SyncOptions, evt any) (SyncOptions, func()) {
	a.historyMu.Lock()
	o := a.historyObserver
	if o != nil {
		captured := o
		o.mu.Lock()
		if o.active {
			o.inFlight++
		} else {
			o = nil
		}
		captured.mu.Unlock()
	}
	a.historyMu.Unlock()
	if o == nil {
		return opts, func() {}
	}
	opts.afterHistorySync = o.after
	opts.historyStoreError = o.storeErr
	return opts, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.inFlight--
		// Match the ordinary message/history activity that can extend sync idle.
		// Typing and keepalive notifications do not hold this window open.
		switch evt.(type) {
		case *events.Message, *events.HistorySync, *events.Receipt, *events.UndecryptableMessage,
			*events.CallOffer, *events.CallAccept, *events.CallPreAccept, *events.CallTransport,
			*events.CallOfferNotice, *events.CallRelayLatency, *events.CallTerminate, *events.CallReject,
			*events.AppState, *events.Star, *events.DeleteForMe,
			*events.Archive, *events.Pin, *events.Mute, *events.MarkChatAsRead:
			if o.active {
				o.last = time.Now()
			}
		}
	}
}

func (o *historyObserver) waitIdle(ctx context.Context, idle time.Duration) error {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			o.mu.Lock()
			remaining := idle - time.Since(o.last)
			if o.inFlight == 0 && remaining <= 0 {
				// Closing and checking in-flight callbacks is one transition, so a
				// successful result cannot race with another observer callback.
				o.active = false
				o.mu.Unlock()
				return ctx.Err()
			}
			o.mu.Unlock()
			if remaining <= 0 {
				remaining = min(idle, 10*time.Millisecond)
			}
			timer.Reset(remaining)
		}
	}
}
