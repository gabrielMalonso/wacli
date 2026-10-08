package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/sqliteutil"
	"github.com/openclaw/wacli/internal/store/storedb"
)

const changeReadBusyWait = 50 * time.Millisecond

// OpenChangesReadOnly opens only an existing archive for feed consumption. Its
// physical connections use short native busy waits, including during opening;
// schema inspection honors ctx and requires no FTS/session/identity inspection.
// Legacy OpenReadOnly and its connection defaults remain unchanged.
func OpenChangesReadOnly(ctx context.Context, path string) (*DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDBPath(path); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("read local store: %w", err)
	}
	params := fmt.Sprintf("_foreign_keys=on&_busy_timeout=%d&mode=ro&_query_only=1", changeReadBusyWait.Milliseconds())
	db, err := sql.Open("sqlite3", sqliteutil.FileURI(path, params))
	if err != nil {
		return nil, err
	}
	d := &DB{path: path, sql: db, q: storedb.New(db)}
	if err = retryChangeRead(ctx, func() error { return d.validateReadableContext(ctx) }); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read-only sqlite: %w", err)
	}
	return d, nil
}

// ListChangesWithShortWait keeps native contention bounded on each lease, and
// releases failed snapshots before cancellable retries. Restore the previous
// setting before returning the connection to callers using legacy readers.
func (d *DB) ListChangesWithShortWait(ctx context.Context, scope string, limit int, token string) (ChangesPage, error) {
	var page ChangesPage
	err := retryChangeRead(ctx, func() error {
		var err error
		page, err = d.listChangesWithShortWait(ctx, scope, limit, token)
		return err
	})
	return page, err
}

func (d *DB) listChangesWithShortWait(ctx context.Context, scope string, limit int, token string) (page ChangesPage, err error) {
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return page, err
	}
	defer conn.Close()
	var previous int
	if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&previous); err != nil {
		return page, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeReadBusyWait)
		defer cancel()
		if _, restoreErr := conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA busy_timeout=%d", previous)); restoreErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = fmt.Errorf("restore change reader busy timeout: %w", restoreErr)
		}
	}()
	busy := min(previous, int(changeReadBusyWait.Milliseconds()))
	if deadline, ok := ctx.Deadline(); ok {
		busy = min(busy, max(0, int(time.Until(deadline).Milliseconds())))
	}
	if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busy)); err != nil {
		return page, err
	}
	if err = ctx.Err(); err != nil {
		return page, err
	}
	return listChanges(ctx, conn, scope, limit, token)
}

// Retry only SQLite contention, for at most five seconds per read. Each attempt
// finishes synchronously; there is no abandoned opener/query or held snapshot.
// Exhausting this contention budget preserves the store error, not a fabricated
// command timeout when the caller's context is still live.
func retryChangeRead(ctx context.Context, read func() error) error {
	until := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := read()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isChangeReadContention(err) || !time.Now().Before(until) {
			return err
		}
		timer := time.NewTimer(min(changeReadBusyWait, time.Until(until)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
