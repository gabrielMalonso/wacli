package out

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAgentEnvelopeExclusivityAndOutputCaps(t *testing.T) {
	type publicData struct {
		Text string `json:"text"`
	}
	ref := "/fixture/store"
	account := AgentAccount{StoreRef: &ref, Name: "fixture"}
	for _, detail := range []string{"compact", "full"} {
		meta := AgentMeta{Source: "local", Detail: detail, Completeness: "unknown", Freshness: "unknown", Page: &AgentPage{Returned: 1, HasMore: false, NextCursor: nil}}
		var buf bytes.Buffer
		if err := WriteAgentJSON(&buf, account, meta, publicData{"hello"}); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if _, ok := decoded["error"]; ok {
			t.Fatal("success includes error")
		}
		buf.Reset()
		cap := 1 << 20
		if detail == "full" {
			cap = 8 << 20
		}
		err := WriteAgentJSON(&buf, account, meta, publicData{strings.Repeat("x", cap)})
		var typed *AgentError
		if !errors.As(err, &typed) || typed.Code != "payload_too_large" || buf.Len() != 0 {
			t.Fatalf("partial/unbounded output: %v len=%d", err, buf.Len())
		}
		if err := WriteAgentError(&buf, account, meta, typed); err != nil {
			t.Fatal(err)
		}
		decoded = nil
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if _, ok := decoded["data"]; ok {
			t.Fatal("failure includes data")
		}
		if strings.Contains(buf.String(), "ExitCode") || strings.Contains(buf.String(), "Cause") {
			t.Fatal("error exposed internal fields")
		}
	}
}
