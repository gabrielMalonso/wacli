package app

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/openclaw/wacli/internal/out"
	"go.mau.fi/whatsmeow/types/events"
)

func TestDiagnosticLateLogoutPreservesObservedStop(t *testing.T) {
	for _, reason := range []string{"failed", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			a := newTestApp(t)
			a.opts.Events = out.NewEventWriter(io.Discard, true)
			f := newFakeWA()
			a.wa = f
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var copiedSync func(any)
			result, err := a.Sync(ctx, SyncOptions{Mode: SyncModeFollow, AfterConnect: func(context.Context) error {
				f.mu.Lock()
				for id, handler := range f.handlers {
					if id != a.sessionHandler {
						copiedSync = handler
					}
				}
				f.mu.Unlock()
				if reason == "failed" {
					return errors.New("synthetic operation failed")
				}
				cancel()
				return nil
			}})
			if (err != nil) != (reason == "failed") || copiedSync == nil {
				t.Fatal("invalid production Sync fixture")
			}
			// The SDK may have copied this handler before RemoveEventHandler. Admissions
			// remain open until App.Close; the late logout is an observed independent fact.
			logout := &events.LoggedOut{Reason: events.ConnectFailureLoggedOut}
			f.emit(logout)
			copiedSync(logout)
			a.Close()
			observed := result.ObservationsSnapshot()
			if observed.Sync.StopReason != reason || observed.Connection.LoggedOutAt == nil {
				t.Fatalf("late logout erased stop %s: %s", reason, observed.Sync.StopReason)
			}
		})
	}
}
