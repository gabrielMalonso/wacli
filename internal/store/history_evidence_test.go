package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyTestAttempt() HistoryAttempt {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return HistoryAttempt{RequestedChatJID: "123@g.us", AttemptID: "0123456789abcdef0123456789abcdef", StartedAt: now, CheckpointAt: now,
		State: HistoryUnfinalized, Phase: HistoryPreparing, ExecutionMode: "standalone", Count: 50, Requests: 1, WaitMS: 1000, IdleMS: 1}
}
func historyTestSuccess(a HistoryAttempt) HistoryAttempt {
	now := a.StartedAt.Add(time.Second)
	a.FinishedAt = &now
	a.CheckpointAt = now
	a.State = HistorySucceeded
	a.Phase = HistoryFinalizing
	a.AccountJID = "100@s.whatsapp.net"
	a.WindowChatJID = a.RequestedChatJID
	a.DispatchPossible = true
	a.BaselineCount = new(int64(1))
	a.FinalCount = new(int64(2))
	a.NetGrowth = new(int64(1))
	a.MessagesSynced = new(int64(1))
	a.CountersFinal = true
	a.RequestsSent = 1
	a.ResponsesSeen = 1
	a.StopReason = "requested_batch_limit"
	return a
}
func TestHistoryEvidenceSlotsReopenAndSupersededFinalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := historyTestAttempt()
	if err = db.BeginHistoryAttempt(ctx, first); err != nil {
		t.Fatal(err)
	}
	success := historyTestSuccess(first)
	if err = db.SaveHistoryAttempt(ctx, success); err != nil {
		t.Fatal(err)
	}
	second := first
	second.AttemptID = "1123456789abcdef0123456789abcdef"
	if err = db.BeginHistoryAttempt(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveHistoryAttempt(ctx, success); !errors.Is(err, ErrHistoryAttemptSuperseded) {
		t.Fatalf("old finalize: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.ListHistoryRecoveryEvidence(ctx, []string{first.RequestedChatJID, "absent@lid", first.RequestedChatJID})
	if err != nil || len(got) != 2 || got[0].Latest.AttemptID != second.AttemptID || got[0].LastSuccess.AttemptID != first.AttemptID || got[1].Latest != nil || got[1].LastSuccess != nil {
		t.Fatalf("retained: %+v %v", got, err)
	}
	if got[0].Latest.FinishedAt != nil || got[0].Latest.NetGrowth != nil || got[0].Latest.State != HistoryUnfinalized {
		t.Fatalf("crash became terminal: %+v", got[0].Latest)
	}
	if _, err = db.ListHistoryCoverage(ListHistoryCoverageParams{}); err != nil {
		t.Fatal(err)
	} // No chat/anchor required for evidence.
}
func TestHistoryEvidenceConstraintsAndImmutableScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	a := historyTestAttempt()
	if err = db.BeginHistoryAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.WindowChatJID = a.RequestedChatJID
	a.AccountJID = "100@s.whatsapp.net"
	a.BaselineCount = new(int64(0))
	a.DispatchPossible = true
	a.Phase = HistoryDispatchPossible
	if err = db.SaveHistoryAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	changed := a
	changed.WindowAliasJID = "unverified@lid"
	if err = db.SaveHistoryAttempt(ctx, changed); !errors.Is(err, ErrHistoryAttemptSuperseded) {
		t.Fatalf("scope changed: %v", err)
	}
	changed = a
	changed.DispatchPossible = false
	if err = db.SaveHistoryAttempt(ctx, changed); !errors.Is(err, ErrHistoryAttemptSuperseded) {
		t.Fatalf("uncertainty regressed: %v", err)
	}
	invalid := historyTestSuccess(a)
	invalid.NetGrowth = nil
	if err = db.SaveHistoryAttempt(ctx, invalid); err == nil {
		t.Fatal("incomplete success accepted")
	}
	invalid = historyTestSuccess(a)
	invalid.State = "invented"
	if err = db.SaveHistoryAttempt(ctx, invalid); err == nil {
		t.Fatal("invalid state accepted")
	}
	got, err := db.ListHistoryRecoveryEvidence(ctx, []string{a.RequestedChatJID})
	if err != nil || got[0].LastSuccess != nil || got[0].Latest.State != HistoryUnfinalized {
		t.Fatalf("failed txn changed slots: %+v %v", got, err)
	}
}
func TestHistoryEvidenceMigrationWritableOnlyAndBoundedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec(`DROP TABLE history_recovery_evidence; DELETE FROM schema_migrations WHERE version=29`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenReadOnly(path); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("readonly upgraded: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("readonly changed archive")
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrateHistoryEvidence(db); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 401)
	for i := range keys {
		keys[i] = strings.Repeat("x", i+1) + "@lid"
	}
	if _, err = db.ListHistoryRecoveryEvidence(context.Background(), keys); err == nil {
		t.Fatal("unbounded lookup")
	}
	if _, err = db.ListHistoryRecoveryEvidence(context.Background(), keys[:400]); err != nil {
		t.Fatal(err)
	}
}
func TestWritableFutureSchemaRefusedBeforeJournalAndPermissions(t *testing.T) {
	for _, version := range []int{30, 0} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wacli.db")
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`PRAGMA journal_mode=DELETE; CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,name TEXT,applied_at INTEGER); INSERT INTO schema_migrations VALUES(?, 'future', 0)`, version); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			if err = os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if _, err = Open(path); err == nil {
				t.Fatal("future accepted")
			}
			after, _ := os.ReadFile(path)
			info, _ := os.Stat(path)
			if !bytes.Equal(before, after) || info.Mode().Perm() != 0640 {
				t.Fatal("future archive modified")
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if _, err = os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("sidecar %s: %v", suffix, err)
				}
			}
		})
	}
}
func TestHistoryEvidenceBusyTimeoutBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := sql.Open("sqlite3", sqliteURI(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = db.BeginHistoryAttempt(ctx, historyTestAttempt())
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded busy: %v %v", err, time.Since(start))
	}
}

func TestHistoryEvidenceReopenAtEveryCheckpoint(t *testing.T) {
	for _, phase := range []HistoryAttemptPhase{HistoryPreparing, HistoryObserving, HistoryDispatchPossible, HistoryFinalizing} {
		t.Run(string(phase), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wacli.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			a := historyTestAttempt()
			ctx := context.Background()
			if err = db.BeginHistoryAttempt(ctx, a); err != nil {
				t.Fatal(err)
			}
			if phase != HistoryPreparing {
				a.Phase = phase
				a.WindowChatJID = a.RequestedChatJID
				a.BaselineCount = new(int64(0))
				if phase == HistoryDispatchPossible || phase == HistoryFinalizing {
					a.DispatchPossible = true
				}
				if err = db.SaveHistoryAttempt(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			_ = db.Close()
			db, err = OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			got, err := db.ListHistoryRecoveryEvidence(ctx, []string{a.RequestedChatJID})
			if err != nil || got[0].Latest.State != HistoryUnfinalized || got[0].Latest.Phase != phase || got[0].Latest.FinishedAt != nil || got[0].Latest.NetGrowth != nil || got[0].LastSuccess != nil {
				t.Fatalf("crash repair: %+v %v", got, err)
			}
		})
	}
}

func BenchmarkHistoryEvidenceSelectedKeys(b *testing.B) {
	path := filepath.Join(b.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	a := historyTestAttempt()
	ctx := context.Background()
	if err = db.BeginHistoryAttempt(ctx, a); err != nil {
		b.Fatal(err)
	}
	if err = db.SaveHistoryAttempt(ctx, historyTestSuccess(a)); err != nil {
		b.Fatal(err)
	}
	tx, err := db.sql.Begin()
	if err != nil {
		b.Fatal(err)
	}
	columns := strings.TrimPrefix(historyAttemptColumns, "requested_chat_jid,")
	for i := range 10000 {
		if _, err = tx.Exec(`INSERT INTO history_recovery_evidence(slot,requested_chat_jid,`+columns+`) SELECT slot,?,`+columns+` FROM history_recovery_evidence WHERE requested_chat_jid=?`, fmt.Sprintf("%d@lid", i), a.RequestedChatJID); err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	keys := make([]string, 400)
	for i := range keys {
		keys[i] = fmt.Sprintf("%d@lid", i*25)
	}
	b.ResetTimer()
	for range b.N {
		if _, err = db.ListHistoryRecoveryEvidence(ctx, keys); err != nil {
			b.Fatal(err)
		}
	}
}
