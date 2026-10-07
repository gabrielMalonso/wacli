package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

// Discovery describes this binary, not an account or a promise of remote support.
type capabilitiesData struct {
	CLIVersion          string              `json:"cli_version"`
	AccountAvailability string              `json:"account_availability"`
	AgentContract       capabilityContract  `json:"agent_contract"`
	Commands            []capabilityCommand `json:"commands"`
}

type capabilityContract struct {
	SchemaVersion   int            `json:"schema_version"`
	Versioning      string         `json:"versioning"`
	Encoding        string         `json:"encoding"`
	SuccessStream   string         `json:"success_stream"`
	ErrorStream     string         `json:"error_stream"`
	Details         []string       `json:"details"`
	MaxResults      int            `json:"max_results"`
	CompactMaxBytes int            `json:"compact_max_bytes"`
	FullMaxBytes    int            `json:"full_max_bytes"`
	ExitCodes       map[string]int `json:"exit_codes"`
}

type capabilityCommand struct {
	Command      string   `json:"command"`
	AgentMode    string   `json:"agent_mode"`
	Capability   string   `json:"capability,omitempty"`
	Source       string   `json:"source,omitempty"`
	ReadOnly     *bool    `json:"read_only,omitempty"`
	Requirements []string `json:"requirements,omitempty"`
	Constraints  []string `json:"constraints,omitempty"`
}

func newCapabilitiesCmd(flags *rootFlags, data *capabilitiesData) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Discover static CLI and agent capabilities without opening an account",
		Long:  "List this binary's commands, agent support, static requirements and contract limits.\nAccount availability remains unknown. No config, store or session is opened.\nRun without --account, --for-account or --store; select an account separately for operations.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateCapabilitiesSelectors(cmd); err != nil {
				return err
			}
			if flags.agent {
				return out.WriteAgentJSON(os.Stdout, out.AgentAccount{}, agentMeta(flags), *data)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, *data)
			}
			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "COMMAND\tAGENT\tCAPABILITY")
			for _, c := range data.Commands {
				fmt.Fprintf(w, "%s\t%s\t%s\n", c.Command, c.AgentMode, c.Capability)
			}
			return w.Flush()
		},
	}
}

func validateCapabilitiesSelectors(cmd *cobra.Command) error {
	if cmd.Flags().Changed("account") || cmd.Flags().Changed("store") {
		return agentUsageError(fmt.Errorf("capabilities is global; omit account/store selectors"))
	}
	return nil
}

func discoverCapabilities(root *cobra.Command) capabilitiesData {
	data := capabilitiesData{
		CLIVersion: effectiveVersion(), AccountAvailability: "unknown",
		AgentContract: capabilityContract{
			SchemaVersion: out.AgentSchemaVersion, Versioning: "additive_within_version",
			Encoding: "single_json_line", SuccessStream: "stdout", ErrorStream: "stderr",
			Details: []string{"compact", "full"}, MaxResults: agentMaxResults,
			CompactMaxBytes: out.AgentCompactMaxBytes, FullMaxBytes: out.AgentFullMaxBytes,
			ExitCodes: map[string]int{"success": 0, "operational": 1, "usage_policy": 2, "not_found": 3, "store": 4},
		},
		Commands: []capabilityCommand{},
	}
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		if cmd != root && cmd.Runnable() {
			path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
			entry := capabilityCommand{Command: path, AgentMode: "unsupported"}
			capability := agentCommandCapability(cmd)
			if capability != agentUnsupported {
				entry.AgentMode = "supported"
				entry.Source = agentCapabilitySource(capability)
				entry.Capability, entry.Requirements, entry.ReadOnly = capabilityRequirements(capability)
				entry.Constraints = capabilityConstraints(path)
				switch path {
				case "auth status":
					entry.Requirements = []string{"existing_store_directory", "readable_local_auth_state"}
				case "draft cleanup apply":
					entry.Requirements = []string{"existing_compatible_archive", "writer_lock_or_compatible_owner"}
				case "doctor":
					entry.Requirements = []string{"existing_compatible_archive", "readable_local_auth_state"}
				}
			} else if path == "help" || path == "version" {
				entry.AgentMode = "text"
			}
			data.Commands = append(data.Commands, entry)
		}
		for _, child := range cmd.Commands() {
			visit(child)
		}
	}
	visit(root)
	slices.SortFunc(data.Commands, func(a, b capabilityCommand) int { return strings.Compare(a.Command, b.Command) })
	return data
}

func capabilityRequirements(capability agentCapability) (name string, requirements []string, readOnly *bool) {
	allowed := true
	readOnly = &allowed
	switch capability {
	case agentDiscovery:
		return "discovery", nil, readOnly
	case agentLocalRead:
		return "local_read", []string{"existing_compatible_archive"}, readOnly
	case agentMediaRead:
		return "media_status", []string{"existing_compatible_archive", "exact_media_reference"}, readOnly
	case agentMediaDownload:
		return "media_download", []string{"existing_compatible_archive", "exact_media_reference", "explicit_output"}, readOnly
	case agentMediaTranscription:
		return "media_transcription", []string{"explicit_local_input", "explicit_adapter_executable"}, readOnly
	case agentLocalDraftWrite:
		allowed = false
		return "local_draft_write", []string{"writable_archive", "writer_lock_or_compatible_owner"}, readOnly
	case agentHistoryRecovery:
		allowed = false
		return "history_recovery", []string{"writable_archive", "writer_lock_or_compatible_owner"}, readOnly
	case agentOutboundSend:
		name = "outbound_send"
	case agentChatState:
		allowed = false
		return "chat_state", []string{"writable_archive", "writer_lock_or_compatible_owner"}, readOnly
	case agentMediaRecovery:
		allowed = false
		return "media_recovery", []string{"existing_compatible_archive", "writer_lock", "exact_media_reference", "explicit_output"}, readOnly
	default:
		return "unknown", nil, nil
	}
	allowed = false
	return name, []string{"existing_compatible_archive", "writer_lock_or_compatible_owner"}, readOnly
}

// These describe important restrictions, not a replacement for command help and
// action-specific validation. In particular, live capability need not connect.
func capabilityConstraints(path string) []string {
	switch path {
	case "capabilities":
		return []string{"Global discovery; account/store selectors are rejected."}
	case "doctor":
		return []string{"--connect is unsupported with --agent; local and historical observations do not prove current liveness."}
	case "auth status":
		return []string{"Local authentication observation only; does not connect or prove current connectivity."}
	case "media download":
		return []string{"May use HTTP and write explicit output under --read-only; archive stays readonly; remote availability remains unknown."}
	case "media retry":
		return []string{"Standalone writer LOCK required; no owner delegation.", "Exact --chat/--id/--output required; bulk flags unsupported.", "Existing authenticated session required when network recovery is needed; cache reuse may avoid connection."}
	case "media transcribe":
		return []string{"No archive/session required; --read-only allows the explicit adapter, which is not sandboxed and may have external effects."}
	case "history backfill":
		return []string{"Requires local anchor and existing authenticated session or compatible owner; primary response is best-effort, not completeness."}
	case "outbound send":
		return []string{"Requires exact draft/revision/hash/key; text/contact/document/image/voice only.", "New dispatch requires existing authenticated session or compatible owner; a retained duplicate may resolve locally.", "Retained idempotency does not survive every archive loss/restore; never automatically resend after uncertainty."}
	case "chats mark-read":
		return []string{"Explicit phone/DM/group JID; --pick and --receipts unsupported; valid local message boundary required.", "Existing authenticated session or compatible owner required; --timeout positive and at most 5m."}
	case "chats mark-unread", "chats archive", "chats unarchive":
		return []string{"Explicit phone/DM/group JID; --pick unsupported; existing authenticated session or compatible owner required."}
	case "draft create", "draft update":
		return []string{"Requires locally observed public account/recipient identity; no network send or approval is implied."}
	case "draft cleanup apply":
		return []string{"Exact eligible document revision/hash and current head required; removes local snapshot bytes only."}
	case "changes list":
		return []string{"Save next_cursor even on empty/final pages; local references begin at schema 33, not a remote completeness or universal restore guarantee."}
	}
	return nil
}
