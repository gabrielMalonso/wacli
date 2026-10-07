//go:build linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

// XDG fixture selection is Linux-specific; other platforms use HOME for the
// registry. Keep fixtures in the checkout without overriding HOME or TMPDIR.
// Retain them as ignored evidence; no real session is opened.
func accountBindingFixture(t *testing.T) (string, string, string) {
	t.Helper()
	scratch, err := filepath.Abs(filepath.Join("..", "..", "dist", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratch, "account-binding-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("WACLI_STORE_DIR", "")
	t.Setenv("WACLI_READONLY", "0")
	cfgPath := config.DefaultConfigPath()
	a, b := filepath.Join(dir, "a % 雪"), filepath.Join(dir, "b")
	cfg := &config.AccountsConfig{DefaultAccount: "fixture-b", Accounts: map[string]config.AccountEntry{
		"fixture-a": {Store: a}, "fixture-b": {Store: b}, "same-path": {Store: a}, "missing-store": {Store: filepath.Join(dir, "absent")},
	}}
	if err = config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a, b} {
		db, err := store.Open(filepath.Join(path, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err = db.UpsertChat(localReadPN, "dm", "Synthetic account binding", time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		for i, text := range []string{"--account=fixture-b", "--store", "--for-account", "--agent", " Unicode 雪 🙂 'spaces' \n", "fixture"} {
			if err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadPN, MsgID: text, SenderJID: localReadPN, Text: text, Timestamp: time.Unix(int64(i+1), 0)}); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath, a, b
}

func TestAccountBindingFormsAndLegacy(t *testing.T) {
	_, a, b := accountBindingFixture(t)
	for _, prefix := range [][]string{
		{"-a", "fixture-a"}, {"-afixture-a"}, {"-a=fixture-a"}, {"--for-account", "fixture-a"}, {"--for-account=fixture-a"},
		{"--account=fixture-a", "-a", "fixture-a"}, {"-a", "fixture-a", "--account", "fixture-a", "--for-account=fixture-a"},
	} {
		for _, tail := range []bool{false, true} {
			args := []string{"--agent", "--read-only", "auth", "status"}
			if tail {
				args = append(args, prefix...)
			} else {
				args = append(append([]string{}, prefix...), args...)
			}
			stdout, stderr, err := runAgentTest(t, args...)
			if err != nil || stderr != "" {
				t.Fatal(args, err, stderr)
			}
			e := decodeAgentTest(t, stdout)
			if e.Account.Name != "fixture-a" || e.Account.StoreRef == nil || *e.Account.StoreRef != a {
				t.Fatal(stdout)
			}
		}
	}
	t.Setenv("WACLI_STORE_DIR", b)
	stdout, stderr, err := runAgentTest(t, "-a", "fixture-a", "--agent", "auth", "status")
	if err != nil || stderr != "" || decodeAgentTest(t, stdout).Account.Name != "fixture-a" {
		t.Fatal(err, stdout, stderr)
	}
	// No new binding: keep legacy last value, empty fallback and permissive trim.
	for _, args := range [][]string{
		{"--account", "fixture-a", "--account=fixture-b"}, {"--account="}, {"--account", " fixture-b "},
	} {
		_, stderr, err = runAgentTest(t, append(args, "--json", "auth", "status")...)
		if err != nil || stderr != "" {
			t.Fatal(args, err, stderr)
		}
	}
	t.Setenv("WACLI_STORE_DIR", "")
	stdout, _, err = runAgentTest(t, "--account", "fixture-a", "--account=fixture-b", "--agent", "auth", "status")
	if err != nil || decodeAgentTest(t, stdout).Account.Name != "fixture-b" {
		t.Fatal(stdout, err)
	}
	stdout, _, err = runAgentTest(t, "--account=", "--agent", "auth", "status")
	if err != nil || decodeAgentTest(t, stdout).Account.Name != "fixture-b" {
		t.Fatal(stdout, err)
	}
}

func TestAccountBindingConflictsBeforeEffects(t *testing.T) {
	cfg, a, b := accountBindingFixture(t)
	before := snapshotLocalStore(t, filepath.Dir(cfg))
	beforeA, beforeB := snapshotLocalStore(t, a), snapshotLocalStore(t, b)
	for _, prefix := range [][]string{
		{"-a", "fixture-a", "--account", "fixture-b"}, {"--account=fixture-b", "-a", "fixture-a"},
		{"--account=fixture-a", "--account=fixture-b", "--account=fixture-a", "-a", "fixture-a"},
		{"-a", "fixture-a", "--account=fixture-b", "--account=fixture-a"},
		{"-a", "fixture-a", "--account="}, {"--account=", "-a", "fixture-a"},
		{"-a", "fixture-a", "--for-account=fixture-b"}, {"-a", "fixture-a", "-a", "same-path"},
		{"-a", "fixture-a", "--account=same-path"}, {"-a", "fixture-a", "--store", a},
		{"--store=", "-a", "fixture-a"}, {"-a", "fixture-a", "--store="},
		{"--store", b, "-a", "fixture-a"}, {"--for-account="}, {"-a", ""}, {"-a", " fixture-a "},
		{"-a", "bad/name"}, {"-a", "雪"}, {"-a"},
	} {
		args := append([]string{"--agent"}, prefix...)
		if prefix[len(prefix)-1] != "-a" {
			args = append(args, "draft", "create", "--message-file", "-", "--to", localReadPN)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if stdout != "" || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatal(args, err, stdout, stderr)
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, filepath.Dir(cfg))) {
		t.Fatal("registry changed")
	}
	if !reflect.DeepEqual(beforeA, snapshotLocalStore(t, a)) || !reflect.DeepEqual(beforeB, snapshotLocalStore(t, b)) {
		t.Fatal("conflict touched an archive")
	}
	for _, path := range []string{a, b} {
		if _, err := os.Stat(filepath.Join(path, "LOCK")); !os.IsNotExist(err) {
			t.Fatal("preflight touched LOCK", err)
		}
	}
}

func TestAccountBindingSelectionErrorsAndHelp(t *testing.T) {
	cfg, _, b := accountBindingFixture(t)
	for _, command := range [][]string{{"auth", "status"}, {"--help"}, {"version"}, {"--version"}} {
		for _, name := range []string{"absent", "fixture-a"} {
			stdout, stderr, err := runAgentTest(t, append([]string{"--agent", "-a", name}, command...)...)
			if name == "absent" {
				if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
					t.Fatal(err, stdout, stderr)
				}
			} else if err != nil || stderr != "" || stdout == "" {
				t.Fatal(err, stdout, stderr)
			}
		}
	}
	// A parser-time selection refusal must not become an action preflight error.
	for _, command := range [][]string{{"history", "backfill"}, {"outbound", "send"}, {"chats", "archive"}, {"media", "download"}, {"media", "retry"}, {"media", "transcribe"}, {"draft", "create"}} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--agent", "-a", "absent"}, command...)...)
		if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
			t.Fatal(command, err, stdout, stderr)
		}
	}
	stdout, stderr, err := runAgentTest(t, "-a", "missing-store", "--agent", "messages", "list")
	if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
		t.Fatal(err, stderr)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(b), "absent")); !os.IsNotExist(err) {
		t.Fatal("missing store created", err)
	}
	// A missing registry must not be created, including before help/version.
	saved := cfg + ".fixture-retained"
	if err = os.Rename(cfg, saved); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runAgentTest(t, "-a", "fixture-a", "--agent", "--help")
	if commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
		t.Fatal(err, stderr)
	}
	if _, err = os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("missing config created", err)
	}
	if err = os.WriteFile(cfg, []byte("PRIVATE_CONFIG_SECRET: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runAgentTest(t, "-a", "fixture-a", "--agent", "auth", "status")
	if commandExitCode(err) != 4 || strings.Contains(stderr, "PRIVATE_CONFIG_SECRET") || strings.Contains(stderr, cfg) {
		t.Fatal(err, stderr)
	}
	stdout, stderr, err = runAgentTest(t, "-a", "fixture-a", "--json", "auth", "status")
	if commandExitCode(err) != 1 || stdout != "" || strings.Contains(stderr, "PRIVATE_CONFIG_SECRET") || strings.Contains(stderr, cfg) || strings.Contains(stderr, "schema_version") {
		t.Fatal(err, stdout, stderr)
	}
	// No binding: help/version must not resolve even malformed account config.
	for _, args := range [][]string{{"--help"}, {"--version"}, {"version"}, {"--agent", "--help"}} {
		stdout, stderr, err = runAgentTest(t, args...)
		if err != nil || stderr != "" || stdout == "" {
			t.Fatal(args, err, stdout, stderr)
		}
	}
}

func TestAccountBindingRegistryAndExistingGuards(t *testing.T) {
	cfg, a, _ := accountBindingFixture(t)
	before := snapshotLocalStore(t, filepath.Dir(cfg))
	for _, action := range [][]string{{"list"}, {"show", "fixture-b"}, {"use", "fixture-b"}, {"remove", "fixture-b"}, {"add", "new-fixture", "--no-auth"}, {"--help"}} {
		for _, agent := range []bool{false, true} {
			args := append([]string{"-a", "fixture-a", "accounts"}, action...)
			if agent {
				args = append([]string{"--agent"}, args...)
			} else {
				args = append([]string{"--json"}, args...)
			}
			stdout, stderr, err := runAgentTest(t, args...)
			if err == nil || stdout != "" {
				t.Fatal(args, err, stdout, stderr)
			}
			if agent && (commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments") {
				t.Fatal(err, stderr)
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, filepath.Dir(cfg))) {
		t.Fatal("registry changed")
	}
	for _, command := range [][]string{{"sync"}, {"send", "text", "--to", localReadPN, "--message", "--for-account"}} {
		_, stderr, err := runAgentTest(t, append([]string{"--agent", "-a", "fixture-a"}, command...)...)
		if commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "unsupported_command" {
			t.Fatal(err, stderr)
		}
	}
	t.Setenv("WACLI_READONLY", "1")
	_, stderr, err := runAgentTest(t, "-a", "fixture-a", "--agent", "--read-only=false", "draft", "create", "--to", localReadPN, "--message-file", "-")
	if commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "read_only" {
		t.Fatal(err, stderr)
	}
	if _, err = os.Stat(filepath.Join(a, "LOCK")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestAccountBindingLiteralContentAndLegacyOutput(t *testing.T) {
	_, a, _ := accountBindingFixture(t)
	for _, text := range []string{"--account=fixture-b", "--store", "--for-account", "--agent", " Unicode 雪 🙂 'spaces' \n"} {
		stdout, stderr, err := runAgentTest(t, "-a", "fixture-a", "--agent", "messages", "search", "--", text)
		if err != nil || stderr != "" {
			t.Fatal(text, err, stderr)
		}
		e := decodeAgentTest(t, stdout)
		if e.Account.Name != "fixture-a" || *e.Account.StoreRef != a {
			t.Fatal(stdout)
		}
		var data struct {
			Messages []struct {
				Text string `json:"text"`
			}
		}
		if err = json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Messages) == 0 {
			t.Fatal("literal query changed", stdout)
		}
	}
	stdout, stderr, err := runAgentTest(t, "-a", "fixture-a", "--json", "messages", "search", "--", "--agent")
	if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") || !strings.Contains(stdout, `"success":true`) {
		t.Fatal(err, stdout, stderr)
	}
	// Known string flag values must not activate a binding or a new output mode.
	stdout, stderr, err = runAgentTest(t, "--json", "auth", "--qr-format", "--for-account", "--help")
	if err != nil || stderr != "" || stdout == "" || strings.Contains(stdout, "schema_version") {
		t.Fatal(err, stdout, stderr)
	}
	_, stderr, err = runAgentTest(t, "-a", "fixture-a", "messages", "search", "fixture", "--chat", "--account=fixture-b", "--agent")
	if commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
		t.Fatal(err, stderr)
	}
	for _, tail := range [][]string{{"--bogus"}, {"--detail", "invalid"}, {"--timeout", "invalid"}} {
		_, stderr, err = runAgentTest(t, append([]string{"-a", "fixture-a", "--agent", "auth", "status"}, tail...)...)
		if commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
			t.Fatal(err, stderr)
		}
	}
}

func TestAccountBindingFreezeAndPreArgs(t *testing.T) {
	cfg, a, b := accountBindingFixture(t)
	flags := rootFlags{agent: true}
	root := &cobra.Command{Use: "wacli"}
	root.PersistentFlags().StringVar(&flags.account, "account", "", "")
	root.PersistentFlags().StringVar(&flags.storeDir, "store", "", "")
	registerAccountBinding(root, &flags)
	if err := root.ParseFlags([]string{"-a", "fixture-a"}); err != nil {
		t.Fatal(err)
	}
	changed := &config.AccountsConfig{Accounts: map[string]config.AccountEntry{"fixture-a": {Store: b}}}
	if err := config.SaveAccountsConfig(cfg, changed); err != nil {
		t.Fatal(err)
	}
	if err := root.PersistentFlags().Set("for-account", "fixture-a"); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []bool{false, true} {
		copy := flags
		copy.agent = agent
		got, err := resolveStoreDirWithConfig(&copy, cfg)
		if err != nil || got != a {
			t.Fatal(got, err)
		}
	}
	calls := 0
	root.AddCommand(&cobra.Command{Use: "probe", Args: func(*cobra.Command, []string) error { calls++; return nil }, PreRun: func(*cobra.Command, []string) { calls++ }, Run: func(*cobra.Command, []string) { calls++ }})
	root.SetArgs([]string{"probe", "--account=fixture-b"})
	root.SilenceErrors = true
	root.SilenceUsage = true
	if err := root.Execute(); err == nil || calls != 0 {
		t.Fatal("conflict reached Args/hook/Run", err, calls)
	}
}

func TestAccountBindingStdinBytesAndEarlyRefusal(t *testing.T) {
	_, a, _ := accountBindingFixture(t)
	// Public identity only, deliberately not a writable whatsmeow credential DB.
	session, err := sql.Open("sqlite3", filepath.Join(a, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Exec("CREATE TABLE whatsmeow_device(jid TEXT,lid TEXT);INSERT INTO whatsmeow_device VALUES('15550000009@s.whatsapp.net','');CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(a, "input.fixture")
	input := []byte("\tUnicode 雪 🙂 --account=fixture-b --store= --for-account\r\n 'quoted' \n")
	if err = os.WriteFile(inputPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := os.Stdin
	os.Stdin = file
	defer func() { os.Stdin = old }()
	_, stderr, err := runAgentTest(t, "-a", "fixture-a", "--account=fixture-b", "--agent", "draft", "create", "--to", localReadPN, "--message-file", "-")
	if commandExitCode(err) != 2 {
		t.Fatal(err, stderr)
	}
	offset, err := file.Seek(0, 1)
	if err != nil || offset != 0 {
		t.Fatal("binding consumed stdin", offset, err)
	}
	stdout, stderr, err := runAgentTest(t, "-a", "fixture-a", "--agent", "draft", "create", "--to", localReadPN, "--message-file", "-", "--detail", "full")
	if err != nil || stderr != "" {
		t.Fatal(err, stdout, stderr)
	}
	var dto draftDTO
	if err = json.Unmarshal(decodeAgentTest(t, stdout).Data, &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Text == nil || !bytes.Equal([]byte(dto.Text.Text), input) {
		t.Fatal("stdin bytes changed", stdout)
	}
	// Typed selection errors survive the pflag wrapper, with no private cause.
	_, stderr, err = runAgentTest(t, "-a", "absent", "--agent", "auth", "status")
	var typed *out.AgentError
	if !errors.As(err, &typed) || typed.Code != "store_unavailable" || typed.ExitCode != 4 {
		t.Fatal(err, stderr)
	}
	// Error mode follows output intent even when parsing stops between bool flags.
	_, stderr, err = runAgentTest(t, "--agent=false", "-a", "absent", "--agent", "auth", "status")
	if commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
		t.Fatal(err, stderr)
	}
	stdout, stderr, err = runAgentTest(t, "--agent", "-a", "absent", "--agent=false", "auth", "status")
	if commandExitCode(err) != 1 || stdout != "" || strings.Contains(stderr, "schema_version") {
		t.Fatal(err, stdout, stderr)
	}
}

func TestAccountBindingHelpVersionOrder(t *testing.T) {
	cfg, _, _ := accountBindingFixture(t)
	validConfig, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"valid", "absent", "invalid-config"} {
		if state == "invalid-config" {
			if err := os.WriteFile(cfg, []byte("PRIVATE_BINDING_CONFIG: [broken"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		name := "fixture-a"
		if state == "absent" {
			name = "absent"
		}
		for _, selector := range [][]string{{"-a", name}, {"-a" + name}, {"-a=" + name}, {"--for-account", name}, {"--for-account=" + name}} {
			for _, helper := range []string{"--help", "--help=true", "-h", "--version", "--version=true", "version"} {
				for _, before := range []bool{true, false} {
					for _, agent := range []bool{false, true} {
						args := append(append([]string{}, selector...), helper)
						if before {
							args = append([]string{helper}, selector...)
						}
						if agent {
							args = append([]string{"--agent"}, args...)
						}
						t.Run(state+"/"+strings.Join(args, " "), func(t *testing.T) {
							stdout, stderr, err := runAgentTest(t, args...)
							if state == "valid" {
								if err != nil || stdout == "" || stderr != "" || strings.Contains(stdout, "schema_version") {
									t.Fatalf("text help/version: %v stdout=%q stderr=%q", err, stdout, stderr)
								}
							} else if agent {
								if commandExitCode(err) != 4 || stdout != "" || len(strings.Split(strings.TrimSpace(stderr), "\n")) != 1 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
									t.Fatalf("selection refusal: %v stdout=%q stderr=%q", err, stdout, stderr)
								}
							} else if commandExitCode(err) != 1 || stdout != "" || stderr == "" || strings.Contains(stderr, "schema_version") {
								t.Fatalf("legacy selection refusal: %v stdout=%q stderr=%q", err, stdout, stderr)
							}
							if strings.Contains(stderr, "PRIVATE_BINDING_CONFIG") || strings.Contains(stderr, cfg) {
								t.Fatal("selection error exposed private config", stderr)
							}
						})
					}
				}
			}
		}
	}
	if err := os.WriteFile(cfg, validConfig, 0600); err != nil {
		t.Fatal(err)
	}
	for _, helper := range []string{"--help", "-h", "--version"} {
		for _, conflict := range [][]string{{"--account="}, {"--account=fixture-b", "--account=fixture-a"}, {"--store="}, {"--for-account=fixture-b"}} {
			args := append([]string{"--agent", helper, "-a", "fixture-a"}, conflict...)
			stdout, stderr, err := runAgentTest(t, args...)
			if commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
				t.Errorf("conflict before helper %v: %v stdout=%q stderr=%q", args, err, stdout, stderr)
			}
		}
	}
}

func TestAccountBindingHelpRegistryTarget(t *testing.T) {
	cfg, a, b := accountBindingFixture(t)
	before := snapshotLocalStore(t, filepath.Dir(cfg))
	beforeA, beforeB := snapshotLocalStore(t, a), snapshotLocalStore(t, b)
	for _, action := range []string{"", "list", "show", "use", "remove", "add"} {
		target := []string{"accounts"}
		if action != "" {
			target = append(target, action)
		}
		for _, route := range [][]string{
			append(append([]string{"-a", "fixture-a", "help"}, target...), "--help=false"),
			append([]string{"help", "-afixture-a"}, target...),
			append(append([]string{"help"}, target...), "--for-account=fixture-a"),
			append([]string{"-afixture-a", "help", "--"}, target...),
			append(append([]string{"-afixture-a", "help"}, target...), "--help"),
			append([]string{"-hafixture-a", "help"}, target...),
		} {
			for _, agent := range []bool{false, true} {
				args := append([]string{}, route...)
				if agent {
					args = append([]string{"--agent"}, args...)
				}
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					stdout, stderr, err := runAgentTest(t, args...)
					if agent {
						if commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
							t.Fatalf("registry help refusal: %v stdout=%q stderr=%q", err, stdout, stderr)
						}
					} else if commandExitCode(err) != 1 || stdout != "" || stderr == "" {
						t.Fatalf("legacy registry help refusal: %v stdout=%q stderr=%q", err, stdout, stderr)
					}
				})
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, filepath.Dir(cfg))) || !reflect.DeepEqual(beforeA, snapshotLocalStore(t, a)) || !reflect.DeepEqual(beforeB, snapshotLocalStore(t, b)) {
		t.Fatal("help target lookup modified registry/archive")
	}
}

func TestAccountBindingHelpAndCompletionNeighbors(t *testing.T) {
	cfg, a, b := accountBindingFixture(t)
	before := snapshotLocalStore(t, filepath.Dir(cfg))
	beforeA, beforeB := snapshotLocalStore(t, a), snapshotLocalStore(t, b)
	for _, args := range [][]string{
		{"--help"}, {"--version"}, {"help", "accounts", "remove"}, {"--agent", "help", "accounts", "use"},
		{"--help", "--account=fixture-a"},
		{"--agent", "-afixture-a", "help", "--detail", "accounts", "messages"},
		{"--agent", "-afixture-a", "help", "--", "--for-account=fixture-b", "messages"},
		{"--agent", "help", "--", "-afixture-a", "accounts"},
		{"--json", "auth", "--qr-format", "--for-account", "--help"},
	} {
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stdout == "" || stderr != "" || strings.Contains(stdout, "schema_version") {
			t.Errorf("literal/unbound help %v: %v stdout=%q stderr=%q", args, err, stdout, stderr)
		}
	}
	// Keep the legacy discovery limitation; metadata repair is opt-in only.
	stdout, stderr, err := runAgentTest(t, "--help", "--account", "fixture-a")
	if commandExitCode(err) != 1 || stdout != "" || !strings.Contains(stderr, `unknown command "fixture-a"`) {
		t.Errorf("unbound split behavior changed: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	for _, name := range []string{"__complete", "__completeNoDesc"} {
		for _, target := range [][]string{{"-afixture-a", "accounts", "use", ""}, {"-a", "absent", "auth", "status", ""}} {
			args := append([]string{name}, target...)
			stdout, stderr, err := runAgentTest(t, args...)
			if err != nil || stdout != ":0\n" || !strings.Contains(stderr, "Completion ended with directive:") || (target[1] == "absent" && !strings.Contains(stderr, "[Error]")) {
				t.Errorf("completion protocol %v: %v stdout=%q stderr=%q", args, err, stdout, stderr)
			}
			stdout, stderr, err = runAgentTest(t, append([]string{"--agent"}, args...)...)
			if commandExitCode(err) != 2 || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
				t.Errorf("agent completion refusal %v: %v stdout=%q stderr=%q", args, err, stdout, stderr)
			}
		}
	}
	// Instrument the registry command itself: protocol completion must not
	// invoke its Args, hooks or Run, even when it discovers that command.
	for _, name := range []string{"__complete", "__completeNoDesc"} {
		for _, target := range [][]string{{"-afixture-a", "accounts", "use", ""}, {"-a", "absent", "auth", "status", ""}} {
			flags := rootFlags{}
			calls := 0
			root := &cobra.Command{Use: "wacli", SilenceErrors: true, SilenceUsage: true}
			root.PersistentFlags().StringVar(&flags.account, "account", "", "")
			root.PersistentFlags().StringVar(&flags.storeDir, "store", "", "")
			registerAccountBinding(root, &flags)
			accounts := &cobra.Command{Use: "accounts"}
			accounts.AddCommand(&cobra.Command{Use: "use", Args: func(*cobra.Command, []string) error { calls++; return nil }, PreRun: func(*cobra.Command, []string) { calls++ }, Run: func(*cobra.Command, []string) { calls++ }})
			root.AddCommand(accounts)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{name}, target...))
			if err := root.Execute(); err != nil || calls != 0 || stdout.String() != ":0\n" {
				t.Errorf("completion executed registry validator/hook/run: %v calls=%d stdout=%q stderr=%q", err, calls, stdout.String(), stderr.String())
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, filepath.Dir(cfg))) || !reflect.DeepEqual(beforeA, snapshotLocalStore(t, a)) || !reflect.DeepEqual(beforeB, snapshotLocalStore(t, b)) {
		t.Fatal("help/completion executed a registry/archive effect")
	}
}
