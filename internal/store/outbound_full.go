package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// Kept at the SQLite boundary so fixtures can exercise lost commits and cleanup
// failures without opening a session or altering the production SQL driver.
type outboundCommitIO struct {
	exec        func(context.Context, *sql.Conn, string, ...any) (sql.Result, error)
	synchronous func(context.Context, *sql.Conn) (int, error)
	busyTimeout func(context.Context, *sql.Conn) (int, error)
}

func outboundSQLiteIO() outboundCommitIO {
	return outboundCommitIO{
		exec: func(ctx context.Context, c *sql.Conn, q string, args ...any) (sql.Result, error) {
			return c.ExecContext(ctx, q, args...)
		},
		synchronous: func(ctx context.Context, c *sql.Conn) (int, error) {
			var n int
			err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&n)
			return n, err
		},
		busyTimeout: func(ctx context.Context, c *sql.Conn) (int, error) {
			var n int
			err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&n)
			return n, err
		},
	}
}

// Lease the SAME archive connection. BEGIN IMMEDIATE serializes local CAS and
// reserve/discard races; no SQL transaction spans an upload or network call.
// A commit error or failed cleanup never returns a value authorizing progress.
func outboundFullWrite[T any](ctx context.Context, db *sql.DB, disk outboundCommitIO, work func(*sql.Conn) (T, error)) (value T, err error) {
	var zero T
	c, e := db.Conn(ctx)
	if e != nil {
		return zero, outboundError("store_error", e)
	}
	defer c.Close()
	before, e := disk.synchronous(ctx, c)
	if e != nil || before < 0 || before > 3 {
		return zero, outboundError("store_error", errors.Join(e, fmt.Errorf("cannot read synchronous configuration")))
	}
	busyBefore, e := disk.busyTimeout(ctx, c)
	if e != nil || busyBefore < 0 {
		return zero, outboundError("store_error", e)
	}
	transaction, commitAttempted := false, false
	defer func() {
		// Caller cancellation cannot skip rollback/restoration. This is bounded
		// independently; failures evict the physical connection from the pool.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var cleanupErr error
		if transaction {
			_, cleanupErr = disk.exec(cleanup, c, "ROLLBACK")
		}
		_, restoreErr := disk.exec(cleanup, c, fmt.Sprintf("PRAGMA synchronous=%d", before))
		got, checkErr := disk.synchronous(cleanup, c)
		if got != before && checkErr == nil {
			checkErr = fmt.Errorf("synchronous restoration not confirmed")
		}
		_, restoreBusy := disk.exec(cleanup, c, fmt.Sprintf("PRAGMA busy_timeout=%d", busyBefore))
		busyGot, checkBusy := disk.busyTimeout(cleanup, c)
		if checkBusy == nil && busyGot != busyBefore {
			checkBusy = fmt.Errorf("busy timeout restoration not confirmed")
		}
		cleanupErr = errors.Join(cleanupErr, restoreErr, checkErr, restoreBusy, checkBusy)
		if cleanupErr != nil {
			// database/sql honors ErrBadConn from Raw and discards this driver
			// connection instead of returning unexpected settings to the pool.
			_ = c.Raw(func(any) error { return driver.ErrBadConn })
			code := "store_error"
			if commitAttempted {
				code = "write_uncertain"
			}
			value = zero
			err = outboundError(code, errors.Join(err, cleanupErr))
		}
	}()
	// Context cancellation alone may wait through sqlite3's native busy handler.
	// Only this lease gets a short contention budget; legacy pool settings return.
	busy := min(busyBefore, 100)
	if deadline, ok := ctx.Deadline(); ok {
		busy = min(busy, max(1, int(time.Until(deadline).Milliseconds())))
	}
	if _, e = disk.exec(ctx, c, fmt.Sprintf("PRAGMA busy_timeout=%d", busy)); e != nil {
		return zero, outboundError("store_error", e)
	}
	if got, check := disk.busyTimeout(ctx, c); check != nil || got != busy {
		return zero, outboundError("store_error", errors.Join(check, fmt.Errorf("busy timeout not confirmed")))
	}
	if _, e = disk.exec(ctx, c, "PRAGMA synchronous=FULL"); e != nil {
		return zero, outboundError("store_error", e)
	}
	got, e := disk.synchronous(ctx, c)
	if e != nil || got != 2 {
		return zero, outboundError("store_error", errors.Join(e, fmt.Errorf("FULL configuration not confirmed")))
	}
	if _, e = disk.exec(ctx, c, "BEGIN IMMEDIATE"); e != nil {
		return zero, outboundError("store_error", e)
	}
	transaction = true
	value, e = work(c)
	if e != nil {
		var failure *OutboundError
		if errors.As(e, &failure) {
			return zero, e
		}
		return zero, outboundError("store_error", e)
	}
	if e = ctx.Err(); e != nil {
		return zero, outboundError("store_error", e)
	}
	commitAttempted = true
	if _, e = disk.exec(ctx, c, "COMMIT"); e != nil {
		return zero, outboundError("write_uncertain", e)
	}
	transaction = false
	return value, nil
}
