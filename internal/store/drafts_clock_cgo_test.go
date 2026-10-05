//go:build cgo

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestDraftDiscardCASRejectsConcurrentRecordChange(t *testing.T) {
	for _, change := range []string{"head", "timestamp"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wacli.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.sql.SetMaxOpenConns(1)
			writer, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			at := time.Now().UTC().Add(time.Hour)
			first := draftFixtureRevision(t, strings.Repeat("a", 32), strings.Repeat("b", 32), "first", at)
			if _, err := db.WriteDraft(t.Context(), first, ""); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			preparing, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			conn, err := db.sql.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = conn.Raw(func(driver any) error {
				sqlite, ok := driver.(*sqlite3.SQLiteConn)
				if !ok {
					return fmt.Errorf("unexpected fixture driver %T", driver)
				}
				// Pause preparation of the discard UPDATE after its separate ReadDraft.
				// A second real connection can now commit the competing record change.
				sqlite.RegisterAuthorizer(func(op int, table, column, database string) int {
					if op == sqlite3.SQLITE_UPDATE && table == "drafts" && column == "state" {
						once.Do(func() {
							close(preparing)
							select {
							case <-resume:
							case <-ctx.Done():
							}
						})
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			})
			conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			type discardResult struct {
				entry DraftEntry
				err   error
			}
			result := make(chan discardResult, 1)
			go func() {
				entry, err := db.DiscardDraft(ctx, first.DraftID(), first.ID())
				result <- discardResult{entry, err}
			}()
			select {
			case <-preparing:
			case <-ctx.Done():
				t.Fatal("discard did not reach the fixture barrier", ctx.Err())
			}
			var updated DraftEntry
			var updateErr error
			if change == "head" {
				second := draftFixtureRevision(t, first.DraftID(), strings.Repeat("c", 32), "second", at.Add(time.Hour))
				updated, updateErr = writer.WriteDraft(ctx, second, first.ID())
			} else {
				// Also reject a stale timestamp calculation if the head stays fixed.
				_, updateErr = writer.sql.ExecContext(ctx, `UPDATE drafts SET updated_at=? WHERE id=?`, at.Add(time.Hour).UnixNano(), first.DraftID())
				if updateErr == nil {
					updated, updateErr = writer.ReadDraft(ctx, first.DraftID(), first.ID())
				}
			}
			close(resume)
			discard := <-result
			if updateErr != nil {
				t.Fatal(updateErr)
			}
			assertDraftCode(t, discard.err, "draft_conflict")
			if !reflect.DeepEqual(discard.entry, DraftEntry{}) {
				t.Fatal("stale discard returned a usable record", discard.entry)
			}
			stored, err := writer.ReadDraft(ctx, first.DraftID(), updated.Revision.ID())
			if err != nil || !reflect.DeepEqual(stored, updated) || stored.Record.State != "active" {
				t.Fatal("stale discard changed the winning head or timestamps", stored, err)
			}
		})
	}
}
