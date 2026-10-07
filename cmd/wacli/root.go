package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

const sourceVersion = "0.20.0"

var version string

const releaseLinkerSettingPrefix = "wacli-release-linker-version=["

var releaseLinkerSetting string

func effectiveVersion() string {
	if version == "" && releaseLinkerSetting == "" {
		return sourceVersion
	}
	// Source/HEAD package builds historically set only main.version. Official
	// release artifacts additionally require the marker during verification.
	if version != "" && releaseLinkerSetting == "" {
		return version
	}
	if version != "" && releaseLinkerSetting == releaseLinkerSettingPrefix+version+"]" {
		return version
	}
	return "invalid-release-linker-version"
}

const docsURL = "https://wacli.sh"

type rootFlags struct {
	agentCapability       agentCapability
	agentHistoryAttemptID string
	agentOutboundRequest  *app.OutboundSendRequest
	agentChatStateRequest *app.ChatStateRequest
	agentRunStarted       bool
	agent                 bool
	cursor                string
	detail                string
	agentAccount          out.AgentAccount
	storeDir              string
	account               string
	accountBinding        accountBinding
	asJSON                bool
	fullOutput            bool
	events                bool
	timeout               time.Duration
	readOnly              bool
	lockWait              time.Duration
}

func execute(args []string) error {
	var flags rootFlags

	rootCmd := &cobra.Command{
		Use:           "wacli",
		Short:         "WhatsApp CLI: sync, search, send",
		Long:          "wacli is a WhatsApp CLI for syncing, searching, and sending from local scripts.\n\nDocs: " + docsURL,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       effectiveVersion(),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			events := out.NewEventWriter(os.Stderr, flags.events)
			if flags.agent && (flags.agentCapability == agentHistoryRecovery || flags.agentCapability == agentOutboundSend || flags.agentCapability == agentChatState || flags.agentCapability == agentMediaRecovery) {
				events = out.NewEventWriter(io.Discard, true)
			}
			wa.SetLibsignalEvents(events)
		},
	}
	rootCmd.SetVersionTemplate("wacli {{.Version}}\n")

	rootCmd.PersistentFlags().StringVar(&flags.storeDir, "store", "", "store directory (default: $WACLI_STORE_DIR, XDG state dir on Linux, or ~/.wacli)")
	rootCmd.PersistentFlags().StringVar(&flags.account, "account", "", "named account from config.yaml")
	registerAccountBinding(rootCmd, &flags)
	rootCmd.PersistentFlags().BoolVar(&flags.asJSON, "json", false, "output JSON instead of human-readable text")
	rootCmd.PersistentFlags().BoolVar(&flags.agent, "agent", false, "output the versioned agent JSON contract (queries, media status/download/retry/transcribe, drafts, history recovery, outbound dispatch and explicit unread/archive state)")
	rootCmd.PersistentFlags().StringVar(&flags.cursor, "cursor", "", "resume agent list or temporal search pagination")
	rootCmd.PersistentFlags().StringVar(&flags.detail, "detail", "compact", "agent detail: compact|full (requires --agent)")
	rootCmd.PersistentFlags().BoolVar(&flags.fullOutput, "full", false, "disable truncation in table output")
	rootCmd.PersistentFlags().BoolVar(&flags.events, "events", false, "emit machine-readable NDJSON lifecycle events on stderr")
	rootCmd.PersistentFlags().DurationVar(&flags.timeout, "timeout", 5*time.Minute, "command timeout (non-sync commands)")
	rootCmd.PersistentFlags().DurationVar(&flags.lockWait, "lock-wait", 0, "wait for the store lock before failing (write commands)")
	rootCmd.PersistentFlags().BoolVar(&flags.readOnly, "read-only", false, "reject commands that intentionally write WhatsApp or the local store (or set WACLI_READONLY=1)")

	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newAccountsCmd(&flags))
	rootCmd.AddCommand(newDoctorCmd(&flags))
	rootCmd.AddCommand(newAuthCmd(&flags))
	rootCmd.AddCommand(newSyncCmd(&flags))
	rootCmd.AddCommand(newMessagesCmd(&flags))
	rootCmd.AddCommand(newCallsCmd(&flags))
	rootCmd.AddCommand(newSendCmd(&flags))
	rootCmd.AddCommand(newPollCmd(&flags))
	rootCmd.AddCommand(newPollsCmd(&flags))
	rootCmd.AddCommand(newMediaCmd(&flags))
	rootCmd.AddCommand(newContactsCmd(&flags))
	rootCmd.AddCommand(newChatsCmd(&flags))
	rootCmd.AddCommand(newGroupsCmd(&flags))
	rootCmd.AddCommand(newChannelsCmd(&flags))
	rootCmd.AddCommand(newHistoryCmd(&flags))
	rootCmd.AddCommand(newDraftCmd(&flags))
	rootCmd.AddCommand(newOutboundCmd(&flags))
	rootCmd.AddCommand(newPresenceCmd(&flags))
	rootCmd.AddCommand(newProfileCmd(&flags))
	rootCmd.AddCommand(newDocsCmd(&flags))
	rootCmd.AddCommand(newStoreCmd(&flags))
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd()
	intent := agentFlagIntent(rootCmd, args)
	flags.agent = intent.agent
	flags.accountBinding.agent = intent.agent
	flags.agentCapability = intent.capability
	if intent.capability == agentChatState {
		requested, _ := store.NormalizeDraftTarget(intent.chat)
		flags.agentChatStateRequest = &app.ChatStateRequest{Version: 1, Requested: requested, Action: app.ChatStateAction(intent.chatAction)}
	}
	// Resolve the command before installing Args wrappers or letting Cobra add
	// hidden shell-completion commands. Find performs no parsing or hooks.
	var agentFindErr error
	selectedCommand, _, findErr := rootCmd.Find(args)
	for c := selectedCommand; c != nil; c = c.Parent() {
		if c.Name() == "accounts" {
			flags.accountBinding.registryCommand = true
		}
	}
	if intent.agent {
		agentFindErr = findErr
	}
	if intent.cursorSet && !intent.help {
		c, _, findErr := rootCmd.Find(args)
		if !intent.agent || findErr != nil || (c.CommandPath() != "wacli messages list" && c.CommandPath() != "wacli messages search" && c.CommandPath() != "wacli chats list" && c.CommandPath() != "wacli contacts list" && c.CommandPath() != "wacli contacts search" && c.CommandPath() != "wacli draft list" && c.CommandPath() != "wacli draft cleanup preview" && c.CommandPath() != "wacli outbound list" && c.CommandPath() != "wacli outbound show") {
			err := agentUsageError(fmt.Errorf("--cursor requires --agent messages list, --agent messages search --sort time, --agent chats list or --agent contacts list/search or --agent draft list/cleanup preview or --agent outbound list/show"))
			writeRootError(flags, err)
			return err
		}
	}
	installAgentGuards(rootCmd, &flags)
	if intent.detailSet && !intent.agent && !intent.help {
		err := fmt.Errorf("--detail requires --agent")
		writeRootError(flags, err)
		return err
	}

	rootCmd.SetArgs(args)
	if err := func() error {
		if agentFindErr != nil {
			return agentUsageError(agentFindErr)
		}
		return rootCmd.Execute()
	}(); err != nil {
		if intent.bindingSet {
			// Binding can fail between repeated bool flags. Use the established
			// output intent rather than the partially parsed --agent value.
			flags.agent = intent.agent
		}
		if intent.agent {
			flags.agent = true
			if flags.agentAccount.StoreRef == nil && intent.store != "" && intent.account == "" && !intent.bindingSet {
				ref, absErr := filepath.Abs(intent.store)
				if absErr == nil {
					flags.agentAccount.StoreRef = &ref
				}
			}
			var typed *out.AgentError
			if !flags.agentRunStarted && !errors.As(err, &typed) {
				err = agentUsageError(err)
			}
			var bindingSelection *accountBindingSelectionError
			if errors.As(err, &bindingSelection) {
				err = bindingSelection
			} else if flags.agentCapability == agentHistoryRecovery {
				err = classifyHistoryAgentError(err, flags.agentHistoryAttemptID)
			} else if flags.agentCapability == agentOutboundSend {
				err = classifyOutboundActionError(err, flags.agentOutboundRequest)
			} else if flags.agentCapability == agentChatState {
				err = classifyChatStateAgentError(err, flags.agentChatStateRequest)
			} else if flags.agentCapability == agentMediaRecovery {
				err = classifyMediaRetryCommandError(err)
			} else if flags.agentCapability == agentMediaTranscription {
				err = classifyTranscriptionCommandError(err)
			} else if flags.agentCapability == agentMediaDownload || flags.agentCapability == agentMediaRead {
				err = classifyMediaCommandError(err)
			} else {
				err = classifyAgentError(err)
			}
		}
		writeRootError(flags, err)
		return err
	}
	return nil
}

func writeRootError(flags rootFlags, err error) {
	if err == nil {
		return
	}
	if flags.agent {
		meta := agentMeta(&flags)
		meta.Recovery = ""
		typed := classifyAgentError(err)
		var bindingSelection *accountBindingSelectionError
		if errors.As(err, &bindingSelection) {
			typed = bindingSelection.AgentError
		} else if flags.agentCapability == agentHistoryRecovery {
			typed = classifyHistoryAgentError(err, flags.agentHistoryAttemptID)
		} else if flags.agentCapability == agentOutboundSend {
			typed = classifyOutboundActionError(err, flags.agentOutboundRequest)
		} else if flags.agentCapability == agentChatState {
			typed = classifyChatStateAgentError(err, flags.agentChatStateRequest)
		} else if flags.agentCapability == agentMediaRecovery {
			typed = classifyMediaRetryCommandError(err)
		} else if flags.agentCapability == agentMediaTranscription {
			typed = classifyTranscriptionCommandError(err)
		} else if flags.agentCapability == agentMediaDownload || flags.agentCapability == agentMediaRead {
			typed = classifyMediaCommandError(err)
		}
		_ = out.WriteAgentError(os.Stderr, flags.agentAccount, meta, typed)
		return
	}
	var cleanup *out.AgentError
	if errors.As(err, &cleanup) && cleanup.Cleanup != nil {
		if flags.asJSON {
			_ = out.WriteDraftCleanupError(os.Stderr, cleanup)
			return
		}
		c := cleanup.Cleanup
		err = fmt.Errorf("%s [%s; draft=%s revision=%s hash=%s effect=%s outcome=%s directory_sync=%s]", cleanup.Message, cleanup.Code, c.DraftID, c.RevisionID, c.Hash, c.Effect, c.Outcome, c.DirectorySync)
	}
	if flags.events {
		_ = out.NewEventWriter(os.Stderr, true).Emit("error", map[string]any{"message": err.Error()})
		return
	}
	_ = out.WriteError(os.Stderr, flags.asJSON, err)
}

// newReadApp opens an existing local archive without a writer lock or session upgrades.
// Keep this explicit: lock-free operations such as media downloads have other semantics.
func newReadApp(ctx context.Context, flags *rootFlags) (*app.App, *lock.Lock, error) {
	readFlags := *flags
	readFlags.readOnly = true
	a, lk, err := newApp(ctx, &readFlags, false, true)
	if flags.agent && err != nil {
		err = agentStoreError(err)
	}
	return a, lk, err
}

func newApp(ctx context.Context, flags *rootFlags, needLock bool, allowUnauthed bool) (*app.App, *lock.Lock, error) {
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return nil, nil, err
	}

	var lk *lock.Lock
	if needLock {
		lk, err = lock.AcquireWithTimeout(ctx, storeDir, flags.lockWait)
		if err != nil {
			return nil, nil, err
		}
	}

	events := out.NewEventWriter(os.Stderr, flags.events)
	var waDiagnostics io.Writer
	if flags.agent && (flags.agentCapability == agentHistoryRecovery || flags.agentCapability == agentOutboundSend || flags.agentCapability == agentChatState || flags.agentCapability == agentMediaRecovery) {
		events = out.NewEventWriter(io.Discard, true)
		waDiagnostics = io.Discard
	}
	a, err := app.New(app.Options{
		StoreDir:           storeDir,
		Version:            effectiveVersion(),
		JSON:               flags.asJSON,
		Events:             events,
		WADiagnosticWriter: waDiagnostics,
		AllowUnauthed:      allowUnauthed,
		ReadOnly:           flags.isReadOnly(),
	})
	if err != nil {
		if lk != nil {
			_ = lk.Release()
		}
		return nil, nil, err
	}

	return a, lk, nil
}

func resolveStoreDir(flags *rootFlags) (string, error) {
	return resolveStoreDirWithConfig(flags, config.DefaultConfigPath())
}

func resolveStoreDirWithConfig(flags *rootFlags, configPath string) (string, error) {
	if flags != nil && flags.accountBinding.name != "" {
		ref := flags.accountBinding.storeRef
		if flags.agent {
			flags.agentAccount = out.AgentAccount{Name: flags.accountBinding.name, StoreRef: &ref}
		}
		return ref, nil
	}
	// Resolve an agent selection once so its envelope and opener cannot disagree
	// if the default account configuration changes during the invocation.
	if flags != nil && flags.agent && flags.agentAccount.StoreRef != nil {
		return *flags.agentAccount.StoreRef, nil
	}
	storeDir := ""
	account := ""
	if flags != nil {
		storeDir = flags.storeDir
		account = strings.TrimSpace(flags.account)
	}
	if storeDir != "" && account != "" {
		return "", fmt.Errorf("--store and --account cannot be combined")
	}
	switch {
	case storeDir != "":
	case account != "":
		resolved, _, err := config.ResolveAccountStore(configPath, account)
		if err != nil {
			return "", err
		}
		storeDir = resolved
	case os.Getenv(config.EnvStoreDir) != "":
		storeDir = config.DefaultStoreDir()
	default:
		cfg, found, err := config.LoadAccountsConfigIfExists(configPath)
		if err != nil {
			return "", err
		}
		if found && strings.TrimSpace(cfg.DefaultAccount) != "" {
			account = strings.TrimSpace(cfg.DefaultAccount)
			resolved, _, err := config.ResolveAccountStore(configPath, cfg.DefaultAccount)
			if err != nil {
				return "", err
			}
			storeDir = resolved
		} else {
			storeDir = config.DefaultStoreDir()
		}
	}
	storeDir, err := filepath.Abs(storeDir)
	if err != nil {
		return "", err
	}
	if flags != nil && flags.agent {
		flags.agentAccount = out.AgentAccount{StoreRef: &storeDir, Name: account}
	}
	return storeDir, nil
}

func (f *rootFlags) isReadOnly() bool {
	if f == nil {
		return false
	}
	if f.readOnly {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WACLI_READONLY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (f *rootFlags) requireWritable() error {
	if f.isReadOnly() {
		return fmt.Errorf("read-only mode: command would intentionally modify WhatsApp or the local store")
	}
	return nil
}

func withTimeout(ctx context.Context, flags *rootFlags) (context.Context, context.CancelFunc) {
	if flags.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, flags.timeout)
}

func closeApp(a *app.App, lk *lock.Lock) {
	if a != nil {
		a.Close()
	}
	if lk != nil {
		_ = lk.Release()
	}
}
