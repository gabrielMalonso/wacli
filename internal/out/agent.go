package out

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// AgentAccount identifies the selected archive without opening credentials.
// StoreRef is null only when argument/config resolution failed.
type AgentAccount struct {
	StoreRef *string `json:"store_ref"`
	Name     string  `json:"name,omitempty"`
}

type AgentMeta struct {
	Source       string   `json:"source"`
	Detail       string   `json:"detail"`
	Completeness string   `json:"completeness"`
	Freshness    string   `json:"freshness"`
	Limit        int      `json:"limit,omitempty"`
	Before       *int     `json:"before,omitempty"`
	After        *int     `json:"after,omitempty"`
	Excluded     []string `json:"excluded,omitempty"`
	Recovery     string   `json:"recovery,omitempty"`
}

// AgentError carries a stable public code and a typed cause for exit handling.
// Recovery is emitted only when an actionable next step is known.
type AgentError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Recovery string `json:"recovery,omitempty"`
	ExitCode int    `json:"-"`
	Cause    error  `json:"-"`
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
	limit := 1 << 20
	if meta.Detail == "full" {
		limit = 8 << 20
	}
	err := writeAgent(w, agentSuccess[T]{1, true, account, meta, data}, limit)
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
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b)+1 > limit {
		recovery := "Narrow the query with smaller list/context limits, fewer resolve inputs, or one selected item."
		return &AgentError{Code: "payload_too_large", Message: fmt.Sprintf("agent output exceeds %d bytes", limit), Recovery: recovery, ExitCode: 1}
	}
	_, err = fmt.Fprintln(w, string(b))
	if isPlatformBrokenPipe(err) {
		return nil
	}
	return err
}
