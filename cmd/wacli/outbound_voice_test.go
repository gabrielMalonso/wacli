package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type voiceOwnerWA struct {
	outboundOwnerWA
	uploads    int
	voiceBytes []byte
}

func (f *voiceOwnerWA) Upload(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.voiceBytes = bytes.Clone(data)
	if kind != whatsmeow.MediaAudio {
		return whatsmeow.UploadResponse{}, errors.New("fixture rejects other uploads")
	}
	digest := sha256.Sum256(data)
	return whatsmeow.UploadResponse{URL: "https://synthetic.invalid/voice", DirectPath: "/fixture/voice", MediaKey: make([]byte, 32), FileEncSHA256: make([]byte, 32), FileSHA256: digest[:], FileLength: uint64(len(data))}, nil
}
func exerciseOutboundVoiceOwner(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	f := &voiceOwnerWA{}
	dir, a := draftOwnerFixtureOptions(t, app.Options{WAFactory: func(wa.Options) (app.WAClient, error) { f.opens.Add(1); return f, nil }})
	path := draftCLIVoiceFile(t, dir)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	stop, err := startSendDelegateServerForStore(t.Context(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	stdout, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "create", "--to", localReadLID, "--voice", path}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	var draft draftDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &draft); err != nil {
		t.Fatal(err)
	}
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		entry, err := a.DB().Outbound().Read(ctx, "", "voice-owner-key", f.LinkedJID(), 20, "")
		voice := msg.GetAudioMessage()
		if err != nil || entry.Operation.Phase != store.OutboundDispatchPossible || entry.Operation.MessageID != id || to.String() != localReadLID || voice == nil || msg.DocumentMessage != nil || !voice.GetPTT() || voice.Seconds != nil || voice.Waveform != nil || voice.GetFileLength() != uint64(len(original)) {
			t.Error("voice dispatch/checkpoint", err)
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	args := []string{"--agent", "--store", dir, "outbound", "send", draft.ID, "--revision", draft.RevisionID, "--expect-hash", draft.Hash, "--key", "voice-owner-key"}
	if err := os.WriteFile(path, []byte("source changed after review"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runDraftBinary(t, binary, args, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "waveform\"") || strings.Contains(stdout, "PRIVATE_VOICE_SOURCE") {
		t.Fatal("public output", stdout)
	}
	var result outboundSendDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation.Kind != store.DraftVoiceKind || result.KnownResult != store.OutboundAccepted || result.KnownACK == nil || result.HistoryWarning {
		t.Fatal(result)
	}
	f.mu.Lock()
	equal := bytes.Equal(original, f.voiceBytes)
	uploads := f.uploads
	f.mu.Unlock()
	if !equal || uploads != 1 || f.sends.Load() != 1 {
		t.Fatal("original voice bytes", uploads, f.sends.Load())
	}
	row, err := a.DB().GetMessage(localReadPN, outboundOwnerMessageID)
	if err != nil || row.MediaType != "audio" || row.MediaCaption != "" || row.Text != "" {
		t.Fatal("invented voice content", row, err)
	}
	_, stderr, err = runDraftBinary(t, binary, args, false)
	if err != nil || f.sends.Load() != 1 || f.ids.Load() != 1 {
		t.Fatal("duplicate replay", stderr, err)
	}
	// A matched voice echo is retained separately and cannot fabricate read/delivery.
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	chat, _ := types.ParseJID(localReadLID)
	actor, _ := types.ParseJID(f.LinkedLID())
	for _, ptt := range []*bool{nil, proto.Bool(false)} {
		handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: outboundOwnerMessageID}, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: ptt}}})
	}
	for _, source := range []types.MessageSource{
		{Chat: chat, Sender: types.NewJID("100000000002", types.HiddenUserServer), IsFromMe: true},
		{Chat: types.NewJID("100000000003", types.HiddenUserServer), Sender: actor, IsFromMe: true},
	} {
		handler(&events.Message{Info: types.MessageInfo{MessageSource: source, ID: outboundOwnerMessageID}, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}})
	}
	before, err := a.DB().Outbound().Read(t.Context(), "", "voice-owner-key", f.LinkedJID(), 20, "")
	if err != nil || before.Evidence.OwnEcho {
		t.Fatal("false/nil PTT or uncorrelated echo", before, err)
	}
	handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: outboundOwnerMessageID, Timestamp: time.Now().UTC()}, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}})
	handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: outboundOwnerMessageID}, SourceWebMsg: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(outboundOwnerMessageID), RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(true)}, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}}})
	for _, kind := range []types.ReceiptType{types.ReceiptTypePlayed, "unknown-fixture"} {
		handler(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("100000000002", types.HiddenUserServer)}, MessageIDs: []string{outboundOwnerMessageID}, Type: kind, Timestamp: time.Now().UTC()})
	}
	entry, err := a.DB().Outbound().Read(t.Context(), "", "voice-owner-key", f.LinkedJID(), 20, "")
	if err != nil || !entry.Evidence.OwnEcho || len(entry.Observations.Items) != 3 || entry.Evidence.Delivered != "unknown" || entry.Evidence.Read != "unknown" {
		t.Fatal(entry, err)
	}
}
func TestOutboundVoiceOwnerCLI(t *testing.T) { exerciseOutboundVoiceOwner(t, "") }
func TestOutboundVoiceProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_OUTBOUND_E2E_BINARY to freshly built binary")
	}
	exerciseOutboundVoiceOwner(t, binary)
}
