package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func TestDraftIPCValidationCategory(t *testing.T) {
	message := "reply"
	req := sendDelegateRequest{Kind: draftWriteKind, Draft: &app.DraftWriteRequest{Version: 1, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/fixture/store", Input: &app.DraftInput{To: "15550000002@s.whatsapp.net", Message: &message, ReplyTo: "quote"}}}
	typed := &store.DraftValidationError{Field: "reply.sender", Reason: "UNTRUSTED_REASON /private/fixture\n"}
	emitted := draftRefusal(req, typed)
	wire, err := json.Marshal(emitted)
	if err != nil {
		t.Fatal(err)
	}
	withoutCategory := emitted
	withoutCategory.DraftValidationField = ""
	oldWire, err := json.Marshal(withoutCategory)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("UNTRUSTED_REASON")) || bytes.Contains(wire, []byte("private")) {
		t.Fatal("raw reason leaked")
	}
	var oldClient struct {
		DraftFailure     *store.DraftError `json:"draft_failure"`
		DraftRequestHash string            `json:"draft_request_hash"`
		OK               bool              `json:"ok"`
	}
	if err = json.Unmarshal(wire, &oldClient); err != nil {
		t.Fatal("old client rejected additive field")
	}
	oldFailure := classifyDraftError(draftIPCFailure(req, sendDelegateResponse{DraftFailure: oldClient.DraftFailure, DraftRequestHash: oldClient.DraftRequestHash, OK: oldClient.OK}))
	if oldFailure.Code != "invalid_arguments" || oldFailure.ExitCode != 2 || oldFailure.Draft.DraftID != req.Draft.DraftID {
		t.Fatal("old client compatibility failed")
	}
	t.Logf("wire without=%d with=%d additive_bytes=%d; old client retains correlated invalid_arguments", len(oldWire), len(wire), len(wire)-len(oldWire))
	cases := []struct {
		name     string
		mutate   func(*sendDelegateRequest, *sendDelegateResponse)
		code     string
		targeted bool
	}{
		{"new_owner_new_client", func(_ *sendDelegateRequest, _ *sendDelegateResponse) {}, "invalid_arguments", true},
		{"old_owner_missing_field", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftValidationField = "" }, "invalid_arguments", false},
		{"future_unknown_field", func(_ *sendDelegateRequest, r *sendDelegateResponse) {
			r.DraftValidationField = "UNKNOWN_PRIVATE_TOKEN"
		}, "invalid_arguments", false},
		{"category_without_quote", func(q *sendDelegateRequest, _ *sendDelegateResponse) { q.Draft.Input.ReplyTo = "" }, "local_write_uncertain", false},
		{"category_without_quote_correlated", func(q *sendDelegateRequest, r *sendDelegateResponse) {
			q.Draft.Input.ReplyTo = ""
			r.DraftRequestHash = draftRequestHash(*q.Draft)
		}, "invalid_arguments", false},
		{"category_other_operation", func(q *sendDelegateRequest, r *sendDelegateResponse) { q.Kind = "other-local-fixture-operation" }, "invalid_arguments", false},
		{"unknown_refusal_code", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure.Code = "unknown-fixture-code" }, "local_write_uncertain", false},
		{"category_on_uncertain_code", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure.Code = "local_write_uncertain" }, "local_write_uncertain", false},
		{"category_on_other_code", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure.Code = "read_only" }, "read_only", false},
		{"mismatch_request_hash", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftRequestHash = strings.Repeat("f", 64) }, "local_write_uncertain", false},
		{"mismatch_draft_id", func(_ *sendDelegateRequest, r *sendDelegateResponse) {
			r.DraftFailure.DraftID = strings.Repeat("c", 32)
		}, "local_write_uncertain", false},
		{"mismatch_revision_id", func(_ *sendDelegateRequest, r *sendDelegateResponse) {
			r.DraftFailure.RevisionID = strings.Repeat("c", 32)
		}, "local_write_uncertain", false},
		{"missing_failure", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure = nil }, "local_write_uncertain", false},
		{"invalid_failure_hash", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure.Hash = "bad" }, "local_write_uncertain", false},
		{"valid_failure_hash", func(_ *sendDelegateRequest, r *sendDelegateResponse) { r.DraftFailure.Hash = strings.Repeat("c", 64) }, "invalid_arguments", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqRaw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			var q sendDelegateRequest
			if err := json.Unmarshal(reqRaw, &q); err != nil {
				t.Fatal(err)
			}
			var r sendDelegateResponse
			if err := json.Unmarshal(wire, &r); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&q, &r)
			// Decode the complete wire response before the production classifier sees it.
			roundtrip, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(roundtrip, &r); err != nil {
				t.Fatal(err)
			}
			failure := classifyDraftError(draftIPCFailure(q, r))
			if failure.Code != tc.code {
				t.Fatalf("code=%s want=%s", failure.Code, tc.code)
			}
			specific := strings.HasPrefix(failure.Message, "Quoted sender identity")
			if specific != tc.targeted {
				t.Fatal("categorical hint escaped its allowed scope")
			}
			if tc.targeted && (!errors.Is(failure, r.DraftFailure) || failure.ExitCode != 2) {
				t.Fatal("typed cause or exit code lost")
			}
			if failure.Draft == nil || failure.Draft.DraftID != q.Draft.DraftID || failure.Draft.RevisionID != q.Draft.RevisionID {
				t.Fatal("correlation not preserved")
			}
			if tc.name == "valid_failure_hash" && failure.Draft.Hash != r.DraftFailure.Hash {
				t.Fatal("hash correlation lost")
			}
			var output bytes.Buffer
			ref := q.Draft.StoreRef
			if err := out.WriteAgentError(&output, out.AgentAccount{StoreRef: &ref}, out.AgentMeta{Source: "local", Detail: "compact", Completeness: "unknown", Freshness: "unknown"}, failure); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(output.Bytes(), []byte("UNTRUSTED_REASON")) || bytes.Contains(output.Bytes(), []byte("UNKNOWN_PRIVATE_TOKEN")) {
				t.Fatal("categorical/raw error echoed publicly")
			}
			t.Logf("%s code=%s specific=%v public_bytes=%d", tc.name, failure.Code, specific, output.Len())
		})
	}
	for _, field := range []string{"reply", "recipient", "reply.sender.extra"} {
		emitted := draftRefusal(req, &store.DraftValidationError{Field: field, Reason: "untrusted"})
		if emitted.DraftValidationField != "" {
			t.Fatal("non-allowlisted field emitted")
		}
	}
	// A malformed new field is rejected by the normal typed JSON decoder.
	malformed := bytes.Replace(wire, []byte(`"reply.sender"`), []byte(`123`), 1)
	var newClient sendDelegateResponse
	if err = json.Unmarshal(malformed, &newClient); err == nil {
		t.Fatal("malformed categorical type accepted")
	}
	if err = json.Unmarshal(malformed, &oldClient); err != nil {
		t.Fatal("old decoder unexpectedly reads new field")
	}
	// The advisory field has no role in success correlation or payload validation.
	entry := draftOutputFixture(t, store.DraftTextKind, "reply", nil)
	success := sendDelegateResponse{OK: true, DraftResult: projectDraftDelegate(*req.Draft, entry), DraftValidationField: draftReplySenderField}
	if _, err := validateDraftDelegateResult(*req.Draft, success.DraftResult); err != nil {
		t.Fatal("success validation changed")
	}
	success.DraftResult = nil
	if _, err := validateDraftDelegateResult(*req.Draft, success.DraftResult); err == nil {
		t.Fatal("category manufactured success")
	}
}

func TestDraftIPCValidationCategoryEmissionIsNarrow(t *testing.T) {
	message := "reply"
	base := app.DraftWriteRequest{Version: 1, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/fixture/store", Input: &app.DraftInput{To: "15550000002@s.whatsapp.net", Message: &message, ReplyTo: "quote"}}
	cases := []struct {
		name   string
		mutate func(*app.DraftWriteRequest)
		field  string
		code   string
		want   bool
	}{
		{"create_quote", func(*app.DraftWriteRequest) {}, "reply.sender", "invalid_arguments", true},
		{"update_quote", func(q *app.DraftWriteRequest) { q.Action = "update"; q.ExpectedRevision = strings.Repeat("c", 32) }, "reply.sender", "invalid_arguments", true},
		{"no_quote", func(q *app.DraftWriteRequest) { q.Input.ReplyTo = "" }, "reply.sender", "invalid_arguments", false},
		{"no_input", func(q *app.DraftWriteRequest) { q.Input = nil }, "reply.sender", "invalid_arguments", false},
		{"discard", func(q *app.DraftWriteRequest) { q.Action = "discard" }, "reply.sender", "invalid_arguments", false},
		{"other_validation", func(*app.DraftWriteRequest) {}, "reply", "invalid_arguments", false},
		{"other_base_error", func(*app.DraftWriteRequest) {}, "reply.sender", "store_unavailable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			input := *base.Input
			q.Input = &input
			tc.mutate(&q)
			validation := &store.DraftValidationError{Field: tc.field, Reason: "SECRET_OWNER_REASON"}
			err := store.DraftFailure(tc.code, q.DraftID, q.RevisionID, "", fmt.Errorf("SECRET_OWNER_WRAPPER: %w", validation))
			response := draftRefusal(sendDelegateRequest{Kind: draftWriteKind, Draft: &q}, err)
			if (response.DraftValidationField == draftReplySenderField) != tc.want {
				t.Fatal("category emitted outside its allowlist/scope")
			}
			if response.DraftFailure.Code != tc.code {
				t.Fatal("categorical advice changed base refusal")
			}
			wire, encodeErr := json.Marshal(response)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if bytes.Contains(wire, []byte("SECRET_OWNER")) {
				t.Fatal("raw cause or reason crossed the wire")
			}
		})
	}
}

func TestDraftIPCQuoteGuidanceThroughFakeOwnerHandler(t *testing.T) {
	t.Setenv("WACLI_READONLY", "0")
	for _, oldOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("old_owner=%v", oldOwner), func(t *testing.T) {
			dir, a := draftOwnerFixture(t, false)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			const badQuote = "bad-quote-fixture"
			if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: badQuote, SenderJID: "15550000003@s.whatsapp.net", Text: "SECRET_QUOTED_FIXTURE_TEXT", Timestamp: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
				calls.Add(1)
				if req.Kind != draftWriteKind {
					return sendDelegateResponse{}, errors.New("fixture rejects non-draft dispatch")
				}
				response, err := executeDelegatedSend(ctx, a, req)
				if response.DraftFailure != nil && response.DraftFailure.Code == "invalid_arguments" && response.DraftValidationField != draftReplySenderField {
					t.Error("handler failed to categorize quote refusal")
				}
				if oldOwner {
					response.DraftValidationField = ""
				}
				return response, err
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			run := func(args ...string) (string, string, error) {
				return runDraftBinary(t, "", append([]string{"--agent", "--store", dir, "--timeout", "2s", "draft"}, args...), false)
			}
			_, stderr, err := run("create", "--read-only", "--to", localReadPN, "--message", "guard")
			if err == nil || calls.Load() != 0 || decodeAgentTest(t, stderr).Error.Code != "read_only" {
				t.Fatal("readonly guard reached owner")
			}
			// A bad sender must be rejected before opening the requested document.
			stdout, stderr, err := run("create", "--to", localReadPN, "--file", filepath.Join(dir, "UNOPENED_FIXTURE_DOCUMENT"), "--reply-to", badQuote)
			if err == nil || stdout != "" || calls.Load() != 1 {
				t.Fatal("quote refusal did not traverse the handler exactly once")
			}
			failure := decodeAgentTest(t, stderr)
			if failure.Error.Code != "invalid_arguments" || failure.Error.Draft == nil || failure.Error.Draft.DraftID == "" || failure.Error.Draft.RevisionID == "" {
				t.Fatal("public quote refusal lost code/correlation")
			}
			if failure.Meta.Source != "local" || failure.Meta.Completeness != "unknown" || failure.Meta.Freshness != "unknown" || strings.Count(stderr, "\n") != 1 {
				t.Fatal("refusal certainty or minified envelope changed")
			}
			if strings.HasPrefix(failure.Error.Message, "Quoted sender identity") == oldOwner {
				t.Fatal("category did not survive handler/client compatibility boundary")
			}
			if strings.Contains(stderr, "SECRET_QUOTED") || strings.Contains(stderr, "UNOPENED_FIXTURE_DOCUMENT") || strings.Contains(failure.Error.Recovery, "draft show") || strings.Contains(failure.Error.Recovery, "replay") {
				t.Fatal("refusal leaked input or suggested inappropriate recovery")
			}
			page, err := a.DB().ListDrafts(t.Context(), dir, true, 20, "")
			if err != nil || len(page.Items) != 0 {
				t.Fatal("rejected quote committed a draft")
			}
			// The same fake owner accepts a coherent quote and returns the frozen projection.
			stdout, stderr, err = run("create", "--to", localReadPN, "--message", strings.Repeat("A", 541), "--reply-to", "m1")
			if err != nil || stderr != "" || calls.Load() != 2 {
				t.Fatal("valid quoted draft failed through handler")
			}
			success := decodeAgentTest(t, stdout)
			var dto draftDTO
			if err := json.Unmarshal(success.Data, &dto); err != nil {
				t.Fatal(err)
			}
			if dto.Reply == nil || dto.Reply.ID != "m1" || dto.Reply.Text != "fixture message" || dto.Hash == "" || dto.RevisionID == "" || utf8.RuneCountInString(dto.Text.Text) != 512 {
				t.Fatal("valid frozen quote/projection changed")
			}
			if success.Meta.Recovery != "See data.recovery." || strings.Count(stdout, dto.Recovery) != 1 || strings.Contains(dto.Recovery, "document bytes") || strings.Count(stdout, "\n") != 1 {
				t.Fatal("handler success lost canonical minified guidance")
			}
		})
	}
}
