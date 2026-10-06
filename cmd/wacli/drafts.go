package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

func newDraftCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "draft", Short: "Prepare and inspect durable local drafts without sending"}
	cmd.AddCommand(newDraftWriteCmd(flags, "create"), newDraftWriteCmd(flags, "update"), newDraftDiscardCmd(flags), newDraftShowCmd(flags), newDraftListCmd(flags), newDraftCleanupCmd(flags))
	return cmd
}

// Freeze selection for both legacy and agent invocations before LOCK/IPC.
func freezeDraftStore(flags *rootFlags) error {
	selection := *flags
	selection.agent = true
	dir, err := resolveStoreDir(&selection)
	if err != nil {
		return agentStoreError(err)
	}
	flags.agentAccount = selection.agentAccount
	flags.storeDir = dir
	flags.account = ""
	return nil
}

func draftWritable(flags *rootFlags) error {
	if err := flags.requireWritable(); err != nil {
		return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects local draft writes.", ExitCode: 2, Cause: err}
	}
	return nil
}

func newDraftWriteCmd(flags *rootFlags, action string) *cobra.Command {
	var input app.DraftInput
	var message, messageFile, imagePath, voicePath, contactName, contactPhone, expected string
	cmd := &cobra.Command{Use: action, Short: action + " a local draft revision", Args: cobra.NoArgs}
	if action == "update" {
		cmd.Use = "update ID"
		cmd.Args = cobra.ExactArgs(1)
		cmd.Flags().StringVar(&expected, "if-revision", "", "current revision required for compare-and-swap")
	}
	cmd.Flags().StringVar(&input.To, "to", "", "explicit DM/group JID or phone; discover names with contacts search/resolve or chats list")
	cmd.Flags().StringVar(&message, "message", "", "literal UTF-8 text (no automatic escape decoding)")
	cmd.Flags().StringVar(&messageFile, "message-file", "", "UTF-8 text file, or - for stdin (64 KiB maximum)")
	cmd.Flags().StringArrayVar(&input.Mentions, "mention", nil, "explicit user phone/JID to mention (repeatable)")
	cmd.Flags().StringVar(&input.ReplyTo, "reply-to", "", "existing local textual message ID in the selected chat")
	cmd.Flags().StringVar(&imagePath, "image", "", "static JPEG/PNG image to snapshot (100 MiB and 40 million pixels maximum)")
	cmd.Flags().StringVar(&voicePath, "voice", "", "complete Ogg/Opus PTT to snapshot (local 100 MiB and one-hour encoded timeline limits)")
	cmd.Flags().StringVar(&input.File, "file", "", "local document to snapshot (100 MiB maximum)")
	cmd.Flags().StringVar(&input.Filename, "filename", "", "document display name (defaults to source basename)")
	cmd.Flags().StringVar(&input.MIME, "mime", "", "explicit document MIME override")
	cmd.Flags().StringVar(&input.Caption, "caption", "", "literal document/image caption")
	cmd.Flags().StringVar(&contactName, "contact-name", "", "explicit contact display name")
	cmd.Flags().StringVar(&contactPhone, "contact-phone", "", "explicit PN phone for one contact card")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := draftWritable(flags); err != nil {
			return err
		}
		if cmd.Flags().Changed("image") {
			for _, option := range []string{"file", "message", "message-file", "contact-name", "contact-phone", "filename", "mime", "voice"} {
				if cmd.Flags().Changed(option) {
					return agentUsageError(fmt.Errorf("--image is exclusive of --%s", option))
				}
			}
		}
		if cmd.Flags().Changed("voice") {
			for _, option := range []string{"image", "file", "message", "message-file", "contact-name", "contact-phone", "filename", "mime", "caption", "mention"} {
				if cmd.Flags().Changed(option) {
					return agentUsageError(fmt.Errorf("--voice is exclusive of --%s", option))
				}
			}
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		if cmd.Flags().Changed("message") && cmd.Flags().Changed("message-file") {
			return agentUsageError(fmt.Errorf("--message and --message-file are exclusive"))
		}
		if cmd.Flags().Changed("message") {
			value := message
			input.Message = &value
		}
		if cmd.Flags().Changed("message-file") {
			reader := os.Stdin
			if messageFile != "-" {
				file, err := os.Open(messageFile)
				if err != nil {
					return &out.AgentError{Code: "invalid_arguments", Message: "Text input file cannot be opened.", ExitCode: 2, Cause: err}
				}
				defer file.Close()
				info, err := file.Stat()
				if err != nil || !info.Mode().IsRegular() {
					return agentUsageError(fmt.Errorf("text input must be a regular file"))
				}
				reader = file
			}
			value, err := app.ReadDraftText(ctx, reader)
			if err != nil {
				return agentUsageError(fmt.Errorf("text input exceeds limits, is invalid UTF-8, or could not be read"))
			}
			input.Message = &value
		}
		if cmd.Flags().Changed("contact-name") || cmd.Flags().Changed("contact-phone") {
			input.Contact = &store.DraftContact{DisplayName: contactName, Phone: contactPhone}
		}
		if cmd.Flags().Changed("image") {
			input.Image = &app.DraftImageInput{Path: imagePath}
		}
		if cmd.Flags().Changed("voice") {
			input.Voice = &app.DraftVoiceInput{Path: voicePath}
		}
		if err := input.Validate(); err != nil {
			return classifyDraftError(err)
		}
		sourcePath := input.File
		if input.Image != nil {
			sourcePath = input.Image.Path
		}
		if input.Voice != nil {
			sourcePath = input.Voice.Path
		}
		if sourcePath != "" {
			path, err := filepath.Abs(sourcePath)
			if err != nil {
				return classifyDraftError(err)
			}
			if input.Image != nil {
				input.Image.Path = path
			} else if input.Voice != nil {
				input.Voice.Path = path
			} else {
				input.File = path
			}
			if err := checkOutboundMediaPath(path); err != nil {
				message := "Document source is unavailable or outside allowed media roots."
				if input.Image != nil {
					message = "Image source is unavailable or outside allowed media roots."
				}
				if input.Voice != nil {
					message = "Voice source is unavailable or outside allowed media roots."
				}
				return &out.AgentError{Code: "invalid_arguments", Message: message, ExitCode: 2, Cause: err}
			}
		}
		if action == "update" {
			if err := store.ValidateDraftID(args[0]); err != nil {
				return classifyDraftError(err)
			}
			if err := store.ValidateDraftID(expected); err != nil {
				return classifyDraftError(err)
			}
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		id, err := store.NewDraftID()
		if err != nil {
			return err
		}
		if action == "update" {
			id = args[0]
		}
		rid, err := store.NewDraftID()
		if err != nil {
			return err
		}
		version := 1
		if input.Image != nil {
			version = 2
		}
		if input.Voice != nil {
			version = 3
		}
		request := app.DraftWriteRequest{Version: version, Action: action, DraftID: id, RevisionID: rid, ExpectedRevision: expected, StoreRef: flags.storeDir, AccountName: flags.agentAccount.Name, Input: &input}
		return runDraftWrite(ctx, flags, request)
	}
	return cmd
}

func newDraftDiscardCmd(flags *rootFlags) *cobra.Command {
	var expected string
	cmd := &cobra.Command{Use: "discard ID", Short: "Abandon a local draft while retaining its revisions and snapshots", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&expected, "if-revision", "", "current revision required for compare-and-swap")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if err := draftWritable(flags); err != nil {
			return err
		}
		if err := store.ValidateDraftID(args[0]); err != nil {
			return classifyDraftError(err)
		}
		if err := store.ValidateDraftID(expected); err != nil {
			return classifyDraftError(err)
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		return runDraftWrite(ctx, flags, app.DraftWriteRequest{Version: 1, Action: "discard", DraftID: args[0], RevisionID: expected, ExpectedRevision: expected, StoreRef: flags.storeDir, AccountName: flags.agentAccount.Name})
	}
	return cmd
}

func runDraftWrite(ctx context.Context, flags *rootFlags, request app.DraftWriteRequest) error {
	if err := request.Validate(); err != nil {
		return classifyDraftError(err)
	}
	a, lk, err := newApp(ctx, flags, true, true)
	if err != nil {
		resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{Kind: draftWriteKind, Draft: &request})
		if !delegated {
			return classifyDraftError(delegateErr)
		}
		if delegateErr != nil {
			return classifyDraftError(delegateErr)
		}
		entry, err := validateDraftDelegateResult(request, resp.DraftResult)
		if err != nil {
			return classifyDraftError(err)
		}
		return writeDraftEntry(flags, entry, true)
	}
	defer closeApp(a, lk)
	entry, err := a.WriteLocalDraft(ctx, request, openOutboundMedia)
	if err != nil {
		return classifyDraftError(err)
	}
	return writeDraftEntry(flags, entry, true)
}

func newDraftShowCmd(flags *rootFlags) *cobra.Command {
	var revision string
	cmd := &cobra.Command{Use: "show ID", Short: "Read a frozen revision without opening snapshot bytes", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&revision, "revision", "", "specific revision ID (defaults to current)")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		if err := store.ValidateDraftID(args[0]); err != nil {
			return classifyDraftError(err)
		}
		if revision != "" {
			if err := store.ValidateDraftID(revision); err != nil {
				return classifyDraftError(err)
			}
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		a, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyDraftError(err)
		}
		defer closeApp(a, lk)
		entry, err := a.DB().ReadDraft(ctx, args[0], revision)
		if err != nil {
			return classifyDraftError(err)
		}
		return writeDraftEntry(flags, entry, false)
	}
	return cmd
}

func newDraftListCmd(flags *rootFlags) *cobra.Command {
	var limit int
	var discarded bool
	cmd := &cobra.Command{Use: "list", Short: "List bounded stored summaries without reading snapshot files", Args: cobra.NoArgs}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum drafts (1 to 200)")
	cmd.Flags().BoolVar(&discarded, "include-discarded", false, "include abandoned local drafts")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if limit < 1 || limit > 200 {
			return agentUsageError(fmt.Errorf("--limit must be 1 to 200"))
		}
		if flags.cursor != "" {
			if err := store.ValidateDraftCursor(flags.cursor); err != nil {
				return classifyDraftError(err)
			}
		}
		if err := freezeDraftStore(flags); err != nil {
			return err
		}
		ctx, cancel := withTimeout(context.Background(), flags)
		defer cancel()
		a, lk, err := newReadApp(ctx, flags)
		if err != nil {
			return classifyDraftError(err)
		}
		defer closeApp(a, lk)
		page, err := a.DB().ListDrafts(ctx, a.StoreDir(), discarded, limit, flags.cursor)
		if err != nil {
			return classifyDraftError(err)
		}
		return writeDraftList(flags, page)
	}
	return cmd
}
