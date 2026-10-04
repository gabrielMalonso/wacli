package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const agentMaxResults = 200

// Recover output intent even if Cobra stops on an earlier parse error. Inspect
// flag definitions, consume their values (including literal "--agent"), and
// honor --. This does not parse commands or execute any hooks.
func agentFlagIntent(root *cobra.Command, args []string) (intent struct {
	agent, detailSet, help bool
	store, account         string
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
	for i := 0; i < len(args); i++ {
		token := args[i]
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
		case "detail":
			intent.detailSet = true
		case "store":
			intent.store = value
		case "account":
			intent.account = value
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
			if !agentSupported(c) {
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

func agentSupported(cmd *cobra.Command) bool {
	switch strings.TrimPrefix(cmd.CommandPath(), "wacli ") {
	case "messages list", "messages search", "messages show", "messages context", "chats list", "chats show", "contacts search", "contacts show", "contacts resolve", "history coverage", "auth status":
		return true
	case "doctor":
		connect, _ := cmd.Flags().GetBool("connect")
		return !connect
	default:
		return false
	}
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
	if cmd.Flags().Lookup("limit") != nil {
		if !cmd.Flags().Changed("limit") {
			_ = cmd.Flags().Set("limit", "20")
		}
		limit, _ := cmd.Flags().GetInt("limit")
		if limit < 1 || limit > agentMaxResults {
			return usage(fmt.Errorf("--limit must be between 1 and %d", agentMaxResults))
		}
	}
	path := strings.TrimPrefix(cmd.CommandPath(), "wacli ")
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
		me, _ := cmd.Flags().GetBool("from-me")
		them, _ := cmd.Flags().GetBool("from-them")
		if me && them {
			return usage(fmt.Errorf("--from-me and --from-them are mutually exclusive"))
		}
	}
	if path == "messages search" {
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
		for _, name := range []string{"archived", "pinned", "muted", "unread"} {
			yes, _ := cmd.Flags().GetBool(name)
			no, _ := cmd.Flags().GetBool("no-" + name)
			if err := validateBoolFilter(name, yes, no); err != nil {
				return usage(err)
			}
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
	meta := out.AgentMeta{Source: "local", Detail: detail, Completeness: "unknown", Freshness: "unknown"}
	if detail == "compact" {
		meta.Recovery = "Use --detail full; retrieve one message with messages show --chat CHAT_JID --id ID --detail full."
	}
	return meta
}
