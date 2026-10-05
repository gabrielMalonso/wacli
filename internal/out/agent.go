package out

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// AgentAccount identifies the selected archive without opening credentials.
// StoreRef is null only when argument/config resolution failed.
type AgentAccount struct {
	StoreRef *string `json:"store_ref"`
	Name     string  `json:"name,omitempty"`
}

// AgentPage describes only the selected local archive, never remote completeness.
type AgentPage struct {
	Returned   int     `json:"returned"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

type AgentMeta struct {
	Page         *AgentPage `json:"page,omitempty"`
	Source       string     `json:"source"`
	Detail       string     `json:"detail"`
	Completeness string     `json:"completeness"`
	Freshness    string     `json:"freshness"`
	Limit        int        `json:"limit,omitempty"`
	Before       *int       `json:"before,omitempty"`
	After        *int       `json:"after,omitempty"`
	Excluded     []string   `json:"excluded,omitempty"`
	Recovery     string     `json:"recovery,omitempty"`
}

// AgentHistoryError is correlation for an explicit history recovery only.
// It is not a replay token or evidence of rollback/remote completeness.
type AgentHistoryError struct {
	AttemptID            string `json:"attempt_id,omitempty"`
	Phase                string `json:"phase"`
	Outcome              string `json:"outcome"`
	CorrelationConfirmed bool   `json:"correlation_confirmed"`
}

// AgentDraftError identifies a local result to inspect, never a replay token.
type AgentDraftError struct {
	DraftID    string `json:"draft_id,omitempty"`
	RevisionID string `json:"revision_id,omitempty"`
	Hash       string `json:"hash,omitempty"`
}

// AgentError carries a stable public code and a typed cause for exit handling.
// Recovery is emitted only when an actionable next step is known.
type AgentError struct {
	Cleanup   *AgentDraftCleanupError `json:"cleanup,omitempty"`
	Media     *AgentMediaError        `json:"media,omitempty"`
	ChatState *AgentChatStateError    `json:"chat_state,omitempty"`
	Outbound  *AgentOutboundError     `json:"outbound,omitempty"`
	Draft     *AgentDraftError        `json:"draft,omitempty"`
	History   *AgentHistoryError      `json:"history,omitempty"`
	Code      string                  `json:"code"`
	Message   string                  `json:"message"`
	Recovery  string                  `json:"recovery,omitempty"`
	ExitCode  int                     `json:"-"`
	Cause     error                   `json:"-"`
}

// Cleanup knowledge concerns logical unlink effects, never freed disk blocks.
type AgentDraftCleanupError struct {
	DraftID       string `json:"draft_id,omitempty"`
	RevisionID    string `json:"revision_id,omitempty"`
	Hash          string `json:"hash,omitempty"`
	Effect        string `json:"effect"`
	Outcome       string `json:"outcome"`
	DirectorySync string `json:"directory_sync"`
	RemovedBytes  *int64 `json:"removed_bytes"`
}

// Publication knowledge does not assert rollback or future file stability.
type AgentMediaError struct {
	ChatJID         string  `json:"chat_jid"`
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	FilePublication string  `json:"file_publication"`
	Recorded        bool    `json:"recorded"`
	Path            *string `json:"path,omitempty"`
	Bytes           *int64  `json:"bytes,omitempty"`
	SHA256          string  `json:"sha256,omitempty"`
}

// Chat-state invocation knowledge is separate from current remote state.
type AgentChatStateError struct {
	Requested   string `json:"requested"`
	Action      string `json:"action"`
	Outcome     string `json:"outcome"`
	LocalMirror string `json:"local_mirror"`
	OwnPN       string `json:"own_pn,omitempty"`
	OwnLID      string `json:"own_lid,omitempty"`
	TargetJID   string `json:"target_jid,omitempty"`
	TargetPN    string `json:"target_pn,omitempty"`
	TargetLID   string `json:"target_lid,omitempty"`
}

type AgentOutboundError struct {
	RequestID      string     `json:"request_id,omitempty"`
	OperationID    string     `json:"operation_id,omitempty"`
	MessageID      string     `json:"message_id,omitempty"`
	DraftID        string     `json:"draft_id,omitempty"`
	RevisionID     string     `json:"revision_id,omitempty"`
	Hash           string     `json:"hash,omitempty"`
	Key            string     `json:"key,omitempty"`
	OwnPN          string     `json:"own_pn,omitempty"`
	Phase          string     `json:"phase"`
	AttemptResult  string     `json:"attempt_result"`
	KnownResult    string     `json:"known_result"`
	Persistence    string     `json:"persistence"`
	KnownACK       bool       `json:"known_ack"`
	HistoryWarning bool       `json:"history_warning"`
	ACKAt          *time.Time `json:"ack_at,omitempty"`
}

func (e *AgentError) Error() string { return e.Message }
func (e *AgentError) Unwrap() error { return e.Cause }

type agentSuccess[T any] struct {
	SchemaVersion int          `json:"schema_version"`
	Success       bool         `json:"success"`
	Account       AgentAccount `json:"account"`
	Meta          AgentMeta    `json:"meta"`
	Data          T            `json:"data"`
}
type agentFailure struct {
	SchemaVersion int          `json:"schema_version"`
	Success       bool         `json:"success"`
	Account       AgentAccount `json:"account"`
	Meta          AgentMeta    `json:"meta"`
	Error         *AgentError  `json:"error"`
}

// WriteAgentJSON bounds the entire encoded payload before writing any bytes.
// Generics are confined to this envelope boundary; callers provide public DTOs.
func WriteAgentJSON[T any](w io.Writer, account AgentAccount, meta AgentMeta, data T) error {
	return writeAgentJSON(w, account, meta, data, true)
}

// WriteAgentActionJSON reports broken output after effects instead of silently
// succeeding. The caller can retain action correlation for a separate query.
func WriteAgentActionJSON[T any](w io.Writer, account AgentAccount, meta AgentMeta, data T) error {
	return writeAgentJSON(w, account, meta, data, false)
}

func writeAgentJSON[T any](w io.Writer, account AgentAccount, meta AgentMeta, data T, ignoreBrokenPipe bool) error {
	limit := 1 << 20
	if meta.Detail == "full" {
		limit = 8 << 20
	}
	err := writeAgentPolicy(w, agentSuccess[T]{1, true, account, meta, data}, limit, ignoreBrokenPipe)
	if meta.Detail == "full" {
		var tooLarge *AgentError
		if errors.As(err, &tooLarge) && tooLarge.Code == "payload_too_large" {
			tooLarge.Recovery = "Use --detail compact, or narrow the query with smaller list/context limits, fewer resolve inputs, or one selected item."
		}
	}
	return err
}
func WriteAgentError(w io.Writer, account AgentAccount, meta AgentMeta, err *AgentError) error {
	return writeAgent(w, agentFailure{1, false, account, meta, err}, 1<<20)
}
func writeAgent(w io.Writer, value any, limit int) error {
	return writeAgentPolicy(w, value, limit, true)
}
func writeAgentPolicy(w io.Writer, value any, limit int, ignoreBrokenPipe bool) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b)+1 > limit {
		recovery := "Narrow the query with smaller list/context limits, fewer resolve inputs, or one selected item."
		return &AgentError{Code: "payload_too_large", Message: fmt.Sprintf("agent output exceeds %d bytes", limit), Recovery: recovery, ExitCode: 1}
	}
	_, err = fmt.Fprintln(w, string(b))
	if ignoreBrokenPipe && isPlatformBrokenPipe(err) {
		return nil
	}
	return err
}
