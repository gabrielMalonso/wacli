package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const MaxDiagnosticSnapshotBytes = 64 * 1024

func migrateDiagnosticSnapshots(d *DB) error {
	_, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS diagnostic_snapshots (
 slot TEXT PRIMARY KEY CHECK(slot IN ('connection','sync')),
 execution_id TEXT NOT NULL,
 payload BLOB NOT NULL CHECK(length(payload) <= 65536)
 )`)
	return err
}

// StartDiagnosticSnapshot replaces one bounded slot, never appending a journal.
func (d *DB) StartDiagnosticSnapshot(ctx context.Context, slot, id string, payload []byte) error {
	if !validDiagnosticSnapshot(slot, id, payload) {
		return fmt.Errorf("invalid diagnostic snapshot")
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO diagnostic_snapshots(slot, execution_id, payload) VALUES(?,?,?) ON CONFLICT(slot) DO UPDATE SET execution_id=excluded.execution_id,payload=excluded.payload`, slot, id, payload)
	return err
}

// DiagnosticSnapshotExecutionID captures the slot before an attempted start.
// Empty means observed absence; an error must not become that empty token.
func (d *DB) DiagnosticSnapshotExecutionID(ctx context.Context, slot string) (string, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, `SELECT execution_id FROM diagnostic_snapshots WHERE slot=?`, slot).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RecoverDiagnosticSnapshot retries retention at a later checkpoint only against
// the observed prior execution or this run's own possible lost start reply.
// A newer retained execution must never be replaced by old cleanup.
func (d *DB) RecoverDiagnosticSnapshot(ctx context.Context, slot, id, previousID string, payload []byte) error {
	if !validDiagnosticSnapshot(slot, id, payload) || previousID != "" && len(previousID) != 32 {
		return errors.New("invalid diagnostic snapshot")
	}
	result, err := d.sql.ExecContext(ctx, `INSERT INTO diagnostic_snapshots(slot,execution_id,payload) VALUES(?,?,?)
 ON CONFLICT(slot) DO UPDATE SET execution_id=excluded.execution_id,payload=excluded.payload
 WHERE diagnostic_snapshots.execution_id=? OR diagnostic_snapshots.execution_id=excluded.execution_id`, slot, id, payload, previousID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("diagnostic execution not retained")
	}
	return err
}

// SaveDiagnosticSnapshot cannot let old cleanup replace a newer execution.
func (d *DB) SaveDiagnosticSnapshot(ctx context.Context, slot, id string, payload []byte) error {
	if !validDiagnosticSnapshot(slot, id, payload) {
		return fmt.Errorf("invalid diagnostic snapshot")
	}
	result, err := d.sql.ExecContext(ctx, `UPDATE diagnostic_snapshots SET payload=? WHERE slot=? AND execution_id=?`, payload, slot, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("diagnostic execution not retained")
	}
	return err
}

func validDiagnosticSnapshot(slot, id string, payload []byte) bool {
	return (slot == "connection" || slot == "sync") && len(id) == 32 && len(payload) > 0 && len(payload) <= MaxDiagnosticSnapshotBytes
}

// ReadDiagnosticSnapshots reads both slots in one SQLite statement, including WAL.
// Missing slots are absent observations; missing tables or failed reads are errors.
func (d *DB) ReadDiagnosticSnapshots(ctx context.Context) (map[string][]byte, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT slot, substr(payload,1,65537) FROM diagnostic_snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string][]byte, 2)
	for rows.Next() {
		var slot string
		var payload []byte
		if err := rows.Scan(&slot, &payload); err != nil {
			return nil, err
		}
		if (slot != "connection" && slot != "sync") || len(payload) > MaxDiagnosticSnapshotBytes || len(result) >= 2 {
			return nil, errors.New("invalid diagnostic snapshot")
		}
		result[slot] = payload
	}
	return result, rows.Err()
}
