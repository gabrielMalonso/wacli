package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticSnapshotsBoundedIsolatedAndGuarded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldID, newID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for i := 0; i < 50; i++ {
		if err := db.StartDiagnosticSnapshot(t.Context(), "sync", oldID, []byte(`{"fixture":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.StartDiagnosticSnapshot(t.Context(), "connection", oldID, []byte(`{"fixture":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartDiagnosticSnapshot(t.Context(), "sync", newID, []byte(`{"fixture":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDiagnosticSnapshot(t.Context(), "sync", oldID, []byte(`{"fixture":4}`)); err == nil {
		t.Fatal("old cleanup accepted")
	}
	if err := db.SaveDiagnosticSnapshot(t.Context(), "sync", newID, []byte(`{"fixture":5}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartDiagnosticSnapshot(t.Context(), "sync", newID, bytes.Repeat([]byte("x"), MaxDiagnosticSnapshotBytes+1)); err == nil {
		t.Fatal("unbounded payload")
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	records, err := ro.ReadDiagnosticSnapshots(t.Context())
	if err != nil || len(records) != 2 || string(records["sync"]) != `{"fixture":5}` || string(records["connection"]) != `{"fixture":2}` {
		t.Fatalf("slots: %v %v", records, err)
	}
	if err := ro.SaveDiagnosticSnapshot(t.Context(), "sync", newID, []byte(`{}`)); err == nil {
		t.Fatal("readonly write accepted")
	}
	other, err := Open(filepath.Join(t.TempDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	isolated, err := other.ReadDiagnosticSnapshots(t.Context())
	if err != nil || len(isolated) != 0 {
		t.Fatal("account evidence crossed files")
	}
}

func TestDiagnosticSnapshotMigrationExplicitOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic previous schema, with all pre-existing domain rows intact.
	if _, err := db.sql.Exec(`DROP TABLE diagnostic_snapshots; DELETE FROM schema_migrations WHERE version=32; INSERT INTO chats(jid,kind) VALUES('fixture@s.whatsapp.net','dm')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if ro, err := OpenReadOnly(path); err == nil {
		ro.Close()
		t.Fatal("old archive silently migrated")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if count, err := db.CountChats(); err != nil || count != 1 {
		t.Fatal("migration changed existing data")
	}
	records, err := db.ReadDiagnosticSnapshots(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatal("migration invented observations")
	}
}

func TestDiagnosticRetentionRecoveryCAS(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, second, third := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	if previous, err := db.DiagnosticSnapshotExecutionID(t.Context(), "sync"); err != nil || previous != "" {
		t.Fatal("absent slot was not observed")
	}
	if err := db.RecoverDiagnosticSnapshot(t.Context(), "sync", first, "", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverDiagnosticSnapshot(t.Context(), "sync", second, first, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.StartDiagnosticSnapshot(t.Context(), "sync", third, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverDiagnosticSnapshot(t.Context(), "sync", second, first, []byte(`{}`)); err == nil {
		t.Fatal("recovery overwrote newer slot")
	}
	if id, err := db.DiagnosticSnapshotExecutionID(t.Context(), "sync"); err != nil || id != third {
		t.Fatal("newer execution lost")
	}
}
