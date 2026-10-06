package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

type DraftPublication string

const (
	DraftUnpublished        DraftPublication = "unpublished"
	DraftPublished          DraftPublication = "published"
	DraftPublicationUnknown DraftPublication = "unknown"
)

// DraftSnapshotError distinguishes publication from the later SQLite commit.
// Published/unknown files must survive failures; this helper never retries a
// publication or removes a final blob, including one from a failed operation.
type DraftSnapshotError struct {
	RevisionID  string
	Stage       string
	Publication DraftPublication
	Cause       error
}

func (e *DraftSnapshotError) Error() string { return "local draft snapshot failed during " + e.Stage }
func (e *DraftSnapshotError) Unwrap() error { return e.Cause }

type DraftSnapshotOptions struct {
	StoreDir, RevisionID, SourcePath string
	Filename, MIME, Caption          string
	Image                            bool
	Voice                            bool
	validateImage                    func(store.DraftImage) error
	validateVoice                    func(store.DraftVoice) error
	// The process opening the file must enforce its media roots here. There
	// is deliberately no unrestricted default or pre-open stat shortcut.
	OpenSource func(string) (*os.File, error)
}

// DraftSnapshot holds immutable metadata. It is not a current integrity check.
type DraftSnapshot struct {
	revisionID, relativePath string
	document                 store.DraftDocument
	verifiedAt               time.Time
	image                    *store.DraftImage
	voice                    *store.DraftVoice
}

func (s DraftSnapshot) RevisionID() string            { return s.revisionID }
func (s DraftSnapshot) Document() store.DraftDocument { return s.document }
func (s DraftSnapshot) VerifiedAtCreate() time.Time   { return s.verifiedAt }
func (s DraftSnapshot) RelativePath() string          { return s.relativePath }

func (s DraftSnapshot) Image() *store.DraftImage {
	if s.image == nil {
		return nil
	}
	value := *s.image
	value.JPEGThumbnail = bytes.Clone(value.JPEGThumbnail)
	return &value
}

func (s DraftSnapshot) Voice() *store.DraftVoice {
	if s.voice == nil {
		return nil
	}
	value := *s.voice
	return &value
}

func draftVoiceFromMetadata(m wa.OggOpusMetadata, size int64, digest string) store.DraftVoice {
	return store.DraftVoice{MIME: wa.OggOpusMIME, Size: size, SHA256: digest, OpusVersion: m.OpusVersion, Channels: m.Channels, MappingFamily: m.MappingFamily, PreSkip: m.PreSkip, InputSampleRate: m.InputSampleRate, OutputGain: m.OutputGain, EncodedSamples: m.EncodedSamples, PlayableSamples: m.PlayableSamples}
}

type draftSnapshotIO struct {
	syncFile func(*os.File) error
	publish  func(*os.Root, string, string) error
	syncDir  func(*os.Root) error
}

var errDraftFileLimit = errors.New("source exceeds draft file limit")

// CreateDraftSnapshot streams one regular source to an exclusive revision file.
// The caller must hold the store LOCK (directly or as its existing sync owner).
// Final files are retained on update/discard; retention cleanup is separate.
func CreateDraftSnapshot(ctx context.Context, opts DraftSnapshotOptions) (DraftSnapshot, error) {
	return createDraftSnapshot(ctx, opts, draftSnapshotIO{
		syncFile: func(file *os.File) error { return file.Sync() },
		publish:  func(root *os.Root, temp, final string) error { return root.Link(temp, final) },
		syncDir:  syncDraftSnapshotDirectory,
	})
}

func createDraftSnapshot(ctx context.Context, opts DraftSnapshotOptions, disk draftSnapshotIO) (DraftSnapshot, error) {
	relativePath, err := store.DraftSnapshotRelativePath(opts.RevisionID)
	if err != nil {
		return DraftSnapshot{}, err
	}
	if opts.OpenSource == nil || opts.StoreDir == "" || opts.SourcePath == "" {
		return DraftSnapshot{}, fmt.Errorf("draft snapshot requires store, source and a root-enforcing opener")
	}
	name := opts.Filename
	if name == "" {
		name = filepath.Base(opts.SourcePath)
	}
	// Validate input fields before touching source or store.
	if opts.Voice {
		if opts.Image || opts.Filename != "" || opts.MIME != "" || opts.Caption != "" {
			return DraftSnapshot{}, fmt.Errorf("invalid voice input")
		}
	} else if opts.Image {
		if opts.Filename != "" || opts.MIME != "" || !utf8.ValidString(opts.Caption) || len(opts.Caption) > store.MaxDraftFieldBytes {
			return DraftSnapshot{}, fmt.Errorf("invalid image input")
		}
	} else {
		probe := store.DraftDocument{Filename: name, MIME: opts.MIME, Caption: opts.Caption, SHA256: strings.Repeat("0", sha256.Size*2)}
		if probe.MIME == "" {
			probe.MIME = "application/octet-stream"
		}
		if _, err := store.NewDraftDocument(probe); err != nil {
			return DraftSnapshot{}, err
		}
	}
	state := DraftUnpublished
	fail := func(stage string, err error) (DraftSnapshot, error) {
		return DraftSnapshot{}, &DraftSnapshotError{opts.RevisionID, stage, state, err}
	}
	if err := ctx.Err(); err != nil {
		return fail("source", err)
	}
	source, err := opts.OpenSource(opts.SourcePath)
	if err != nil {
		return fail("source", err)
	}
	if source == nil {
		return fail("source", fmt.Errorf("source opener returned no descriptor"))
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fail("source", err)
	}
	if !info.Mode().IsRegular() {
		return fail("source", fmt.Errorf("opened source is not a regular file"))
	}
	if info.Size() > store.MaxDraftFileBytes {
		if opts.Voice {
			return fail("voice", &wa.OggOpusError{Code: "quota", Reason: "local 100 MiB file byte limit exceeded"})
		}
		return fail("source", fmt.Errorf("source exceeds 100 MiB"))
	}
	if err := ctx.Err(); err != nil {
		return fail("source", err)
	}
	root, err := openDraftMediaRoot(opts.StoreDir)
	if err != nil {
		return fail("directory", err)
	}
	defer root.Close()
	tempID, err := store.NewDraftID()
	if err != nil {
		return fail("temporary", err)
	}
	tempName := "." + opts.RevisionID + ".tmp-" + tempID
	temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail("temporary", err)
	}
	closed := false
	removeTemp := true
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		if removeTemp {
			// Only this attempt's exclusive temporary name can be removed.
			_ = root.Remove(tempName)
		}
	}()
	var captured bytes.Buffer
	var destination io.Writer = temp
	if opts.Image || opts.Voice {
		destination = io.MultiWriter(temp, &captured)
	}
	meta, err := copyDraftSnapshotBytes(ctx, destination, source, store.MaxDraftFileBytes)
	if err != nil {
		if opts.Voice && errors.Is(err, errDraftFileLimit) {
			return fail("voice", &wa.OggOpusError{Code: "quota", Reason: "local 100 MiB file byte limit exceeded"})
		}
		return fail("copy", err)
	}
	var document store.DraftDocument
	var image *store.DraftImage
	var voice *store.DraftVoice
	if opts.Image {
		metadata, err := wa.PrepareStaticImage(captured.Bytes())
		if err != nil {
			return fail("image", err)
		}
		value, err := store.NewDraftImage(store.DraftImage{MIME: metadata.MIME, Caption: opts.Caption, Size: meta.size, SHA256: meta.sha256, Width: metadata.Width, Height: metadata.Height, JPEGThumbnail: metadata.JPEGThumbnail})
		if err != nil {
			return fail("image", err)
		}
		if opts.validateImage != nil {
			if err := opts.validateImage(value); err != nil {
				return fail("image", err)
			}
		}
		image = &value
	} else if opts.Voice {
		metadata, err := wa.InspectOggOpus(ctx, captured.Bytes())
		if err != nil {
			return fail("voice", err)
		}
		value, err := store.NewDraftVoice(draftVoiceFromMetadata(metadata, meta.size, meta.sha256))
		if err != nil {
			return fail("voice", err)
		}
		if opts.validateVoice != nil {
			if err := opts.validateVoice(value); err != nil {
				return fail("voice_payload", err)
			}
		}
		voice = &value
	} else {
		document = store.DraftDocument{Filename: name, MIME: detectDraftDocumentMIME(opts.MIME, meta.sniff), Caption: opts.Caption, Size: meta.size, SHA256: meta.sha256}
		document, err = store.NewDraftDocument(document)
		if err != nil {
			return fail("metadata", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail("copy", err)
	}
	if err := disk.syncFile(temp); err != nil {
		return fail("file_sync", err)
	}
	err = temp.Close()
	closed = true
	if err != nil {
		return fail("close", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("publish", err)
	}
	finalName := opts.RevisionID + ".blob"
	// Link is an atomic no-replace publication on the same filesystem.
	// Once attempted, ambiguous errors retain both names for recovery.
	state = DraftPublicationUnknown
	if err := disk.publish(root, tempName, finalName); err != nil {
		if errors.Is(err, os.ErrExist) {
			state = DraftUnpublished
		} else {
			removeTemp = false
		}
		return fail("publish", err)
	}
	state = DraftPublished
	if err := root.Remove(tempName); err != nil {
		removeTemp = false
		return fail("temporary_unlink", err)
	}
	removeTemp = false
	if err := disk.syncDir(root); err != nil {
		return fail("directory_sync", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("publish", err)
	}
	return DraftSnapshot{revisionID: opts.RevisionID, relativePath: relativePath, document: document, verifiedAt: time.Now().UTC(), image: image, voice: voice}, nil
}

// Opening the existing store is intentional: this helper cannot initialize
// another store or silently follow a pre-existing media-directory symlink.
func openDraftMediaRoot(storeDir string) (*os.Root, error) {
	root, err := os.OpenRoot(storeDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(store.DraftMediaDirectory)
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir(store.DraftMediaDirectory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err = root.Lstat(store.DraftMediaDirectory)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, fmt.Errorf("draft media directory must be an existing private directory or newly created here")
	}
	// Persist the parent's media-directory entry as well as the child contents.
	if err := syncDraftSnapshotDirectory(root); err != nil {
		return nil, err
	}
	return root.OpenRoot(store.DraftMediaDirectory)
}

func syncDraftSnapshotDirectory(root *os.Root) error {
	// Windows does not support Sync on a directory handle. This does not
	// promise power-loss atomicity there, or across the subsequent DB commit.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type draftSnapshotBytes struct {
	size   int64
	sha256 string
	sniff  []byte
}

func copyDraftSnapshotBytes(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (draftSnapshotBytes, error) {
	hash := sha256.New()
	meta := draftSnapshotBytes{sniff: make([]byte, 0, 512)}
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return draftSnapshotBytes{}, err
		}
		count := min(int64(len(buffer)), limit-meta.size+1)
		n, readErr := src.Read(buffer[:count])
		if err := ctx.Err(); err != nil {
			return draftSnapshotBytes{}, err
		}
		if n > 0 {
			if int64(n) > limit-meta.size {
				return draftSnapshotBytes{}, errDraftFileLimit
			}
			chunk := buffer[:n]
			written, err := dst.Write(chunk)
			if err != nil {
				return draftSnapshotBytes{}, err
			}
			if written != n {
				return draftSnapshotBytes{}, io.ErrShortWrite
			}
			_, _ = hash.Write(chunk)
			if len(meta.sniff) < 512 {
				meta.sniff = append(meta.sniff, chunk[:min(n, 512-len(meta.sniff))]...)
			}
			meta.size += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return draftSnapshotBytes{}, readErr
			}
			meta.sha256 = hex.EncodeToString(hash.Sum(nil))
			return meta, nil
		}
		if n == 0 {
			return draftSnapshotBytes{}, io.ErrNoProgress
		}
	}
}

func detectDraftDocumentMIME(override string, sniff []byte) string {
	if override != "" {
		return override
	}
	return http.DetectContentType(sniff)
}
