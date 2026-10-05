package app

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
)

func imageDraftInput(t *testing.T, a *App) (DraftInput, []byte) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.StoreDir(), "private-source.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return DraftInput{To: "90002@lid", Image: &DraftImageInput{Path: path}, Caption: " literal\\n\n🔷 "}, data.Bytes()
}
func TestDraftImageVersionAndInputBeforeEffects(t *testing.T) {
	a := draftAppFixture(t)
	input, _ := imageDraftInput(t, a)
	request := draftAppRequest(t, a, input)
	for _, modify := range []func(*DraftWriteRequest){func(r *DraftWriteRequest) { r.Version = 1 }, func(r *DraftWriteRequest) { r.Input.Image = nil; r.Input.Message = draftTextPointer("text") }, func(r *DraftWriteRequest) { r.Input.File = "document" }, func(r *DraftWriteRequest) { r.Input.MIME = "image/png" }, func(r *DraftWriteRequest) { r.Input.Image.Path = "" }} {
		r := request
		in := input
		img := *input.Image
		in.Image = &img
		r.Input = &in
		modify(&r)
		if _, err := a.WriteLocalDraft(t.Context(), r, func(string) (*os.File, error) { t.Fatal("opened invalid input"); return nil, nil }); err == nil {
			t.Fatal("accepted incompatible request")
		}
	}
	if _, err := os.Stat(filepath.Join(a.StoreDir(), store.DraftMediaDirectory)); !os.IsNotExist(err) {
		t.Fatal("invalid request created snapshot directory", err)
	}
}
func TestDraftImageSnapshotQuoteImmutableAndNoStatReads(t *testing.T) {
	a := draftAppFixture(t)
	input, original := imageDraftInput(t, a)
	if err := a.DB().UpsertChat("90002@lid", "dm", "fixture", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "90002@lid", MsgID: "quoted", SenderJID: "90002@lid", Text: " raw quoted text ", Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	input.ReplyTo = "quoted"
	first, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, input), os.Open)
	if err != nil {
		t.Fatal(err)
	}
	value := first.Revision.Payload().Data().Image
	path := filepath.Join(a.StoreDir(), first.Revision.Review().SnapshotPath)
	if value.MIME != "image/png" || value.Width != 3 || value.Height != 2 || first.Revision.Payload().Data().Reply.Text != " raw quoted text " {
		t.Fatal(value)
	}
	copy := first.Revision.Payload().Data()
	copy.Image.JPEGThumbnail[0] ^= 0xff
	if err := os.WriteFile(input.Image.Path, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(captured, original) {
		t.Fatal("source followed", err)
	}
	// Missing bytes remain inspectable; readers must never stat/open the snapshot.
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	read, err := a.DB().ReadDraft(t.Context(), first.Record.ID, first.Revision.ID())
	if err != nil || read.Revision.Payload().Hash() != first.Revision.Payload().Hash() {
		t.Fatal(err)
	}
	if _, err := a.DB().ListDrafts(t.Context(), a.StoreDir(), false, 20, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().DiscardDraft(t.Context(), first.Record.ID, first.Revision.ID()); err != nil {
		t.Fatal(err)
	}
	page, err := a.DB().PreviewDraftCleanup(t.Context(), a.StoreDir(), first.Record.ID, 20, "")
	if err != nil || page.Items[0].Eligibility != store.DraftCleanupNonDocument {
		t.Fatal(page, err)
	}
}
func TestDraftImageInvalidCancellationAndPublicationRetention(t *testing.T) {
	for _, mode := range []string{"invalid", "canceled", "publication"} {
		t.Run(mode, func(t *testing.T) {
			a := draftAppFixture(t)
			input, _ := imageDraftInput(t, a)
			request := draftAppRequest(t, a, input)
			opts := DraftSnapshotOptions{StoreDir: a.StoreDir(), RevisionID: request.RevisionID, SourcePath: input.Image.Path, Image: true, Caption: input.Caption, OpenSource: os.Open}
			ctx := t.Context()
			disk := snapshotDiskIO()
			if mode == "invalid" {
				if err := os.WriteFile(input.Image.Path, []byte("secret malformed image"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if mode == "publication" {
				original := disk.publish
				disk.publish = func(root *os.Root, temp, final string) error {
					if err := original(root, temp, final); err != nil {
						return err
					}
					return errors.New("lost publication confirmation")
				}
			}
			_, err := createDraftSnapshot(ctx, opts, disk)
			if err == nil {
				t.Fatal("unexpected success")
			}
			var typed *DraftSnapshotError
			if !errors.As(err, &typed) || strings.Contains(err.Error(), input.Image.Path) || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
			path, _ := store.DraftSnapshotRelativePath(request.RevisionID)
			_, statErr := os.Stat(filepath.Join(a.StoreDir(), path))
			if mode == "publication" {
				if statErr != nil || typed.Publication != DraftPublicationUnknown {
					t.Fatal(statErr, typed)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal("published invalid/canceled bytes", statErr)
			}
		})
	}
}

func TestOutboundImageSnapshotBeforeNetworkAndRetainedResult(t *testing.T) {
	for _, failure := range []string{"missing", "tampered", "symlink", "mime", "dimensions", "history"} {
		t.Run(failure, func(t *testing.T) {
			a, x, r, f, revision := outboundAppFixture(t, store.DraftImageKind)
			path := filepath.Join(a.StoreDir(), revision.Review().SnapshotPath)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "missing":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				original[len(original)/2] ^= 0xff
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".retained", path); err != nil {
					t.Fatal(err)
				}
			case "history":
				x.history = func(store.DraftPayloadData, store.OutboundOperation, whatsmeow.SendResponse, *whatsmeow.UploadResponse) error {
					return errors.New("synthetic history failure")
				}
			case "mime", "dimensions":
				// Inject internally valid metadata with a matching byte digest but
				// inconsistent header at the managed-reader boundary.
				data := revision.Payload().Data()
				if failure == "mime" {
					data.Image.MIME = "image/jpeg"
				} else {
					data.Image.Width++
				}
				payload, err := store.NewDraftPayload(data)
				if err != nil {
					t.Fatal(err)
				}
				inconsistent, err := store.NewDraftRevision(revision.DraftID(), revision.ID(), revision.CreatedAt(), payload, revision.Review())
				if err != nil {
					t.Fatal(err)
				}
				x.document = func(ctx context.Context, _ store.DraftRevision) ([]byte, error) {
					return readOutboundDocument(ctx, a.StoreDir(), inconsistent)
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
			again, err := x.send(t.Context(), r, nil)
			if err != nil || !again.Duplicate {
				t.Fatal(again, err)
			}
		})
	}
}

func TestDraftImageEncodedPayloadCapBeforePublication(t *testing.T) {
	a := draftAppFixture(t)
	input, _ := imageDraftInput(t, a)
	input.Caption = strings.Repeat("<", 43560)
	request := draftAppRequest(t, a, input)
	if err := request.Validate(); err != nil {
		t.Fatal("fixture must fit the request cap", err)
	}
	_, err := a.WriteLocalDraft(t.Context(), request, os.Open)
	var failure *store.DraftError
	if !errors.As(err, &failure) || failure.Code != "image_unavailable" {
		t.Fatal("accepted escaped image payload over cap", err)
	}
	entries, err := os.ReadDir(filepath.Join(a.StoreDir(), store.DraftMediaDirectory))
	if err != nil || len(entries) != 0 {
		t.Fatal("published invalid payload or left temporary bytes", entries, err)
	}
	page, err := a.DB().ListDrafts(t.Context(), a.StoreDir(), false, 20, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
}
