package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OutboundArchive is the concrete local repository boundary. Constructing it
// exposes the typed store actions without invoking them. Sending uses the writer
// under the existing LOCK/owner slot and checks every commit before progressing.
type OutboundArchive struct {
	Reserve    func(context.Context, OutboundReservation) (OutboundOperation, error)
	Checkpoint func(context.Context, OutboundCheckpoint) (OutboundOperation, error)
	Observe    func(context.Context, string, DraftIdentity, string, OutboundObservation) (OutboundOperation, error)
	Read       func(context.Context, string, string, string, int, string) (OutboundEntry, error)
	List       func(context.Context, string, string, int, string) (OutboundPage, error)
}

func (d *DB) Outbound() OutboundArchive {
	return OutboundArchive{d.reserveOutbound, d.checkpointOutbound, d.observeOutbound, d.readOutbound, d.listOutbound}
}

const outboundColumns = `o.id,o.version,o.account_jid,o.idempotency_key,o.draft_id,o.revision_id,o.payload_hash,o.message_id,o.phase,o.attempt_result,o.generation,o.created_at,o.updated_at,o.preparing_at,o.upload_possible_at,o.upload_returned_at,o.dispatch_possible_at,o.finalized_at,o.error_code,r.summary_json,r.payload_hash,r.payload_version,d.account_jid`
const outboundFrom = ` FROM outbound_operations o LEFT JOIN draft_revisions r ON r.draft_id=o.draft_id AND r.id=o.revision_id LEFT JOIN drafts d ON d.id=o.draft_id `

func scanOutbound(row interface{ Scan(...any) error }) (OutboundOperation, error) {
	var o OutboundOperation
	var account, raw, hash, draftAccount string
	var version int
	var created, updated int64
	var preparing, upload, uploaded, dispatch, final sql.NullInt64
	err := row.Scan(&o.ID, &o.Version, &account, &o.Key, &o.DraftID, &o.RevisionID, &o.Hash, &o.MessageID, &o.Phase, &o.Result, &o.Generation, &created, &updated, &preparing, &upload, &uploaded, &dispatch, &final, &o.ErrorCode, &raw, &hash, &version, &draftAccount)
	if errors.Is(err, sql.ErrNoRows) {
		return o, outboundError("not_found", err)
	}
	if err != nil {
		return o, outboundError("store_error", err)
	}
	bad := func(err error) (OutboundOperation, error) {
		return OutboundOperation{}, outboundError("store_error", err)
	}
	if len(raw) > 4096 {
		return bad(fmt.Errorf("invalid stored summary size"))
	}
	var summary DraftSummary
	if err = json.Unmarshal([]byte(raw), &summary); err != nil {
		return bad(err)
	}
	canonical, err := json.Marshal(summary)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return bad(fmt.Errorf("noncanonical stored summary"))
	}
	o.Account, o.Recipient, o.Kind = summary.Account, summary.Recipient, summary.Kind
	o.CreatedAt, o.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	o.PreparingAt, o.UploadPossibleAt, o.UploadReturnedAt, o.DispatchPossibleAt, o.FinalizedAt = outboundOptionalTime(preparing), outboundOptionalTime(upload), outboundOptionalTime(uploaded), outboundOptionalTime(dispatch), outboundOptionalTime(final)
	if account != o.Account.PN || account != draftAccount || hash != o.Hash || version != DraftPayloadVersion {
		return bad(fmt.Errorf("stored outbound binding disagrees with revision"))
	}
	if err = o.validate(); err != nil {
		return bad(err)
	}
	return o, nil
}
func outboundOptionalTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}
func outboundSQLTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func getOutbound(ctx context.Context, q draftReader, id string) (OutboundOperation, error) {
	return scanOutbound(q.QueryRowContext(ctx, "SELECT "+outboundColumns+outboundFrom+"WHERE o.id=?", id))
}

func (d *DB) reserveOutbound(ctx context.Context, r OutboundReservation) (OutboundOperation, error) {
	if err := r.validate(); err != nil {
		return OutboundOperation{}, err
	}
	return outboundFullWrite(ctx, d.sql, outboundSQLiteIO(), func(c *sql.Conn) (OutboundOperation, error) {
		var account string
		err := c.QueryRowContext(ctx, `SELECT d.account_jid FROM drafts d JOIN draft_revisions r ON r.draft_id=d.id WHERE d.id=? AND r.id=?`, r.DraftID, r.RevisionID).Scan(&account)
		if errors.Is(err, sql.ErrNoRows) {
			return OutboundOperation{}, outboundError("not_found", err)
		}
		if err != nil || ValidateOutboundAccount(account) != nil {
			return OutboundOperation{}, outboundError("store_error", err)
		}
		old, err := scanOutbound(c.QueryRowContext(ctx, "SELECT "+outboundColumns+outboundFrom+"WHERE o.account_jid=? AND o.idempotency_key=?", account, r.Key))
		if err == nil {
			if old.Version != r.Version || old.DraftID != r.DraftID || old.RevisionID != r.RevisionID || old.Hash != r.Hash {
				return OutboundOperation{}, outboundError("idempotency_conflict", nil)
			}
			return old, nil
		}
		var failure *OutboundError
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			return OutboundOperation{}, err
		}
		entry, err := readDraft(ctx, c, r.DraftID, r.RevisionID)
		if err != nil {
			return OutboundOperation{}, outboundError("store_error", err)
		}
		p := entry.Revision.Payload().Data()
		if entry.Revision.Payload().Hash() != r.Hash {
			return OutboundOperation{}, outboundError("hash_conflict", nil)
		}
		if entry.Record.State != "active" {
			return OutboundOperation{}, outboundError("draft_conflict", nil)
		}
		if r.Account != p.Account {
			return OutboundOperation{}, outboundError("identity_unavailable", nil)
		}
		o := OutboundOperation{ID: r.ID, Version: r.Version, Key: r.Key, DraftID: r.DraftID, RevisionID: r.RevisionID, Hash: r.Hash, MessageID: r.MessageID, Account: p.Account, Recipient: p.Recipient, Kind: p.Kind, Phase: OutboundReserved, Result: OutboundPending, Generation: 1, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.CreatedAt.UTC()}
		if err = o.validate(); err != nil {
			return OutboundOperation{}, invalidOutbound("reservation")
		}
		_, err = c.ExecContext(ctx, `INSERT INTO outbound_operations(id,version,account_jid,idempotency_key,draft_id,revision_id,payload_hash,message_id,phase,attempt_result,generation,created_at,updated_at,error_code) VALUES(?,?,?,?,?,?,?,?,'reserved','pending',1,?,?,'')`, o.ID, o.Version, account, o.Key, o.DraftID, o.RevisionID, o.Hash, o.MessageID, o.CreatedAt.UnixNano(), o.UpdatedAt.UnixNano())
		if err != nil {
			return OutboundOperation{}, outboundError("store_error", err)
		}
		return o, nil
	})
}

func (d *DB) checkpointOutbound(ctx context.Context, ch OutboundCheckpoint) (OutboundOperation, error) {
	if ValidateDraftID(ch.ID) != nil || !validOutboundToken(ch.MessageID, true) || ch.Generation < 1 || !validOutboundTime(ch.At) || !validOutboundErrorCode(ch.ErrorCode) {
		return OutboundOperation{}, invalidOutbound("checkpoint")
	}
	return outboundFullWrite(ctx, d.sql, outboundSQLiteIO(), func(c *sql.Conn) (OutboundOperation, error) {
		o, err := getOutbound(ctx, c, ch.ID)
		if err != nil {
			return o, err
		}
		if o.Account != ch.Account || o.MessageID != ch.MessageID {
			return OutboundOperation{}, outboundError("checkpoint_scope_conflict", nil)
		}
		if o.Generation != ch.Generation || o.Phase == OutboundFinalized {
			return OutboundOperation{}, outboundError("checkpoint_conflict", nil)
		}
		next := o
		next.Phase, next.Result, next.ErrorCode = ch.Phase, ch.Result, ch.ErrorCode
		next.Generation++
		next.UpdatedAt = ch.At.UTC()
		at := ch.At.UTC()
		allowed := false
		switch ch.Phase {
		case OutboundPreparing:
			allowed = o.Phase == OutboundReserved
			next.PreparingAt = &at
		case OutboundUploadPossible:
			allowed = o.Phase == OutboundPreparing && o.Kind == DraftDocumentKind
			next.UploadPossibleAt = &at
		case OutboundUploadReturned:
			allowed = o.Phase == OutboundUploadPossible
			next.UploadReturnedAt = &at
		case OutboundDispatchPossible:
			allowed = o.Phase == OutboundPreparing && o.Kind != DraftDocumentKind || o.Phase == OutboundUploadReturned
			next.DispatchPossibleAt = &at
		case OutboundFinalized:
			allowed = true
			next.FinalizedAt = &at
		}
		if !allowed || next.validate() != nil || (ch.Result == OutboundAccepted) != (ch.Ack != nil) {
			return OutboundOperation{}, invalidOutbound("transition")
		}
		if ch.Ack != nil {
			if ch.Ack.Fact != OutboundAck || ch.Ack.ObservedAt.After(ch.At) || validateOutboundObservation(next, *ch.Ack) != nil {
				return OutboundOperation{}, invalidOutbound("ack")
			}
			if _, err = insertOutboundObservation(ctx, c, o.ID, *ch.Ack); err != nil {
				return OutboundOperation{}, err
			}
		}
		res, err := c.ExecContext(ctx, `UPDATE outbound_operations SET phase=?,attempt_result=?,generation=?,updated_at=?,preparing_at=?,upload_possible_at=?,upload_returned_at=?,dispatch_possible_at=?,finalized_at=?,error_code=? WHERE id=? AND generation=?`, next.Phase, next.Result, next.Generation, next.UpdatedAt.UnixNano(), outboundSQLTime(next.PreparingAt), outboundSQLTime(next.UploadPossibleAt), outboundSQLTime(next.UploadReturnedAt), outboundSQLTime(next.DispatchPossibleAt), outboundSQLTime(next.FinalizedAt), next.ErrorCode, o.ID, o.Generation)
		if err != nil {
			return OutboundOperation{}, outboundError("store_error", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return OutboundOperation{}, outboundError("checkpoint_conflict", err)
		}
		return next, nil
	})
}
