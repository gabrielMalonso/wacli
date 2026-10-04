package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openclaw/wacli/internal/store"
)

// Managed snapshots have their own root boundary. Import source roots and paths
// are deliberately absent. The verified buffer is the uploader's exact input.
func readOutboundDocument(ctx context.Context, dir string, r store.DraftRevision) ([]byte, error) {
	rel, err := store.DraftSnapshotRelativePath(r.ID())
	if err != nil {
		return nil, err
	}
	doc := r.Payload().Data().Document
	if doc == nil {
		return nil, fmt.Errorf("document metadata required")
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
	if err != nil || !info.Mode().IsRegular() || info.Size() != doc.Size {
		return nil, fmt.Errorf("managed snapshot size/type mismatch")
	}
	f, err := media.OpenFile(name, outboundReadFlags(), 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd, err := f.Stat()
	if err != nil || !fd.Mode().IsRegular() || !os.SameFile(info, fd) || fd.Size() != doc.Size {
		return nil, fmt.Errorf("managed snapshot changed")
	}
	current, err := media.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, fd) {
		return nil, fmt.Errorf("managed snapshot path changed")
	}
	var b bytes.Buffer
	b.Grow(int(doc.Size + 1))
	// The streaming helper reads one overflow byte beyond this limit.
	meta, err := copyDraftSnapshotBytes(ctx, &b, f, store.MaxDraftFileBytes)
	if err != nil {
		return nil, err
	}
	if int64(b.Len()) != doc.Size || meta.size != doc.Size || meta.sha256 != doc.SHA256 {
		return nil, fmt.Errorf("managed snapshot digest/size mismatch")
	}
	return b.Bytes(), nil
}
