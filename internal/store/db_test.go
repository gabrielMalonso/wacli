package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenUsesExactPercentEscapedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store%3fprod%23one", "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.UpsertChat("123@s.whatsapp.net", "direct", "Exact", nowUTC()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat exact db path: %v", err)
	}

	roDB, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer roDB.Close()
	count, err := roDB.CountChats()
	if err != nil {
		t.Fatalf("CountChats: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountChats = %d, want exact DB row", count)
	}
}

func TestOpenReadOnlyCleanWALKeepsDatabaseBytesAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertNoSQLiteSidecars(t, path)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	roDB, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if err := roDB.Close(); err != nil {
		t.Fatalf("Close read-only: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || info.Mode().Perm() != 0o640 {
		t.Fatal("readonly changed the database bytes or permissions")
	}
}

func TestOpenReadOnlyReadsLiveWALSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.UpsertChat("123@s.whatsapp.net", "direct", "Live", nowUTC()); err != nil {
		t.Fatalf("UpsertChat: %v", err)
	}
	assertSQLiteSidecarsExist(t, path)

	roDB, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer roDB.Close()
	count, err := roDB.CountChats()
	if err != nil {
		t.Fatalf("CountChats: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountChats = %d, want live WAL row", count)
	}
}

func TestOpenReadOnlyUsesNormalSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	uri := sqliteURI(path, true)
	if !strings.Contains(uri, "mode=ro") || !strings.Contains(uri, "_query_only=1") || strings.Contains(uri, "immutable=") {
		t.Fatalf("readonly URI must use normal SQLite: %s", uri)
	}
}

func assertNoSQLiteSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("%s stat error = %v, want not exist", path+suffix, err)
		}
	}
}

func assertSQLiteSidecarsExist(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("%s stat error = %v, want exist", path+suffix, err)
		}
	}
}
