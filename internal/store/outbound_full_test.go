package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestOutboundFULLCheckedRestoredCanceledAndDiscarded(t *testing.T) {
	for _, stage := range []string{"success", "read_configuration", "set_full", "check_full", "begin", "work", "cancel", "commit_lost", "rollback", "restore", "restore_check"} {
		t.Run(stage, func(t *testing.T) {
			db := openTestDB(t)
			db.sql.SetMaxOpenConns(1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			baseline := 0
			if err := db.sql.QueryRow("PRAGMA synchronous").Scan(&baseline); err != nil {
				t.Fatal(err)
			}
			disk := outboundSQLiteIO()
			baseExec, baseSync := disk.exec, disk.synchronous
			marker := errors.New("synthetic fixture failure")
			firstPointer := ""
			checks := 0
			commits := 0
			disk.synchronous = func(ctx context.Context, c *sql.Conn) (int, error) {
				checks++
				if firstPointer == "" {
					_ = c.Raw(func(driver any) error { firstPointer = fmt.Sprintf("%p", driver); return nil })
				}
				if stage == "read_configuration" && checks == 1 {
					return 0, marker
				}
				if stage == "check_full" && checks == 2 {
					return 1, nil
				}
				if stage == "restore_check" && checks == 3 {
					return -1, marker
				}
				return baseSync(ctx, c)
			}
			disk.exec = func(ctx context.Context, c *sql.Conn, q string, args ...any) (sql.Result, error) {
				if stage == "set_full" && q == "PRAGMA synchronous=FULL" || stage == "begin" && q == "BEGIN IMMEDIATE" || stage == "rollback" && q == "ROLLBACK" || stage == "restore" && q == fmt.Sprintf("PRAGMA synchronous=%d", baseline) {
					return nil, marker
				}
				res, err := baseExec(ctx, c, q, args...)
				if q == "COMMIT" {
					commits++
					if stage == "commit_lost" && err == nil {
						return res, marker
					}
				}
				return res, err
			}
			called := false
			value, err := outboundFullWrite(ctx, db.sql, disk, func(c *sql.Conn) (int, error) {
				called = true
				n, e := baseSync(ctx, c)
				if e != nil || n != 2 {
					t.Fatal("work outside FULL", n, e)
				}
				if _, e = c.ExecContext(ctx, "INSERT INTO contacts(jid,phone,updated_at) VALUES('synthetic-full-fixture','fixture',1)"); e != nil {
					return 0, e
				}
				if stage == "work" || stage == "rollback" {
					return 0, marker
				}
				if stage == "cancel" {
					cancel()
					return 0, ctx.Err()
				}
				return 7, nil
			})
			if stage == "success" {
				if err != nil || value != 7 {
					t.Fatal(value, err)
				}
			} else if err == nil || value != 0 {
				t.Fatal("failure authorized next boundary", value, err)
			}
			if stage == "commit_lost" || stage == "restore" || stage == "restore_check" {
				assertOutboundCode(t, err, "write_uncertain")
			}
			if (stage == "read_configuration" || stage == "set_full" || stage == "check_full" || stage == "begin") && called {
				t.Fatal("work after configuration failure")
			}
			if stage == "cancel" && commits != 0 {
				t.Fatal("commit after cancellation")
			}
			if db.sql.Stats().InUse != 0 {
				t.Fatal("leaked lease", db.sql.Stats())
			}
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer stop()
			c, e := db.sql.Conn(cleanup)
			if e != nil {
				t.Fatal("pool deadlocked", e)
			}
			defer c.Close()
			pointer := ""
			_ = c.Raw(func(driver any) error { pointer = fmt.Sprintf("%p", driver); return nil })
			got, e := baseSync(cleanup, c)
			if e != nil {
				t.Fatal(e)
			}
			discarded := stage == "rollback" || stage == "restore" || stage == "restore_check" || stage == "commit_lost"
			if discarded {
				if pointer == firstPointer {
					t.Fatal("failed cleanup returned physical connection")
				}
			} else if got != baseline {
				t.Fatal("settings leaked", got, baseline)
			}
		})
	}
}

func TestOutboundFULLCommitFailureRetainsUncertaintyAndStopsProgress(t *testing.T) {
	db := openTestDB(t)
	disk := outboundSQLiteIO()
	exec := disk.exec
	disk.exec = func(ctx context.Context, c *sql.Conn, q string, args ...any) (sql.Result, error) {
		if q == "COMMIT" {
			return nil, errors.New("synthetic commit result lost")
		}
		return exec(ctx, c, q, args...)
	}
	value, err := outboundFullWrite(t.Context(), db.sql, disk, func(c *sql.Conn) (string, error) {
		_, err := c.ExecContext(t.Context(), "INSERT INTO contacts(jid,phone,updated_at) VALUES('commit-fixture','fixture',1)")
		return "must-not-authorize-effect", err
	})
	assertOutboundCode(t, err, "write_uncertain")
	if value != "" {
		t.Fatal("uncertain result leaked progress value")
	}
}

func TestOutboundFULLContentionBudgetAndBusyRestoration(t *testing.T) {
	db := openTestDB(t)
	holder, err := db.sql.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(context.Background(), "ROLLBACK")
	baseline := 0
	if err := db.sql.QueryRow("PRAGMA busy_timeout").Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	called := false
	_, err = outboundFullWrite(ctx, db.sql, outboundSQLiteIO(), func(*sql.Conn) (int, error) { called = true; return 1, nil })
	if err == nil || called {
		t.Fatal("contended transaction authorized work", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("native busy handler ignored lease budget: %s", elapsed)
	}
	restored := 0
	if err := db.sql.QueryRow("PRAGMA busy_timeout").Scan(&restored); err != nil || restored != baseline {
		t.Fatal("busy setting leaked", baseline, restored, err)
	}
	t.Log("local contention returned within tolerance; no strict OS/driver wall-clock guarantee")
}

func BenchmarkOutboundFULLCheckpoint(b *testing.B) {
	// Synthetic local SQLite transaction cost, not network/storage guarantees.
	db, err := Open(b.TempDir() + "/wacli.db")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := outboundFullWrite(b.Context(), db.sql, outboundSQLiteIO(), func(c *sql.Conn) (int, error) {
			_, err := c.ExecContext(b.Context(), "INSERT OR REPLACE INTO contacts(jid,phone,updated_at) VALUES('benchmark-fixture','fixture',1)")
			return i, err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
