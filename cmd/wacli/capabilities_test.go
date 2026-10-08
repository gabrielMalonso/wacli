package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func readCapabilities(t *testing.T, args ...string) (capabilitiesData, string) {
	t.Helper()
	stdout, stderr, err := runAgentTest(t, args...)
	if err != nil || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("discovery err=%v stdout=%s stderr=%s", err, stdout, stderr)
	}
	env := decodeAgentTest(t, stdout)
	if !env.Success || env.Account.StoreRef != nil || env.Account.Name != "" || env.Meta.Source != "local" || env.Meta.Freshness != "unknown" || env.Meta.Completeness != "unknown" {
		t.Fatalf("discovery implied account knowledge: %s", stdout)
	}
	var data capabilitiesData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data, stdout
}

func TestCapabilitiesDoesNotOpenConfigStoreOrSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WACLI_READONLY", "1")
	t.Setenv("WACLI_STORE_DIR", filepath.Join(dir, "absent"))
	if runtime.GOOS == "linux" {
		t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
		cfg := config.DefaultConfigPath()
		if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte("PRIVATE_CONFIG: [invalid yaml"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotLocalStore(t, dir)
	data, raw := readCapabilities(t, "--agent", "--read-only", "capabilities")
	if data.AccountAvailability != "unknown" || data.CLIVersion != effectiveVersion() || data.AgentContract.SchemaVersion != out.AgentSchemaVersion {
		t.Fatalf("incorrect versions/availability: %s", raw)
	}
	if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
		t.Fatal("discovery created or changed files")
	}
	t.Setenv("WACLI_STORE_DIR", "")
	defaultData, _ := readCapabilities(t, "capabilities", "--agent")
	if !reflect.DeepEqual(data, defaultData) {
		t.Fatal("discovery depends on default account configuration")
	}
	// Neither a corrupt session nor an incompatible archive may be inspected or
	// migrated. The catalog is independent of all such invocation-local state.
	for _, file := range []string{"session.db", "wacli.db", "LOCK", "HEARTBEAT"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("PRIVATE_FIXTURE"), 0400); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WACLI_STORE_DIR", dir)
	before = snapshotLocalStore(t, dir)
	full, fullRaw := readCapabilities(t, "capabilities", "--agent", "--json", "--detail", "full")
	if !reflect.DeepEqual(data, full) || strings.Contains(raw+fullRaw, dir) || strings.Contains(raw+fullRaw, "PRIVATE_") {
		t.Fatal("discovery depends on or reveals local state")
	}
	if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
		t.Fatal("discovery modified unreadable fixture")
	}
	archive := seedLocalReadStore(t)
	lk, err := lock.Acquire(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	t.Setenv("WACLI_STORE_DIR", archive)
	before = snapshotLocalStore(t, archive)
	locked, _ := readCapabilities(t, "capabilities", "--agent")
	if !reflect.DeepEqual(data, locked) || !reflect.DeepEqual(snapshotLocalStore(t, archive), before) {
		t.Fatal("discovery inspected or modified locked archive/old session")
	}
}

func TestCapabilitiesCatalogMatchesExecutablePolicy(t *testing.T) {
	data, _ := readCapabilities(t, "capabilities", "--agent")
	commands := make(map[string]capabilityCommand)
	for i, c := range data.Commands {
		if i > 0 && data.Commands[i-1].Command >= c.Command {
			t.Fatal("catalog must be sorted with unique canonical commands")
		}
		commands[c.Command] = c
	}
	for _, path := range []string{"capabilities", "auth status", "doctor", "sync status", "messages list", "messages search", "messages show", "messages context", "changes list", "changes watch", "chats list", "chats show", "contacts list", "contacts search", "contacts show", "contacts resolve", "history coverage", "history backfill", "draft create", "draft update", "draft discard", "draft show", "draft list", "draft cleanup preview", "draft cleanup apply", "outbound send", "outbound list", "outbound show", "media status", "media download", "media retry", "media transcribe", "chats mark-read", "chats mark-unread", "chats archive", "chats unarchive"} {
		c, ok := commands[path]
		if !ok || c.AgentMode != "supported" || c.Capability == "" || c.ReadOnly == nil || (path != "capabilities" && len(c.Requirements) == 0) {
			t.Fatalf("missing supported policy for %s: %+v", path, c)
		}
	}
	for _, args := range [][]string{{"auth"}, {"sync"}, {"send", "text"}, {"history", "fill"}, {"chats", "pin"}, {"messages", "purge"}, {"completion", "bash"}} {
		path := strings.Join(args, " ")
		if commands[path].AgentMode != "unsupported" || commands[path].Capability != "" {
			t.Fatalf("advertised unsupported command %s", path)
		}
		stdout, stderr, err := runAgentTest(t, append([]string{"--agent", "--read-only"}, args...)...)
		if stdout != "" || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "unsupported_command" {
			t.Fatalf("catalog/execution mismatch for %s: %v %s", path, err, stderr)
		}
	}
	if commands["help"].AgentMode != "text" || commands["version"].AgentMode != "text" || !slices.Contains(commands["media retry"].Requirements, "writer_lock") || slices.Contains(commands["media retry"].Requirements, "writer_lock_or_compatible_owner") {
		t.Fatal("incorrect helper or standalone policy")
	}
	watch := commands["changes watch"]
	if watch.Encoding != "ndjson" || watch.Capability != "local_read" || watch.Source != "local" || !*watch.ReadOnly || !strings.Contains(strings.Join(watch.Constraints, " "), "next_cursor") {
		t.Fatalf("incorrect watch stream policy: %+v", watch)
	}
	if commands["sync status"].Source != "live" || !*commands["sync status"].ReadOnly || commands["sync status"].Capability != "sync_status" {
		t.Fatal("incorrect sync status policy")
	}
	statusConstraints := strings.Join(commands["sync status"].Constraints, " ")
	if !strings.Contains(statusConstraints, "owner_ready") || !strings.Contains(statusConstraints, "transport_connected") || !strings.Contains(statusConstraints, "current_authentication_unsupported") {
		t.Fatal("discovery omits local readiness or pinned authentication gap")
	}
	if *commands["outbound send"].ReadOnly || !*commands["media download"].ReadOnly || !*commands["media transcribe"].ReadOnly || commands["outbound send"].Source != "live" || commands["media transcribe"].Source != "local" {
		t.Fatal("incorrect effect policy")
	}
	if len(commands["doctor"].Constraints) == 0 || len(commands["outbound send"].Constraints) == 0 || len(commands["changes list"].Constraints) == 0 {
		t.Fatal("missing important restrictions")
	}
	// Commands added to Cobra are discovered without another legacy whitelist.
	root := &cobra.Command{Use: "wacli"}
	root.AddCommand(&cobra.Command{Use: "future", Run: func(*cobra.Command, []string) {}}, &cobra.Command{Use: "hidden", Hidden: true, Run: func(*cobra.Command, []string) {}})
	future := discoverCapabilities(root)
	if len(future.Commands) != 1 || future.Commands[0].Command != "future" || future.Commands[0].AgentMode != "unsupported" {
		t.Fatalf("tree discovery failed: %+v", future.Commands)
	}
}

func TestCapabilitiesFormatsAndUsageErrors(t *testing.T) {
	data, _ := readCapabilities(t, "--agent", "capabilities")
	stdout, stderr, err := runAgentTest(t, "capabilities", "--json", "--agent=false")
	var legacy struct {
		Success bool             `json:"success"`
		Data    capabilitiesData `json:"data"`
		Error   *string          `json:"error"`
	}
	if err != nil || stderr != "" || json.Unmarshal([]byte(stdout), &legacy) != nil || !legacy.Success || legacy.Error != nil || !reflect.DeepEqual(data, legacy.Data) || strings.Contains(stdout, `"schema_version":1,"success"`) {
		t.Fatalf("incorrect legacy envelope: %v %s %s", err, stdout, stderr)
	}
	stdout, stderr, err = runAgentTest(t, "capabilities")
	if err != nil || stderr != "" || !strings.Contains(stdout, "COMMAND") || !strings.Contains(stdout, "outbound send") {
		t.Fatalf("incorrect human output: %v %s %s", err, stdout, stderr)
	}
	for _, args := range [][]string{{"capabilities", "extra"}, {"capabilities", "--unknown"}, {"capabilities", "--events"}, {"capabilities", "--detail", "bad"}, {"capabilities", "--cursor", "bad"}, {"capabilities", "--store", "PRIVATE_PATH"}, {"capabilities", "--store="}, {"capabilities", "--account", "private-account"}, {"capabilities", "--account="}, {"-a", "private-account", "capabilities"}, {"help", "capabilities", "-a", "private-account"}} {
		stdout, stderr, err = runAgentTest(t, append([]string{"--agent"}, args...)...)
		env := decodeAgentTest(t, stderr)
		if stdout != "" || commandExitCode(err) != 2 || env.Error.Code != "invalid_arguments" || env.Account.StoreRef != nil || env.Account.Name != "" || strings.Contains(stderr, "PRIVATE_PATH") {
			t.Fatalf("incorrect discovery refusal: args=%v err=%v stdout=%s stderr=%s", args, err, stdout, stderr)
		}
	}
	for _, args := range [][]string{{"--agent", "capabilities", "--help"}, {"--agent", "help", "capabilities"}, {"--agent", "--version"}, {"--agent", "version"}} {
		stdout, stderr, err = runAgentTest(t, args...)
		if err != nil || stderr != "" || stdout == "" || strings.Contains(stdout, "schema_version") {
			t.Fatalf("changed textual helper: %v %s %s", err, stdout, stderr)
		}
	}
}

func TestCapabilitiesMainExitAndBrokenPipe(t *testing.T) {
	if os.Getenv("WACLI_CAPABILITIES_TEST_PROCESS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"wacli"}, os.Args[i+1:]...)
				main()
				os.Exit(0)
			}
		}
		t.Fatal("missing subprocess arguments")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		exit int
	}{{[]string{"capabilities", "--agent"}, 0}, {[]string{"capabilities", "--agent", "--events"}, 2}} {
		cmd := exec.Command(exe, append([]string{"-test.run=^TestCapabilitiesMainExitAndBrokenPipe$", "--"}, tc.args...)...)
		cmd.Env = append(os.Environ(), "WACLI_CAPABILITIES_TEST_PROCESS=1")
		raw, err := cmd.CombinedOutput()
		exit := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if exit != tc.exit || decodeAgentTest(t, string(raw)).Success != (exit == 0) {
			t.Fatalf("main exit mismatch %d: %s", exit, raw)
		}
	}
	if runtime.GOOS == "windows" {
		return // Platform-specific broken-pipe handling is covered in internal/out.
	}
	for _, mode := range []string{"--agent", "--json"} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		original := os.Stdout
		os.Stdout = w
		stderr := captureRootStderr(t, func() { err = execute([]string{"capabilities", mode}) })
		os.Stdout = original
		_ = w.Close()
		if err != nil || stderr != "" {
			t.Fatalf("query broken pipe changed: %v %s", err, stderr)
		}
	}
}
