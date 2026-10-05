package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic catalogue costs, excluding initialization. The reference query may
// scan all operations in SQLite, but Go retains only one page of reference counts.
func BenchmarkDraftCleanupPreview(b *testing.B) {
	for _, limit := range []int{20, 200} {
		for _, operations := range []int{0, 1000} {
			b.Run(fmt.Sprintf("limit=%d/operations=%d", limit, operations), func(b *testing.B) {
				db, err := Open(filepath.Join(b.TempDir(), "wacli.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				id, head := strings.Repeat("a", 32), ""
				var first DraftRevision
				for i := 1; i <= 200; i++ {
					r := cleanupFixtureRevision(b, id, fmt.Sprintf("%032x", i), DraftDocumentKind, time.Unix(1700000000+int64(i), 0), strings.Repeat("f", 1024))
					if _, err := db.WriteDraft(context.Background(), r, head); err != nil {
						b.Fatal(err)
					}
					if i == 1 {
						first = r
					}
					head = r.ID()
				}
				for i := 0; i < operations; i++ {
					_, err := db.Outbound().Reserve(context.Background(), OutboundReservation{Version: 1, ID: fmt.Sprintf("%032x", i+10000), DraftID: id, RevisionID: first.ID(), Hash: first.Payload().Hash(), Key: fmt.Sprintf("fixture-key-%d", i), MessageID: fmt.Sprintf("fixture-message-%d", i), Account: first.Payload().Data().Account, CreatedAt: time.Unix(1700001000, 0)})
					if err != nil {
						b.Fatal(err)
					}
				}
				if _, err := db.DiscardDraft(context.Background(), id, head); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					page, err := db.PreviewDraftCleanup(context.Background(), filepath.Dir(db.path), id, limit, "")
					if err != nil || len(page.Items) != limit {
						b.Fatal(page.Counts, err)
					}
				}
			})
		}
	}
}

func BenchmarkDraftCleanupFULLGuard(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "wacli.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	r := cleanupFixtureRevision(b, strings.Repeat("a", 32), strings.Repeat("b", 32), DraftDocumentKind, time.Unix(1700000000, 0), "fixture")
	if _, err := db.WriteDraft(context.Background(), r, ""); err != nil {
		b.Fatal(err)
	}
	if _, err := db.DiscardDraft(context.Background(), r.DraftID(), r.ID()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.ConfirmDraftCleanup(context.Background(), cleanupSelection(r, r.ID())); err != nil {
			b.Fatal(err)
		}
	}
}
