package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
)

const OutboundSendVersion = 1

// RequestID correlates IPC only. Key binds the immutable revision in one archive.
type OutboundSendRequest struct {
	Version    int    `json:"version"`
	RequestID  string `json:"request_id"`
	StoreRef   string `json:"store_ref"`
	OwnPN      string `json:"own_pn"`
	DraftID    string `json:"draft_id"`
	RevisionID string `json:"revision_id"`
	Hash       string `json:"hash"`
	Key        string `json:"key"`
}

func ValidateOutboundSelection(draft, revision, hash, key string) error {
	b, err := hex.DecodeString(hash)
	if store.ValidateDraftID(draft) != nil || store.ValidateDraftID(revision) != nil || err != nil || len(b) != 32 || hex.EncodeToString(b) != hash || store.ValidateOutboundKey(key) != nil {
		return fmt.Errorf("explicit draft/revision/hash and bounded ASCII key are required")
	}
	return nil
}

func (r OutboundSendRequest) Validate() error {
	if r.Version != OutboundSendVersion || store.ValidateDraftID(r.RequestID) != nil || store.ValidateOutboundAccount(r.OwnPN) != nil || !filepath.IsAbs(r.StoreRef) || len(r.StoreRef) > 4096 || !utf8.ValidString(r.StoreRef) {
		return fmt.Errorf("invalid outbound request scope/version")
	}
	return ValidateOutboundSelection(r.DraftID, r.RevisionID, r.Hash, r.Key)
}

func (r *OutboundSendRequest) UnmarshalJSON(raw []byte) error {
	type plain OutboundSendRequest
	if len(raw) > 8192 || !utf8.Valid(raw) {
		return fmt.Errorf("invalid outbound request encoding/size")
	}
	var p plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return fmt.Errorf("invalid typed outbound request")
	}
	b, err := json.Marshal(p)
	if err != nil || !bytes.Equal(b, raw) {
		return fmt.Errorf("noncanonical outbound request")
	}
	value := OutboundSendRequest(p)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

type OutboundSendResult struct {
	Entry          store.OutboundEntry        `json:"entry"`
	Duplicate      bool                       `json:"duplicate"`
	Persistence    string                     `json:"persistence"`
	KnownResult    store.OutboundResult       `json:"known_result"`
	KnownACK       *store.OutboundObservation `json:"known_ack,omitempty"`
	HistoryWarning bool                       `json:"history_warning"`
}

// Error carries sanitized correlation plus the known result when persistence or
// transport cannot confirm retention. Causes never cross the public boundary.
type OutboundSendError struct {
	Code        string              `json:"code"`
	Request     OutboundSendRequest `json:"request"`
	OperationID string              `json:"operation_id,omitempty"`
	MessageID   string              `json:"message_id,omitempty"`
	Result      *OutboundSendResult `json:"result,omitempty"`
	Cause       error               `json:"-"`
}

func (e *OutboundSendError) Error() string { return "outbound action: " + e.Code }
func (e *OutboundSendError) Unwrap() error { return e.Cause }

func outboundFailure(r OutboundSendRequest, code string, cause error) *OutboundSendError {
	return &OutboundSendError{Code: code, Request: r, Cause: cause}
}
