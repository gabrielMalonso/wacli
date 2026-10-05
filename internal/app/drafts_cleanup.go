package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
)

type DraftCleanupRequest struct {
	Version   int                         `json:"version"`
	StoreRef  string                      `json:"store_ref"`
	Selection store.DraftCleanupSelection `json:"selection"`
}

func (r DraftCleanupRequest) Validate() error {
	if err := r.Selection.Validate(); err != nil {
		return err
	}
	if r.Version != 1 || !filepath.IsAbs(r.StoreRef) || len(r.StoreRef) > 4096 || !utf8.ValidString(r.StoreRef) || strings.ContainsAny(r.StoreRef, "\x00?#") {
		return store.DraftCleanupFailure(store.DraftCleanupInvalidArguments, r.Selection, nil)
	}
	return nil
}

// Effects describe this call's filesystem action, separately from the earlier
// catalogue commit. They never attribute a pre-existing absence to cleanup.
type DraftCleanupEffect string

const (
	DraftCleanupNotRemoved DraftCleanupEffect = "not_removed"
	DraftCleanupRemoved    DraftCleanupEffect = "removed"
	DraftCleanupUnknown    DraftCleanupEffect = "unknown"
)

type DraftCleanupOutcome string

const (
	DraftCleanupCompleted     DraftCleanupOutcome = "removed"
	DraftCleanupAlreadyAbsent DraftCleanupOutcome = "already_absent_unknown"
	DraftCleanupFailed        DraftCleanupOutcome = "failed"
)

type DraftCleanupDirectorySync string

const (
	DraftCleanupSyncNotAttempted DraftCleanupDirectorySync = "not_attempted"
	DraftCleanupSyncCompleted    DraftCleanupDirectorySync = "completed"
	DraftCleanupSyncUnsupported  DraftCleanupDirectorySync = "unsupported"
	DraftCleanupSyncUnknown      DraftCleanupDirectorySync = "unknown"
)

type DraftCleanupResult struct {
	DraftID       string                    `json:"draft_id"`
	RevisionID    string                    `json:"revision_id"`
	Hash          string                    `json:"hash"`
	Effect        DraftCleanupEffect        `json:"effect"`
	Outcome       DraftCleanupOutcome       `json:"outcome"`
	DirectorySync DraftCleanupDirectorySync `json:"directory_sync"`
	// Logical bytes unlinked by this call; nil means the effect is unknown.
	// Hard links/open descriptors can retain storage after successful unlink.
	RemovedBytes *int64 `json:"removed_bytes"`
}

type draftCleanupIO struct {
	confirm func(context.Context, store.DraftCleanupSelection) (store.DraftEntry, error)
	remove  func(*os.Root, string) error
	syncDir func(*os.Root) error
}

// CleanupDraftSnapshot requires the caller to own the existing store LOCK/owner
// slot until it returns. It neither opens a WA/session client nor uses pacing.
// The FULL catalogue commit precedes unlink; filesystem and DB are not atomic.
func (a *App) CleanupDraftSnapshot(ctx context.Context, request DraftCleanupRequest) (DraftCleanupResult, error) {
	return a.cleanupDraftSnapshot(ctx, request, draftCleanupIO{
		confirm: a.DB().ConfirmDraftCleanup,
		remove:  func(root *os.Root, name string) error { return root.Remove(name) },
		syncDir: syncDraftSnapshotDirectory,
	})
}

func (a *App) cleanupDraftSnapshot(ctx context.Context, request DraftCleanupRequest, disk draftCleanupIO) (DraftCleanupResult, error) {
	zero := int64(0)
	result := DraftCleanupResult{Effect: DraftCleanupNotRemoved, Outcome: DraftCleanupFailed, DirectorySync: DraftCleanupSyncNotAttempted, RemovedBytes: &zero}
	fail := func(code store.DraftCleanupCode, cause error) (DraftCleanupResult, error) {
		return result, store.DraftCleanupFailure(code, request.Selection, cause)
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	s := request.Selection
	result.DraftID, result.RevisionID, result.Hash = s.DraftID, s.RevisionID, s.Hash
	if a.ReadOnly() {
		return fail(store.DraftCleanupReadOnly, nil)
	}
	if request.StoreRef != a.StoreDir() || disk.confirm == nil || disk.remove == nil || disk.syncDir == nil {
		return fail(store.DraftCleanupInvalidArguments, nil)
	}
	if err := ctx.Err(); err != nil {
		return fail(store.DraftCleanupCanceled, err)
	}
	entry, err := a.DB().ReadDraftCleanup(ctx, s)
	if err != nil {
		return result, err
	}
	snapshot, absent, err := openDraftCleanupSnapshot(a.StoreDir(), entry.Revision)
	if err != nil {
		return fail(store.DraftCleanupSnapshotError, err)
	}
	if absent {
		if err := ctx.Err(); err != nil {
			return fail(store.DraftCleanupCanceled, err)
		}
		result.Outcome = DraftCleanupAlreadyAbsent
		return result, nil
	}
	defer snapshot.close()
	document := entry.Revision.Payload().Data().Document
	meta, err := copyDraftSnapshotBytes(ctx, io.Discard, snapshot.file, store.MaxDraftFileBytes)
	if err != nil {
		if ctx.Err() != nil {
			return fail(store.DraftCleanupCanceled, ctx.Err())
		}
		return fail(store.DraftCleanupSnapshotError, err)
	}
	if meta.size != document.Size || meta.sha256 != document.SHA256 {
		return fail(store.DraftCleanupSnapshotChanged, nil)
	}
	if err := snapshot.checkIdentity(document.Size); err != nil {
		return fail(store.DraftCleanupSnapshotChanged, err)
	}
	// Never authorize unlink from a failed/uncertain commit or restoration. The
	// writer/owner slot remains held while discarded state forbids new updates
	// and reservations. Background outbound observers do not create operations.
	confirmed, err := disk.confirm(ctx, s)
	if err != nil {
		return result, err
	}
	if confirmed.Record.State != "discarded" || confirmed.Record.HeadRevisionID != s.ExpectedHeadID || confirmed.Revision.DraftID() != s.DraftID || confirmed.Revision.ID() != s.RevisionID || confirmed.Revision.Payload().Hash() != s.Hash {
		return fail(store.DraftCleanupStoreError, nil)
	}
	if err := ctx.Err(); err != nil {
		return fail(store.DraftCleanupCanceled, err)
	}
	if err := snapshot.checkIdentity(document.Size); err != nil {
		return fail(store.DraftCleanupSnapshotChanged, err)
	}
	if err := ctx.Err(); err != nil {
		return fail(store.DraftCleanupCanceled, err)
	}
	// os.Root confines the operation, but portable APIs have no atomic
	// compare-inode-and-unlink. External non-cooperating path/content changes can
	// race this last check; LOCK protects cooperating wacli writers only.
	result.Effect, result.RemovedBytes = DraftCleanupUnknown, nil
	if err := disk.remove(snapshot.media, snapshot.name); err != nil {
		return fail(store.DraftCleanupOutcomeUncertain, err)
	}
	removed := document.Size
	result.Effect, result.RemovedBytes = DraftCleanupRemoved, &removed
	result.DirectorySync = DraftCleanupSyncUnknown
	// Once unlink succeeds, request cancellation cannot skip its directory sync.
	// Filesystem syscalls do not promise strict interruption/deadline bounds.
	if err := disk.syncDir(snapshot.media); err != nil {
		return fail(store.DraftCleanupOutcomeUncertain, err)
	}
	result.DirectorySync = DraftCleanupSyncCompleted
	if runtime.GOOS == "windows" {
		result.DirectorySync = DraftCleanupSyncUnsupported
	}
	if err := ctx.Err(); err != nil {
		return fail(store.DraftCleanupOutcomeUncertain, err)
	}
	result.Outcome = DraftCleanupCompleted
	return result, nil
}

type draftCleanupSnapshot struct {
	root, media  *os.Root
	file         *os.File
	parent, info os.FileInfo
	name         string
}

func (s *draftCleanupSnapshot) close() {
	if s.file != nil {
		_ = s.file.Close()
	}
	if s.media != nil {
		_ = s.media.Close()
	}
	if s.root != nil {
		_ = s.root.Close()
	}
}

// This opener only reads existing managed entries; it never creates, chmods,
// follows a source path or enumerates unknown/temporary/orphan files.
func openDraftCleanupSnapshot(dir string, revision store.DraftRevision) (_ *draftCleanupSnapshot, absent bool, err error) {
	rel, err := store.DraftSnapshotRelativePath(revision.ID())
	if err != nil {
		return nil, false, err
	}
	s := &draftCleanupSnapshot{name: filepath.Base(filepath.FromSlash(rel))}
	defer func() {
		if err != nil || absent {
			s.close()
		}
	}()
	s.root, err = os.OpenRoot(dir)
	if err != nil {
		return nil, false, err
	}
	s.parent, err = s.root.Lstat(store.DraftMediaDirectory)
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !s.parent.IsDir() || s.parent.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && s.parent.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("managed snapshot directory unavailable")
	}
	s.media, err = s.root.OpenRoot(store.DraftMediaDirectory)
	if err != nil {
		return nil, false, err
	}
	directory, err := s.media.Stat(".")
	if err != nil || !os.SameFile(s.parent, directory) {
		return nil, false, fmt.Errorf("managed snapshot directory changed")
	}
	s.info, err = s.media.Lstat(s.name)
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	document := revision.Payload().Data().Document
	if document == nil || !s.info.Mode().IsRegular() || s.info.Size() != document.Size {
		return nil, false, fmt.Errorf("managed snapshot size/type mismatch")
	}
	s.file, err = s.media.OpenFile(s.name, outboundReadFlags(), 0)
	if err != nil {
		return nil, false, err
	}
	if err := s.checkIdentity(document.Size); err != nil {
		return nil, false, err
	}
	return s, false, nil
}

func (s *draftCleanupSnapshot) checkIdentity(size int64) error {
	parent, err := s.root.Lstat(store.DraftMediaDirectory)
	if err != nil || !parent.IsDir() || parent.Mode() != s.parent.Mode() || !os.SameFile(parent, s.parent) {
		return fmt.Errorf("managed snapshot directory changed")
	}
	fd, err := s.file.Stat()
	if err != nil || !fd.Mode().IsRegular() || fd.Mode() != s.info.Mode() || !os.SameFile(fd, s.info) || fd.Size() != size || !fd.ModTime().Equal(s.info.ModTime()) {
		return fmt.Errorf("managed snapshot descriptor changed")
	}
	current, err := s.media.Lstat(s.name)
	if err != nil || !current.Mode().IsRegular() || current.Mode() != s.info.Mode() || !os.SameFile(current, fd) || current.Size() != size || !current.ModTime().Equal(s.info.ModTime()) {
		return fmt.Errorf("managed snapshot path changed")
	}
	return nil
}
