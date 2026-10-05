package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/openclaw/wacli/internal/wa"
)

// MediaLocation is an already-confined parent directory. Check must recheck
// its path binding before and after IO; OpenRoot alone allows internal symlinks.
type MediaLocation struct {
	Root       *os.Root
	Name, Path string
	Check      func() error
}

type MediaArtifact struct {
	State        string     `json:"state"`
	Path         *string    `json:"path"`
	Bytes        *int64     `json:"bytes"`
	SHA256       string     `json:"sha256,omitempty"`
	Verification string     `json:"verification"`
	Checks       []string   `json:"checks"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

// MediaArtifactError retains publication knowledge without exposing a cause.
type MediaArtifactError struct {
	Code, Publication string
	Cause             error
}

func (e *MediaArtifactError) Error() string { return e.Code }
func (e *MediaArtifactError) Unwrap() error { return e.Cause }
func mediaArtifactFailure(code, publication string, err error) error {
	return &MediaArtifactError{code, publication, err}
}

func emptyMediaArtifact(state string) MediaArtifact {
	return MediaArtifact{State: state, Verification: "not_checked", Checks: []string{}}
}

// InspectMediaArtifact never writes. Missing SHA-256 cannot establish a binding
// to the stored message even when the observed size matches.
func InspectMediaArtifact(ctx context.Context, loc MediaLocation, digest []byte, declared uint64, verify bool) (MediaArtifact, error) {
	result := emptyMediaArtifact("absent")
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := loc.Check(); err != nil {
		return result, err
	}
	info, err := loc.Root.Lstat(loc.Name)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return emptyMediaArtifact("unreadable"), err
	}
	if !info.Mode().IsRegular() {
		return result, mediaArtifactFailure("path_not_allowed", "not_written", nil)
	}
	f, err := loc.Root.OpenFile(loc.Name, outboundReadFlags(), 0)
	if err != nil {
		return emptyMediaArtifact("unreadable"), err
	}
	defer f.Close()
	if err := checkMediaFile(loc, f, info); err != nil {
		return emptyMediaArtifact("changed"), err
	}
	n := info.Size()
	result = emptyMediaArtifact("existing")
	result.Path, result.Bytes = &loc.Path, &n
	if !verify {
		return result, nil
	}
	if n > wa.MaxMediaDownloadSize {
		return result, wa.ErrMediaTooLarge
	}
	meta, err := copyMediaArtifact(ctx, io.Discard, f)
	if err != nil {
		return result, err
	}
	if err := checkMediaFile(loc, f, info); err != nil {
		return emptyMediaArtifact("changed"), err
	}
	result.SHA256, result.Bytes = hex.EncodeToString(meta.digest), &meta.size
	result.Verification = "unknown"
	if declared > 0 {
		if uint64(meta.size) != declared {
			result.Verification = "mismatch"
			return result, nil
		}
		result.Checks = append(result.Checks, "declared_size")
	}
	if len(digest) != sha256.Size {
		return result, nil
	}
	if !bytes.Equal(meta.digest, digest) {
		result.Verification = "mismatch"
		return result, nil
	}
	result.Checks = append(result.Checks, "sha256")
	result.State, result.Verification = "verified", "sha256_verified"
	now := time.Now().UTC()
	result.VerifiedAt = &now
	return result, nil
}

func checkMediaFile(loc MediaLocation, f *os.File, initial os.FileInfo) error {
	if err := loc.Check(); err != nil {
		return err
	}
	fd, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := loc.Root.Lstat(loc.Name)
	if err != nil {
		return mediaArtifactFailure("media_changed", "not_written", err)
	}
	for _, info := range []os.FileInfo{fd, current} {
		if !info.Mode().IsRegular() || !os.SameFile(initial, info) || info.Size() != initial.Size() || !info.ModTime().Equal(initial.ModTime()) {
			return mediaArtifactFailure("media_changed", "not_written", nil)
		}
	}
	return nil
}

// CopyMediaArtifact reopens and hashes the bytes actually copied; an earlier
// status observation never authenticates a later path read.
func CopyMediaArtifact(ctx context.Context, src, dst MediaLocation, digest []byte, declared uint64, beforePublish func() error) (MediaArtifact, error) {
	info, err := src.Root.Lstat(src.Name)
	if err != nil {
		return emptyMediaArtifact("changed"), err
	}
	if !info.Mode().IsRegular() {
		return emptyMediaArtifact("changed"), mediaArtifactFailure("path_not_allowed", "not_written", nil)
	}
	f, err := src.Root.OpenFile(src.Name, outboundReadFlags(), 0)
	if err != nil {
		return emptyMediaArtifact("changed"), err
	}
	defer f.Close()
	if err := checkMediaFile(src, f, info); err != nil {
		return emptyMediaArtifact("changed"), err
	}
	return PublishMediaArtifact(ctx, dst, f, digest, declared, func() error {
		if err := checkMediaFile(src, f, info); err != nil {
			return err
		}
		if err := beforePublish(); err != nil {
			return err
		}
		return checkMediaFile(src, f, info)
	})
}

// PublishMediaArtifact creates a private temporary file, hashes copied bytes,
// then links without replacing a destination. No rename fallback is allowed.
func PublishMediaArtifact(ctx context.Context, loc MediaLocation, src io.Reader, digest []byte, declared uint64, beforePublish func() error) (MediaArtifact, error) {
	result := emptyMediaArtifact("absent")
	publication := "not_written"
	fail := func(code string, err error) (MediaArtifact, error) {
		return result, mediaArtifactFailure(code, publication, err)
	}
	if len(digest) != sha256.Size {
		return fail("media_binding_unverified", nil)
	}
	if declared > wa.MaxMediaDownloadSize {
		return fail("media_too_large", wa.ErrMediaTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	if err := loc.Check(); err != nil {
		return fail("media_changed", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail("output_failed", err)
	}
	name := ".wacli-download-" + hex.EncodeToString(nonce[:])
	f, err := loc.Root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail("output_failed", err)
	}
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = loc.Root.Remove(name)
		}
	}()
	meta, err := copyMediaArtifact(ctx, f, src)
	if err != nil {
		return fail("output_failed", err)
	}
	if !bytes.Equal(digest, meta.digest) || (declared > 0 && uint64(meta.size) != declared) {
		return fail("integrity_failed", nil)
	}
	if err := f.Sync(); err != nil {
		return fail("output_failed", err)
	}
	info, err := f.Stat()
	if err != nil {
		return fail("output_failed", err)
	}
	if err := checkMediaFile(MediaLocation{loc.Root, name, loc.Path, loc.Check}, f, info); err != nil {
		return fail("media_changed", err)
	}
	if err := beforePublish(); err != nil {
		return fail("media_changed", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	if err := loc.Check(); err != nil {
		return fail("media_changed", err)
	}
	if err := checkMediaFile(MediaLocation{loc.Root, name, loc.Path, loc.Check}, f, info); err != nil {
		return fail("media_changed", err)
	}
	publication = "unknown"
	if err := loc.Root.Link(name, loc.Name); err != nil {
		if errors.Is(err, os.ErrExist) {
			publication = "not_written"
			return fail("output_conflict", err)
		}
		remove = false // An ambiguous link failure is not proof of no publication.
		return fail("output_failed", err)
	}
	publication = "written"
	now := time.Now().UTC()
	result = MediaArtifact{State: "verified", Path: &loc.Path, Bytes: &meta.size, SHA256: hex.EncodeToString(meta.digest), Verification: "sha256_verified", Checks: []string{"sha256"}, VerifiedAt: &now}
	if declared > 0 {
		result.Checks = append(result.Checks, "declared_size")
	}
	if err := checkMediaFile(loc, f, info); err != nil {
		return fail("media_changed", err)
	}
	if err := loc.Root.Remove(name); err != nil {
		remove = false
		return fail("output_failed", err)
	}
	remove = false
	if err := ctx.Err(); err != nil {
		return fail("cancelled", err)
	}
	return result, nil
}

type mediaCopiedBytes struct {
	size   int64
	digest []byte
}

func copyMediaArtifact(ctx context.Context, dst io.Writer, src io.Reader) (mediaCopiedBytes, error) {
	h := sha256.New()
	var result mediaCopiedBytes
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, err := src.Read(buf[:min(int64(len(buf)), wa.MaxMediaDownloadSize-result.size+1)])
		if errCtx := ctx.Err(); errCtx != nil {
			return result, errCtx
		}
		if n > 0 {
			if result.size+int64(n) > wa.MaxMediaDownloadSize {
				return result, wa.ErrMediaTooLarge
			}
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return result, writeErr
			}
			if written != n {
				return result, io.ErrShortWrite
			}
			_, _ = h.Write(buf[:n])
			result.size += int64(n)
		}
		if errors.Is(err, io.EOF) {
			result.digest = h.Sum(nil)
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if n == 0 {
			return result, io.ErrNoProgress
		}
	}
}
