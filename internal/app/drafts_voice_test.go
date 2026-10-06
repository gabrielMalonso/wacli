package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func voiceFixtureBytes(t *testing.T) []byte {
	t.Helper()
	// A nonempty compressed frame with declared 20 ms; no entropy/quality claim.
	b, err := hex.DecodeString("4f676753000200000000000000001818181800000000c3c6153d01134f707573486561640101000080bb00000000004f67675300000000000000000000181818180100000085270dd201194f707573546167730900000073796e746865746963000000004f6767530004c0030000000000001818181802000000290c03cf0103f8fffe")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func voiceDraftInput(t *testing.T, a *App) (DraftInput, []byte) {
	t.Helper()
	b := voiceFixtureBytes(t)
	path := filepath.Join(a.StoreDir(), "PRIVATE_VOICE_SOURCE.ogg")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	in := DraftInput{To: "90002@lid", Voice: &DraftVoiceInput{Path: path}, ReplyTo: "voice-quoted"}
	if err := a.DB().UpsertChat(in.To, "dm", "fixture", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: in.To, MsgID: in.ReplyTo, SenderJID: in.To, Text: " frozen\n voice quote ", Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	return in, b
}
func TestDraftVoiceVersionReadonlyBeforeEffects(t *testing.T) {
	r := DraftWriteRequest{Version: 3, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/synthetic", Input: &DraftInput{To: "90002@lid", Voice: &DraftVoiceInput{Path: "UNOPENED"}}}
	for _, v := range []int{0, 1, 2, 4} {
		bad := r
		bad.Version = v
		var a *App
		if _, err := a.WriteLocalDraft(t.Context(), bad, func(string) (*os.File, error) { t.Fatal("source opened"); return nil, nil }); err == nil {
			t.Fatal(v)
		}
	}
	for _, mutate := range []func(*DraftInput){func(in *DraftInput) { in.File = "document" }, func(in *DraftInput) { in.Image = &DraftImageInput{Path: "image"} }, func(in *DraftInput) { in.Message = draftTextPointer("text") }, func(in *DraftInput) { in.Caption = "caption" }, func(in *DraftInput) { in.MIME = "audio/ogg" }, func(in *DraftInput) { in.Filename = "name" }, func(in *DraftInput) { in.Mentions = []string{"90002@lid"} }, func(in *DraftInput) { in.Voice = &DraftVoiceInput{} }} {
		in := *r.Input
		mutate(&in)
		bad := r
		bad.Input = &in
		var a *App
		if _, err := a.WriteLocalDraft(t.Context(), bad, nil); err == nil {
			t.Fatal("invalid union/options accepted")
		}
	}
	a := &App{opts: Options{ReadOnly: true}}
	_, err := a.WriteLocalDraft(t.Context(), r, func(string) (*os.File, error) { t.Fatal("readonly source"); return nil, nil })
	draftTestErrorCode(t, err, "read_only")
}
func TestDraftVoiceSnapshotQuoteCopiesRetentionAndNoMediaReads(t *testing.T) {
	a := draftAppFixture(t)
	in, b := voiceDraftInput(t, a)
	r := draftAppRequest(t, a, in)
	opens := 0
	e, err := a.WriteLocalDraft(t.Context(), r, func(path string) (*os.File, error) { opens++; return os.Open(path) })
	if err != nil || opens != 1 {
		t.Fatal(e, err, opens)
	}
	v := e.Revision.Payload().Data().Voice
	if v.MIME != "audio/ogg; codecs=opus" || v.Size != int64(len(b)) || v.EncodedSamples != 960 || v.PlayableSamples != 960 {
		t.Fatal(v)
	}
	copied := e.Revision.Payload().Data()
	copied.Voice.InputSampleRate++
	if copied.Voice.InputSampleRate == e.Revision.Payload().Data().Voice.InputSampleRate {
		t.Fatal("payload aliases")
	}
	path := filepath.Join(a.StoreDir(), e.Revision.Review().SnapshotPath)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatal("snapshot bytes", err)
	}
	if err := os.Rename(in.Voice.Path, in.Voice.Path+".retained"); err != nil {
		t.Fatal(err)
	}
	if got, err := readOutboundDocument(t.Context(), a.StoreDir(), e.Revision); err != nil || !bytes.Equal(got, b) {
		t.Fatal("source reread", err)
	}
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().ReadDraft(t.Context(), r.DraftID, r.RevisionID); err != nil {
		t.Fatal("show reads media", err)
	}
	if _, err := a.DB().ListDrafts(t.Context(), a.StoreDir(), false, 20, ""); err != nil {
		t.Fatal("list reads media", err)
	}
	// A new voice revision may retain the old missing snapshot metadata.
	if err := os.WriteFile(in.Voice.Path, b, 0600); err != nil {
		t.Fatal(err)
	}
	next := draftAppRequest(t, a, in)
	next.Action = "update"
	next.DraftID = r.DraftID
	next.ExpectedRevision = r.RevisionID
	update, err := a.WriteLocalDraft(t.Context(), next, os.Open)
	if err != nil {
		t.Fatal(err)
	}
	if update.Number != 2 {
		t.Fatal(update)
	}
	if _, err := a.DB().DiscardDraft(t.Context(), next.DraftID, next.RevisionID); err != nil {
		t.Fatal(err)
	}
	selection := store.DraftCleanupSelection{DraftID: next.DraftID, RevisionID: next.RevisionID, ExpectedHeadID: next.RevisionID, Hash: update.Revision.Payload().Hash()}
	_, err = a.DB().ReadDraftCleanup(t.Context(), selection)
	var protected *store.DraftCleanupError
	if !errors.As(err, &protected) || protected.Code != store.DraftCleanupProtected {
		t.Fatal("voice cleanup became eligible", err)
	}
	if _, err := os.Stat(filepath.Join(a.StoreDir(), update.Revision.Review().SnapshotPath)); err != nil {
		t.Fatal("discard removed voice", err)
	}
}
func TestDraftVoiceStructuralRefusalAndCancellationCertainty(t *testing.T) {
	a := draftAppFixture(t)
	in, _ := voiceDraftInput(t, a)
	r := draftAppRequest(t, a, in)
	if err := os.WriteFile(in.Voice.Path, []byte("not Ogg"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := a.WriteLocalDraft(t.Context(), r, os.Open)
	var validation *store.DraftValidationError
	if !errors.As(err, &validation) || validation.Field != "voice.invalid" {
		t.Fatal(err)
	}
	if _, err := a.DB().ReadDraftRecord(t.Context(), r.DraftID); err == nil {
		t.Fatal("invalid voice persisted")
	}
	files, err := os.ReadDir(filepath.Join(a.StoreDir(), store.DraftMediaDirectory))
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	for _, state := range []DraftPublication{DraftUnpublished, DraftPublished, DraftPublicationUnknown} {
		for _, stage := range []string{"source", "copy", "voice", "voice_payload", "file_sync", "publish", "directory_sync"} {
			failure := &DraftSnapshotError{RevisionID: r.RevisionID, Stage: stage, Publication: state, Cause: context.Canceled}
			got := voiceSnapshotValidation(failure)
			if (got != nil) != (state == DraftUnpublished && stage == "voice") {
				t.Fatal("cancel certainty", state, stage, got)
			}
		}
	}
	if voiceSnapshotValidation(context.Canceled) != nil {
		t.Fatal("generic cancellation promoted")
	}
	// A cancel/error after the publication attempt preserves artifacts and never
	// acquires structural refusal advice, even when the syscall result was lost.
	in, b := voiceDraftInput(t, a)
	opts := DraftSnapshotOptions{StoreDir: a.StoreDir(), RevisionID: r.RevisionID, SourcePath: in.Voice.Path, Voice: true, OpenSource: os.Open}
	_, err = createDraftSnapshot(t.Context(), opts, draftSnapshotIO{syncFile: func(*os.File) error { return nil }, syncDir: func(*os.Root) error { return nil }, publish: func(root *os.Root, from, to string) error {
		if err := root.Link(from, to); err != nil {
			return err
		}
		return context.Canceled
	}})
	var snapshot *DraftSnapshotError
	if !errors.As(err, &snapshot) || snapshot.Publication != DraftPublicationUnknown || voiceSnapshotValidation(err) != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(filepath.Join(a.StoreDir(), store.DraftMediaDirectory, r.RevisionID+".blob"))
	if err != nil || !bytes.Equal(retained, b) {
		t.Fatal("uncertain publication lost", err)
	}
}

type voicePublicationCancelContext struct {
	context.Context
	path   string
	cancel context.CancelFunc
}

func (c voicePublicationCancelContext) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}

func TestDraftVoiceCancelAfterPublicationIsUncertain(t *testing.T) {
	a := draftAppFixture(t)
	in, original := voiceDraftInput(t, a)
	r := draftAppRequest(t, a, in)
	path := filepath.Join(a.StoreDir(), store.DraftMediaDirectory, r.RevisionID+".blob")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := a.WriteLocalDraft(voicePublicationCancelContext{ctx, path, cancel}, r, os.Open)
	draftTestErrorCode(t, err, "local_write_uncertain")
	retained, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(retained, original) {
		t.Fatal("published bytes removed", readErr)
	}
	if _, err := a.DB().ReadDraftRecord(t.Context(), r.DraftID); err == nil {
		t.Fatal("revision unexpectedly persisted")
	}
}
func TestOutboundVoicePreflightMetadataHistoryAndCopies(t *testing.T) {
	for _, failure := range []string{"missing", "tampered", "symlink", "channels", "samples", "rate", "history"} {
		t.Run(failure, func(t *testing.T) {
			a, x, r, f, rev := outboundAppFixture(t, store.DraftVoiceKind)
			path := filepath.Join(a.StoreDir(), rev.Review().SnapshotPath)
			switch failure {
			case "missing":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				b := voiceFixtureBytes(t)
				b[len(b)-1] ^= 1
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			case "channels", "samples", "rate":
				data := rev.Payload().Data()
				switch failure {
				case "channels":
					data.Voice.Channels = 2
				case "samples":
					data.Voice.EncodedSamples += 120
				case "rate":
					data.Voice.InputSampleRate++
				}
				payload, err := store.NewDraftPayload(data)
				if err != nil {
					t.Fatal(err)
				}
				mismatch, err := store.NewDraftRevision(rev.DraftID(), rev.ID(), rev.CreatedAt(), payload, rev.Review())
				if err != nil {
					t.Fatal(err)
				}
				x.document = func(ctx context.Context, _ store.DraftRevision) ([]byte, error) {
					return readOutboundDocument(ctx, a.StoreDir(), mismatch)
				}
			case "history":
				x.history = func(store.DraftPayloadData, store.OutboundOperation, whatsmeow.SendResponse, *whatsmeow.UploadResponse) error {
					return errors.New("fixture history failure")
				}
			}
			if failure != "history" {
				x.connect = func(context.Context) error { t.Fatal("network before verification"); return nil }
			}
			result, err := x.send(t.Context(), r, nil)
			if failure == "history" {
				if err != nil || !result.HistoryWarning || result.KnownResult != store.OutboundAccepted || f.sends != 1 {
					t.Fatal(result, err)
				}
			} else if err == nil || result.KnownResult != store.OutboundNotDispatched || f.sends != 0 || f.uploads != 0 {
				t.Fatal(result, err)
			}
			duplicate, err := x.send(t.Context(), r, nil)
			if err != nil || !duplicate.Duplicate {
				t.Fatal(duplicate, err)
			}
		})
	}
	_, _, _, _, rev := outboundAppFixture(t, store.DraftVoiceKind)
	up := &whatsmeow.UploadResponse{MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}}
	_, msg, err := prepareOutboundPayload(rev.Payload().Data(), up)
	if err != nil {
		t.Fatal(err)
	}
	up.MediaKey[0] = 9
	up.FileSHA256[0] = 9
	up.FileEncSHA256[0] = 9
	if msg.GetAudioMessage().GetMediaKey()[0] != 1 || msg.GetAudioMessage().GetFileSHA256()[0] != 2 || msg.GetAudioMessage().GetFileEncSHA256()[0] != 3 {
		t.Fatal("proto bytes alias upload")
	}
}

func TestDraftVoiceEncodedPayloadCapBeforePublication(t *testing.T) {
	a := draftAppFixture(t)
	in, _ := voiceDraftInput(t, a)
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: in.To, MsgID: in.ReplyTo, SenderJID: in.To, Text: strings.Repeat("<", 43580), Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	r := draftAppRequest(t, a, in)
	if err := r.Validate(); err != nil {
		t.Fatal("fixture must fit request quota", err)
	}
	opens := 0
	_, err := a.WriteLocalDraft(t.Context(), r, func(path string) (*os.File, error) { opens++; return os.Open(path) })
	draftTestErrorCode(t, err, "store_unavailable")
	var snapshot *DraftSnapshotError
	if opens != 1 || !errors.As(err, &snapshot) || snapshot.Stage != "voice_payload" || snapshot.Publication != DraftUnpublished || voiceSnapshotValidation(err) != nil {
		t.Fatal("final escaped payload quota was not checked before publication", opens, err)
	}
	files, err := os.ReadDir(filepath.Join(a.StoreDir(), store.DraftMediaDirectory))
	if err != nil || len(files) != 0 {
		t.Fatal("published payload over quota", files, err)
	}
	if _, err := a.DB().ReadDraftRecord(t.Context(), r.DraftID); err == nil {
		t.Fatal("persisted payload over quota")
	}
}
func TestOutboundVoiceEchoKinds(t *testing.T) {
	for _, ptt := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
		msg := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: ptt}}
		want := store.DraftKind("")
		if ptt != nil && *ptt {
			want = store.DraftVoiceKind
		}
		if got := outboundEchoKind(msg); got != want {
			t.Fatal(got, want)
		}
		edited := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: msg}}
		if outboundEchoKind(edited) != "" {
			t.Fatal("voice edit confirmed echo")
		}
	}
}
