package store

import (
	"fmt"
	"strings"
	"time"
)

const OutboundVersion = 1

type OutboundPhase string

const (
	OutboundReserved         OutboundPhase = "reserved"
	OutboundPreparing        OutboundPhase = "preparing"
	OutboundUploadPossible   OutboundPhase = "upload_possible"
	OutboundUploadReturned   OutboundPhase = "upload_returned"
	OutboundDispatchPossible OutboundPhase = "dispatch_possible"
	OutboundFinalized        OutboundPhase = "finalized"
)

type OutboundResult string

const (
	OutboundPending       OutboundResult = "pending"
	OutboundAccepted      OutboundResult = "accepted"
	OutboundRejected      OutboundResult = "rejected"
	OutboundNotDispatched OutboundResult = "not_dispatched"
	OutboundUncertain     OutboundResult = "uncertain"
)

type OutboundFact string

const (
	OutboundAck         OutboundFact = "ack"
	OutboundOwnEcho     OutboundFact = "own_echo"
	OutboundDelivered   OutboundFact = "delivered"
	OutboundRead        OutboundFact = "read"
	OutboundServerError OutboundFact = "server_error"
)

type OutboundSource string

const (
	OutboundSendResponse OutboundSource = "send_response"
	OutboundLiveReceipt  OutboundSource = "live_receipt"
	OutboundLiveEcho     OutboundSource = "live_echo"
	OutboundHistoryEcho  OutboundSource = "history_echo"
)

// OutboundError keeps persisted-data failures distinct from invalid caller input.
type OutboundError struct {
	Code  string
	Cause error
}

func (e *OutboundError) Error() string             { return "local outbound archive: " + e.Code }
func (e *OutboundError) Unwrap() error             { return e.Cause }
func outboundError(code string, cause error) error { return &OutboundError{code, cause} }
func invalidOutbound(field string) error {
	return outboundError("invalid_arguments", fmt.Errorf("invalid outbound %s", field))
}

// Reservation binds one immutable revision. Account is a current public identity
// supplied by the future caller, checked only when inserting a NEW operation.
// MessageID is supplied by that caller; the store never constructs a WA client.
type OutboundReservation struct {
	Version                                       int
	ID, DraftID, RevisionID, Hash, Key, MessageID string
	Account                                       DraftIdentity
	CreatedAt                                     time.Time
}

type OutboundOperation struct {
	ID                 string         `json:"id"`
	Version            int            `json:"version"`
	Key                string         `json:"key"`
	DraftID            string         `json:"draft_id"`
	RevisionID         string         `json:"revision_id"`
	Hash               string         `json:"hash"`
	MessageID          string         `json:"message_id"`
	Account            DraftIdentity  `json:"account_identity"`
	Recipient          DraftRecipient `json:"recipient"`
	Kind               DraftKind      `json:"kind"`
	Phase              OutboundPhase  `json:"phase"`
	Result             OutboundResult `json:"attempt_result"`
	Generation         int64          `json:"generation"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	PreparingAt        *time.Time     `json:"preparing_at"`
	UploadPossibleAt   *time.Time     `json:"upload_possible_at"`
	UploadReturnedAt   *time.Time     `json:"upload_returned_at"`
	DispatchPossibleAt *time.Time     `json:"dispatch_possible_at"`
	FinalizedAt        *time.Time     `json:"finalized_at"`
	ErrorCode          string         `json:"error_code"`
}

// Checkpoint is a CAS on the local attempt, not permission to replay a send.
// Finalizing as accepted requires an ack, inserted in the same transaction.
type OutboundCheckpoint struct {
	ID         string
	Account    DraftIdentity
	MessageID  string
	Generation int64
	Phase      OutboundPhase
	Result     OutboundResult
	At         time.Time
	ErrorCode  string
	Ack        *OutboundObservation
}

// Device is retained separately from the normalized public user identity.
// EventAt is null when the source did not supply a measured timestamp.
type OutboundObservation struct {
	Fact       OutboundFact   `json:"fact"`
	Source     OutboundSource `json:"source"`
	ChatJID    string         `json:"chat_jid"`
	ActorJID   string         `json:"actor_jid"`
	ActorAlias string         `json:"actor_alias"`
	Device     uint16         `json:"device"`
	EventAt    *time.Time     `json:"event_at"`
	ObservedAt time.Time      `json:"observed_at"`
	ErrorCode  string         `json:"error_code"`
}

type OutboundEvidence struct {
	Accepted              string `json:"accepted"`
	Delivered             string `json:"delivered"`
	Read                  string `json:"read"`
	OwnEcho               bool   `json:"own_echo_observed"`
	ServerError           bool   `json:"server_error_observed"`
	Scope                 string `json:"scope"`
	DeliveredParticipants *int   `json:"observed_delivered_participant_scopes"`
	ReadParticipants      *int   `json:"observed_read_participant_scopes"`
}

func ValidateOutboundKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return invalidOutbound("key")
	}
	for i := range key {
		if key[i] < '!' || key[i] > '~' {
			return invalidOutbound("key")
		}
	}
	return nil
}

func ValidateOutboundMessageID(id string) error {
	if !validOutboundToken(id, true) {
		return invalidOutbound("message-id")
	}
	return nil
}

func ValidateOutboundAccount(pn string) error {
	n, err := NormalizeDraftTarget(pn)
	if err != nil || n != pn || len(pn) > 128 || !strings.HasSuffix(pn, "@s.whatsapp.net") {
		return invalidOutbound("account-jid")
	}
	return nil
}

func validOutboundToken(s string, required bool) bool {
	if len(s) > 128 || required && len(s) == 0 {
		return false
	}
	for i := range s {
		if !(s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9' || s[i] == '_' || s[i] == '-' || s[i] == '.') {
			return false
		}
	}
	return true
}
func validOutboundErrorCode(code string) bool {
	switch code {
	case "", "canceled", "deadline", "preparation_failed", "identity_changed", "upload_error", "transport_error", "server_error", "store_error":
		return true
	}
	return false
}
func validOutboundTime(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}
func validOutboundUser(jid string) bool {
	n, err := NormalizeDraftTarget(jid)
	return err == nil && n == jid && len(jid) <= 128 && (strings.HasSuffix(jid, "@s.whatsapp.net") || strings.HasSuffix(jid, "@lid"))
}
func (r OutboundReservation) validate() error {
	if r.Version != OutboundVersion || ValidateDraftID(r.ID) != nil || ValidateDraftID(r.DraftID) != nil || ValidateDraftID(r.RevisionID) != nil || !draftHex(r.Hash, 64) || !validOutboundToken(r.MessageID, true) || !validOutboundTime(r.CreatedAt) {
		return invalidOutbound("reservation")
	}
	return ValidateOutboundKey(r.Key)
}

func (o OutboundOperation) validate() error {
	if o.Version != OutboundVersion || ValidateDraftID(o.ID) != nil || ValidateDraftID(o.DraftID) != nil || ValidateDraftID(o.RevisionID) != nil || !draftHex(o.Hash, 64) || ValidateOutboundKey(o.Key) != nil || !validOutboundToken(o.MessageID, true) || o.Generation < 1 || !validOutboundTime(o.CreatedAt) || !validOutboundTime(o.UpdatedAt) || o.UpdatedAt.Before(o.CreatedAt) || !validOutboundErrorCode(o.ErrorCode) {
		return fmt.Errorf("invalid stored operation fields")
	}
	if account, err := normalizeDraftAccount(o.Account); err != nil || account != o.Account || ValidateOutboundAccount(o.Account.PN) != nil || len(o.Account.LID) > 128 {
		return fmt.Errorf("invalid stored account")
	}
	r, err := normalizeDraftRecipient(o.Recipient, false)
	if err != nil || r != o.Recipient || len(r.JID) > 128 || len(r.PN) > 128 || len(r.LID) > 128 || draftRecipientIsSelf(r, o.Account) {
		return fmt.Errorf("invalid stored recipient")
	}
	if o.Kind != DraftTextKind && !o.Kind.HasUpload() && o.Kind != DraftContactKind {
		return fmt.Errorf("invalid stored kind")
	}
	last := o.CreatedAt
	for _, at := range []*time.Time{o.PreparingAt, o.UploadPossibleAt, o.UploadReturnedAt, o.DispatchPossibleAt, o.FinalizedAt} {
		if at != nil {
			if !validOutboundTime(*at) || at.Before(last) || at.After(o.UpdatedAt) {
				return fmt.Errorf("invalid stored checkpoint time")
			}
			last = *at
		}
	}
	if o.UploadPossibleAt != nil && (!o.Kind.HasUpload() || o.PreparingAt == nil) || o.UploadReturnedAt != nil && o.UploadPossibleAt == nil || o.DispatchPossibleAt != nil && (o.PreparingAt == nil || o.Kind.HasUpload() && o.UploadReturnedAt == nil) {
		return fmt.Errorf("invalid stored checkpoint order")
	}
	switch o.Phase {
	case OutboundReserved:
		if o.PreparingAt != nil || o.UploadPossibleAt != nil || o.DispatchPossibleAt != nil {
			return fmt.Errorf("reserved with checkpoints")
		}
	case OutboundPreparing:
		if o.PreparingAt == nil || o.UploadPossibleAt != nil || o.DispatchPossibleAt != nil {
			return fmt.Errorf("invalid preparing checkpoint")
		}
	case OutboundUploadPossible:
		if o.UploadPossibleAt == nil || o.UploadReturnedAt != nil || o.DispatchPossibleAt != nil {
			return fmt.Errorf("invalid upload checkpoint")
		}
	case OutboundUploadReturned:
		if o.UploadReturnedAt == nil || o.DispatchPossibleAt != nil {
			return fmt.Errorf("invalid upload return")
		}
	case OutboundDispatchPossible:
		if o.DispatchPossibleAt == nil {
			return fmt.Errorf("missing dispatch checkpoint")
		}
	case OutboundFinalized:
		if o.FinalizedAt == nil {
			return fmt.Errorf("missing finalization")
		}
	default:
		return fmt.Errorf("unknown phase")
	}
	if o.Phase != OutboundFinalized {
		if o.Result != OutboundPending || o.FinalizedAt != nil || o.ErrorCode != "" {
			return fmt.Errorf("unfinished attempt with result")
		}
		return nil
	}
	switch o.Result {
	case OutboundAccepted, OutboundRejected:
		if o.DispatchPossibleAt == nil {
			return fmt.Errorf("remote result without possible dispatch")
		}
	case OutboundNotDispatched:
		if o.DispatchPossibleAt != nil {
			return fmt.Errorf("not dispatched after possible dispatch")
		}
	case OutboundUncertain:
		if o.DispatchPossibleAt == nil && o.UploadPossibleAt == nil {
			return fmt.Errorf("uncertain without possible effect")
		}
	default:
		return fmt.Errorf("invalid finalized result")
	}
	return nil
}

// ValidateOutboundOperation checks typed IPC snapshots with the same invariants
// as retained records. It does not assert that a peer actually committed them.
func ValidateOutboundOperation(o OutboundOperation) error { return o.validate() }

func (o OutboundOperation) EvidenceStatus(e OutboundEvidence) string {
	if e.Scope == "recipient" {
		if e.Read == "observed" {
			return "read"
		}
		if e.Delivered == "observed" {
			return "delivered"
		}
	}
	if e.Accepted == "observed" {
		return "accepted"
	}
	if o.Result == OutboundRejected || o.Result == OutboundNotDispatched {
		return "failed"
	}
	if o.Result == OutboundUncertain || o.DispatchPossibleAt != nil {
		return "uncertain"
	}
	return "incomplete"
}
