package main

import (
	"context"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
)

// Maintenance previews only read the archive; execution keeps the writer lock
// and writable session/store initialization used by destructive commands.
func newMaintenanceApp(ctx context.Context, flags *rootFlags, dryRun bool) (*app.App, *lock.Lock, error) {
	if dryRun {
		return newReadApp(ctx, flags)
	}
	return newApp(ctx, flags, true, false)
}
