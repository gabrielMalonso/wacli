package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPendingMediaQueriesHonorCanceledContext(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := db.CountPendingMediaDownloads(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountPendingMediaDownloads error = %v, want context.Canceled", err)
	}
	if _, err := db.GetMediaDownloadInfoContext(ctx, "chat", "msg"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetMediaDownloadInfoContext error = %v", err)
	}
	if _, err := db.ListPendingMediaDownloads(ctx, "", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPendingMediaDownloads error = %v, want context.Canceled", err)
	}
	if _, err := db.ListPendingMediaBefore(ctx, "", 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListPendingMediaBefore error = %v, want context.Canceled", err)
	}
	if err := db.MarkMediaUnavailable(ctx, "chat", "msg", nowUTC()); !errors.Is(err, context.Canceled) {
		t.Fatalf("MarkMediaUnavailable error = %v, want context.Canceled", err)
	}
}

func TestMarkMediaDownloadedContextRestoresBusyTimeoutAndStopsWriting(t *testing.T) {
	for _, mode := range []string{"success", "busy", "cancel", "cancel_release", "deadline", "pre_cancel"} {
		t.Run(mode, func(t *testing.T) {
			db := openTestDB(t)
			db.sql.SetMaxOpenConns(1)
			chat, id := "123@s.whatsapp.net", "synthetic"
			if err := db.UpsertChat(chat, "dm", "Synthetic", nowUTC()); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: nowUTC(), MediaType: "image"}); err != nil {
				t.Fatal(err)
			}
			if err := db.MarkMediaUnavailable(t.Context(), chat, id, nowUTC()); err != nil {
				t.Fatal(err)
			}
			const previous = 731 // Restore the actual lease setting, not a default.
			if _, err := db.sql.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", previous)); err != nil {
				t.Fatal(err)
			}
			disk, err := sql.Open("sqlite3", db.path)
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			lock, err := disk.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if mode != "success" {
				if _, err := lock.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
					t.Fatal(err)
				}
				defer lock.ExecContext(context.Background(), "ROLLBACK")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var release chan error
			switch mode {
			case "cancel":
				timer := time.AfterFunc(5*time.Millisecond, cancel)
				defer timer.Stop()
			case "cancel_release":
				release = make(chan error, 1)
				time.AfterFunc(5*time.Millisecond, func() {
					cancel()
					_, err := lock.ExecContext(context.Background(), "ROLLBACK")
					release <- err
				})
			case "deadline":
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			case "pre_cancel":
				cancel()
			}
			started := time.Now()
			recorded, err := db.MarkMediaDownloadedContext(ctx, chat, id, "synthetic-output", nowUTC())
			if release != nil {
				if releaseErr := <-release; releaseErr != nil {
					t.Fatal(releaseErr)
				}
			}
			if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
				t.Fatalf("native contention exceeded scoped budget: %s", elapsed)
			}
			if mode == "success" {
				if err != nil || !recorded {
					t.Fatalf("successful record: %t %v", recorded, err)
				}
			} else if err == nil || recorded {
				t.Fatalf("write while locked/cancelled: %t %v", recorded, err)
			}
			if (mode == "cancel" || mode == "cancel_release" || mode == "pre_cancel") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if db.sql.Stats().InUse != 0 {
				t.Fatal("write connection still in use after return")
			}
			// Release only after the call returned. A surviving writer would now
			// be able to commit; the same single-connection pool must be idle.
			if mode != "success" && mode != "cancel_release" {
				if _, err := lock.ExecContext(t.Context(), "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
			}
			var restored int
			if err := db.sql.QueryRow("PRAGMA busy_timeout").Scan(&restored); err != nil || restored != previous {
				t.Fatalf("busy timeout=%d, want %d: %v", restored, previous, err)
			}
			info, err := db.GetMediaDownloadInfo(chat, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" {
				if info.LocalPath != "synthetic-output" || !info.MediaUnavailableAt.IsZero() {
					t.Fatalf("successful record lost: %+v", info)
				}
			} else if info.LocalPath != "" || !info.DownloadedAt.IsZero() || info.MediaUnavailableAt.IsZero() {
				t.Fatalf("failed write committed after release: %+v", info)
			}
			// Confirm the leased connection remains usable and the legacy API
			// still records/clears its marker with its unchanged return contract.
			if err := db.MarkMediaDownloaded(chat, id, "legacy-output", nowUTC()); err != nil {
				t.Fatal(err)
			}
			assertMediaUnavailableCleared(t, db, chat, id)
		})
	}
}

func TestFreshMediaMetadataClearsUnavailableState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	chat := "123@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Chat", time.Now()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	message := UpsertMessageParams{
		ChatJID:    chat,
		MsgID:      "m1",
		Timestamp:  time.Now(),
		MediaType:  "image",
		DirectPath: "/old",
		MediaKey:   []byte{1},
	}
	if err := db.UpsertMessage(message); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := db.MarkMediaUnavailable(ctx, chat, "m1", time.Now()); err != nil {
		t.Fatalf("MarkMediaUnavailable: %v", err)
	}

	message.DirectPath = "/fresh"
	message.Timestamp = message.Timestamp.Add(time.Second)
	if err := db.UpsertMessage(message); err != nil {
		t.Fatalf("UpsertMessage fresh path: %v", err)
	}
	assertMediaUnavailableCleared(t, db, chat, "m1")

	if err := db.MarkMediaUnavailable(ctx, chat, "m1", time.Now()); err != nil {
		t.Fatalf("MarkMediaUnavailable again: %v", err)
	}
	message.DirectPath = "/stale"
	message.Timestamp = message.Timestamp.Add(-time.Minute)
	if err := db.UpsertMessage(message); err != nil {
		t.Fatalf("UpsertMessage stale path: %v", err)
	}
	var directPath string
	var unavailableAt sql.NullInt64
	if err := db.sql.QueryRow(`SELECT direct_path, media_unavailable_at FROM messages WHERE chat_jid = ? AND msg_id = ?`, chat, "m1").Scan(&directPath, &unavailableAt); err != nil {
		t.Fatalf("query stale upsert result: %v", err)
	}
	if directPath != "/fresh" {
		t.Fatalf("direct_path = %q, want /fresh", directPath)
	}
	if !unavailableAt.Valid {
		t.Fatal("stale media metadata cleared media_unavailable_at")
	}
	if err := db.MarkMediaDownloaded(chat, "m1", "/tmp/m1", time.Now()); err != nil {
		t.Fatalf("MarkMediaDownloaded: %v", err)
	}
	assertMediaUnavailableCleared(t, db, chat, "m1")
}

func assertMediaUnavailableCleared(t *testing.T, db *DB, chatJID, msgID string) {
	t.Helper()
	var unavailableAt sql.NullInt64
	if err := db.sql.QueryRow(`SELECT media_unavailable_at FROM messages WHERE chat_jid = ? AND msg_id = ?`, chatJID, msgID).Scan(&unavailableAt); err != nil {
		t.Fatalf("query media_unavailable_at: %v", err)
	}
	if unavailableAt.Valid {
		t.Fatalf("media_unavailable_at still set: %d", unavailableAt.Int64)
	}
}

func TestMediaDownloadInfoObservesTombstonePresenceAndInvalidLength(t *testing.T) {
	db := openTestDB(t)
	chat := "123@s.whatsapp.net"
	if err := db.UpsertChat(chat, "dm", "Fixture", time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "media", Timestamp: time.Unix(1000, 0), MediaType: "audio"}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{0, -1, 1001} {
		if _, err := db.sql.Exec(`UPDATE messages SET deleted_at=? WHERE chat_jid=? AND msg_id=?`, at, chat, "media"); err != nil {
			t.Fatal(err)
		}
		info, err := db.GetMediaDownloadInfo(chat, "media")
		if err != nil || !info.Tombstone {
			t.Fatalf("tombstone at %d: %+v %v", at, info, err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE messages SET deleted_at=NULL,file_length=-1 WHERE chat_jid=? AND msg_id=?`, chat, "media"); err != nil {
		t.Fatal(err)
	}
	info, err := db.GetMediaDownloadInfo(chat, "media")
	if err != nil || info.Tombstone || !info.InvalidFileLength {
		t.Fatalf("invalid length: %+v %v", info, err)
	}
}
