package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/sqliteutil"
	"go.mau.fi/whatsmeow/types"
)

func publicSessionWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", sqliteutil.FileURI(path, "_busy_timeout=1000"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPublicReadonlySession(t *testing.T, journal string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	db := publicSessionWriter(t, path)
	if _, err := db.Exec("PRAGMA journal_mode=" + journal + `;
		CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY, pn TEXT);
		CREATE TABLE whatsmeow_device(jid TEXT, lid TEXT);
		INSERT INTO whatsmeow_lid_map VALUES('100000000001', '15550000001')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoAppSQLiteSidecars(t, path)
	return path
}

func readonlyPublicSessionDirectory(t *testing.T, path string) {
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

func assertPublicSessionReadonlyOpen(t *testing.T, path string, wantReadable bool, wantPN string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := openReadOnlySessionResolver(path)
	if wantReadable {
		if err != nil {
			t.Fatal(err)
		}
		defer resolver.Close()
		lid := types.NewJID("100000000001", types.HiddenUserServer)
		if pn := resolver.ResolveLIDToPN(context.Background(), lid); pn.User != wantPN {
			t.Fatalf("public map = %s, want %s", pn, wantPN)
		}
		var queryOnly int
		if err := resolver.db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("query_only = %d, %v", queryOnly, err)
		}
		if _, err := resolver.db.Exec("DELETE FROM whatsmeow_lid_map"); err == nil {
			t.Fatal("public session reader accepted a SQL mutation")
		}
	} else if err == nil {
		_ = resolver.Close()
		t.Fatal("expected explicit error without immutable fallback")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || info.Mode() != afterInfo.Mode() {
		t.Fatal("public reader changed session bytes or permissions")
	}
}

func TestPublicSessionReadonlyCleanJournalPermissions(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		for _, readonlyDir := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readonly-dir=%t", journal, readonlyDir), func(t *testing.T) {
				path := seedPublicReadonlySession(t, journal)
				if readonlyDir {
					readonlyPublicSessionDirectory(t, path)
				}
				assertPublicSessionReadonlyOpen(t, path, journal == "DELETE" || !readonlyDir, "15550000001")
				if journal == "DELETE" {
					assertNoAppSQLiteSidecars(t, path)
				}
			})
		}
	}
}

func TestPublicSessionReadonlyReaderBeforeWriter(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		t.Run(journal, func(t *testing.T) {
			path := seedPublicReadonlySession(t, journal)
			resolver, err := openReadOnlySessionResolver(path)
			if err != nil {
				t.Fatal(err)
			}
			defer resolver.Close()
			lid := types.NewJID("100000000001", types.HiddenUserServer)
			if pn := resolver.ResolveLIDToPN(context.Background(), lid); pn.User != "15550000001" {
				t.Fatal("initial mapping", pn)
			}
			writer := publicSessionWriter(t, path)
			if _, err := writer.Exec("UPDATE whatsmeow_lid_map SET pn='15550000002'"); err != nil {
				t.Fatal(err)
			}
			if pn := resolver.ResolveLIDToPN(context.Background(), lid); pn.User != "15550000002" {
				t.Fatal("reader missed later mapping commit", pn)
			}
		})
	}
}

func TestPublicSessionReadonlyCommittedWALPermissions(t *testing.T) {
	for _, withSHM := range []bool{false, true} {
		for _, readonlyDir := range []bool{false, true} {
			t.Run(fmt.Sprintf("shm=%t/readonly-dir=%t", withSHM, readonlyDir), func(t *testing.T) {
				source := seedPublicReadonlySession(t, "WAL")
				writer := publicSessionWriter(t, source)
				if _, err := writer.Exec("UPDATE whatsmeow_lid_map SET pn='15550000002'"); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "session.db")
				suffixes := []string{"", "-wal"}
				if withSHM {
					suffixes = append(suffixes, "-shm")
				}
				// A paused synthetic writer makes this copy coherent for the fixture.
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
					readonlyPublicSessionDirectory(t, path)
				}
				assertPublicSessionReadonlyOpen(t, path, withSHM || !readonlyDir, "15550000002")
				if readonlyDir && withSHM {
					for _, suffix := range suffixes {
						info, err := os.Stat(path + suffix)
						if err != nil || info.Mode().Perm() != 0o400 {
							t.Fatalf("changed public %s permissions: %v", suffix, err)
						}
					}
				}
			})
		}
	}
}

func TestPublicSessionReadonlyRejectsMissingInvalidAndURIInjection(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "session.db"), filepath.Join(dir, "absent", "session.db")} {
		if _, err := openReadOnlySessionResolver(path); err == nil {
			t.Fatal("missing session accepted")
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("created missing session/directory", entries, err)
	}
	for _, suffix := range []string{"?mode=rw", "#fragment"} {
		if _, err := openReadOnlySessionResolver(filepath.Join(dir, "session.db") + suffix); err == nil || !strings.Contains(err.Error(), "must not contain") {
			t.Fatal("URI injection not rejected", err)
		}
	}
	path := filepath.Join(dir, "session.db")
	if err := os.WriteFile(path, []byte("synthetic invalid sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertPublicSessionReadonlyOpen(t, path, false, "")
}

func TestPublicSessionReadonlyRejectsUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix file permissions without root bypass")
	}
	path := seedPublicReadonlySession(t, "DELETE")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if resolver, err := openReadOnlySessionResolver(path); err == nil {
		_ = resolver.Close()
		t.Fatal("unreadable session accepted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatal("public reader normalized permissions", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("public reader changed unreadable session", err)
	}
	assertNoAppSQLiteSidecars(t, path)
}
