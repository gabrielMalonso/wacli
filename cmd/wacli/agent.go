package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const agentMaxResults = 200

type agentCapability uint8

const (
	agentUnsupported agentCapability = iota
	agentLocalRead
	agentHistoryRecovery
	agentLocalDraftWrite
	agentOutboundSend
	agentChatState
	agentMediaDownload
	agentMediaRead
	agentMediaRecovery
)

// Recover output intent even if Cobra stops on an earlier parse error. Inspect
// flag definitions, consume their values (including literal "--agent"), and
// honor --. Recognize defined command names for action source without parsing
// values or executing hooks.
func agentFlagIntent(root *cobra.Command, args []string) (intent struct {
	agent, detailSet, cursorSet, help bool
	store, account, chat, chatAction  string
	capability                        agentCapability
}) {
	known := make(map[string]*pflag.Flag)
	short := make(map[byte]*pflag.Flag)
	var collect func(*cobra.Command)
	collect = func(cmd *cobra.Command) {
		for _, fs := range []*pflag.FlagSet{cmd.Flags(), cmd.PersistentFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				known[f.Name] = f
				if len(f.Shorthand) == 1 {
					short[f.Shorthand[0]] = f
				}
			})
		}
		for _, c := range cmd.Commands() {
			collect(c)
		}
	}
	collect(root)
	node := root
	for i := 0; i < len(args); i++ {
		token := args[i]
		if !strings.HasPrefix(token, "-") {
			for _, child := range node.Commands() {
				if child.Name() == token {
					node = child
					intent.capability = agentCommandCapability(node)
					if intent.capability == agentChatState {
						intent.chatAction = node.Name()
					}
					break
				}
			}
		}
		if token == "--" {
			break
		}
		if token == "-h" || token == "--help" || token == "--help=true" {
			intent.help = true
			continue
		}
		var f *pflag.Flag
		value, hasValue := "", false
		if strings.HasPrefix(token, "--") {
			name, val, found := strings.Cut(token[2:], "=")
			f = known[name]
			value, hasValue = val, found
		} else if strings.HasPrefix(token, "-") && len(token) > 1 {
			for j := 1; j < len(token); j++ {
				f = short[token[j]]
				if f == nil {
					break
				}
				if f.NoOptDefVal == "" {
					if j+1 < len(token) {
						value = strings.TrimPrefix(token[j+1:], "=")
						hasValue = true
					}
					break
				}
			}
		}
		if f == nil {
			continue
		}
		if !hasValue {
			if f.NoOptDefVal != "" {
				value = f.NoOptDefVal
			} else if i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		switch f.Name {
		case "agent":
			enabled, err := strconv.ParseBool(value)
			intent.agent = enabled || err != nil
		case "cursor":
			intent.cursorSet = true
		case "detail":
			intent.detailSet = true
		case "store":
			intent.store = value
		case "account":
			intent.account = value
		case "chat":
			intent.chat = value
		}
	}
	return intent
}

func installAgentGuards(root *cobra.Command, flags *rootFlags) {
	if !flags.agent {
		return
	}
	var install func(*cobra.Command)
	install = func(cmd *cobra.Command) {
		if flags.agent && cmd.RunE == nil && cmd.Run == nil {
			cmd.RunE = func(c *cobra.Command, args []string) error { return c.Help() }
		}
		run := cmd.RunE
		if run != nil {
			cmd.RunE = func(c *cobra.Command, args []string) error { flags.agentRunStarted = true; return run(c, args) }
		}
		original := cmd.Args
		cmd.Args = func(c *cobra.Command, args []string) error {
			if !flags.agent {
				if original != nil {
					return original(c, args)
				}
				return nil
			}
			if c == root {
				if len(args) > 0 {
					return agentUsageError(fmt.Errorf("unknown command %q", args[0]))
				}
				return nil
			}
			if c.Name() == "version" || c.Name() == "help" {
				if original != nil {
					return original(c, args)
				}
				return nil
			}
			flags.agentCapability = agentCommandCapability(c)
			if flags.agentCapability == agentUnsupported {
				return &out.AgentError{Code: "unsupported_command", Message: c.CommandPath() + " is not supported with --agent", ExitCode: 2}
			}
			if original != nil {
				if err := original(c, args); err != nil {
					return agentUsageError(err)
				}
			} else if len(args) > 0 {
				return agentUsageError(fmt.Errorf("%s accepts no positional arguments", c.CommandPath()))
			}
			return validateAgentCommand(c, args, flags)
		}
		for _, child := range cmd.Commands() {
			install(child)
		}
	}
	install(root)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		if flags.agent {
			return agentUsageError(err)
		}
		return err
	})
}

func agentCommandCapability(cmd *cobra.Command) agentCapability {
	switch strings.TrimPrefix(cmd.CommandPath(), "wacli ") {
	case "messages list", "messages search", "messages show", "messages context", "chats list", "chats show", "contacts list", "contacts search", "contacts show", "contacts resolve", "history coverage", "auth status":
		return agentLocalRead
	case "draft show", "draft list", "draft cleanup preview", "outbound show", "outbound list":
		return agentLocalRead
	case "media status":
		return agentMediaRead
	case "media download":
		return agentMediaDownload
	case "media retry":
		return agentMediaRecovery
	case "draft create", "draft update", "draft discard", "draft cleanup apply":
		return agentLocalDraftWrite
	case "history backfill":
		return agentHistoryRecovery
	case "outbound send":
		return agentOutboundSend
	case "chats mark-unread", "chats archive", "chats unarchive":
		return agentChatState
	case "doctor":
		connect, _ := cmd.Flags().GetBool("connect")
		if !connect {
			return agentLocalRead
		}
	default:
		return agentUnsupported
	}
	return agentUnsupported
}

func validateAgentCommand(cmd *cobra.Command, args []string, flags *rootFlags) error {
	usage := func(err error) error { return agentUsageError(err) }
	if flags.detail != "compact" && flags.detail != "full" {
		return usage(fmt.Errorf("--detail must be compact or full"))
	}
	if flags.events {
		return usage(fmt.Errorf("--events cannot be combined with --agent"))
	}
	if flags.storeDir != "" && flags.account != "" {
		return usage(fmt.Errorf("--store and --account cannot be combined"))
	}
	if flags.account != "" {
		if err := config.ValidateAccountName(flags.account); err != nil {
			return usage(err)
		}
	}
	if flags.agentCapability != agentMediaRecovery && cmd.Flags().Lookup("limit") != nil {
		if !cmd.Flags().Changed("limit") {
			_ = cmd.Flags().Set("limit", "20")
		}
		limit, _ := cmd.Flags().GetInt("limit")
		if limit < 1 || limit > agentMaxResults {
			return usage(fmt.Errorf("--limit must be between 1 and %d", agentMaxResults))
		}
	}
	path := strings.TrimPrefix(cmd.CommandPath(), "wacli ")
	if path == "media retry" {
		if flags.isReadOnly() {
			return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects explicit media recovery.", ExitCode: 2}
		}
		for _, name := range []string{"before", "limit", "batch"} {
			if cmd.Flags().Changed(name) {
				return usage(fmt.Errorf("bulk flags cannot select exact media recovery"))
			}
		}
		wait, _ := cmd.Flags().GetDuration("wait")
		if wait < time.Second || wait > 120*time.Second || flags.timeout < time.Second || flags.timeout > 5*time.Minute {
			return usage(fmt.Errorf("media retry requires --wait 1s..120s and --timeout 1s..5m"))
		}
	}
	if path == "media status" || path == "media download" || path == "media retry" {
		chat, _ := cmd.Flags().GetString("chat")
		id, _ := cmd.Flags().GetString("id")
		if err := validateAgentMediaSelection(chat, id); err != nil {
			return usage(err)
		}
		output, _ := cmd.Flags().GetString("output")
		if len(output) > 4096 || strings.Contains(output, "://") || strings.IndexFunc(output, unicode.IsControl) >= 0 {
			return usage(fmt.Errorf("--output requires a local filesystem path without controls"))
		}
		if path == "media download" || path == "media retry" {
			if strings.TrimSpace(output) == "" {
				return usage(fmt.Errorf("--output is required with --agent media download"))
			}
		}
		if flags.timeout <= 0 || flags.timeout > 5*time.Minute {
			return usage(fmt.Errorf("media --timeout must be positive and at most 5m"))
		}
		if _, err := outboundMediaRoots(); err != nil {
			return usage(fmt.Errorf("invalid WACLI_MEDIA_ROOTS configuration"))
		}
	}
	if flags.agentCapability == agentChatState {
		if flags.isReadOnly() {
			return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects chat state mutations.", ExitCode: 2}
		}
		chat, _ := cmd.Flags().GetString("chat")
		if _, err := store.NormalizeDraftTarget(chat); err != nil || cmd.Flags().Changed("pick") {
			return usage(fmt.Errorf("--chat requires an explicit phone/DM/group JID; --pick is not supported with --agent"))
		}
	}
	if path == "outbound send" {
		if flags.isReadOnly() {
			return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects outbound dispatch.", ExitCode: 2}
		}
		revision, _ := cmd.Flags().GetString("revision")
		hash, _ := cmd.Flags().GetString("expect-hash")
		key, _ := cmd.Flags().GetString("key")
		if err := app.ValidateOutboundSelection(args[0], revision, hash, key); err != nil {
			return usage(err)
		}
	}
	if path == "history backfill" {
		if flags.isReadOnly() {
			return &out.AgentError{Code: "read_only", Message: "Read-only policy rejects history recovery.", ExitCode: 2}
		}
		chat, _ := cmd.Flags().GetString("chat")
		count, _ := cmd.Flags().GetInt("count")
		requests, _ := cmd.Flags().GetInt("requests")
		wait, _ := cmd.Flags().GetDuration("wait")
		idle, _ := cmd.Flags().GetDuration("idle-exit")
		if _, err := app.PrepareBackfillOptions(app.BackfillOptions{ChatJID: chat, Count: count, Requests: requests, WaitPerRequest: wait, IdleExit: idle}); err != nil {
			return usage(fmt.Errorf("invalid history recovery options"))
		}
	}
	switch path {
	case "messages show", "messages context":
		chat, _ := cmd.Flags().GetString("chat")
		id, _ := cmd.Flags().GetString("id")
		if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
			return usage(fmt.Errorf("--chat and --id are required"))
		}
	case "chats show", "contacts show":
		jid, _ := cmd.Flags().GetString("jid")
		if strings.TrimSpace(jid) == "" {
			return usage(fmt.Errorf("--jid is required"))
		}
	case "contacts resolve":
		if len(args) > agentMaxResults {
			return usage(fmt.Errorf("contacts resolve accepts at most %d inputs/results", agentMaxResults))
		}
		for _, arg := range args {
			if r := resolveContactIdentity(cmd.Context(), nil, arg); r.Error != "" {
				return usage(errors.New(r.Error))
			}
		}
	case "messages search", "contacts search":
		if strings.TrimSpace(args[0]) == "" {
			return usage(fmt.Errorf("query is required"))
		}
	}
	if path == "messages context" {
		before, _ := cmd.Flags().GetInt("before")
		after, _ := cmd.Flags().GetInt("after")
		if before < 0 || after < 0 || before >= agentMaxResults || after >= agentMaxResults || before+after+1 > agentMaxResults {
			return usage(fmt.Errorf("context before/after must be nonnegative and total before+after+1 at most %d", agentMaxResults))
		}
	} else if strings.HasPrefix(path, "messages ") {
		after, _ := cmd.Flags().GetString("after")
		before, _ := cmd.Flags().GetString("before")
		if _, _, err := messageTimeBounds(after, before); err != nil {
			return usage(err)
		}
	}
	for _, name := range []string{"chat", "jid", "sender", "from"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		values := []string{}
		if f.Value.Type() == "stringSlice" {
			values, _ = cmd.Flags().GetStringSlice(name)
		} else {
			value, _ := cmd.Flags().GetString(name)
			values = append(values, value)
		}
		for _, value := range values {
			if value != "" {
				if _, err := wa.ParseUserOrJID(value); err != nil {
					return usage(err)
				}
			}
		}
	}
	if path == "messages list" {
		if cmd.Flags().Changed("cursor") {
			if err := store.ValidateMessagesCursor(flags.cursor); err != nil {
				return classifyAgentError(err)
			}
		}
		me, _ := cmd.Flags().GetBool("from-me")
		them, _ := cmd.Flags().GetBool("from-them")
		if me && them {
			return usage(fmt.Errorf("--from-me and --from-them are mutually exclusive"))
		}
	}
	if path == "messages search" {
		sortBy, _ := cmd.Flags().GetString("sort")
		if sortBy != "relevance" && sortBy != "time" {
			return usage(fmt.Errorf("--sort must be relevance or time"))
		}
		if sortBy != "time" && cmd.Flags().Changed("cursor") {
			return usage(fmt.Errorf("search --cursor requires --sort time; relevance search cannot be paginated"))
		}
		if sortBy != "time" && cmd.Flags().Changed("asc") {
			return usage(fmt.Errorf("search --asc requires --sort time"))
		}
		if cmd.Flags().Changed("cursor") {
			if err := store.ValidateMessagesCursor(flags.cursor); err != nil {
				return classifyAgentError(err)
			}
		}
		typ, _ := cmd.Flags().GetString("type")
		typ = strings.ToLower(strings.TrimSpace(typ))
		media, _ := cmd.Flags().GetBool("has-media")
		switch typ {
		case "", "text", "image", "video", "audio", "document":
		default:
			return usage(fmt.Errorf("unsupported message type %q", typ))
		}
		if media && typ == "text" {
			return usage(fmt.Errorf("cannot combine --has-media with --type=text"))
		}
	}
	if path == "chats list" {
		if cmd.Flags().Changed("cursor") {
			if err := store.ValidateChatsCursor(flags.cursor); err != nil {
				return classifyAgentError(err)
			}
		}
		for _, name := range []string{"archived", "pinned", "muted", "unread"} {
			yes, _ := cmd.Flags().GetBool(name)
			no, _ := cmd.Flags().GetBool("no-" + name)
			if err := validateBoolFilter(name, yes, no); err != nil {
				return usage(err)
			}
		}
	}
	if (path == "contacts list" || path == "contacts search") && cmd.Flags().Changed("cursor") {
		if err := app.ValidateContactsCursor(flags.cursor); err != nil {
			return classifyAgentError(err)
		}
	}

	if path == "history coverage" {
		kind, _ := cmd.Flags().GetString("kind")
		switch kind {
		case "", "dm", "group", "broadcast", "newsletter", "unknown":
		default:
			return usage(fmt.Errorf("unsupported chat kind %q", kind))
		}
	}
	if _, err := resolveStoreDir(flags); err != nil {
		return agentStoreError(err)
	}
	return nil
}

func agentUsageError(err error) *out.AgentError {
	return &out.AgentError{Code: "invalid_arguments", Message: err.Error(), ExitCode: 2, Cause: err}
}
func agentStoreError(err error) *out.AgentError {
	message := "Selected local archive or configuration is unreadable or incompatible."
	recovery := "Inspect offline with doctor without --agent or --connect; inspect session observations separately with auth status --agent."
	switch {
	case errors.Is(err, os.ErrNotExist):
		message = "Selected local archive or configuration is missing."
		recovery = "Select an existing initialized archive with --store or --account."
	case errors.Is(err, os.ErrPermission):
		message = "Selected local archive or configuration is not readable with current permissions."
		recovery = "Check local read permissions for the selected archive and account configuration."
	}
	return &out.AgentError{Code: "store_unavailable", Message: message, Recovery: recovery, ExitCode: 4, Cause: err}
}
func classifyAgentError(err error) *out.AgentError {
	var typed *out.AgentError
	if errors.As(err, &typed) {
		return typed
	}
	var cursor *store.MessagesCursorError
	if errors.As(err, &cursor) {
		return &out.AgentError{Code: "invalid_cursor", Message: cursor.Error(), Recovery: "Restart messages list/search without --cursor using the selected archive and filters.", ExitCode: 2, Cause: err}
	}
	var chatsCursor *store.ChatsCursorError
	if errors.As(err, &chatsCursor) {
		return &out.AgentError{Code: "invalid_cursor", Message: chatsCursor.Error(), Recovery: "Restart chats list without --cursor using the selected archive and filters.", ExitCode: 2, Cause: err}
	}
	var contactsCursor *app.ContactsCursorError
	if errors.As(err, &contactsCursor) {
		return &out.AgentError{Code: "invalid_cursor", Message: contactsCursor.Error(), Recovery: "Restart contacts list/search without --cursor using the selected archive and query.", ExitCode: 2, Cause: err}
	}
	var contactIdentity *app.ContactIdentityError
	if errors.As(err, &contactIdentity) {
		return &out.AgentError{Code: "store_unavailable", Message: "Selected local identity state cannot be read.", ExitCode: 4, Cause: err}
	}

	var identity *localIdentityError
	if errors.As(err, &identity) {
		return &out.AgentError{Code: "store_unavailable", Message: "Selected local identity state cannot be read.", ExitCode: 4, Cause: err}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &out.AgentError{Code: "not_found", Message: "Requested item was not found in the selected local archive.", ExitCode: 3, Cause: err}
	}
	return &out.AgentError{Code: "internal_error", Message: "Local query failed.", ExitCode: 1, Cause: err}
}
func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var typed *out.AgentError
	if errors.As(err, &typed) {
		return typed.ExitCode
	}
	return 1
}
func agentMeta(flags *rootFlags) out.AgentMeta {
	detail := flags.detail
	if detail != "full" {
		detail = "compact"
	}
	source := "local"
	if flags.agentCapability == agentHistoryRecovery || flags.agentCapability == agentOutboundSend || flags.agentCapability == agentChatState || flags.agentCapability == agentMediaDownload || flags.agentCapability == agentMediaRecovery {
		source = "live"
	}
	meta := out.AgentMeta{Source: source, Detail: detail, Completeness: "unknown", Freshness: "unknown"}
	if detail == "compact" {
		meta.Recovery = "Use --detail full; retrieve one message with messages show --chat CHAT_JID --id ID --detail full."
	}
	return meta
}
