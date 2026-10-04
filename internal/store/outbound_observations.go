package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func validateOutboundObservation(o OutboundOperation, f OutboundObservation) error {
	if !validOutboundTime(f.ObservedAt) || f.ObservedAt.Before(o.CreatedAt) || f.EventAt != nil && !validOutboundTime(*f.EventAt) || !validOutboundErrorCode(f.ErrorCode) {
		return invalidOutbound("observation")
	}
	if f.ChatJID != o.Recipient.JID && f.ChatJID != o.Recipient.PN && f.ChatJID != o.Recipient.LID || f.ChatJID == "" {
		return invalidOutbound("observation scope")
	}
	if f.ActorJID != "" && !validOutboundUser(f.ActorJID) || f.ActorAlias != "" && (!validOutboundUser(f.ActorAlias) || f.ActorJID == "" || strings.HasSuffix(f.ActorJID, "@lid") == strings.HasSuffix(f.ActorAlias, "@lid")) {
		return invalidOutbound("observation actor")
	}
	if f.Fact != OutboundServerError && f.ErrorCode != "" {
		return invalidOutbound("observation error")
	}
	own := func(s string) bool { return s != "" && (s == o.Account.PN || s == o.Account.LID) }
	peer := func(s string) bool {
		return s != "" && (s == o.Recipient.JID || s == o.Recipient.PN || s == o.Recipient.LID)
	}
	switch f.Fact {
	case OutboundAck:
		if f.Source != OutboundSendResponse || o.DispatchPossibleAt == nil || f.ActorJID != "" && !own(f.ActorJID) || f.ActorAlias != "" && !own(f.ActorAlias) {
			return invalidOutbound("ack scope")
		}
	case OutboundOwnEcho:
		if f.Source != OutboundLiveEcho && f.Source != OutboundHistoryEcho || !own(f.ActorJID) || f.ActorAlias != "" && !own(f.ActorAlias) {
			return invalidOutbound("echo scope")
		}
	case OutboundDelivered, OutboundRead:
		if f.Source != OutboundLiveReceipt || f.ActorJID == "" || own(f.ActorJID) || own(f.ActorAlias) {
			return invalidOutbound("receipt scope")
		}
		if !strings.HasSuffix(o.Recipient.JID, "@g.us") && (!peer(f.ActorJID) || f.ActorAlias != "" && !peer(f.ActorAlias)) {
			return invalidOutbound("receipt peer")
		}
	case OutboundServerError:
		if f.Source != OutboundLiveReceipt || o.DispatchPossibleAt == nil {
			return invalidOutbound("server error scope")
		}
	default:
		return invalidOutbound("observation fact")
	}
	// A receipt cannot be used to invent dispatch of a purely local reservation.
	if (f.Fact == OutboundDelivered || f.Fact == OutboundRead) && o.DispatchPossibleAt == nil {
		return invalidOutbound("receipt without dispatch checkpoint")
	}
	return nil
}

func insertOutboundObservation(ctx context.Context, c *sql.Conn, id string, f OutboundObservation) (bool, error) {
	res, err := c.ExecContext(ctx, `INSERT INTO outbound_observations(operation_id,fact,source,chat_jid,actor_jid,actor_alias,device,event_at,observed_at,error_code) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, id, f.Fact, f.Source, f.ChatJID, f.ActorJID, f.ActorAlias, f.Device, outboundSQLTime(f.EventAt), f.ObservedAt.UnixNano(), f.ErrorCode)
	if err != nil {
		return false, outboundError("store_error", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, outboundError("store_error", err)
	}
	return n == 1, nil
}

func (d *DB) observeOutbound(ctx context.Context, id string, account DraftIdentity, messageID string, f OutboundObservation) (OutboundOperation, error) {
	if ValidateDraftID(id) != nil || !validOutboundToken(messageID, true) {
		return OutboundOperation{}, invalidOutbound("observation correlation")
	}
	return outboundFullWrite(ctx, d.sql, outboundSQLiteIO(), func(c *sql.Conn) (OutboundOperation, error) {
		o, err := getOutbound(ctx, c, id)
		if err != nil {
			return o, err
		}
		if o.Account != account || o.MessageID != messageID {
			return OutboundOperation{}, outboundError("observation_scope_conflict", nil)
		}
		if err = validateOutboundObservation(o, f); err != nil {
			return OutboundOperation{}, err
		}
		// Reject contradictory asserted participant aliases rather than silently
		// merging two identities. Unknown aliases still remain distinct scopes.
		if f.ActorAlias != "" {
			var conflicts int
			err = c.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_observations WHERE operation_id=? AND actor_alias!='' AND (actor_jid IN (?,?) OR actor_alias IN (?,?)) AND NOT ((actor_jid=? AND actor_alias=?) OR (actor_jid=? AND actor_alias=?))`, id, f.ActorJID, f.ActorAlias, f.ActorJID, f.ActorAlias, f.ActorJID, f.ActorAlias, f.ActorAlias, f.ActorJID).Scan(&conflicts)
			if err != nil {
				return OutboundOperation{}, outboundError("store_error", err)
			}
			if conflicts != 0 {
				return OutboundOperation{}, outboundError("observation_scope_conflict", nil)
			}
		}
		inserted, err := insertOutboundObservation(ctx, c, id, f)
		if err != nil {
			return OutboundOperation{}, err
		}
		if !inserted {
			return o, nil
		}
		// Only the record generation/observation time is projected. Status is
		// derived from immutable facts in a read snapshot, never stored twice.
		at := f.ObservedAt.UTC()
		if at.Before(o.UpdatedAt) {
			at = o.UpdatedAt
		}
		res, err := c.ExecContext(ctx, `UPDATE outbound_operations SET generation=generation+1,updated_at=? WHERE id=? AND generation=?`, at.UnixNano(), id, o.Generation)
		if err != nil {
			return OutboundOperation{}, outboundError("store_error", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return OutboundOperation{}, outboundError("checkpoint_conflict", err)
		}
		o.Generation++
		o.UpdatedAt = at
		return o, nil
	})
}

type outboundStoredObservation struct {
	ID int64
	OutboundObservation
}

func scanOutboundObservation(row interface{ Scan(...any) error }, o OutboundOperation) (outboundStoredObservation, error) {
	var f outboundStoredObservation
	var event sql.NullInt64
	var observed int64
	err := row.Scan(&f.ID, &f.Fact, &f.Source, &f.ChatJID, &f.ActorJID, &f.ActorAlias, &f.Device, &event, &observed, &f.ErrorCode)
	if err != nil {
		return f, outboundError("store_error", err)
	}
	f.EventAt = outboundOptionalTime(event)
	f.ObservedAt = time.Unix(0, observed).UTC()
	if f.ID < 1 || f.ObservedAt.After(o.UpdatedAt) {
		return f, outboundError("store_error", fmt.Errorf("invalid stored observation projection"))
	}
	if err = validateOutboundObservation(o, f.OutboundObservation); err != nil {
		return f, outboundError("store_error", err)
	}
	return f, nil
}

// A read transaction streams facts once, validating even those outside the
// requested evidence page. Memory is bounded by the page and participant scopes;
// work is proportional to this operation's retained observations.
func outboundEvidence(ctx context.Context, tx *sql.Tx, o OutboundOperation, after int64, limit int) (OutboundEvidence, []outboundStoredObservation, bool, error) {
	e := OutboundEvidence{Accepted: "unknown", Delivered: "unknown", Read: "unknown", Scope: "recipient"}
	group := strings.HasSuffix(o.Recipient.JID, "@g.us")
	if group {
		e.Scope = "participants"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,fact,source,chat_jid,actor_jid,actor_alias,device,event_at,observed_at,error_code FROM outbound_observations WHERE operation_id=? ORDER BY id`, o.ID)
	if err != nil {
		return e, nil, false, outboundError("store_error", err)
	}
	defer rows.Close()
	page := make([]outboundStoredObservation, 0, limit+1)
	delivered, read := map[string]bool{}, map[string]bool{}
	aliases := map[string]string{}
	ackObserved := false
	for rows.Next() {
		f, err := scanOutboundObservation(rows, o)
		if err != nil {
			return e, nil, false, err
		}
		if f.ID > after && len(page) < limit+1 {
			page = append(page, f)
		}
		if f.ActorAlias != "" {
			if aliases[f.ActorJID] != "" && aliases[f.ActorJID] != f.ActorAlias || aliases[f.ActorAlias] != "" && aliases[f.ActorAlias] != f.ActorJID {
				return e, nil, false, outboundError("store_error", fmt.Errorf("contradictory retained aliases"))
			}
			aliases[f.ActorJID], aliases[f.ActorAlias] = f.ActorAlias, f.ActorJID
		}
		switch f.Fact {
		case OutboundAck:
			e.Accepted = "observed"
			ackObserved = true
		case OutboundOwnEcho:
			e.OwnEcho = true
		case OutboundServerError:
			e.ServerError = true
		case OutboundDelivered:
			delivered[f.ActorJID] = true
			if !group {
				e.Delivered = "observed"
			}
		case OutboundRead:
			read[f.ActorJID] = true
			delivered[f.ActorJID] = true
			if !group {
				e.Read = "observed"
				e.Delivered = "observed"
			}
		}
	}
	if err = rows.Err(); err != nil {
		return e, nil, false, outboundError("store_error", err)
	}
	// Stronger receipts imply acceptance, but do not change the retained result
	// of the attempt or fabricate an ack/its timestamp.
	if len(delivered) > 0 {
		e.Accepted = "observed"
	}
	if group {
		// A later verified pair can resolve an earlier LID-only scope. Retain
		// individual facts/devices while counting that known pair only once.
		count := func(scopes map[string]bool) int {
			known := map[string]bool{}
			for actor := range scopes {
				if pn := aliases[actor]; strings.HasSuffix(pn, "@s.whatsapp.net") {
					actor = pn
				}
				known[actor] = true
			}
			return len(known)
		}
		deliveredCount, readCount := count(delivered), count(read)
		e.DeliveredParticipants = &deliveredCount
		e.ReadParticipants = &readCount
	}
	if o.Result == OutboundAccepted && !ackObserved {
		return e, nil, false, outboundError("store_error", fmt.Errorf("accepted attempt without retained acknowledgement"))
	}
	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	return e, page, hasMore, nil
}
