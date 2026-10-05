package main

import (
	"context"
	"os"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newDraftCleanupCmd(flags *rootFlags) *cobra.Command {
	group := &cobra.Command{Use: "cleanup", Short: "Explicitly inspect or remove discarded, unreferenced document bytes"}
	var limit int
	preview := &cobra.Command{Use: "preview ID", Short: "Read catalogue eligibility only; counts cover this page", Args: cobra.ExactArgs(1)}
	preview.Flags().IntVar(&limit, "limit", 20, "maximum revisions in this page (1 to 200)")
	preview.RunE = func(_ *cobra.Command, args []string) error {
		if store.ValidateDraftID(args[0]) != nil || limit < 1 || limit > 200 {
			return classifyDraftCleanupError(store.DraftCleanupFailure(store.DraftCleanupInvalidArguments, store.DraftCleanupSelection{}, nil), nil)
		}
		if flags.cursor != "" {
			if err := store.ValidateDraftCleanupCursor(flags.cursor); err != nil {
				return classifyDraftCleanupError(err, nil)
			}
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		a, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyDraftCleanupError(err, nil)
		}
		defer closeApp(a, lk)
		page, err := a.DB().PreviewDraftCleanup(ctx, a.StoreDir(), args[0], limit, flags.cursor)
		if err != nil {
			return classifyDraftCleanupError(err, nil)
		}
		return writeDraftCleanupPreview(os.Stdout, flags, page)
	}
	var selection store.DraftCleanupSelection
	apply := &cobra.Command{Use: "apply ID", Short: "Remove one verified snapshot while retaining all catalogue and outbound evidence", Args: cobra.ExactArgs(1)}
	apply.Flags().StringVar(&selection.RevisionID, "revision", "", "exact document revision ID (required)")
	apply.Flags().StringVar(&selection.ExpectedHeadID, "if-revision", "", "expected current head revision ID (required)")
	apply.Flags().StringVar(&selection.Hash, "expect-hash", "", "complete canonical payload SHA-256 (required)")
	apply.RunE = func(_ *cobra.Command, args []string) error {
		selection.DraftID = args[0]
		// Policy is checked before even readonly archive preflight or IPC submission.
		if err := flags.requireWritable(); err != nil {
			return classifyDraftCleanupError(store.DraftCleanupFailure(store.DraftCleanupReadOnly, selection, err), nil)
		}
		if err := selection.Validate(); err != nil {
			return classifyDraftCleanupError(err, nil)
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		request := app.DraftCleanupRequest{Version: 1, StoreRef: flags.storeDir, Selection: selection}
		if err := request.Validate(); err != nil {
			return classifyDraftCleanupError(err, nil)
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		return runDraftCleanup(ctx, flags, request)
	}
	group.AddCommand(preview, apply)
	return group
}

func runDraftCleanup(ctx context.Context, flags *rootFlags, request app.DraftCleanupRequest) error {
	result := draftCleanupNoRemoval(request.Selection)
	// Require a compatible existing archive before writable open can initialize,
	// upgrade or normalize permissions. Eligibility is rechecked under exclusion.
	reader, lk, err := newReadApp(ctx, flags)
	if err != nil {
		return classifyDraftCleanupError(err, &result)
	}
	_, err = reader.DB().ReadDraftCleanup(ctx, request.Selection)
	closeApp(reader, lk)
	if err != nil {
		return classifyDraftCleanupError(err, &result)
	}
	writer, writerLock, err := newApp(ctx, flags, true, true)
	if err != nil {
		resp, _, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{Kind: draftCleanupKind, DraftCleanup: &request})
		if delegateErr != nil {
			return classifyDraftCleanupError(delegateErr, &result)
		}
		result, err = validateDraftCleanupDelegate(request, resp)
	} else {
		defer closeApp(writer, writerLock)
		result, err = writer.CleanupDraftSnapshot(ctx, request)
	}
	if err != nil {
		return classifyDraftCleanupError(err, &result)
	}
	return writeDraftCleanupResult(os.Stdout, flags, result)
}

// Exact selection supplies correlation, never an idempotency or replay token.
func draftCleanupNoRemoval(s store.DraftCleanupSelection) app.DraftCleanupResult {
	zero := int64(0)
	return app.DraftCleanupResult{DraftID: s.DraftID, RevisionID: s.RevisionID, Hash: s.Hash, Effect: app.DraftCleanupNotRemoved, Outcome: app.DraftCleanupFailed, DirectorySync: app.DraftCleanupSyncNotAttempted, RemovedBytes: &zero}
}

const draftCleanupRecovery = "Preview reports catalogue eligibility only. Inspect exact retained revision and bytes; never repeat an uncertain cleanup automatically. Absence does not prove prior cleanup or reclaimed disk space."
