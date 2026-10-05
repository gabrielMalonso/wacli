package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

func cleanupAppFixture(t *testing.T, discarded bool) (*App, DraftCleanupRequest, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{StoreDir: dir, WAFactory: func(wa.Options) (WAClient, error) {
		t.Error("cleanup opened a WhatsApp client")
		return nil, errors.New("fixture rejects WhatsApp")
	}})
	if err != nil {
		lk.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); lk.Release() })
	id, rid := strings.Repeat("a", 32), strings.Repeat("b", 32)
	body := []byte("fixture\x00\nexact")
	source := filepath.Join(dir, "source.fixture")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CreateDraftSnapshot(t.Context(), DraftSnapshotOptions{StoreDir: dir, RevisionID: rid, SourcePath: source, OpenSource: os.Open})
	if err != nil {
		t.Fatal(err)
	}
	document := snapshot.Document()
	payload, err := store.NewDraftPayload(store.DraftPayloadData{Account: store.DraftIdentity{PN: "15550000001@s.whatsapp.net"}, Recipient: store.DraftRecipient{JID: "15550000002@s.whatsapp.net"}, Kind: store.DraftDocumentKind, Document: &document})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.NewDraftRevision(id, rid, time.Now(), payload, store.DraftReviewSnapshot{RequestedRaw: "15550000002@s.whatsapp.net", SnapshotPath: snapshot.RelativePath(), VerifiedAtCreate: snapshot.VerifiedAtCreate()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), revision, ""); err != nil {
		t.Fatal(err)
	}
	if discarded {
		if _, err := a.DB().DiscardDraft(t.Context(), id, rid); err != nil {
			t.Fatal(err)
		}
	}
	request := DraftCleanupRequest{Version: 1, StoreRef: dir, Selection: store.DraftCleanupSelection{DraftID: id, RevisionID: rid, ExpectedHeadID: rid, Hash: payload.Hash()}}
	return a, request, filepath.Join(dir, filepath.FromSlash(snapshot.RelativePath())), body
}

func cleanupAppIO(a *App) draftCleanupIO {
	return draftCleanupIO{confirm: a.DB().ConfirmDraftCleanup, remove: func(root *os.Root, name string) error { return root.Remove(name) }, syncDir: syncDraftSnapshotDirectory}
}

func assertAppCleanupCode(t *testing.T, err error, code store.DraftCleanupCode) {
	t.Helper()
	var failure *store.DraftCleanupError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want local cleanup %s, got %v", code, err)
	}
}

func TestDraftCleanupSingleHeadRemovalRetainsRevisionAndUnknownFiles(t *testing.T) {
	a, request, path, body := cleanupAppFixture(t, true)
	message := store.UpsertMessageParams{ChatJID: "15550000002@s.whatsapp.net", MsgID: "history-fixture", SenderJID: "15550000002@s.whatsapp.net", Text: "retained history", Timestamp: time.Unix(1700000000, 0)}
	if err := a.DB().UpsertChat(message.ChatJID, "dm", "fixture", message.Timestamp); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(message); err != nil {
		t.Fatal(err)
	}
	historyBefore, err := a.DB().GetMessage(message.ChatJID, message.MsgID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	unknown := []string{strings.Repeat("f", 32) + ".blob", "." + request.Selection.RevisionID + ".tmp-" + strings.Repeat("e", 32), "unexpected"}
	for _, name := range unknown {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := a.CleanupDraftSnapshot(t.Context(), request)
	if err != nil || result.Effect != DraftCleanupRemoved || result.Outcome != DraftCleanupCompleted || result.RemovedBytes == nil || *result.RemovedBytes != int64(len(body)) {
		t.Fatal("single discarded head removal", result, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("snapshot still exists", err)
	}
	after, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
	if err != nil || before.Revision.Review() != after.Revision.Review() || !bytes.Equal(before.Revision.Payload().CanonicalJSON(), after.Revision.Payload().CanonicalJSON()) || before.Record.HeadRevisionID != after.Record.HeadRevisionID || before.Record.State != after.Record.State || !before.Record.DiscardedAt.Equal(*after.Record.DiscardedAt) || !after.Record.UpdatedAt.After(before.Record.UpdatedAt) {
		t.Fatal("retained revision/metadata changed", err)
	}
	historyAfter, err := a.DB().GetMessage(message.ChatJID, message.MsgID)
	if err != nil || !reflect.DeepEqual(historyBefore, historyAfter) {
		t.Fatal("cleanup changed history", err)
	}
	for _, name := range unknown {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatal("unknown artifact removed", name, err)
		}
	}
	result, err = a.CleanupDraftSnapshot(t.Context(), request)
	if err != nil || result.Effect != DraftCleanupNotRemoved || result.Outcome != DraftCleanupAlreadyAbsent || result.RemovedBytes == nil || *result.RemovedBytes != 0 {
		t.Fatal("absence attributed to a prior cleanup", result, err)
	}
	unchanged, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
	if err != nil || !unchanged.Record.UpdatedAt.Equal(after.Record.UpdatedAt) {
		t.Fatal("absence wrote another guard", err)
	}
	if _, err := os.Lstat(filepath.Join(a.StoreDir(), "session.db")); !os.IsNotExist(err) {
		t.Fatal("cleanup opened a session", err)
	}
	a.Close()
	ro, err := New(Options{StoreDir: a.StoreDir(), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	retained, err := ro.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
	if err != nil || retained.Revision.Payload().Hash() != request.Selection.Hash {
		t.Fatal("reopen lost retained head", err)
	}
	preview, err := ro.DB().PreviewDraftCleanup(t.Context(), a.StoreDir(), request.Selection.DraftID, 20, "")
	if err != nil || preview.Counts.Eligible != 1 || preview.Counts.EligibleBytesAtCreate != int64(len(body)) {
		t.Fatal("catalogue eligibility falsely depended on snapshot presence", preview.Counts, err)
	}
}

func TestDraftCleanupUnreferencedHeadKeepsOlderOutboundSnapshotAndKey(t *testing.T) {
	a, request, oldPath, body := cleanupAppFixture(t, false)
	old, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	reservation := store.OutboundReservation{Version: 1, ID: strings.Repeat("d", 32), DraftID: old.Record.ID, RevisionID: old.Revision.ID(), Hash: old.Revision.Payload().Hash(), Key: "retained-fixture-key", MessageID: "retained-fixture-message", Account: old.Revision.Payload().Data().Account, CreatedAt: time.Now()}
	operation, err := a.DB().Outbound().Reserve(t.Context(), reservation)
	if err != nil {
		t.Fatal(err)
	}
	rid := strings.Repeat("c", 32)
	snapshot, err := CreateDraftSnapshot(t.Context(), DraftSnapshotOptions{StoreDir: a.StoreDir(), RevisionID: rid, SourcePath: filepath.Join(a.StoreDir(), "source.fixture"), Caption: "new revision", OpenSource: os.Open})
	if err != nil {
		t.Fatal(err)
	}
	data := old.Revision.Payload().Data()
	doc := snapshot.Document()
	data.Document = &doc
	payload, err := store.NewDraftPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.NewDraftRevision(old.Record.ID, rid, time.Now(), payload, store.DraftReviewSnapshot{RequestedRaw: data.Recipient.JID, SnapshotPath: snapshot.RelativePath(), VerifiedAtCreate: snapshot.VerifiedAtCreate()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), revision, old.Revision.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().DiscardDraft(t.Context(), old.Record.ID, rid); err != nil {
		t.Fatal(err)
	}
	request.Selection.RevisionID, request.Selection.ExpectedHeadID, request.Selection.Hash = rid, rid, payload.Hash()
	result, err := a.CleanupDraftSnapshot(t.Context(), request)
	if err != nil || result.Effect != DraftCleanupRemoved {
		t.Fatal(result, err)
	}
	got, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("referenced older bytes lost", err)
	}
	duplicate := reservation
	duplicate.ID, duplicate.MessageID, duplicate.Account = strings.Repeat("e", 32), "ignored-candidate", store.DraftIdentity{}
	retained, err := a.DB().Outbound().Reserve(t.Context(), duplicate)
	if err != nil || !reflect.DeepEqual(retained, operation) {
		t.Fatal("cleanup changed retained key binding", err)
	}
	request.Selection.RevisionID, request.Selection.Hash = old.Revision.ID(), old.Revision.Payload().Hash()
	result, err = a.CleanupDraftSnapshot(t.Context(), request)
	assertAppCleanupCode(t, err, store.DraftCleanupProtected)
	if result.Effect != DraftCleanupNotRemoved {
		t.Fatal(result)
	}
}

func TestDraftCleanupPreflightReadOnlyAndActiveNoEffects(t *testing.T) {
	a, request, path, body := cleanupAppFixture(t, false)
	result, err := a.CleanupDraftSnapshot(t.Context(), request)
	assertAppCleanupCode(t, err, store.DraftCleanupProtected)
	if result.Effect != DraftCleanupNotRemoved {
		t.Fatal(result)
	}
	bad := request
	bad.Selection.RevisionID = "../unexpected"
	_, err = a.CleanupDraftSnapshot(t.Context(), bad)
	assertAppCleanupCode(t, err, store.DraftCleanupInvalidArguments)
	bad = request
	bad.StoreRef = filepath.Join(a.StoreDir(), "other")
	_, err = a.CleanupDraftSnapshot(t.Context(), bad)
	assertAppCleanupCode(t, err, store.DraftCleanupInvalidArguments)
	ro, err := New(Options{StoreDir: a.StoreDir(), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	_, err = ro.CleanupDraftSnapshot(t.Context(), request)
	assertAppCleanupCode(t, err, store.DraftCleanupReadOnly)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("policy/preflight changed bytes", err)
	}
}

func TestDraftCleanupCommitCancellationAndFilesystemEffects(t *testing.T) {
	for _, stage := range []string{"commit_uncertain", "restore_failure", "cancel_before", "cancel_after_commit", "remove_before_effect", "remove_after_effect", "sync_failure", "cancel_after_unlink"} {
		t.Run(stage, func(t *testing.T) {
			a, request, path, body := cleanupAppFixture(t, true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			disk := cleanupAppIO(a)
			baseConfirm, baseRemove, baseSync := disk.confirm, disk.remove, disk.syncDir
			removes, syncs := 0, 0
			marker := errors.New("synthetic cleanup fault")
			disk.confirm = func(ctx context.Context, selection store.DraftCleanupSelection) (store.DraftEntry, error) {
				entry, err := baseConfirm(ctx, selection)
				if err != nil {
					return store.DraftEntry{}, err
				}
				if stage == "commit_uncertain" || stage == "restore_failure" {
					return store.DraftEntry{}, store.DraftCleanupFailure(store.DraftCleanupCommitUncertain, selection, marker)
				}
				if stage == "cancel_after_commit" {
					cancel()
				}
				return entry, nil
			}
			disk.remove = func(root *os.Root, name string) error {
				removes++
				if stage == "remove_before_effect" {
					return marker
				}
				if err := baseRemove(root, name); err != nil {
					return err
				}
				if stage == "remove_after_effect" {
					return marker
				}
				if stage == "cancel_after_unlink" {
					cancel()
				}
				return nil
			}
			disk.syncDir = func(root *os.Root) error {
				syncs++
				if stage == "sync_failure" {
					return marker
				}
				return baseSync(root)
			}
			if stage == "cancel_before" {
				cancel()
			}
			result, err := a.cleanupDraftSnapshot(ctx, request, disk)
			if err == nil {
				t.Fatal("fault reported success", result)
			}
			switch stage {
			case "commit_uncertain", "restore_failure", "cancel_before", "cancel_after_commit":
				if result.Effect != DraftCleanupNotRemoved || removes != 0 || syncs != 0 || result.RemovedBytes == nil || *result.RemovedBytes != 0 {
					t.Fatal("unconfirmed guard/cancellation reached unlink", result)
				}
				got, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(got, body) {
					t.Fatal("bytes lost before unlink", e)
				}
			case "remove_before_effect", "remove_after_effect":
				if result.Effect != DraftCleanupUnknown || result.RemovedBytes != nil || removes != 1 || syncs != 0 {
					t.Fatal("ambiguous unlink asserted an effect", result)
				}
				assertAppCleanupCode(t, err, store.DraftCleanupOutcomeUncertain)
			case "sync_failure", "cancel_after_unlink":
				if result.Effect != DraftCleanupRemoved || result.RemovedBytes == nil || *result.RemovedBytes != int64(len(body)) || removes != 1 || syncs != 1 {
					t.Fatal("post-unlink fault lost actual effect/sync", result)
				}
				assertAppCleanupCode(t, err, store.DraftCleanupOutcomeUncertain)
			}
			a.Close()
			reopened, err := New(Options{StoreDir: a.StoreDir(), ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			entry, err := reopened.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
			if err != nil || entry.Record.State != "discarded" || entry.Revision.Payload().Hash() != request.Selection.Hash {
				t.Fatal("failure/reopen lost retained evidence", err)
			}
		})
	}
}

func TestDraftCleanupChangedUnexpectedAndSymlinkEntriesPreserved(t *testing.T) {
	for _, change := range []string{"digest", "size", "directory", "leaf_symlink", "parent_symlink", "replace_after_commit", "write_after_commit"} {
		t.Run(change, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.Contains(change, "symlink") {
				t.Skip("fixture symlinks require privileges on Windows")
			}
			a, request, path, body := cleanupAppFixture(t, true)
			disk := cleanupAppIO(a)
			baseConfirm := disk.confirm
			removes := 0
			disk.remove = func(*os.Root, string) error { removes++; return errors.New("must not reach unlink") }
			before, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(a.StoreDir(), "external.fixture")
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "digest":
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(body)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "size":
				if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "leaf_symlink":
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "parent_symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+".saved", parent); err != nil {
					t.Fatal(err)
				}
			case "replace_after_commit", "write_after_commit":
				disk.confirm = func(ctx context.Context, selection store.DraftCleanupSelection) (store.DraftEntry, error) {
					entry, err := baseConfirm(ctx, selection)
					if err != nil {
						return entry, err
					}
					if change == "replace_after_commit" {
						if err := os.Rename(path, path+".saved"); err != nil {
							return entry, err
						}
					}
					if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(body)), 0o600); err != nil {
						return entry, err
					}
					// Ensure the same-inode fixture changes its mtime on coarse filesystems.
					if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
						return entry, err
					}
					return entry, nil
				}
			}
			result, err := a.cleanupDraftSnapshot(t.Context(), request, disk)
			if err == nil || removes != 0 || result.Effect != DraftCleanupNotRemoved {
				t.Fatal("changed/unexpected entry reached unlink", result, err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("symlink target modified", err)
			}
			after, err := a.DB().ReadDraft(t.Context(), request.Selection.DraftID, request.Selection.RevisionID)
			if err != nil || !reflect.DeepEqual(before.Revision.Payload().CanonicalJSON(), after.Revision.Payload().CanonicalJSON()) || before.Revision.Review() != after.Revision.Review() {
				t.Fatal("filesystem mismatch changed immutable evidence", err)
			}
			if !strings.Contains(change, "after_commit") && !after.Record.UpdatedAt.Equal(before.Record.UpdatedAt) {
				t.Fatal("invalid snapshot reached FULL guard")
			}
		})
	}
}

func TestDraftCleanupHardLinksReportLogicalBytesOnly(t *testing.T) {
	a, request, path, body := cleanupAppFixture(t, true)
	retained := filepath.Join(a.StoreDir(), "retained-hardlink.fixture")
	if err := os.Link(path, retained); err != nil {
		t.Skipf("fixture filesystem lacks hardlinks: %v", err)
	}
	result, err := a.CleanupDraftSnapshot(t.Context(), request)
	if err != nil || result.Effect != DraftCleanupRemoved || result.RemovedBytes == nil || *result.RemovedBytes != int64(len(body)) {
		t.Fatal(result, err)
	}
	got, err := os.ReadFile(retained)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("unlink removed hardlink target bytes", err)
	}
}

func BenchmarkDraftCleanupSnapshotVerification(b *testing.B) {
	for _, size := range []int{1 << 20, 100 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			body := bytes.Repeat([]byte("f"), size)
			path := filepath.Join(dir, "fixture.bin")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				b.Fatal(err)
			}
			digest := sha256.Sum256(body)
			wantHash := hex.EncodeToString(digest[:])
			body = nil
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				file, err := os.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				meta, err := copyDraftSnapshotBytes(context.Background(), io.Discard, file, store.MaxDraftFileBytes)
				file.Close()
				if err != nil || meta.size != int64(size) || meta.sha256 != wantHash {
					b.Fatal(meta, err)
				}
			}
		})
	}
}
