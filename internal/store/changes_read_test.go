package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChangesReadOnlyContextAndLegacyBusyDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "wacli.db")
	if db, err := OpenChangesReadOnly(t.Context(), path); err == nil {
		db.Close()
		t.Fatal("opened missing archive")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("created directory: %v", err)
	}
	writer := openTestDB(t)
	reader, err := OpenChangesReadOnly(t.Context(), writer.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var busy int
	if err = reader.sql.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 50 {
		t.Fatalf("watch busy=%d err=%v", busy, err)
	}
	if _, err = reader.sql.Exec("INSERT INTO archive_changes(kind,chat_jid,msg_id) VALUES('message_insert','fixture','forbidden')"); err == nil {
		t.Fatal("reader wrote archive")
	}
	legacy, err := OpenReadOnly(writer.path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err = legacy.ListChangesWithShortWait(t.Context(), "fixture", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err = legacy.sql.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 5000 {
		t.Fatalf("changed legacy busy=%d err=%v", busy, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if db, err := OpenChangesReadOnly(ctx, writer.path); !errors.Is(err, context.Canceled) || db != nil {
		t.Fatalf("cancelled open db=%v err=%v", db, err)
	}
	if _, err = reader.ListChangesWithShortWait(ctx, "fixture", 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestChangesShortWaitRestoresLegacyBusyAfterCancelledPoll(t *testing.T) {
	writer := openTestDB(t)
	if _, err := writer.sql.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(writer.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// Keep the exclusive fixture transaction on one physical connection.
	writer.sql.SetMaxOpenConns(1)
	if _, err = writer.sql.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer writer.sql.Exec("ROLLBACK")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = reader.ListChangesWithShortWait(ctx, "fixture", 1, "")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("elapsed=%s err=%v", time.Since(started), err)
	}
	if _, err = writer.sql.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var busy int
	if err = reader.sql.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 5000 {
		t.Fatalf("busy=%d err=%v", busy, err)
	}
	if _, err = reader.ListChangesWithShortWait(t.Context(), "fixture", 1, ""); err != nil {
		t.Fatal(err)
	}
}
