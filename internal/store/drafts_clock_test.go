package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDraftClockRollbackUpdateDiscardReopenAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name           string
		previous, next time.Duration
	}{
		{"before creation", 0, -time.Minute},
		{"before latest update", time.Hour, time.Minute},
		{"forward update", time.Hour, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wacli.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { db.Close() }()
			// Future fixture timestamps represent a backward clock step without
			// changing the process clock or creating any snapshot bytes/session.
			created := time.Now().UTC().Add(time.Hour)
			id := strings.Repeat("a", 32)
			first := cleanupFixtureRevision(t, id, strings.Repeat("b", 32), DraftDocumentKind, created, "first")
			before, err := db.WriteDraft(t.Context(), first, "")
			if err != nil {
				t.Fatal(err)
			}
			revisions := []DraftRevision{first}
			if tc.previous != 0 {
				previous := cleanupFixtureRevision(t, id, strings.Repeat("c", 32), DraftDocumentKind, created.Add(tc.previous), "previous")
				before, err = db.WriteDraft(t.Context(), previous, first.ID())
				if err != nil {
					t.Fatal(err)
				}
				revisions = append(revisions, previous)
			}
			latest := cleanupFixtureRevision(t, id, strings.Repeat("d", 32), DraftDocumentKind, created.Add(tc.next), "latest")
			updated, err := db.WriteDraft(t.Context(), latest, before.Revision.ID())
			if err != nil {
				t.Fatal(err)
			}
			revisions = append(revisions, latest)
			wantAt := time.Unix(0, max(created.UnixNano(), before.Record.UpdatedAt.UnixNano(), latest.CreatedAt().UnixNano())).UTC()
			if updated.Record.CreatedAt != created || updated.Record.UpdatedAt != wantAt || updated.Revision != latest || updated.Number != len(revisions) {
				t.Fatalf("update changed retained scope/revision or regressed time: %+v", updated)
			}
			stored, err := db.ReadDraft(t.Context(), id, latest.ID())
			if err != nil || !reflect.DeepEqual(stored, updated) {
				t.Fatal("update response differs from storage", stored, err)
			}
			discarded, err := db.DiscardDraft(t.Context(), id, latest.ID())
			if err != nil {
				t.Fatal(err)
			}
			if discarded.Record.CreatedAt != created || discarded.Record.UpdatedAt != wantAt || discarded.Record.DiscardedAt == nil || *discarded.Record.DiscardedAt != wantAt || discarded.Revision != latest {
				t.Fatalf("discard regressed time or changed immutable revision: %+v", discarded)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, revision := range revisions {
				stored, err := db.ReadDraft(t.Context(), id, revision.ID())
				if err != nil || stored.Revision != revision || !reflect.DeepEqual(stored.Record, discarded.Record) {
					t.Fatal("reopen lost record time or immutable revision/hash", stored, err)
				}
			}
			selection := cleanupSelection(latest, latest.ID())
			selected, err := db.ReadDraftCleanup(t.Context(), selection)
			if err != nil || !reflect.DeepEqual(selected, discarded) {
				t.Fatal("cleanup selection rejected valid discard", selected, err)
			}
			confirmed, err := db.confirmDraftCleanup(t.Context(), selection, outboundSQLiteIO(), created.Add(-time.Minute))
			if err != nil || confirmed.Record.UpdatedAt.UnixNano() != wantAt.UnixNano()+1 || *confirmed.Record.DiscardedAt != wantAt || confirmed.Revision != latest {
				t.Fatal("FULL guard rejected valid discard or rewrote revision", confirmed, err)
			}
			stored, err = db.ReadDraft(t.Context(), id, latest.ID())
			if err != nil || !reflect.DeepEqual(stored, confirmed) {
				t.Fatal("FULL response differs from storage", stored, err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			page, err := db.PreviewDraftCleanup(t.Context(), filepath.Dir(path), id, 20, "")
			if err != nil || page.Counts.Eligible != len(revisions) || !reflect.DeepEqual(page.Record, confirmed.Record) {
				t.Fatal("readonly reopened preview rejected eligible revisions", page, err)
			}
		})
	}
}

func TestDraftClockRollbackDoesNotRepairDiscardedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wacli.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	created := time.Now().UTC()
	revision := cleanupFixtureRevision(t, strings.Repeat("a", 32), strings.Repeat("b", 32), DraftDocumentKind, created, "retained")
	if _, err := db.WriteDraft(t.Context(), revision, ""); err != nil {
		t.Fatal(err)
	}
	// Model a discarded row retained by an older writer, without weakening the
	// current cleanup validator or modifying its immutable revision.
	oldAt := created.Add(-time.Minute).UnixNano()
	if _, err := db.sql.Exec(`UPDATE drafts SET state='discarded',discarded_at=?,updated_at=? WHERE id=?`, oldAt, oldAt, revision.DraftID()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ConfirmDraftCleanup(t.Context(), cleanupSelection(revision, revision.ID()))
	assertCleanupCode(t, err, DraftCleanupStoreError)
	stored, err := db.ReadDraft(t.Context(), revision.DraftID(), revision.ID())
	if err != nil || stored.Record.UpdatedAt.UnixNano() != oldAt || stored.Record.DiscardedAt == nil || stored.Record.DiscardedAt.UnixNano() != oldAt || stored.Revision != revision {
		t.Fatal("open/FULL repaired an old inconsistent discard", stored, err)
	}
}
