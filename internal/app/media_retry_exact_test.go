package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	"google.golang.org/protobuf/proto"
)

const exactFixtureChat = "15550000001@s.whatsapp.net"

var exactFixtureBytes = []byte("synthetic exact recovery plaintext")

func exactRetryFixture(t *testing.T) (*App, *fakeWA, RetryMediaExactOptions) {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	f.onMediaRetry = notOnPhoneHook
	insertExactMedia(t, a, exactFixtureChat, "selected")
	return a, f, RetryMediaExactOptions{
		ChatJID: exactFixtureChat, MsgID: "selected", Wait: time.Second,
		Output: mediaArtifactFixture(t, "output"),
		DownloadBytes: func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error) {
			return bytes.Clone(exactFixtureBytes), nil
		},
	}
}

func insertExactMedia(t *testing.T, a *App, chat, id string) {
	t.Helper()
	if err := a.db.UpsertChat(chat, "dm", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(exactFixtureBytes)
	if err := a.db.UpsertMessage(store.UpsertMessageParams{
		ChatJID: chat, MsgID: id, Timestamp: time.Now(), SenderJID: exactFixtureChat,
		MediaType: "image", DirectPath: "/synthetic", MediaKey: bytes.Repeat([]byte{7}, 32),
		FileSHA256: sum[:], FileEncSHA256: bytes.Repeat([]byte{9}, 32),
	}); err != nil {
		t.Fatal(err)
	}
}

func exactRetryContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func assertExactFailure(t *testing.T, result MediaRetryExactResult, err error, code, publication string, recorded bool) {
	t.Helper()
	var typed *MediaRetryExactError
	if !errors.As(err, &typed) || typed.Code != code || err.Error() != code || result.FilePublication != publication || result.Recorded != recorded {
		t.Fatalf("result=%+v error=%v; want %s/%s/%t", result, err, code, publication, recorded)
	}
}

func assertExactOutput(t *testing.T, opts RetryMediaExactOptions) {
	t.Helper()
	got, err := os.ReadFile(opts.Output.Path)
	if err != nil || !bytes.Equal(got, exactFixtureBytes) {
		t.Fatalf("output=%q error=%v", got, err)
	}
}

func exactSuccessEvent(t *testing.T, info *types.MessageInfo, mediaKey []byte) *events.MediaRetry {
	t.Helper()
	data, err := proto.Marshal(&waMmsRetry.MediaRetryNotification{
		StanzaID: proto.String(info.ID), DirectPath: proto.String("/reuploaded"), Result: waMmsRetry.MediaRetryNotification_SUCCESS.Enum(),
	})
	if err != nil {
		t.Fatal(err)
	}
	iv := bytes.Repeat([]byte{5}, 12)
	key := hkdfutil.SHA256(mediaKey, nil, []byte("WhatsApp Media Retry Notification"), 32)
	ciphertext, err := gcmutil.Encrypt(key, iv, data, []byte(info.ID))
	if err != nil {
		t.Fatal(err)
	}
	return &events.MediaRetry{ChatID: info.Chat, MessageID: info.ID, IV: iv, Ciphertext: ciphertext}
}

func TestRetryMediaExactIgnoresBulkEligibilityAndOtherChat(t *testing.T) {
	for _, marker := range []string{"stale_cache", "old_unavailable", "neither"} {
		t.Run(marker, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			insertExactMedia(t, a, "15550000002@s.whatsapp.net", opts.MsgID)
			for i := 0; i < 20; i++ {
				insertExactMedia(t, a, exactFixtureChat, "other"+strings.Repeat("x", i))
			}
			switch marker {
			case "stale_cache":
				cache := mediaArtifactFixture(t, "missing")
				opts.Cache = &cache
				if err := a.db.MarkMediaDownloaded(opts.ChatJID, opts.MsgID, cache.Path, time.Now()); err != nil {
					t.Fatal(err)
				}
			case "old_unavailable":
				if err := a.db.MarkMediaUnavailable(context.Background(), opts.ChatJID, opts.MsgID, time.Unix(100, 0)); err != nil {
					t.Fatal(err)
				}
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if err != nil || res.Status != "downloaded" || !res.Recorded || res.RecordedAt == nil || res.CurrentAvailability != "unknown" || res.FilePublication != "written" || len(f.mediaRetryReceipts) != 1 {
				t.Fatalf("result=%+v error=%v receipts=%v", res, err, f.mediaRetryReceipts)
			}
			assertExactOutput(t, opts)
			selected, err := a.db.GetMediaDownloadInfo(opts.ChatJID, opts.MsgID)
			if err != nil || selected.LocalPath != opts.Output.Path || selected.DownloadedAt.IsZero() || !selected.MediaUnavailableAt.IsZero() {
				t.Fatalf("selected=%+v error=%v", selected, err)
			}
			other, err := a.db.GetMediaDownloadInfo("15550000002@s.whatsapp.net", opts.MsgID)
			if err != nil || other.LocalPath != "" || !other.MediaUnavailableAt.IsZero() {
				t.Fatalf("other chat affected: %+v %v", other, err)
			}
		})
	}
}

func TestRetryMediaExactRejectsTombstonesAndInvalidMetadataWithoutEffects(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"deleted", "UPDATE messages SET deleted_at=10", "media_deleted"},
		{"deleted_zero", "UPDATE messages SET deleted_at=0", "media_deleted"},
		{"deleted_negative", "UPDATE messages SET deleted_at=-1", "media_deleted"},
		{"length_negative", "UPDATE messages SET file_length=-1", "media_metadata_incomplete"},
		{"no_hash", "UPDATE messages SET file_sha256=NULL", "media_binding_unverified"},
		{"short_hash", "UPDATE messages SET file_sha256=X'01'", "media_binding_unverified"},
		{"short_key", "UPDATE messages SET media_key=X'01'", "media_metadata_incomplete"},
		{"no_media", "UPDATE messages SET media_type=NULL", "no_media"},
		{"oversized", "UPDATE messages SET file_length=104857601", "media_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			evidenceFixtureSQL(t, a, tc.sql)
			downloads := 0
			opts.DownloadBytes = func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error) {
				downloads++
				return nil, nil
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			assertExactFailure(t, res, err, tc.code, "not_written", false)
			if len(f.mediaRetryReceipts) != 0 || downloads != 0 {
				t.Fatalf("effects: receipts=%v downloads=%d", f.mediaRetryReceipts, downloads)
			}
			if _, err := os.Stat(opts.Output.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected output: %v", err)
			}
		})
	}
}

func TestRetryMediaExactVerifiedLocalArtifactsAvoidProtocolWithoutKey(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache", true: "destination"}[existing], func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			a.wa = nil // Verified local bytes require no WA client or handshake.
			opts.Connect = func(context.Context) error { t.Fatal("connection for verified local bytes"); return nil }
			evidenceFixtureSQL(t, a, "UPDATE messages SET media_key=NULL,direct_path=NULL,file_length=0")
			loc := opts.Output
			status := "existing"
			publication := "not_written"
			if !existing {
				loc = mediaArtifactFixture(t, "cache")
				opts.Cache = &loc
				status, publication = "cached", "written"
			}
			if err := os.WriteFile(loc.Path, exactFixtureBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if err != nil || res.Status != status || res.FilePublication != publication || res.Recorded == existing || len(f.mediaRetryReceipts) != 0 || res.Observation.Phone != "not_requested" || res.Output.Verification != "sha256_verified" {
				t.Fatalf("result=%+v error=%v receipts=%v", res, err, f.mediaRetryReceipts)
			}
			assertExactOutput(t, opts)
		})
	}
}

func TestRetryMediaExactCDNEvidenceAndHonestUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name              string
		err               error
		status, code, cdn string
		recorded          bool
	}{
		{"fallback_valid", nil, "downloaded", "", "downloaded", true},
		{"expired403", whatsmeow.ErrMediaDownloadFailedWith403, "unavailable", "", "expired", true},
		{"expired404", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 404}}, "unavailable", "", "expired", true},
		{"expired410", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 410}}, "unavailable", "", "expired", true},
		{"transient", errors.New("https://private.example?key=PRIVATE SQL secret"), "unknown", "download_failed", "unknown", false},
		{"server500", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, "unknown", "download_failed", "unknown", false},
		{"integrity", whatsmeow.ErrInvalidMediaSHA256, "unknown", "integrity_failed", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			opts.DownloadBytes = func(_ context.Context, info store.MediaDownloadInfo, path string, reuploaded bool) ([]byte, error) {
				if reuploaded || path != info.DirectPath || len(info.FileEncSHA256) != 32 {
					t.Fatal("fallback lost original path/ciphertext hash")
				}
				return exactFixtureBytes, tc.err
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if tc.code != "" {
				assertExactFailure(t, res, err, tc.code, "not_written", false)
			} else if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.status || res.Recorded != tc.recorded || res.Observation.Phone != "not_found" || res.Observation.CDN != tc.cdn || res.Observation.ObservedAt == nil || res.CurrentAvailability != "unknown" || len(f.mediaRetryReceipts) != 1 {
				t.Fatalf("result=%+v error=%v", res, err)
			}
			info, err := a.db.GetMediaDownloadInfo(opts.ChatJID, opts.MsgID)
			if err != nil || info.MediaUnavailableAt.IsZero() == (tc.status == "unavailable") {
				t.Fatalf("unavailable marker=%v error=%v", info.MediaUnavailableAt, err)
			}
		})
	}
}

func TestRetryMediaExactCiphertextHashOmissionRequiresAuthenticatedReupload(t *testing.T) {
	for _, reuploaded := range []bool{false, true} {
		for _, hash := range []string{"NULL", "X'01'"} {
			t.Run(map[bool]string{false: "fallback", true: "reupload"}[reuploaded]+"/"+hash, func(t *testing.T) {
				a, f, opts := exactRetryFixture(t)
				evidenceFixtureSQL(t, a, "UPDATE messages SET file_enc_sha256="+hash)
				if reuploaded {
					f.onMediaRetry = func(info *types.MessageInfo, key []byte) any { return exactSuccessEvent(t, info, key) }
				}
				downloads := 0
				opts.DownloadBytes = func(_ context.Context, _ store.MediaDownloadInfo, _ string, authenticated bool) ([]byte, error) {
					downloads++
					if !authenticated {
						t.Fatal("fallback omitted its ciphertext binding")
					}
					return exactFixtureBytes, nil
				}
				result, err := a.RetryMediaExact(exactRetryContext(t), opts)
				if reuploaded {
					if err != nil || downloads != 1 || result.Status != "downloaded" {
						t.Fatalf("%+v %v downloads=%d", result, err, downloads)
					}
				} else {
					assertExactFailure(t, result, err, "media_metadata_incomplete", "not_written", false)
					if downloads != 0 || result.Observation.CDN != "not_requested" || result.UnavailableAt != nil {
						t.Fatalf("fallback effects: %+v downloads=%d", result, downloads)
					}
					info, err := a.db.GetMediaDownloadInfo(opts.ChatJID, opts.MsgID)
					if err != nil || info.LocalPath != "" || !info.MediaUnavailableAt.IsZero() {
						t.Fatalf("fallback recorded unavailable: %+v %v", info, err)
					}
				}
				if len(f.mediaRetryReceipts) != 1 {
					t.Fatal("response authorized another receipt")
				}
			})
		}
	}
}

func TestRetryMediaExactAuthenticatedReuploadKeepsPlaintextIdentity(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "expired_reupload"}[expired], func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			f.onMediaRetry = func(info *types.MessageInfo, key []byte) any { return exactSuccessEvent(t, info, key) }
			opts.DownloadBytes = func(_ context.Context, info store.MediaDownloadInfo, path string, reuploaded bool) ([]byte, error) {
				if !reuploaded || path != "/reuploaded" || len(info.FileSHA256) != sha256.Size || len(info.MediaKey) != 32 {
					t.Fatal("lost authenticated reupload identity")
				}
				if expired {
					return nil, whatsmeow.ErrMediaDownloadFailedWith403
				}
				return exactFixtureBytes, nil
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if expired {
				assertExactFailure(t, res, err, "media_expired", "not_written", false)
				if res.Status != "unknown" || res.UnavailableAt != nil {
					t.Fatalf("expired CDN incorrectly proved unavailable: %+v", res)
				}
			} else if err != nil || res.Status != "downloaded" {
				t.Fatalf("result=%+v error=%v", res, err)
			}
			if len(f.mediaRetryReceipts) != 1 || res.Observation.Phone != "reuploaded" {
				t.Fatalf("result=%+v receipts=%v", res, f.mediaRetryReceipts)
			}
		})
	}
}

func TestRetryMediaExactMismatchingAndUnknownAliasReceiptsDoNotSelectOtherMessages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response types.JID
		mapping  string
		want     string
	}{
		{"other_chat_same_id", types.NewJID("15550000002", types.DefaultUserServer), "", "no_response"},
		{"known_lid", types.NewJID("9001", types.HiddenUserServer), exactFixtureChat, "unavailable"},
		{"unknown_lid", types.NewJID("9001", types.HiddenUserServer), "", "no_response"},
		{"contradictory_lid", types.NewJID("9001", types.HiddenUserServer), "15550000002@s.whatsapp.net", "no_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			if tc.mapping != "" {
				pn, _ := types.ParseJID(tc.mapping)
				f.lids[tc.response] = pn
			}
			f.onMediaRetry = func(info *types.MessageInfo, _ []byte) any {
				return &events.MediaRetry{MessageID: info.ID, ChatID: tc.response, Error: &events.MediaRetryError{Code: 2}}
			}
			opts.DownloadBytes = func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error) {
				return nil, whatsmeow.ErrMediaDownloadFailedWith403
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			attempts := 2
			if tc.want == "unavailable" {
				attempts = 1
			}
			if err != nil || res.Status != tc.want || len(f.mediaRetryReceipts) != attempts {
				t.Fatalf("result=%+v error=%v receipts=%v", res, err, f.mediaRetryReceipts)
			}
			if tc.want == "no_response" && (res.Recorded || res.UnavailableAt != nil || res.Observation.CDN != "not_requested") {
				t.Fatalf("silence proved availability: %+v", res)
			}
		})
	}
}

func TestRetryMediaExactGroupAndBroadcastAddressing(t *testing.T) {
	for _, tc := range []struct {
		chat      string
		mode      types.AddressingMode
		fromMe    bool
		sender    string
		wantError bool
	}{
		{"123@g.us", types.AddressingModeLID, false, "9001@lid", false},
		{"123@g.us", types.AddressingModePN, true, "", false},
		{"123@broadcast", types.AddressingModePN, true, "", false},
		{"123@broadcast", types.AddressingModePN, false, "", true},
	} {
		t.Run(tc.chat+string(tc.mode)+tc.sender+map[bool]string{true: "outgoing", false: "incoming"}[tc.fromMe], func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			insertExactMedia(t, a, tc.chat, opts.MsgID)
			opts.ChatJID = tc.chat
			evidenceFixtureSQL(t, a, "UPDATE messages SET from_me="+map[bool]string{true: "1", false: "0"}[tc.fromMe]+",sender_jid='"+tc.sender+"' WHERE chat_jid='"+tc.chat+"'")
			jid, _ := types.ParseJID(tc.chat)
			f.groups[jid] = &types.GroupInfo{JID: jid, AddressingMode: tc.mode}
			f.onMediaRetry = func(info *types.MessageInfo, _ []byte) any {
				wantSender := tc.sender
				if tc.fromMe {
					wantSender = f.LinkedJID()
				}
				if !info.IsGroup || info.Sender.String() != wantSender || info.AddressingMode != tc.mode || info.Chat.String() != tc.chat {
					t.Fatalf("receipt source=%+v", info.MessageSource)
				}
				return notOnPhoneHook(info, nil)
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if tc.wantError {
				assertExactFailure(t, res, err, "retry_failed", "not_written", false)
				if len(f.mediaRetryReceipts) != 0 {
					t.Fatal("sent unaddressable receipt")
				}
			} else if err != nil || res.Status != "downloaded" {
				t.Fatalf("result=%+v error=%v", res, err)
			}
		})
	}
}

func TestRetryMediaExactBoundsReadOnlyAndMissingSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wait       time.Duration
		timeout    time.Duration
		noDeadline bool
		readonly   bool
		env        string
		id         string
		code       string
	}{
		{"wait_negative", -time.Second, time.Second, false, false, "", "selected", "invalid_arguments"},
		{"wait_short", time.Millisecond, time.Second, false, false, "", "selected", "invalid_arguments"},
		{"wait_long", 121 * time.Second, time.Second, false, false, "", "selected", "invalid_arguments"},
		{"unbounded", time.Second, 0, true, false, "", "selected", "invalid_arguments"},
		{"timeout_long", time.Second, 6 * time.Minute, false, false, "", "selected", "invalid_arguments"},
		{"expired", time.Second, -time.Second, false, false, "", "selected", "cancelled"},
		{"readonly", time.Second, time.Second, false, true, "", "selected", "read_only"},
		{"readonly_env", time.Second, time.Second, false, false, "1", "selected", "read_only"},
		{"missing", time.Second, time.Second, false, false, "", "absent", "not_found"},
		{"blank", time.Second, time.Second, false, false, "", "", "invalid_arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			t.Setenv("WACLI_READONLY", tc.env)
			a.opts.ReadOnly = tc.readonly
			opts.Wait, opts.MsgID = tc.wait, tc.id
			checks := 0
			opts.Output.Check = func() error { checks++; return nil }
			ctx := context.Background()
			if !tc.noDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			res, err := a.RetryMediaExact(ctx, opts)
			assertExactFailure(t, res, err, tc.code, "not_written", false)
			if checks != 0 || len(f.mediaRetryReceipts) != 0 {
				t.Fatalf("early rejection had effects: %d %v", checks, f.mediaRetryReceipts)
			}
		})
	}
}

func TestRetryMediaExactCancellationAndOldCallbackAreOperationScoped(t *testing.T) {
	a, f, opts := exactRetryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var oldCallback func(any)
	f.onMediaRetry = func(info *types.MessageInfo, _ []byte) any {
		f.mu.Lock()
		for _, callback := range f.handlers {
			oldCallback = callback
		}
		f.mu.Unlock()
		cancel()
		return notOnPhoneHook(info, nil)
	}
	res, err := a.RetryMediaExact(ctx, opts)
	assertExactFailure(t, res, err, "cancelled", "not_written", false)
	if len(f.mediaRetryReceipts) != 1 || oldCallback == nil || len(f.handlers) != 0 || res.Observation.Phone != "unknown" {
		t.Fatal("cancelled operation retained handler or sent second attempt")
	}
	f.onMediaRetry = func(info *types.MessageInfo, key []byte) any {
		oldCallback(notOnPhoneHook(info, key))
		return exactSuccessEvent(t, info, key)
	}
	res, err = a.RetryMediaExact(exactRetryContext(t), opts)
	if err != nil || res.Observation.Phone != "reuploaded" || res.Status != "downloaded" || len(f.mediaRetryReceipts) != 2 {
		t.Fatalf("old callback contaminated new operation: %+v %v", res, err)
	}
}

func TestRetryMediaExactPublicationAndPersistenceEffectsRemainIndependent(t *testing.T) {
	for _, tc := range []struct{ name, code, publication string }{
		{"wrong_bytes", "integrity_failed", "not_written"},
		{"destination_conflict", "output_conflict", "not_written"},
		{"concurrent_destination", "output_conflict", "not_written"},
		{"changed_before_publish", "media_changed", "not_written"},
		{"changed_after_publish", "media_changed", "written"},
		{"db_after_publish", "store_failed", "written"},
		{"cancel_after_publish", "cancelled", "written"},
		{"path_check_after_publish", "media_changed", "written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var modified bool
			opts.Output.Check = func() error {
				_, err := os.Stat(opts.Output.Path)
				if err != nil || modified {
					return nil
				}
				modified = true
				switch tc.name {
				case "changed_after_publish":
					evidenceFixtureSQL(t, a, "UPDATE messages SET deleted_at=0")
				case "cancel_after_publish":
					cancel()
				case "path_check_after_publish":
					return errors.New("private path cause")
				}
				return nil
			}
			if tc.name == "destination_conflict" {
				if err := os.WriteFile(opts.Output.Path, []byte("existing other bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "db_after_publish" {
				evidenceFixtureSQL(t, a, "CREATE TRIGGER fixture_mark_failure BEFORE UPDATE OF local_path ON messages BEGIN SELECT RAISE(ABORT,'private SQL cause'); END")
			}
			opts.DownloadBytes = func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error) {
				switch tc.name {
				case "wrong_bytes":
					return []byte("invalid bytes"), nil
				case "changed_before_publish":
					evidenceFixtureSQL(t, a, "UPDATE messages SET file_sha256=X'01'")
				case "concurrent_destination":
					if err := os.WriteFile(opts.Output.Path, []byte("concurrent bytes"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return exactFixtureBytes, nil
			}
			res, err := a.RetryMediaExact(ctx, opts)
			assertExactFailure(t, res, err, tc.code, tc.publication, false)
			if tc.publication == "written" {
				assertExactOutput(t, opts)
				if res.Output.Path == nil || res.Output.SHA256 == "" {
					t.Fatal("published evidence lost")
				}
			} else if tc.name == "destination_conflict" || tc.name == "concurrent_destination" {
				got, _ := os.ReadFile(opts.Output.Path)
				if bytes.Equal(got, exactFixtureBytes) {
					t.Fatal("existing destination clobbered")
				}
			}
			if tc.name == "destination_conflict" && len(f.mediaRetryReceipts) != 0 {
				t.Fatal("protocol ran for conflicting destination")
			}
			info, readErr := a.db.GetMediaDownloadInfo(opts.ChatJID, opts.MsgID)
			if readErr != nil || info.LocalPath != "" || !info.DownloadedAt.IsZero() {
				t.Fatalf("failure recorded download: %+v %v", info, readErr)
			}
		})
	}
}

func TestRetryMediaExactRetriesOnlyNonRespondersAndRechecksSelection(t *testing.T) {
	for _, tc := range []struct {
		name, code, status string
		attempts           int
	}{
		{"second_response", "", "downloaded", 2},
		{"wrong_message", "", "no_response", 2},
		{"changed_before_second", "media_changed", "unknown", 1},
		{"phone_error", "retry_failed", "unknown", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			calls := 0
			f.onMediaRetry = func(info *types.MessageInfo, key []byte) any {
				calls++
				switch tc.name {
				case "second_response":
					if calls == 2 {
						return exactSuccessEvent(t, info, key)
					}
				case "wrong_message":
					return &events.MediaRetry{ChatID: info.Chat, MessageID: "unselected", Error: &events.MediaRetryError{Code: 2}}
				case "changed_before_second":
					evidenceFixtureSQL(t, a, "UPDATE messages SET deleted_at=0")
				case "phone_error":
					return &events.MediaRetry{ChatID: info.Chat, MessageID: info.ID, Error: &events.MediaRetryError{Code: 9}}
				}
				return nil
			}
			res, err := a.RetryMediaExact(exactRetryContext(t), opts)
			if tc.code != "" {
				assertExactFailure(t, res, err, tc.code, "not_written", false)
			} else if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.status || len(f.mediaRetryReceipts) != tc.attempts {
				t.Fatalf("result=%+v error=%v receipts=%v", res, err, f.mediaRetryReceipts)
			}
		})
	}
}

func TestRetryMediaExactDeadlineStopsBeforeSecondReceipt(t *testing.T) {
	a, f, opts := exactRetryFixture(t)
	f.onMediaRetry = nil
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	res, err := a.RetryMediaExact(ctx, opts)
	assertExactFailure(t, res, err, "cancelled", "not_written", false)
	if len(f.mediaRetryReceipts) != 1 || len(f.handlers) != 0 {
		t.Fatalf("deadline receipts=%v handlers=%d", f.mediaRetryReceipts, len(f.handlers))
	}
}

func TestRetryMediaExactAcceptsDefaultAndMaximumWait(t *testing.T) {
	for _, wait := range []time.Duration{0, 120 * time.Second} {
		a, _, opts := exactRetryFixture(t)
		opts.Wait = wait
		res, err := a.RetryMediaExact(exactRetryContext(t), opts)
		if err != nil || res.Status != "downloaded" {
			t.Fatalf("wait=%v result=%+v error=%v", wait, res, err)
		}
	}
}

func TestRetryMediaExactExplicitStoredLIDDoesNotNeedAnInferredAlias(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		a, f, opts := exactRetryFixture(t)
		lid := types.NewJID("9001", types.HiddenUserServer)
		insertExactMedia(t, a, lid.String(), opts.MsgID)
		opts.ChatJID = lid.String()
		if mapped {
			pn, _ := types.ParseJID(exactFixtureChat)
			f.lids[lid] = pn
		}
		f.onMediaRetry = func(info *types.MessageInfo, key []byte) any {
			if info.Chat != lid {
				t.Fatalf("selection recipient broadened: %s", info.Chat)
			}
			return notOnPhoneHook(info, key)
		}
		res, err := a.RetryMediaExact(exactRetryContext(t), opts)
		if err != nil || res.Status != "downloaded" || len(f.mediaRetryReceipts) != 1 {
			t.Fatalf("mapped=%t result=%+v error=%v", mapped, res, err)
		}
	}
}

func TestRetryMediaExactLazyConnectionIsBoundedAndRechecksBinding(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"success", ""}, {"changed", "media_changed"}, {"deleted", "media_changed"},
		{"failed", "connect_failed"}, {"unauthenticated", "not_authenticated"}, {"cancelled", "cancelled"},
		{"missing_client", "not_connected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, opts := exactRetryFixture(t)
			a.wa = nil
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			connections := 0
			if tc.name != "missing_client" {
				opts.Connect = func(context.Context) error {
					connections++
					switch tc.name {
					case "failed":
						return errors.New("secret URL connect cause")
					case "unauthenticated":
						return &MediaRetryExactError{Code: "not_authenticated"}
					case "cancelled":
						cancel()
						return ctx.Err()
					case "changed":
						evidenceFixtureSQL(t, a, "UPDATE messages SET file_sha256=X'01'")
					case "deleted":
						evidenceFixtureSQL(t, a, "DELETE FROM messages")
					}
					a.wa = f
					return nil
				}
			}
			result, err := a.RetryMediaExact(ctx, opts)
			if tc.code != "" {
				assertExactFailure(t, result, err, tc.code, "not_written", false)
				if len(f.mediaRetryReceipts) != 0 {
					t.Fatal("receipt after failed connection/binding")
				}
			} else if err != nil || result.Status != "downloaded" {
				t.Fatalf("%+v %v", result, err)
			}
			if connections > 1 {
				t.Fatal("connection operation repeated")
			}
		})
	}
}
