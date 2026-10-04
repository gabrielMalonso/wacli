package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/store"
)

// Opt-in evidence for SQLite's parser re-evaluation. Both plans consume the
// same complete row projection on the same fixture; they do not benchmark CLI
// startup or claim native RSS/total allocation is bounded by the page size.
func TestContactReadMaterializationEvidence(t *testing.T) {
	if os.Getenv("WACLI_CONTACT_MEASURE") != "1" {
		t.Skip("performance evidence is opt-in")
	}
	for _, n := range []int{10000, 100000} {
		dir := seedContactBenchmark(t, n)
		db, err := store.OpenReadOnly(filepath.Join(dir, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"before", "after"} {
			conn, owner, err := db.OpenContactReadConn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(context.Background(), "ATTACH DATABASE ? AS identity", readOnlySessionURI(filepath.Join(dir, "session.db"))); err != nil {
				t.Fatal(err)
			}
			calls := 0
			err = conn.Raw(func(dc any) error {
				sqlite := dc.(*sqlite3.SQLiteConn)
				functions := []struct {
					name string
					fn   func(string) string
				}{{"wacli_contact_kind", contactJIDKind}, {"wacli_contact_user", contactJIDUser}, {"wacli_contact_normal", contactJIDNormal}, {"wacli_contact_trim", strings.TrimSpace}}
				for _, f := range functions {
					fn := f.fn
					if err := sqlite.RegisterFunc(f.name, func(raw string) string { calls++; return fn(raw) }, true); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := conn.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			q, args := contactRowsQuery(contactIdentitySources{Map: true, Own: true}, ContactReadOptions{Operation: ContactList})
			if mode == "before" {
				q = strings.ReplaceAll(q, "canonical AS MATERIALIZED", "canonical AS NOT MATERIALIZED")
				q = strings.ReplaceAll(q, "own_pair AS MATERIALIZED", "own_pair AS NOT MATERIALIZED")
			}
			runtime.GC()
			var initial runtime.MemStats
			runtime.ReadMemStats(&initial)
			baseRSS := contactMeasureRSS()
			peakRSS, peakGo := baseRSS, initial.HeapAlloc
			done := make(chan struct{})
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ticker.C:
						var mem runtime.MemStats
						runtime.ReadMemStats(&mem)
						peakGo = max(peakGo, mem.HeapAlloc)
						peakRSS = max(peakRSS, contactMeasureRSS())
					}
				}
			}()
			start := time.Now()
			rows, err := tx.QueryContext(context.Background(), q, args...)
			if err != nil {
				close(done)
				<-stopped
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var c store.Contact
				var updated int64
				var primary, match bool
				var preferred, alternate string
				if err := rows.Scan(&c.JID, &c.Phone, &c.Alias, &c.SystemName, &c.Name, &updated, &primary, &preferred, &alternate, &match); err != nil {
					close(done)
					<-stopped
					t.Fatal(err)
				}
				count++
			}
			err = rows.Err()
			rows.Close()
			elapsed := time.Since(start)
			close(done)
			<-stopped
			if err != nil {
				t.Fatal(err)
			}
			var final runtime.MemStats
			runtime.ReadMemStats(&final)
			t.Logf("%s identities=%d rows=%d time=%s udf_calls=%d alloc_bytes=%d heap_baseline=%d heap_sample_peak=%d rss_baseline=%d rss_sample_peak=%d", mode, n, count, elapsed, calls, final.TotalAlloc-initial.TotalAlloc, initial.HeapAlloc, peakGo, baseRSS, peakRSS)
			plan, err := tx.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s/%d plan: %s", mode, n, detail)
			}
			err = plan.Err()
			plan.Close()
			if err != nil {
				t.Fatal(err)
			}
			tx.Rollback()
			conn.Close()
			owner.Close()
		}
		db.Close()
	}
}
func contactMeasureRSS() uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.ParseUint(fields[1], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}
