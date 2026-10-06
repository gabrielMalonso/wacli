package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
)

func voiceAdviceRequest() sendDelegateRequest {
	return sendDelegateRequest{Version: 1, Kind: draftWriteKind, Draft: &app.DraftWriteRequest{Version: 3, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/synthetic", Input: &app.DraftInput{To: localReadPN, Voice: &app.DraftVoiceInput{Path: "UNOPENED"}}}}
}
func TestDraftVoiceAdviceParityUnknownAndScope(t *testing.T) {
	text := "text"
	fields := []draftValidationField{draftVoiceInvalidField, draftVoiceProfileField, draftVoiceQuotaField, draftVoiceCanceledField}
	for _, field := range fields {
		t.Run(string(field), func(t *testing.T) {
			req := voiceAdviceRequest()
			validation := &store.DraftValidationError{Field: string(field), Reason: "PRIVATE_PATH_AND_CONTENT"}
			standalone := classifyDraftError(validation)
			resp := draftRefusal(req, validation)
			raw, err := json.Marshal(resp)
			if err != nil || strings.Contains(string(raw), "PRIVATE_PATH_AND_CONTENT") {
				t.Fatal("private advice", err)
			}
			got := draftIPCFailure(req, resp)
			var owner *out.AgentError
			if !errors.As(got, &owner) || owner.Message != standalone.Message || owner.Recovery != standalone.Recovery || owner.Code != "invalid_arguments" || owner.ExitCode != 2 || owner.Draft == nil {
				t.Fatal("standalone/owner parity", got)
			}
			for _, unknown := range []draftValidationField{"", "voice.future"} {
				missing := resp
				missing.DraftValidationField = unknown
				got := draftIPCFailure(req, missing)
				var base *store.DraftError
				if !errors.As(got, &base) || base.Code != "invalid_arguments" {
					t.Fatal("optional advice changed base certainty", got)
				}
			}
			for _, change := range []func(*sendDelegateResponse){func(v *sendDelegateResponse) { v.DraftRequestHash = strings.Repeat("0", 64) }, func(v *sendDelegateResponse) {
				v.DraftFailure = &store.DraftError{Code: "invalid_arguments", DraftID: strings.Repeat("c", 32), RevisionID: req.Draft.RevisionID}
			}, func(v *sendDelegateResponse) {
				v.DraftFailure = &store.DraftError{Code: "invalid_arguments", DraftID: req.Draft.DraftID, RevisionID: strings.Repeat("c", 32)}
			}} {
				mismatched := resp
				change(&mismatched)
				got := draftIPCFailure(req, mismatched)
				var failure *store.DraftError
				if !errors.As(got, &failure) || failure.Code != "local_write_uncertain" {
					t.Fatal("mismatch advice", got)
				}
			}
			for _, change := range []func(*sendDelegateRequest){func(r *sendDelegateRequest) { r.Kind = "voice" }, func(r *sendDelegateRequest) { r.File = "UNOPENED" }, func(r *sendDelegateRequest) { r.Version = 2 }, func(r *sendDelegateRequest) { r.Draft.Version = 1 }, func(r *sendDelegateRequest) { r.Draft.Version = 2 }, func(r *sendDelegateRequest) { r.Draft.Action = "discard" }, func(r *sendDelegateRequest) {
				r.Draft.Input = &app.DraftInput{To: localReadPN, Message: &text}
			}, func(r *sendDelegateRequest) {
				r.Draft.Input = &app.DraftInput{To: localReadPN, Image: &app.DraftImageInput{Path: "UNOPENED"}}
			}, func(r *sendDelegateRequest) { r.Draft.Input.Voice.Path = "" }} {
				wrong := voiceAdviceRequest()
				change(&wrong)
				response := draftRefusal(wrong, validation)
				if response.DraftValidationField != "" {
					t.Fatal("advice on wrong selection")
				}
				response.DraftValidationField = field
				got := draftIPCFailure(wrong, response)
				var base *store.DraftError
				if !errors.As(got, &base) || base.Code != "invalid_arguments" {
					t.Fatal("advice applied on wrong selection", got)
				}
			}
		})
	}
	req := voiceAdviceRequest()
	for _, err := range []error{context.Canceled, store.DraftFailure("local_write_uncertain", req.Draft.DraftID, req.Draft.RevisionID, "", context.Canceled), store.DraftFailure("local_write_not_dispatched", req.Draft.DraftID, req.Draft.RevisionID, "", context.DeadlineExceeded)} {
		response := draftRefusal(req, err)
		if response.DraftValidationField != "" {
			t.Fatal("generic cancellation converted")
		}
		response.DraftValidationField = draftVoiceCanceledField
		got := draftIPCFailure(req, response)
		var base *store.DraftError
		if !errors.As(got, &base) || base.Code != response.DraftFailure.Code {
			t.Fatal("cancel certainty changed", got)
		}
	}
}
