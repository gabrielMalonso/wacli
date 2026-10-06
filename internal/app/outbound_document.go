package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

// Managed snapshots have their own root boundary. Import source roots and paths
// are deliberately absent. The verified buffer is the uploader's exact input.
func readOutboundDocument(ctx context.Context, dir string, r store.DraftRevision) ([]byte, error) {
	rel, err := store.DraftSnapshotRelativePath(r.ID())
	if err != nil {
		return nil, err
	}
	p := r.Payload().Data()
	var size int64
	var digest string
	if p.Document != nil {
		size, digest = p.Document.Size, p.Document.SHA256
	} else if p.Image != nil {
		size, digest = p.Image.Size, p.Image.SHA256
	} else if p.Voice != nil {
		size, digest = p.Voice.Size, p.Voice.SHA256
	} else {
		return nil, fmt.Errorf("snapshot metadata required")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parent, err := root.Lstat(store.DraftMediaDirectory)
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("managed snapshot directory unavailable")
	}
	media, err := root.OpenRoot(store.DraftMediaDirectory)
	if err != nil {
		return nil, err
	}
	defer media.Close()
	directory, err := media.Stat(".")
	if err != nil || !os.SameFile(parent, directory) {
		return nil, fmt.Errorf("managed snapshot directory changed")
	}
	currentParent, err := root.Lstat(store.DraftMediaDirectory)
	if err != nil || !currentParent.IsDir() || currentParent.Mode()&os.ModeSymlink != 0 || !os.SameFile(currentParent, directory) {
		return nil, fmt.Errorf("managed snapshot directory changed")
	}
	name := filepath.Base(filepath.FromSlash(rel))
	info, err := media.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, fmt.Errorf("managed snapshot size/type mismatch")
	}
	f, err := media.OpenFile(name, outboundReadFlags(), 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd, err := f.Stat()
	if err != nil || !fd.Mode().IsRegular() || !os.SameFile(info, fd) || fd.Size() != size {
		return nil, fmt.Errorf("managed snapshot changed")
	}
	current, err := media.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, fd) {
		return nil, fmt.Errorf("managed snapshot path changed")
	}
	var b bytes.Buffer
	b.Grow(int(size + 1))
	// The streaming helper reads one overflow byte beyond this limit.
	meta, err := copyDraftSnapshotBytes(ctx, &b, f, store.MaxDraftFileBytes)
	if err != nil {
		return nil, err
	}
	if int64(b.Len()) != size || meta.size != size || meta.sha256 != digest {
		return nil, fmt.Errorf("managed snapshot digest/size mismatch")
	}
	if p.Image != nil {
		if err := wa.ValidateStaticImageMetadata(b.Bytes(), wa.ImageMetadata{MIME: p.Image.MIME, Width: p.Image.Width, Height: p.Image.Height}); err != nil {
			return nil, err
		}
	}
	if p.Voice != nil {
		// A second structural pass compares all frozen metadata on the upload
		// buffer, never reopening the source, decoding or invoking a probe.
		metadata, err := wa.InspectOggOpus(ctx, b.Bytes())
		if err != nil {
			return nil, err
		}
		if draftVoiceFromMetadata(metadata, size, digest) != *p.Voice {
			return nil, fmt.Errorf("voice metadata does not match snapshot structure")
		}
	}
	return b.Bytes(), nil
}
