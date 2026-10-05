package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func seedReadonlyArchive(t *testing.T, journal string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openReadonlyArchiveWriter(t, path)
	var mode string
	if err := raw.QueryRow("PRAGMA journal_mode=" + journal).Scan(&mode); err != nil || !strings.EqualFold(mode, journal) {
		t.Fatalf("journal mode = %s, %v", mode, err)
	}
	if _, err := raw.Exec("INSERT INTO chats(jid, kind, last_message_ts) VALUES('synthetic@s.whatsapp.net', 'dm', 1)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoSQLiteSidecars(t, path)
	return path
}

func openReadonlyArchiveWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", sqliteURI(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func makeReadonlyArchiveDirectory(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permissions without root bypass")
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Chmod(filepath.Join(dir, entry.Name()), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
}

func assertReadonlyArchiveOpen(t *testing.T, path string, wantReadable bool, wantChats int64) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(path)
	if wantReadable {
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if count, err := db.CountChats(); err != nil || count != wantChats {
			t.Fatalf("chats = %d, %v, want %d", count, err, wantChats)
		}
		var queryOnly, migrations int
		if err := db.sql.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("query_only = %d, %v", queryOnly, err)
		}
		if err := db.sql.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrations); err != nil || migrations != len(schemaMigrations) {
			t.Fatalf("migrations = %d, %v", migrations, err)
		}
		if _, err := db.sql.Exec("DELETE FROM chats"); err == nil {
			t.Fatal("readonly accepted a SQL mutation")
		}
	} else if err == nil {
		_ = db.Close()
		t.Fatal("expected explicit failure, without an immutable fallback")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterDir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || info.Mode() != afterInfo.Mode() || dirInfo.Mode() != afterDir.Mode() {
		t.Fatal("readonly changed database bytes or file/directory permissions")
	}
}

func TestReadonlyArchiveCleanJournalPermissions(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		for _, readonlyDir := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readonly-dir=%t", journal, readonlyDir), func(t *testing.T) {
				path := seedReadonlyArchive(t, journal)
				if readonlyDir {
					makeReadonlyArchiveDirectory(t, path)
				}
				assertReadonlyArchiveOpen(t, path, journal == "DELETE" || !readonlyDir, 1)
				if journal == "DELETE" {
					assertNoSQLiteSidecars(t, path)
				}
			})
		}
	}
}

func TestReadonlyArchiveReaderBeforeWriter(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		t.Run(journal, func(t *testing.T) {
			path := seedReadonlyArchive(t, journal)
			reader, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if count, err := reader.CountChats(); err != nil || count != 1 {
				t.Fatalf("initial read = %d, %v", count, err)
			}
			writer := openReadonlyArchiveWriter(t, path)
			if _, err := writer.Exec("INSERT INTO chats(jid, kind) VALUES('later@s.whatsapp.net', 'dm')"); err != nil {
				t.Fatal(err)
			}
			if count, err := reader.CountChats(); err != nil || count != 2 {
				t.Fatalf("reader missed later commit: %d, %v", count, err)
			}
		})
	}
}

func TestReadonlyArchiveCommittedWALPermissions(t *testing.T) {
	for _, withSHM := range []bool{false, true} {
		for _, readonlyDir := range []bool{false, true} {
			t.Run(fmt.Sprintf("shm=%t/readonly-dir=%t", withSHM, readonlyDir), func(t *testing.T) {
				source := seedReadonlyArchive(t, "WAL")
				writer := openReadonlyArchiveWriter(t, source)
				if _, err := writer.Exec("INSERT INTO chats(jid, kind) VALUES('wal@s.whatsapp.net', 'dm')"); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "wacli.db")
				suffixes := []string{"", "-wal"}
				if withSHM {
					suffixes = append(suffixes, "-shm")
				}
				// The synthetic writer is paused during copying; this is not a backup API.
				for _, suffix := range suffixes {
					data, err := os.ReadFile(source + suffix)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path+suffix, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if readonlyDir {
					makeReadonlyArchiveDirectory(t, path)
				}
				assertReadonlyArchiveOpen(t, path, withSHM || !readonlyDir, 2)
				if readonlyDir && withSHM {
					for _, suffix := range suffixes {
						info, err := os.Stat(path + suffix)
						if err != nil || info.Mode().Perm() != 0o400 {
							t.Fatalf("changed %s permissions: %v", suffix, err)
						}
					}
				}
			})
		}
	}
}

func TestReadonlyArchiveMissingDoesNotCreate(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			dir := t.TempDir()
			if nested {
				dir = filepath.Join(dir, "absent")
			}
			path := filepath.Join(dir, "wacli.db")
			if _, err := OpenReadOnly(path); err == nil {
				t.Fatal("missing archive accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("created archive", err)
			}
			if nested {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("created directory", err)
				}
			}
		})
	}
}

func TestReadonlyArchiveRejectsUnreadableFileAndURIInjection(t *testing.T) {
	for _, suffix := range []string{"?mode=rw", "#fragment"} {
		if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "wacli.db") + suffix); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Fatal("URI injection not rejected", err)
		}
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix file permissions without root bypass")
	}
	path := seedReadonlyArchive(t, "DELETE")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenReadOnly(path); err == nil {
		_ = db.Close()
		t.Fatal("unreadable archive accepted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatal("reader normalized permissions", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reader changed unreadable archive", err)
	}
	assertNoSQLiteSidecars(t, path)
}
