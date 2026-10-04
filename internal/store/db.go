package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/fsutil"
	"github.com/openclaw/wacli/internal/sqliteutil"
	"github.com/openclaw/wacli/internal/store/storedb"
)

type DB struct {
	path       string
	sql        *sql.DB
	q          *storedb.Queries
	ftsEnabled bool
}

func Open(path string) (*DB, error) {
	return open(path, false)
}

func OpenReadOnly(path string) (*DB, error) {
	return open(path, true)
}

func open(path string, readOnly bool) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("db path is required")
	}
	// Reject paths that could inject SQLite URI parameters (#59).
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("db path must not contain '?' or '#'")
	}
	if readOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("read local store (initialize explicitly with wacli auth or sync; these commands may connect to WhatsApp): %w", err)
		}
	} else {
		if err := checkWritableSchema(path); err != nil {
			return nil, err
		}
		if err := fsutil.EnsurePrivateDir(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite3", sqliteURI(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	s := &DB{path: path, sql: db, q: storedb.New(db)}
	if readOnly {
		if err := s.validateReadable(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open read-only sqlite: %w", err)
		}
		s.ftsEnabled = s.detectMessagesFTS()
		return s, nil
	}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := sqliteutil.ChmodFiles(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Inspect existing versions without a writer connection: journal_mode and chmod
// must not alter an archive produced by a newer or unknown migration.
func checkWritableSchema(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", sqliteURI(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	d := &DB{sql: db}
	exists, err := d.tableExists("schema_migrations")
	if err != nil || !exists {
		return err // Unversioned legacy archives retain their writable migration.
	}
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	defer rows.Close()
	current := schemaMigrations[len(schemaMigrations)-1].version
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return err
		}
		if version < 1 || version > current {
			return fmt.Errorf("local store schema %d is unknown or newer than supported schema %d; upgrade wacli before writing", version, current)
		}
	}
	return rows.Err()
}

func (d *DB) validateReadable() error {
	hasMigrations, err := d.tableExists("schema_migrations")
	if err != nil {
		return err
	}
	if !hasMigrations {
		return fmt.Errorf("local store has no schema version; an explicit writable upgrade with wacli auth or sync is required (these commands may connect to WhatsApp)")
	}
	var version, applied int
	if err := d.sql.QueryRow("SELECT COALESCE(MAX(version), 0), COUNT(*) FROM schema_migrations").Scan(&version, &applied); err != nil {
		return err
	}
	current := schemaMigrations[len(schemaMigrations)-1].version
	if version < current {
		return fmt.Errorf("local store schema %d is older than required schema %d; an explicit writable upgrade with wacli auth or sync is required (these commands may connect to WhatsApp)", version, current)
	}
	if version > current {
		return fmt.Errorf("local store schema %d is newer than supported schema %d; upgrade wacli to read it", version, current)
	}
	if applied != len(schemaMigrations) {
		return fmt.Errorf("local store schema has missing migrations; an explicit writable upgrade with wacli auth or sync is required (these commands may connect to WhatsApp)")
	}
	return nil
}

func sqliteURI(path string, readOnly bool) string {
	params := "_foreign_keys=on&_busy_timeout=5000"
	if readOnly {
		params += "&mode=ro&_query_only=1"
		if !sqliteSidecarsExist(path) {
			params += "&immutable=1"
		}
	}
	return sqliteutil.FileURI(path, params)
}

func sqliteSidecarsExist(path string) bool {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			return true
		}
	}
	return false
}

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

func (d *DB) init() error {
	// Pragmas: keep consistent for writers/readers.
	_, _ = d.sql.Exec("PRAGMA journal_mode=WAL;")
	_, _ = d.sql.Exec("PRAGMA synchronous=NORMAL;")
	_, _ = d.sql.Exec("PRAGMA temp_store=MEMORY;")
	_, _ = d.sql.Exec("PRAGMA foreign_keys=ON;")

	if err := d.ensureSchema(); err != nil {
		return err
	}

	// Detect FTS5 availability independently of migration state. The migration
	// sets ftsEnabled only on first run; subsequent opens skip the migration.
	if !d.ftsEnabled {
		d.ftsEnabled = d.detectMessagesFTS()
	}

	return nil
}

func (d *DB) detectMessagesFTS() bool {
	ok, err := d.tableExists("messages_fts")
	if err != nil || !ok {
		return false
	}
	hasDisplayText, err := d.tableHasColumn("messages_fts", "display_text")
	if err != nil || !hasDisplayText {
		return false
	}
	// One row is enough: a build without FTS5 fails as soon as the table is
	// read. count(*) would walk the whole index on every open, a cost that
	// grows with the archive and that every command pays.
	var rowid int64
	err = d.sql.QueryRow("SELECT rowid FROM messages_fts LIMIT 1").Scan(&rowid)
	return err == nil || errors.Is(err, sql.ErrNoRows)
}
