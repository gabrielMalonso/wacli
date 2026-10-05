package store

import (
	"context"
	"database/sql"
	"math"
	"time"
)

// ConfirmDraftCleanup revalidates and durably reaffirms discard before unlink.
// The caller owns LOCK/owner slot through the later filesystem operation.
// updated_at is not a cleanup journal or evidence that any bytes were removed.
func (d *DB) ConfirmDraftCleanup(ctx context.Context, selection DraftCleanupSelection) (DraftEntry, error) {
	return d.confirmDraftCleanup(ctx, selection, outboundSQLiteIO(), time.Now().UTC())
}

func (d *DB) confirmDraftCleanup(ctx context.Context, selection DraftCleanupSelection, disk outboundCommitIO, at time.Time) (DraftEntry, error) {
	if err := selection.Validate(); err != nil {
		return DraftEntry{}, err
	}
	entry, err := outboundFullWrite(ctx, d.sql, disk, func(c *sql.Conn) (DraftEntry, error) {
		entry, err := readDraftCleanupSelection(ctx, c, selection)
		if err != nil {
			return DraftEntry{}, err
		}
		before := entry.Record.UpdatedAt.UnixNano()
		if before == math.MaxInt64 || at.IsZero() || at.UnixNano() <= 0 || !time.Unix(0, at.UnixNano()).Equal(at) {
			return DraftEntry{}, DraftCleanupFailure(DraftCleanupStoreError, selection, nil)
		}
		// Always dirty the row, even with a backwards clock or repeated timestamp;
		// a no-op transaction is not a durability barrier for prior NORMAL writes.
		next := max(before+1, at.UnixNano())
		result, err := c.ExecContext(ctx, `UPDATE drafts SET updated_at=? WHERE id=? AND state='discarded' AND head_revision_id=? AND updated_at=?`, next, selection.DraftID, selection.ExpectedHeadID, before)
		if err != nil {
			return DraftEntry{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return DraftEntry{}, err
		}
		if n != 1 {
			return DraftEntry{}, DraftCleanupFailure(DraftCleanupConflict, selection, nil)
		}
		entry.Record.UpdatedAt = time.Unix(0, next).UTC()
		return entry, nil
	})
	if err != nil {
		return DraftEntry{}, draftCleanupStoreFailure(err, selection)
	}
	return entry, nil
}
