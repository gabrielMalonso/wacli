package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/openclaw/wacli/internal/wa"
)

func mediaArtifactFixture(t *testing.T, name string) MediaLocation {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return MediaLocation{root, name, filepath.Join(dir, name), func() error { return nil }}
}
func TestMediaArtifactVerificationUsesHashWithoutKeyAndUnknownSize(t *testing.T) {
	loc := mediaArtifactFixture(t, "audio")
	data := []byte("synthetic audio")
	digest := sha256.Sum256(data)
	if err := os.WriteFile(loc.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hash                []byte
		size                uint64
		verify              bool
		state, verification string
		checks              int
	}{
		{digest[:], 0, false, "existing", "not_checked", 0},
		{nil, 0, true, "existing", "unknown", 0},
		{nil, uint64(len(data)), true, "existing", "unknown", 1},
		{digest[:], 0, true, "verified", "sha256_verified", 1},
		{digest[:], uint64(len(data)), true, "verified", "sha256_verified", 2},
		{digest[:], 1, true, "existing", "mismatch", 0},
		{bytes.Repeat([]byte{1}, 32), 0, true, "existing", "mismatch", 0},
	} {
		a, err := InspectMediaArtifact(context.Background(), loc, tc.hash, tc.size, tc.verify)
		if err != nil || a.State != tc.state || a.Verification != tc.verification || len(a.Checks) != tc.checks || a.Bytes == nil || *a.Bytes != int64(len(data)) {
			t.Fatalf("%+v: %+v %v", tc, a, err)
		}
	}
}
func TestMediaArtifactConcurrentPublicationNeverClobbers(t *testing.T) {
	loc := mediaArtifactFixture(t, "target")
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			data := []byte("synthetic complete payload")
			sum := sha256.Sum256(data)
			_, err := PublishMediaArtifact(context.Background(), loc, bytes.NewReader(data), sum[:], 0, func() error { return nil })
			if err == nil {
				winners.Add(1)
				return
			}
			var failure *MediaArtifactError
			if !errors.As(err, &failure) || failure.Code != "output_conflict" || failure.Publication != "not_written" {
				t.Errorf("unexpected failure: %v", err)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
	data, err := os.ReadFile(loc.Path)
	if err != nil || string(data) != "synthetic complete payload" {
		t.Fatalf("data=%q %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(loc.Path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("leftovers=%v %v", entries, err)
	}
	st, err := os.Stat(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("permissions=%v", st.Mode())
	}
}

type cancelMediaReader struct{ cancel context.CancelFunc }

func (r cancelMediaReader) Read(p []byte) (int, error) { r.cancel(); copy(p, "bytes"); return 5, nil }

type repeatedMediaReader struct{ remaining int64 }

func (r *repeatedMediaReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:n])
	r.remaining -= n
	return int(n), nil
}

func TestMediaArtifactFailuresPreserveDestination(t *testing.T) {
	for _, kind := range []string{"digest", "length", "cancel", "declared_limit", "body_limit", "path_change", "source_failure"} {
		t.Run(kind, func(t *testing.T) {
			loc := mediaArtifactFixture(t, "target")
			original := []byte("existing file")
			if err := os.WriteFile(loc.Path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			data := []byte("synthetic download")
			sum := sha256.Sum256(data)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reader io.Reader = bytes.NewReader(data)
			declared := uint64(0)
			before := func() error { return nil }
			switch kind {
			case "digest":
				sum[0] ^= 1
			case "length":
				declared = 1
			case "cancel":
				reader = cancelMediaReader{cancel}
			case "declared_limit":
				declared = wa.MaxMediaDownloadSize + 1
			case "body_limit":
				reader = &repeatedMediaReader{wa.MaxMediaDownloadSize + 1}
			case "path_change":
				before = func() error { return mediaArtifactFailure("media_changed", "not_written", nil) }
			case "source_failure":
				reader = io.MultiReader(bytes.NewReader(data), iotestMediaErrorReader{})
			}
			_, err := PublishMediaArtifact(ctx, loc, reader, sum[:], declared, before)
			if err == nil {
				t.Fatal("expected failure")
			}
			var failure *MediaArtifactError
			if !errors.As(err, &failure) || failure.Publication != "not_written" {
				t.Fatalf("failure=%v", err)
			}
			got, err := os.ReadFile(loc.Path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("replaced destination: %q %v", got, err)
			}
			entries, err := os.ReadDir(filepath.Dir(loc.Path))
			if err != nil || len(entries) != 1 {
				t.Fatalf("leftovers=%v %v", entries, err)
			}
		})
	}
}

type iotestMediaErrorReader struct{}

func (iotestMediaErrorReader) Read([]byte) (int, error) { return 0, errors.New("synthetic IO failure") }

func TestMediaArtifactCopyRechecksSourceAndPublishedEffects(t *testing.T) {
	src := mediaArtifactFixture(t, "source")
	dst := mediaArtifactFixture(t, "target")
	data := []byte("cached bytes")
	digest := sha256.Sum256(data)
	if err := os.WriteFile(src.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := CopyMediaArtifact(context.Background(), src, dst, digest[:], 0, func() error {
		return os.WriteFile(src.Path, []byte("changed"), 0o600)
	})
	// The callback models archive changes, not a source mutation hook. Recheck
	// the source after the callback as well as after copying.
	if err == nil {
		t.Fatal("source change escaped pre-publication check")
	}
	if _, err := os.Stat(dst.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published changed source: %v", err)
	}
	loc := mediaArtifactFixture(t, "final")
	var checks int
	loc.Check = func() error {
		checks++
		if checks >= 5 {
			return errors.New("directory replaced")
		}
		return nil
	}
	_, err = PublishMediaArtifact(context.Background(), loc, strings.NewReader("cached bytes"), digest[:], 0, func() error { return nil })
	var failure *MediaArtifactError
	if !errors.As(err, &failure) || failure.Publication != "written" {
		t.Fatalf("lost publication knowledge: %v", err)
	}
}

func TestMediaArtifactRejectsSymlinkAndChangedDescriptor(t *testing.T) {
	loc := mediaArtifactFixture(t, "target")
	if err := os.WriteFile(loc.Path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(loc.Path, loc.Path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loc.Path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaFile(loc, f, info); err == nil {
		t.Fatal("replacement accepted")
	}
	link := mediaArtifactFixture(t, "link")
	if err := os.Symlink(loc.Path, link.Path); err != nil {
		t.Skip(err)
	}
	if _, err := InspectMediaArtifact(context.Background(), link, nil, 0, false); err == nil {
		t.Fatal("symlink accepted")
	}
}
