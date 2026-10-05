package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const draftCleanupPolicy = "discarded-unreferenced-document-bytes-v1"

type draftCleanupCursor struct {
	Version  int    `json:"v"`
	Domain   string `json:"domain"`
	Store    string `json:"store"`
	DraftID  string `json:"draft_id"`
	Policy   string `json:"policy"`
	Revision int    `json:"revision"`
}

func validDraftCleanupStoreRef(value string) bool {
	return filepath.IsAbs(value) && utf8.ValidString(value) && len(value) <= 4096 && !strings.ContainsAny(value, "\x00?#")
}

func ValidateDraftCleanupCursor(token string) error {
	_, err := decodeDraftCleanupCursor(token)
	return err
}

func decodeDraftCleanupCursor(token string) (draftCleanupCursor, error) {
	var c draftCleanupCursor
	fail := func() (draftCleanupCursor, error) {
		return c, DraftCleanupFailure(DraftCleanupInvalidCursor, DraftCleanupSelection{}, nil)
	}
	if len(token) == 0 || len(token) > 16384 {
		return fail()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || !utf8.Valid(raw) || json.Unmarshal(raw, &c) != nil {
		return fail()
	}
	canonical, err := json.Marshal(c)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != token || c.Version != 1 || c.Domain != "draft-cleanup" || c.Policy != draftCleanupPolicy || !validDraftCleanupStoreRef(c.Store) || ValidateDraftID(c.DraftID) != nil || c.Revision < 1 {
		return fail()
	}
	return c, nil
}

// PreviewDraftCleanup reads one catalogue snapshot without filesystem access.
// Page IDs use the existing draft/revision-number index. References use one
// bounded IN query (at most 200 IDs); SQLite may scan the outbound table.
func (d *DB) PreviewDraftCleanup(ctx context.Context, storeRef, id string, limit int, cursor string) (DraftCleanupPage, error) {
	selection := DraftCleanupSelection{DraftID: id}
	if ValidateDraftID(id) != nil || !validDraftCleanupStoreRef(storeRef) || limit < 1 || limit > 200 {
		return DraftCleanupPage{}, DraftCleanupFailure(DraftCleanupInvalidArguments, selection, nil)
	}
	after := 0
	if cursor != "" {
		c, err := decodeDraftCleanupCursor(cursor)
		if err != nil {
			return DraftCleanupPage{}, err
		}
		if c.Store != storeRef || c.DraftID != id {
			return DraftCleanupPage{}, DraftCleanupFailure(DraftCleanupInvalidCursor, selection, nil)
		}
		after = c.Revision
	}
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DraftCleanupPage{}, draftCleanupStoreFailure(err, selection)
	}
	defer tx.Rollback()
	page, err := previewDraftCleanup(ctx, tx, storeRef, id, limit, after)
	if err != nil {
		return DraftCleanupPage{}, draftCleanupStoreFailure(err, selection)
	}
	return page, nil
}

func previewDraftCleanup(ctx context.Context, tx *sql.Tx, storeRef, id string, limit, after int) (DraftCleanupPage, error) {
	head, err := readDraftCleanupHead(ctx, tx, id)
	if err != nil {
		return DraftCleanupPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM draft_revisions WHERE draft_id=? AND revision_no>? ORDER BY revision_no LIMIT ?`, id, after, limit+1)
	if err != nil {
		return DraftCleanupPage{}, err
	}
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var rid string
		if err = rows.Scan(&rid); err != nil {
			break
		}
		if ValidateDraftID(rid) != nil {
			err = DraftCleanupFailure(DraftCleanupStoreError, DraftCleanupSelection{DraftID: id}, nil)
			break
		}
		ids = append(ids, rid)
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return DraftCleanupPage{}, err
	}
	if closeErr != nil {
		return DraftCleanupPage{}, closeErr
	}
	page := DraftCleanupPage{Record: head.Record, Items: make([]DraftCleanupItem, 0, limit), HasMore: len(ids) > limit}
	if page.HasMore {
		ids = ids[:limit]
	}
	refs, err := draftCleanupPageReferences(ctx, tx, ids)
	if err != nil {
		return DraftCleanupPage{}, err
	}
	for _, rid := range ids {
		entry := head
		if rid != head.Revision.ID() {
			entry, err = readDraft(ctx, tx, id, rid)
			if err != nil {
				return DraftCleanupPage{}, err
			}
		}
		data := entry.Revision.Payload().Data()
		item := DraftCleanupItem{RevisionID: rid, Hash: entry.Revision.Payload().Hash(), Revision: entry.Number, CreatedAt: entry.Revision.CreatedAt(), Kind: data.Kind, Eligibility: draftCleanupEligibility(entry, refs[rid] > 0), OutboundRefs: refs[rid]}
		if data.Document != nil {
			item.Document = &DraftCleanupDocument{BytesAtCreate: data.Document.Size, SHA256: data.Document.SHA256}
		}
		page.Items = append(page.Items, item)
		page.Counts.Examined++
		switch item.Eligibility {
		case DraftCleanupEligible:
			page.Counts.Eligible++
			page.Counts.EligibleBytesAtCreate += item.Document.BytesAtCreate
		case DraftCleanupActive:
			page.Counts.Active++
		case DraftCleanupNonDocument:
			page.Counts.NonDocument++
		case DraftCleanupOutboundProtected:
			page.Counts.OutboundProtected++
		}
	}
	if page.HasMore {
		last := page.Items[len(page.Items)-1]
		raw, err := json.Marshal(draftCleanupCursor{Version: 1, Domain: "draft-cleanup", Store: storeRef, DraftID: id, Policy: draftCleanupPolicy, Revision: last.Revision})
		if err != nil {
			return DraftCleanupPage{}, err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		if len(token) > 16384 {
			return DraftCleanupPage{}, DraftCleanupFailure(DraftCleanupInvalidArguments, DraftCleanupSelection{DraftID: id}, nil)
		}
		page.NextCursor = &token
	}
	return page, nil
}

func draftCleanupPageReferences(ctx context.Context, tx *sql.Tx, ids []string) (map[string]int64, error) {
	refs := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return refs, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT revision_id,COUNT(*) FROM outbound_operations WHERE revision_id IN (`+placeholders+`) GROUP BY revision_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		refs[id] = count
	}
	return refs, rows.Err()
}
