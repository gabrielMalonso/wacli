package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DraftError describes local outcomes only. Correlation is not idempotency.
type DraftError struct {
	Code       string `json:"code"`
	DraftID    string `json:"draft_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Hash       string `json:"hash,omitempty"`
	Cause      error  `json:"-"`
}

func (e *DraftError) Error() string { return "local draft operation: " + e.Code }
func (e *DraftError) Unwrap() error { return e.Cause }

func DraftFailure(code, id, revision, hash string, cause error) *DraftError {
	return &DraftError{code, id, revision, hash, cause}
}

type DraftRecord struct {
	ID, AccountID, State, HeadRevisionID string
	CreatedAt, UpdatedAt                 time.Time
	DiscardedAt                          *time.Time
}

type DraftEntry struct {
	Record   DraftRecord
	Revision DraftRevision
	Number   int
}

// DraftSummary is stored separately so list never loads full content rows.
type DraftSummary struct {
	Kind          DraftKind      `json:"kind"`
	Account       DraftIdentity  `json:"account_identity"`
	Recipient     DraftRecipient `json:"recipient"`
	RecipientName string         `json:"recipient_name"`
	Preview       string         `json:"preview"`
	Filename      string         `json:"filename,omitempty"`
	ContactPhone  string         `json:"contact_phone,omitempty"`
	TextBytes     int            `json:"text_bytes"`
	Mentions      int            `json:"mention_count"`
	HasReply      bool           `json:"has_reply"`
	Truncated     bool           `json:"truncated"`
}

func DraftRevisionSummary(revision DraftRevision) DraftSummary {
	p := revision.Payload().Data()
	s := DraftSummary{Kind: p.Kind, Account: p.Account, Recipient: p.Recipient, HasReply: p.Reply != nil}
	cut := func(value string, n int) string {
		runes := []rune(value)
		if len(runes) > n {
			s.Truncated = true
			return string(runes[:n])
		}
		return value
	}
	s.RecipientName = cut(revision.Review().RecipientName, 64)
	switch p.Kind {
	case DraftTextKind:
		s.Preview = cut(p.Text.Text, 128)
		s.TextBytes = len(p.Text.Text)
		s.Mentions = len(p.Text.Mentions)
	case DraftDocumentKind:
		s.Preview = cut(p.Document.Caption, 128)
		s.TextBytes = len(p.Document.Caption)
		s.Filename = cut(p.Document.Filename, 64)
	case DraftContactKind:
		s.Preview = cut(p.Contact.DisplayName, 128)
		s.TextBytes = len(p.Contact.VCard)
		s.ContactPhone = p.Contact.Phone
	}
	return s
}

func migrateDrafts(d *DB) error {
	_, err := d.sql.Exec(`
CREATE TABLE IF NOT EXISTS drafts (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 account_jid TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('active','discarded')),
 head_revision_id TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, discarded_at INTEGER,
 FOREIGN KEY(id,head_revision_id) REFERENCES draft_revisions(draft_id,id) DEFERRABLE INITIALLY DEFERRED,
 CHECK((state='active' AND discarded_at IS NULL) OR (state='discarded' AND discarded_at IS NOT NULL))
);
CREATE TABLE IF NOT EXISTS draft_revisions (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 draft_id TEXT NOT NULL REFERENCES drafts(id) DEFERRABLE INITIALLY DEFERRED,
 revision_no INTEGER NOT NULL CHECK(revision_no>0), payload_version INTEGER NOT NULL CHECK(payload_version=1),
 payload_hash TEXT NOT NULL CHECK(length(payload_hash)=64 AND payload_hash NOT GLOB '*[^0-9a-f]*'),
 payload_json TEXT NOT NULL CHECK(length(CAST(payload_json AS BLOB))<=262144),
 review_json TEXT NOT NULL CHECK(length(CAST(review_json AS BLOB))<=262144),
 summary_json TEXT NOT NULL CHECK(length(CAST(summary_json AS BLOB))<=4096), created_at INTEGER NOT NULL,
 UNIQUE(draft_id,revision_no), UNIQUE(draft_id,id)
);
CREATE INDEX IF NOT EXISTS idx_drafts_created_id ON drafts(created_at,id);
CREATE TRIGGER IF NOT EXISTS draft_revision_no_update BEFORE UPDATE ON draft_revisions BEGIN SELECT RAISE(ABORT,'draft revisions are immutable'); END;
CREATE TRIGGER IF NOT EXISTS draft_revision_no_delete BEFORE DELETE ON draft_revisions BEGIN SELECT RAISE(ABORT,'draft revisions are retained'); END;
CREATE TRIGGER IF NOT EXISTS draft_scope_no_update BEFORE UPDATE OF id,account_jid,created_at ON drafts BEGIN SELECT RAISE(ABORT,'draft scope is immutable'); END;
`)
	return err
}

func scanDraftRecord(row interface{ Scan(...any) error }) (DraftRecord, error) {
	var r DraftRecord
	var created, updated int64
	var discarded sql.NullInt64
	err := row.Scan(&r.ID, &r.AccountID, &r.State, &r.HeadRevisionID, &created, &updated, &discarded)
	r.CreatedAt = time.Unix(0, created).UTC()
	r.UpdatedAt = time.Unix(0, updated).UTC()
	if discarded.Valid {
		v := time.Unix(0, discarded.Int64).UTC()
		r.DiscardedAt = &v
	}
	return r, err
}

const draftRecordColumns = "id,account_jid,state,head_revision_id,created_at,updated_at,discarded_at"

func (d *DB) ReadDraftRecord(ctx context.Context, id string) (DraftRecord, error) {
	if err := ValidateDraftID(id); err != nil {
		return DraftRecord{}, err
	}
	r, err := scanDraftRecord(d.sql.QueryRowContext(ctx, "SELECT "+draftRecordColumns+" FROM drafts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, DraftFailure("not_found", id, "", "", err)
	}
	return r, err
}

func (d *DB) ReadDraft(ctx context.Context, id, revisionID string) (DraftEntry, error) {
	return readDraft(ctx, d.sql, id, revisionID)
}

type draftReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// The outbound reservation uses the same validator inside its leased transaction.
func readDraft(ctx context.Context, reader draftReader, id, revisionID string) (DraftEntry, error) {
	// Caller syntax stays a usage error; failures after reading persisted data
	// belong to the archive boundary, even when a model validator is the cause.
	if err := ValidateDraftID(id); err != nil {
		return DraftEntry{}, err
	}
	if revisionID != "" {
		if err := ValidateDraftID(revisionID); err != nil {
			return DraftEntry{}, err
		}
	}
	r, err := scanDraftRecord(reader.QueryRowContext(ctx, "SELECT "+draftRecordColumns+" FROM drafts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return DraftEntry{}, DraftFailure("not_found", id, revisionID, "", err)
	}
	if err != nil {
		return DraftEntry{}, err
	}
	if revisionID == "" {
		revisionID = r.HeadRevisionID
	}
	if err := ValidateDraftID(revisionID); err != nil {
		return DraftEntry{}, DraftFailure("store_unavailable", id, "", "", err)
	}
	fail := func(err error) (DraftEntry, error) {
		return DraftEntry{}, DraftFailure("store_unavailable", id, revisionID, "", err)
	}
	var number, version int
	var created int64
	var raw, reviewRaw, hash string
	err = reader.QueryRowContext(ctx, `SELECT revision_no,payload_version,created_at,payload_json,review_json,payload_hash FROM draft_revisions WHERE draft_id=? AND id=?`, id, revisionID).Scan(&number, &version, &created, &raw, &reviewRaw, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return DraftEntry{}, DraftFailure("not_found", id, revisionID, "", err)
	}
	if err != nil {
		return fail(err)
	}
	if version != DraftPayloadVersion || number < 1 {
		return fail(fmt.Errorf("invalid stored draft revision version or number"))
	}
	payload, err := DecodeDraftPayload([]byte(raw))
	if err != nil {
		return fail(err)
	}
	if payload.Hash() != hash || payload.Data().Account.PN != r.AccountID {
		return fail(fmt.Errorf("invalid stored draft identity or hash"))
	}
	var review DraftReviewSnapshot
	if len(reviewRaw) > MaxDraftPayloadBytes {
		return fail(fmt.Errorf("invalid stored draft review size"))
	}
	if err := json.Unmarshal([]byte(reviewRaw), &review); err != nil {
		return fail(err)
	}
	revision, err := NewDraftRevision(id, revisionID, time.Unix(0, created), payload, review)
	if err != nil {
		return fail(err)
	}
	canonical, err := json.Marshal(revision.Review())
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(canonical, []byte(reviewRaw)) {
		return fail(fmt.Errorf("noncanonical stored draft review"))
	}
	return DraftEntry{r, revision, number}, nil
}

// WriteDraft rechecks CAS/account in the transaction even after preparation.
// expected=="" creates a new draft. A commit error cannot prove rollback.
func (d *DB) WriteDraft(ctx context.Context, revision DraftRevision, expected string) (DraftEntry, error) {
	id, rid, hash := revision.DraftID(), revision.ID(), revision.Payload().Hash()
	if err := ValidateDraftID(id); err != nil {
		return DraftEntry{}, err
	}
	if err := ValidateDraftID(rid); err != nil {
		return DraftEntry{}, err
	}
	if expected != "" {
		if err := ValidateDraftID(expected); err != nil {
			return DraftEntry{}, err
		}
	}
	p := revision.Payload().Data()
	if p.Account.PN == "" {
		return DraftEntry{}, fmt.Errorf("validated draft payload required")
	}
	review, err := json.Marshal(revision.Review())
	if err != nil {
		return DraftEntry{}, err
	}
	summary, err := json.Marshal(DraftRevisionSummary(revision))
	if err != nil || len(summary) > 4096 {
		return DraftEntry{}, fmt.Errorf("draft summary budget exceeded")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return DraftEntry{}, err
	}
	defer tx.Rollback()
	number := 1
	at := revision.CreatedAt().UnixNano()
	r := DraftRecord{ID: id, AccountID: p.Account.PN, State: "active", HeadRevisionID: rid, CreatedAt: revision.CreatedAt(), UpdatedAt: revision.CreatedAt()}
	if expected == "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO drafts(id,account_jid,state,head_revision_id,created_at,updated_at) VALUES(?,?,'active',?,?,?)`, id, p.Account.PN, rid, at, at)
	} else {
		r, err = scanDraftRecord(tx.QueryRowContext(ctx, "SELECT "+draftRecordColumns+" FROM drafts WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return DraftEntry{}, DraftFailure("not_found", id, rid, hash, err)
		}
		if err != nil {
			return DraftEntry{}, err
		}
		if r.AccountID != p.Account.PN {
			return DraftEntry{}, DraftFailure("identity_unavailable", id, rid, hash, nil)
		}
		if r.State != "active" || r.HeadRevisionID != expected {
			return DraftEntry{}, DraftFailure("draft_conflict", id, rid, hash, nil)
		}
		if err = tx.QueryRowContext(ctx, "SELECT revision_no+1 FROM draft_revisions WHERE id=? AND draft_id=?", expected, id).Scan(&number); err != nil {
			return DraftEntry{}, err
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE drafts SET head_revision_id=?,updated_at=? WHERE id=? AND head_revision_id=? AND state='active' AND account_jid=?`, rid, at, id, expected, p.Account.PN)
		if err == nil {
			n, rowErr := result.RowsAffected()
			if rowErr != nil {
				return DraftEntry{}, rowErr
			}
			if n != 1 {
				return DraftEntry{}, DraftFailure("draft_conflict", id, rid, hash, nil)
			}
		}
		r.HeadRevisionID = rid
		r.UpdatedAt = revision.CreatedAt()
	}
	if err != nil {
		return DraftEntry{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO draft_revisions(id,draft_id,revision_no,payload_version,payload_hash,payload_json,review_json,summary_json,created_at) VALUES(?,?,?,1,?,?,?,?,?)`, rid, id, number, hash, string(revision.Payload().CanonicalJSON()), string(review), string(summary), at)
	if err != nil {
		return DraftEntry{}, err
	}
	if err = tx.Commit(); err != nil {
		return DraftEntry{}, DraftFailure("local_write_uncertain", id, rid, hash, err)
	}
	return DraftEntry{r, revision, number}, nil
}

func (d *DB) DiscardDraft(ctx context.Context, id, expected string) (DraftEntry, error) {
	entry, err := d.ReadDraft(ctx, id, expected)
	if err != nil {
		return DraftEntry{}, err
	}
	at := time.Now().UTC()
	result, err := d.sql.ExecContext(ctx, `UPDATE drafts SET state='discarded',discarded_at=?,updated_at=? WHERE id=? AND head_revision_id=? AND state='active'`, at.UnixNano(), at.UnixNano(), id, expected)
	if err != nil {
		return DraftEntry{}, DraftFailure("local_write_uncertain", id, expected, entry.Revision.Payload().Hash(), err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return DraftEntry{}, DraftFailure("local_write_uncertain", id, expected, entry.Revision.Payload().Hash(), err)
	}
	if n != 1 {
		return DraftEntry{}, DraftFailure("draft_conflict", id, expected, entry.Revision.Payload().Hash(), nil)
	}
	// No second post-write database read whose failure could obscure the write.
	entry.Record.State = "discarded"
	entry.Record.DiscardedAt = &at
	entry.Record.UpdatedAt = at
	return entry, nil
}

type DraftListItem struct {
	Record           DraftRecord
	RevisionID, Hash string
	Number           int
	Summary          DraftSummary
}
type DraftPage struct {
	Items      []DraftListItem
	HasMore    bool
	NextCursor *string
}
type draftCursor struct {
	Version   int    `json:"v"`
	Store     string `json:"store"`
	Discarded bool   `json:"discarded"`
	Created   int64  `json:"created"`
	ID        string `json:"id"`
}

func ValidateDraftCursor(token string) error { _, err := decodeDraftCursor(token); return err }
func decodeDraftCursor(token string) (draftCursor, error) {
	var c draftCursor
	if len(token) == 0 || len(token) > 16384 {
		return c, invalidDraft("cursor", "invalid size")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return c, invalidDraft("cursor", "invalid encoding")
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, invalidDraft("cursor", "invalid JSON")
	}
	encoded, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(encoded) != token || c.Version != 1 || ValidateDraftID(c.ID) != nil {
		return c, invalidDraft("cursor", "noncanonical token")
	}
	return c, nil
}

func (d *DB) ListDrafts(ctx context.Context, storeRef string, includeDiscarded bool, limit int, cursor string) (DraftPage, error) {
	if limit < 1 || limit > 200 {
		return DraftPage{}, invalidDraft("limit", "expected 1 to 200")
	}
	query := `SELECT d.id,d.account_jid,d.state,d.head_revision_id,d.created_at,d.updated_at,d.discarded_at,r.id,r.payload_hash,r.revision_no,r.summary_json FROM drafts d JOIN draft_revisions r ON r.draft_id=d.id AND r.id=d.head_revision_id WHERE 1=1`
	args := []any{}
	if !includeDiscarded {
		query += ` AND d.state='active'`
	}
	if cursor != "" {
		c, err := decodeDraftCursor(cursor)
		if err != nil {
			return DraftPage{}, err
		}
		if c.Store != storeRef || c.Discarded != includeDiscarded {
			return DraftPage{}, invalidDraft("cursor", "scope mismatch; restart without cursor")
		}
		query += ` AND (d.created_at>? OR (d.created_at=? AND d.id>?))`
		args = append(args, c.Created, c.Created, c.ID)
	}
	query += ` ORDER BY d.created_at,d.id LIMIT ?`
	args = append(args, limit+1)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return DraftPage{}, err
	}
	defer rows.Close()
	page := DraftPage{Items: make([]DraftListItem, 0, limit+1)}
	for rows.Next() {
		var item DraftListItem
		var created, updated int64
		var discarded sql.NullInt64
		var raw string
		if err = rows.Scan(&item.Record.ID, &item.Record.AccountID, &item.Record.State, &item.Record.HeadRevisionID, &created, &updated, &discarded, &item.RevisionID, &item.Hash, &item.Number, &raw); err != nil {
			return DraftPage{}, err
		}
		if len(raw) > 4096 {
			return DraftPage{}, fmt.Errorf("invalid stored draft summary")
		}
		if err = json.Unmarshal([]byte(raw), &item.Summary); err != nil {
			return DraftPage{}, err
		}
		item.Record.CreatedAt = time.Unix(0, created).UTC()
		item.Record.UpdatedAt = time.Unix(0, updated).UTC()
		if discarded.Valid {
			at := time.Unix(0, discarded.Int64).UTC()
			item.Record.DiscardedAt = &at
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return DraftPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
		last := page.Items[limit-1]
		raw, _ := json.Marshal(draftCursor{1, storeRef, includeDiscarded, last.Record.CreatedAt.UnixNano(), last.Record.ID})
		token := base64.RawURLEncoding.EncodeToString(raw)
		if err := ValidateDraftCursor(token); err != nil {
			return DraftPage{}, err
		}
		page.NextCursor = &token
	}
	return page, nil
}

type DraftQuoteRecord struct {
	ChatJID, ID, SenderJID, Text     string
	FromMe, Unavailable, Unsupported bool
}

// ReadDraftQuote reads both verified aliases in one SQL snapshot and preserves
// collisions for the preparer to compare, rather than silently picking a row.
func (d *DB) ReadDraftQuote(ctx context.Context, chat, alias, id string) ([]DraftQuoteRecord, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT chat_jid,msg_id,COALESCE(sender_jid,''),COALESCE(text,''),from_me,
 revoked OR deleted_for_me OR COALESCE(payload_purged_at,0)!=0,
 COALESCE(media_type,'')!='' OR COALESCE(reaction_to_id,'')!='' OR COALESCE(buttons,'')!=''
 FROM messages WHERE msg_id=? AND (chat_jid=? OR (?!='' AND chat_jid=?)) ORDER BY chat_jid`, id, chat, alias, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DraftQuoteRecord
	for rows.Next() {
		var r DraftQuoteRecord
		if err := rows.Scan(&r.ChatJID, &r.ID, &r.SenderJID, &r.Text, &r.FromMe, &r.Unavailable, &r.Unsupported); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func DraftRecipientFromPublic(jid, alias string) DraftRecipient {
	r := DraftRecipient{JID: jid}
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		r.PN = jid
		if strings.HasSuffix(alias, "@lid") {
			r.LID = alias
		}
	} else if strings.HasSuffix(jid, "@lid") {
		r.LID = jid
	}
	return r
}
