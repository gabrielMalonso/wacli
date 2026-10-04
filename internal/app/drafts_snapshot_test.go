package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func draftSnapshotFixture(t *testing.T) DraftSnapshotOptions {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "fixture.bin")
	if err := os.WriteFile(source, []byte("exact\x00bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return DraftSnapshotOptions{StoreDir: dir, RevisionID: strings.Repeat("a", 32), SourcePath: source, OpenSource: os.Open}
}

func snapshotDiskIO() draftSnapshotIO {
	return draftSnapshotIO{
		syncFile: func(f *os.File) error { return f.Sync() },
		publish:  func(root *os.Root, temp, final string) error { return root.Link(temp, final) },
		syncDir:  syncDraftSnapshotDirectory,
	}
}

func TestDraftSnapshotExactBytesAndImmutableMetadata(t *testing.T) {
	opts := draftSnapshotFixture(t)
	snapshot, err := CreateDraftSnapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("exact\x00bytes\n")
	sum := sha256.Sum256(want)
	document := snapshot.Document()
	if snapshot.RevisionID() != opts.RevisionID || document.Filename != "fixture.bin" || document.MIME != "application/octet-stream" || document.Size != int64(len(want)) || document.SHA256 != hex.EncodeToString(sum[:]) || snapshot.VerifiedAtCreate().IsZero() {
		t.Fatalf("snapshot = %+v, document = %+v", snapshot, document)
	}
	if err := os.WriteFile(opts.SourcePath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StoreDir, snapshot.RelativePath())
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("snapshot followed changed source: %q, %v", got, err)
	}
	document.Filename = "mutated"
	if snapshot.Document().Filename != "fixture.bin" {
		t.Fatal("snapshot metadata mutated")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != opts.RevisionID+".blob" {
		t.Fatalf("unexpected retained temp: %v %v", entries, err)
	}
	if runtime.GOOS != "windows" {
		for target, perm := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != perm {
				t.Fatalf("permissions: %v, %v", info, err)
			}
		}
	}
	data := store.DraftPayloadData{
		Account: store.DraftIdentity{PN: "15550000001@s.whatsapp.net"}, Recipient: store.DraftRecipient{JID: "15550000002@s.whatsapp.net"},
		Kind: store.DraftDocumentKind,
	}
	// Revision content is distinct from the inspection/verification metadata.
	document = snapshot.Document()
	data.Document = &document
	payload, err := store.NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	review := store.DraftReviewSnapshot{RequestedRaw: data.Recipient.JID, SnapshotPath: snapshot.RelativePath(), VerifiedAtCreate: snapshot.VerifiedAtCreate()}
	revision, err := store.NewDraftRevision(strings.Repeat("b", 32), snapshot.RevisionID(), time.Now(), payload, review)
	if err != nil || revision.Review().SnapshotPath != snapshot.RelativePath() {
		t.Fatalf("revision: %v", err)
	}
}

func TestDraftSnapshotNeverOverwritesAnotherRevision(t *testing.T) {
	opts := draftSnapshotFixture(t)
	first, err := CreateDraftSnapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StoreDir, first.RelativePath())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.SourcePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = CreateDraftSnapshot(context.Background(), opts)
	var typed *DraftSnapshotError
	if !errors.As(err, &typed) || typed.Stage != "publish" || typed.Publication != DraftUnpublished || !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatal("overwrote existing revision")
	}
	opts.RevisionID = strings.Repeat("c", 32)
	second, err := CreateDraftSnapshot(context.Background(), opts)
	if err != nil || second.RelativePath() == first.RelativePath() {
		t.Fatalf("new revision: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 2 {
		t.Fatalf("not one blob per revision: %v %v", entries, err)
	}
}

func TestDraftSnapshotValidatesBeforeEffectsAndUsesOpenedFD(t *testing.T) {
	for name, mutate := range map[string]func(*DraftSnapshotOptions){
		"invalid ID":             func(o *DraftSnapshotOptions) { o.RevisionID = "../escape" },
		"invalid MIME":           func(o *DraftSnapshotOptions) { o.MIME = "invalid\r\ncontent" },
		"caption limit":          func(o *DraftSnapshotOptions) { o.Caption = strings.Repeat("x", store.MaxDraftFieldBytes+1) },
		"name UTF8":              func(o *DraftSnapshotOptions) { o.Filename = string([]byte{0xff}) },
		"encoded metadata limit": func(o *DraftSnapshotOptions) { o.Filename = strings.Repeat("<", store.MaxDraftFieldBytes) },
	} {
		t.Run(name, func(t *testing.T) {
			opts := draftSnapshotFixture(t)
			opened := false
			opts.OpenSource = func(string) (*os.File, error) { opened = true; return os.Open(opts.SourcePath) }
			mutate(&opts)
			if _, err := CreateDraftSnapshot(context.Background(), opts); err == nil {
				t.Fatal("accepted invalid options")
			}
			if opened {
				t.Fatal("opened source for invalid input")
			}
			if _, err := os.Stat(filepath.Join(opts.StoreDir, store.DraftMediaDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created managed directory before validation")
			}
		})
	}
	opts := draftSnapshotFixture(t)
	opts.OpenSource = func(string) (*os.File, error) { return os.Open(opts.StoreDir) }
	if _, err := CreateDraftSnapshot(context.Background(), opts); err == nil {
		t.Fatal("trusted source path stat instead of descriptor")
	}
	if _, err := os.Stat(filepath.Join(opts.StoreDir, store.DraftMediaDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created managed directory for irregular FD")
	}
	opts.OpenSource = nil
	if _, err := CreateDraftSnapshot(context.Background(), opts); err == nil {
		t.Fatal("unrestricted opener fallback")
	}
	if runtime.GOOS == "windows" {
		return
	}
	opts.OpenSource = func(path string) (*os.File, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, path+".previous"); err != nil {
			f.Close()
			return nil, err
		}
		if err := os.WriteFile(path, []byte("different path bytes"), 0o600); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	snapshot, err := CreateDraftSnapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(opts.StoreDir, snapshot.RelativePath()))
	if err != nil || string(got) != "exact\x00bytes\n" {
		t.Fatalf("reopened original path: %q %v", got, err)
	}
}

func TestDraftSnapshotRootsAndUnknownFilesSurvive(t *testing.T) {
	opts := draftSnapshotFixture(t)
	outside := t.TempDir()
	unknown := filepath.Join(outside, "unknown-work")
	if err := os.WriteFile(unknown, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(opts.StoreDir, store.DraftMediaDirectory)); err != nil {
			t.Fatal(err)
		}
		if _, err := CreateDraftSnapshot(context.Background(), opts); err == nil {
			t.Fatal("followed managed directory symlink")
		}
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "preserve" {
		t.Fatal("modified unknown work")
	}
	opts = draftSnapshotFixture(t)
	root, err := os.OpenRoot(opts.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	opened := 0
	opts.OpenSource = func(path string) (*os.File, error) { opened++; return root.Open(path) }
	opts.SourcePath = "../outside"
	if _, err := CreateDraftSnapshot(context.Background(), opts); err == nil || opened != 1 {
		t.Fatalf("root restriction was not checked by opener: %v", err)
	}
}

func TestDraftSnapshotPublicationFailuresRetainPossibleWork(t *testing.T) {
	injected := errors.New("fixture failure")
	for _, stage := range []string{"file_sync", "publish_before_link", "publish_after_link", "directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			opts := draftSnapshotFixture(t)
			mediaDir := filepath.Join(opts.StoreDir, store.DraftMediaDirectory)
			if err := os.Mkdir(mediaDir, 0o700); err != nil {
				t.Fatal(err)
			}
			unknown := filepath.Join(mediaDir, ".unknown.tmp")
			if err := os.WriteFile(unknown, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			disk := snapshotDiskIO()
			switch stage {
			case "file_sync":
				disk.syncFile = func(*os.File) error { return injected }
			case "publish_before_link":
				disk.publish = func(*os.Root, string, string) error { return injected }
			case "publish_after_link":
				disk.publish = func(root *os.Root, temp, final string) error {
					if err := root.Link(temp, final); err != nil {
						return err
					}
					return injected
				}
			case "directory_sync":
				disk.syncDir = func(*os.Root) error { return injected }
			}
			_, err := createDraftSnapshot(context.Background(), opts, disk)
			var typed *DraftSnapshotError
			if !errors.As(err, &typed) || !errors.Is(err, injected) || typed.RevisionID != opts.RevisionID {
				t.Fatalf("typed failure: %v", err)
			}
			entries, err := os.ReadDir(mediaDir)
			if err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "file_sync":
				if typed.Publication != DraftUnpublished || len(entries) != 1 {
					t.Fatal("pre-publication temporary was not cleaned")
				}
			case "publish_before_link":
				if typed.Publication != DraftPublicationUnknown || len(entries) != 2 {
					t.Fatal("ambiguous attempt temporary lost")
				}
			case "publish_after_link":
				if typed.Publication != DraftPublicationUnknown || len(entries) != 3 {
					t.Fatal("possible published work lost")
				}
			case "directory_sync":
				if typed.Publication != DraftPublished || len(entries) != 2 {
					t.Fatal("published work lost")
				}
			}
			if got, err := os.ReadFile(unknown); err != nil || string(got) != "preserve" {
				t.Fatal("cleaned unknown file")
			}
		})
	}
}

type draftZeroReader struct{}

func (draftZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type draftChunkWriter struct{ total, max int }

func (w *draftChunkWriter) Write(p []byte) (int, error) {
	w.total += len(p)
	w.max = max(w.max, len(p))
	return len(p), nil
}

type draftShortWriter struct{}

func (draftShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type draftErrorReader struct{ err error }

func (r draftErrorReader) Read([]byte) (int, error) { return 0, r.err }

type draftCancelReader struct {
	cancel    context.CancelFunc
	remaining int
}

func (r *draftCancelReader) Read(p []byte) (int, error) {
	clear(p)
	r.remaining--
	if r.remaining == 0 {
		r.cancel()
	}
	return len(p), nil
}

func TestDraftSnapshotStreamBoundsCancellationAndIOFailures(t *testing.T) {
	writer := new(draftChunkWriter)
	meta, err := copyDraftSnapshotBytes(context.Background(), writer, io.LimitReader(draftZeroReader{}, store.MaxDraftFileBytes), store.MaxDraftFileBytes)
	if err != nil || meta.size != store.MaxDraftFileBytes || writer.total != store.MaxDraftFileBytes || writer.max > 32<<10 || len(meta.sniff) != 512 {
		t.Fatalf("stream was not bounded: %+v %+v %v", meta, writer, err)
	}
	writer = new(draftChunkWriter)
	if _, err := copyDraftSnapshotBytes(context.Background(), writer, io.LimitReader(draftZeroReader{}, store.MaxDraftFileBytes+1), store.MaxDraftFileBytes); err == nil || writer.total > store.MaxDraftFileBytes {
		t.Fatal("accepted growing oversized file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &draftCancelReader{cancel: cancel, remaining: 3}
	writer = new(draftChunkWriter)
	_, err = copyDraftSnapshotBytes(ctx, writer, reader, store.MaxDraftFileBytes)
	if !errors.Is(err, context.Canceled) || writer.total != 2*(32<<10) {
		t.Fatalf("cancellation: %v %+v", err, writer)
	}
	if _, err := copyDraftSnapshotBytes(context.Background(), draftShortWriter{}, strings.NewReader("fixture"), 100); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
	injected := errors.New("read failed")
	if _, err := copyDraftSnapshotBytes(context.Background(), io.Discard, draftErrorReader{injected}, 100); !errors.Is(err, injected) {
		t.Fatalf("read failure: %v", err)
	}
}

func TestDraftSnapshotCancellationHasNoPublication(t *testing.T) {
	opts := draftSnapshotFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateDraftSnapshot(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.StoreDir, store.DraftMediaDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled preparation wrote files")
	}
	ctx, cancel = context.WithCancel(context.Background())
	disk := snapshotDiskIO()
	disk.syncFile = func(f *os.File) error { cancel(); return f.Sync() }
	if _, err := createDraftSnapshot(ctx, opts, disk); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepublication cancellation: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(opts.StoreDir, store.DraftMediaDirectory))
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled temporary retained: %v %v", entries, err)
	}
}

func TestDraftTextStreamingPreservesLiteralAndLimit(t *testing.T) {
	text := " \tOlá 👋\r\n\\n e\u0301 "
	got, err := ReadDraftText(context.Background(), strings.NewReader(text))
	if err != nil || got != text {
		t.Fatalf("literal text: %q %v", got, err)
	}
	for _, input := range []string{strings.Repeat("x", store.MaxDraftFieldBytes+1), string([]byte{0xff})} {
		if _, err := ReadDraftText(context.Background(), strings.NewReader(input)); err == nil {
			t.Fatal("accepted invalid text input")
		}
	}
	if _, err := ReadDraftText(context.Background(), strings.NewReader(strings.Repeat("x", store.MaxDraftFieldBytes))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadDraftText(ctx, strings.NewReader(text)); !errors.Is(err, context.Canceled) {
		t.Fatalf("text cancellation: %v", err)
	}
}

func BenchmarkDraftSnapshotStream(b *testing.B) {
	b.SetBytes(store.MaxDraftFileBytes)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := copyDraftSnapshotBytes(context.Background(), io.Discard, io.LimitReader(draftZeroReader{}, store.MaxDraftFileBytes), store.MaxDraftFileBytes); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDraftSnapshotMIMESniffsCapturedBytes(t *testing.T) {
	source := filepath.Join(t.TempDir(), "misleading.jpg")
	if err := os.WriteFile(source, []byte("plain fixture text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CreateDraftSnapshot(context.Background(), DraftSnapshotOptions{StoreDir: t.TempDir(), RevisionID: strings.Repeat("e", 32), SourcePath: source, OpenSource: os.Open})
	if err != nil || snapshot.Document().MIME != "text/plain; charset=utf-8" {
		t.Fatal("MIME followed path instead of bytes", err)
	}
}
