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

type imageOwnerWA struct {
	outboundOwnerWA
	uploads    int
	imageBytes []byte
}

func (f *imageOwnerWA) Upload(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	f.imageBytes = bytes.Clone(data)
	if kind != whatsmeow.MediaImage {
		return whatsmeow.UploadResponse{}, errors.New("fixture rejects other uploads")
	}
	digest := sha256.Sum256(data)
	return whatsmeow.UploadResponse{URL: "https://synthetic.invalid/image", DirectPath: "/fixture/image", MediaKey: make([]byte, 32), FileEncSHA256: make([]byte, 32), FileSHA256: digest[:], FileLength: uint64(len(data))}, nil
}
func exerciseOutboundImageOwner(t *testing.T, binary string) {
	t.Helper()
	t.Setenv("WACLI_READONLY", "0")
	f := &imageOwnerWA{}
	dir, a := draftOwnerFixtureOptions(t, app.Options{WAFactory: func(wa.Options) (app.WAClient, error) { f.opens.Add(1); return f, nil }})
	path := draftCLIImageFile(t, dir)
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
	stdout, stderr, err := runDraftBinary(t, binary, []string{"--agent", "--store", dir, "draft", "create", "--to", localReadLID, "--image", path, "--caption", " literal\\n\n🔷 "}, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	var draft draftDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &draft); err != nil {
		t.Fatal(err)
	}
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		entry, err := a.DB().Outbound().Read(ctx, "", "image-owner-key", f.LinkedJID(), 20, "")
		image := msg.GetImageMessage()
		if err != nil || entry.Operation.Phase != store.OutboundDispatchPossible || entry.Operation.MessageID != id || to.String() != localReadLID || image == nil || msg.DocumentMessage != nil || image.GetCaption() != draft.Image.Caption || image.GetWidth() != draft.Image.Width || image.GetFileLength() != uint64(len(original)) {
			t.Error("image dispatch/checkpoint", err)
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("100000000009", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	args := []string{"--agent", "--store", dir, "outbound", "send", draft.ID, "--revision", draft.RevisionID, "--expect-hash", draft.Hash, "--key", "image-owner-key"}
	if err := os.WriteFile(path, []byte("source changed after review"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runDraftBinary(t, binary, args, false)
	if err != nil {
		t.Fatal(stderr, err)
	}
	if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "jpeg_thumbnail") || strings.Contains(stdout, "PRIVATE_IMAGE_SOURCE") {
		t.Fatal("public output", stdout)
	}
	var result outboundSendDTO
	if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation.Kind != store.DraftImageKind || result.KnownResult != store.OutboundAccepted || result.KnownACK == nil || result.HistoryWarning {
		t.Fatal(result)
	}
	f.mu.Lock()
	equal := bytes.Equal(original, f.imageBytes)
	uploads := f.uploads
	f.mu.Unlock()
	if !equal || uploads != 1 || f.sends.Load() != 1 {
		t.Fatal("original image bytes", uploads, f.sends.Load())
	}
	rows, err := a.DB().SearchMessages(store.SearchMessagesParams{Query: "literal", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].MediaType != "image" || rows[0].Text != draft.Image.Caption {
		t.Fatal(rows, err)
	}
	_, stderr, err = runDraftBinary(t, binary, args, false)
	if err != nil || f.sends.Load() != 1 || f.ids.Load() != 1 {
		t.Fatal("duplicate replay", stderr, err)
	}
	// A matched image echo is retained separately and cannot fabricate read/delivery.
	f.mu.Lock()
	handler := f.handler
	f.mu.Unlock()
	chat, _ := types.ParseJID(localReadLID)
	actor, _ := types.ParseJID(f.LinkedLID())
	handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: outboundOwnerMessageID, Timestamp: time.Now().UTC()}, Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}})
	handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: actor, IsFromMe: true}, ID: outboundOwnerMessageID}, SourceWebMsg: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String(outboundOwnerMessageID), RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(true)}, Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}}})
	handler(&events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("100000000002", types.HiddenUserServer)}, MessageIDs: []string{outboundOwnerMessageID}, Type: types.ReceiptTypePlayed, Timestamp: time.Now().UTC()})
	entry, err := a.DB().Outbound().Read(t.Context(), "", "image-owner-key", f.LinkedJID(), 20, "")
	if err != nil || !entry.Evidence.OwnEcho || len(entry.Observations.Items) != 3 || entry.Evidence.Delivered != "unknown" || entry.Evidence.Read != "unknown" {
		t.Fatal(entry, err)
	}
}
func TestOutboundImageOwnerCLI(t *testing.T) { exerciseOutboundImageOwner(t, "") }
func TestOutboundImageProductionBinary(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_OUTBOUND_E2E_BINARY to freshly built binary")
	}
	exerciseOutboundImageOwner(t, binary)
}
