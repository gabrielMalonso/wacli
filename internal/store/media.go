package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/openclaw/wacli/internal/store/storedb"
)

func (d *DB) GetMediaDownloadInfo(chatJID, msgID string) (MediaDownloadInfo, error) {
	return d.GetMediaDownloadInfoContext(storeCtx(), chatJID, msgID)
}

func (d *DB) GetMediaDownloadInfoContext(ctx context.Context, chatJID, msgID string) (MediaDownloadInfo, error) {
	row, err := d.q.GetMediaDownloadInfo(ctx, storedb.GetMediaDownloadInfoParams{ChatJid: chatJID, MsgID: msgID})
	if err != nil {
		return MediaDownloadInfo{}, err
	}
	info := MediaDownloadInfo{
		ChatJID:            row.ChatJid,
		ChatName:           row.Name,
		MsgID:              row.MsgID,
		MediaType:          row.MediaType,
		Filename:           row.Filename,
		MimeType:           row.MimeType,
		DirectPath:         row.DirectPath,
		MediaKey:           row.MediaKey,
		FileSHA256:         row.FileSha256,
		FileEncSHA256:      row.FileEncSha256,
		LocalPath:          row.LocalPath,
		DownloadedAt:       fromUnix(row.DownloadedAt),
		MediaUnavailableAt: fromUnix(row.MediaUnavailableAt),
		Tombstone:          row.Tombstone != 0,
		InvalidFileLength:  row.FileLength < 0,
	}
	if row.FileLength > 0 {
		info.FileLength = uint64(row.FileLength)
	}
	return info, nil
}

// PendingMediaDownload identifies a message whose media has downloadable
// metadata (direct_path + media_key) but has not yet been fetched to disk.
type PendingMediaDownload struct {
	ChatJID string
	MsgID   string
}

// CountPendingMediaDownloads returns how many stored messages have downloadable
// media that has not been fetched yet. Pass a non-empty chatJID to scope the
// count to a single chat.
func (d *DB) CountPendingMediaDownloads(ctx context.Context, chatJID string) (int, error) {
	n, err := d.q.CountPendingMediaDownloads(ctx, chatJID)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// ListPendingMediaDownloads returns messages with downloadable but not-yet-fetched
// media, newest first. Pass a non-empty chatJID to scope to a single chat, and a
// positive limit to cap the number of rows (limit <= 0 means no limit).
func (d *DB) ListPendingMediaDownloads(ctx context.Context, chatJID string, limit int) ([]PendingMediaDownload, error) {
	rows, err := d.q.ListPendingMediaDownloads(ctx, storedb.ListPendingMediaDownloadsParams{
		ChatJid:    chatJID,
		LimitCount: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	pending := make([]PendingMediaDownload, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, PendingMediaDownload{ChatJID: row.ChatJid, MsgID: row.MsgID})
	}
	return pending, nil
}

// ListPendingMediaBefore is like ListPendingMediaDownloads but only returns
// messages older than beforeUnix (seconds). Used to sample pending media by age.
func (d *DB) ListPendingMediaBefore(ctx context.Context, chatJID string, beforeUnix int64, limit int) ([]PendingMediaDownload, error) {
	rows, err := d.q.ListPendingMediaBefore(ctx, storedb.ListPendingMediaBeforeParams{
		BeforeTs:   beforeUnix,
		ChatJid:    chatJID,
		LimitCount: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	pending := make([]PendingMediaDownload, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, PendingMediaDownload{ChatJID: row.ChatJid, MsgID: row.MsgID})
	}
	return pending, nil
}

// MarkMediaUnavailable records that a message's media is no longer retrievable
// (the phone reported it gone via media retry), so pending queries skip it.
func (d *DB) MarkMediaUnavailable(ctx context.Context, chatJID, msgID string, at time.Time) error {
	return d.q.MarkMediaUnavailable(ctx, storedb.MarkMediaUnavailableParams{
		MediaUnavailableAt: sqlNullInt64(unix(at)),
		ChatJid:            chatJID,
		MsgID:              msgID,
	})
}

func (d *DB) MarkMediaDownloaded(chatJID, msgID, localPath string, downloadedAt time.Time) error {
	return d.q.MarkMediaDownloaded(storeCtx(), storedb.MarkMediaDownloadedParams{
		LocalPath:    nullStringIfEmpty(localPath),
		DownloadedAt: sqlNullInt64(unix(downloadedAt)),
		ChatJid:      chatJID,
		MsgID:        msgID,
	})
}

// MarkMediaDownloadedContext limits native SQLite contention on this lease only.
// recorded confirms the update completed, even if restoring the connection later
// fails. Calls finish synchronously; cancellation does not roll back a known write
// or promise interruption of arbitrary filesystem calls.
func (d *DB) MarkMediaDownloadedContext(ctx context.Context, chatJID, msgID, localPath string, downloadedAt time.Time) (recorded bool, err error) {
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var previous int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&previous); err != nil {
		return false, err
	}
	defer func() {
		// Connection-local restoration needs no write lock. Caller cancellation
		// cannot return altered settings to the pool; failed restoration evicts it.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, restoreErr := conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA busy_timeout=%d", previous))
		var restored int
		checkErr := conn.QueryRowContext(cleanup, "PRAGMA busy_timeout").Scan(&restored)
		if checkErr == nil && restored != previous {
			checkErr = errors.New("media busy timeout restoration not confirmed")
		}
		if restoreErr != nil || checkErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, restoreErr, checkErr)
		}
	}()
	// ExecContext joins the driver's work, but its interrupt can wait through
	// sqlite3's native busy handler. Shorten that wait without changing defaults.
	busy := min(previous, 50)
	if deadline, ok := ctx.Deadline(); ok {
		busy = min(busy, max(0, int(time.Until(deadline).Milliseconds())))
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busy)); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	err = storedb.New(conn).MarkMediaDownloaded(ctx, storedb.MarkMediaDownloadedParams{
		LocalPath:    nullStringIfEmpty(localPath),
		DownloadedAt: sqlNullInt64(unix(downloadedAt)),
		ChatJid:      chatJID,
		MsgID:        msgID,
	})
	if err != nil {
		return false, errors.Join(err, ctx.Err())
	}
	return true, nil
}
