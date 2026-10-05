package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
)

type agentTestEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Success       bool             `json:"success"`
	Account       out.AgentAccount `json:"account"`
	Meta          out.AgentMeta    `json:"meta"`
	Data          json.RawMessage  `json:"data"`
	Error         *out.AgentError  `json:"error"`
}

func runAgentTest(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureRootStderr(t, func() { stdout = captureRootStdout(t, func() { err = execute(args) }) })
	return
}
func decodeAgentTest(t *testing.T, raw string) agentTestEnvelope {
	t.Helper()
	var value agentTestEnvelope
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, raw)
	}
	if value.SchemaVersion != 1 {
		t.Fatalf("missing schema version: %s", raw)
	}
	if value.Success {
		if value.Error != nil || len(value.Data) == 0 {
			t.Fatalf("bad success: %s", raw)
		}
	} else if value.Error == nil || len(value.Data) > 0 {
		t.Fatalf("bad failure: %s", raw)
	}
	return value
}
func agentTestCommands() [][]string {
	return [][]string{
		{"messages", "list", "--chat", localReadPN},
		{"messages", "search", "fixture", "--chat", localReadPN},
		{"messages", "show", "--chat", localReadPN, "--id", "m1"},
		{"messages", "context", "--chat", localReadPN, "--id", "m1"},
		{"chats", "list"}, {"chats", "show", "--jid", localReadPN},
		{"contacts", "list"}, {"contacts", "search", "Fixture"}, {"contacts", "show", "--jid", localReadLID},
		{"contacts", "resolve", localReadLID, "999999@lid", localReadPN},
		{"history", "coverage", "--include-blocked"}, {"auth", "status"}, {"doctor"},
	}
}
func TestAgentSupportedCommandsPreserveReadOnlyStore(t *testing.T) {
	dir := seedLocalReadStore(t)
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	before := snapshotLocalStore(t, dir)
	for _, detail := range []string{"compact", "full"} {
		for _, cmd := range agentTestCommands() {
			t.Run(detail+"/"+strings.Join(cmd, " "), func(t *testing.T) {
				args := append([]string{"--store", dir, "--agent", "--json", "--detail", detail}, cmd...)
				stdout, stderr, err := runAgentTest(t, args...)
				if err != nil || stderr != "" {
					t.Fatalf("err=%v stderr=%s", err, stderr)
				}
				value := decodeAgentTest(t, stdout)
				if !value.Success || value.Account.StoreRef == nil || *value.Account.StoreRef != dir || value.Account.Name != "" {
					t.Fatalf("bad identity: %s", stdout)
				}
				if value.Meta.Source != "local" || value.Meta.Detail != detail || value.Meta.Freshness != "unknown" || value.Meta.Completeness != "unknown" {
					t.Fatalf("misleading meta: %s", stdout)
				}
				if value.Meta.Recovery != "" {
					t.Fatalf("recovery without truncation: %s", stdout)
				}
				if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
					t.Fatalf("query modified fixture: before=%v after=%v", before, got)
				}
				if strings.Contains(stdout, "last_sync") || strings.Contains(stdout, "direct_path") || strings.Contains(stdout, "session.db") {
					t.Fatalf("internal/misleading metadata: %s", stdout)
				}
			})
		}
	}
}
func TestAgentUnicodeAndPublicMessageContent(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("🙂é漢", 150)
	for _, id := range []string{"long", "revoked", "removed"} {
		err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: id, SenderJID: localReadLID, Timestamp: time.Unix(100, 0), Text: text, DisplayText: text, MediaType: "document", MediaCaption: text, Filename: strings.Repeat("á", 400), MimeType: "application/pdf", DirectPath: "SECRET_RETRIEVAL_FIXTURE", MediaKey: []byte("SECRET_KEY_FIXTURE"), QuotedMsgID: "quote-1", QuotedSenderJID: "999999@lid", Edited: true, Revoked: id == "revoked", DeletedForMe: id == "removed", Buttons: []store.Button{{Type: "url", DisplayText: "public label", ID: "button-1", URL: "SECRET_URL_FIXTURE"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	for _, detail := range []string{"compact", "full"} {
		for _, id := range []string{"long", "revoked", "removed"} {
			stdout, stderr, err := runAgentTest(t, "--store", dir, "messages", "show", "--agent", "--detail", detail, "--chat", localReadPN, "--id", id)
			if err != nil || stderr != "" {
				t.Fatalf("%v %s", err, stderr)
			}
			value := decodeAgentTest(t, stdout)
			var msg agentMessage
			if err := json.Unmarshal(value.Data, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.ChatJID != localReadLID || msg.SenderJID != localReadLID || msg.Quote.ID != "quote-1" || msg.Quote.SenderJID != "999999@lid" || !msg.Edited {
				t.Fatalf("lost identity/semantics: %s", stdout)
			}
			if strings.Contains(stdout, "SECRET_") || strings.Contains(stdout, "local_path") {
				t.Fatalf("private data: %s", stdout)
			}
			if id == "revoked" && (!msg.Revoked || msg.DeletedAt == nil) {
				t.Fatalf("lost revoke: %s", stdout)
			}
			if id == "removed" && (!msg.DeletedForMe || msg.DeletedAt == nil) {
				t.Fatalf("lost removal: %s", stdout)
			}
			if id == "long" && detail == "compact" && (!msg.TextTruncated || utf8.RuneCountInString(msg.Text) != 320 || !utf8.ValidString(msg.Text) || msg.Text != string([]rune(text)[:320])) {
				t.Fatalf("bad Unicode truncation: %s", stdout)
			}
			if detail == "full" && (msg.TextTruncated || msg.Full == nil || msg.Full.Content != text || len(msg.Full.Buttons) != 1) {
				t.Fatalf("missing public full: %s", stdout)
			}
			if strings.Contains(value.Meta.Recovery, "messages show") != (msg.TextTruncated || len(msg.FieldsTruncated) > 0) {
				t.Fatalf("wrong text recovery: %s", stdout)
			}
		}
	}
	stdout, _, err := runAgentTest(t, "--store", dir, "--agent", "messages", "list")
	if err != nil {
		t.Fatal(err)
	}
	env := decodeAgentTest(t, stdout)
	if !reflect.DeepEqual(env.Meta.Excluded, []string{"tombstones"}) || strings.Contains(stdout, `"id":"revoked"`) || strings.Contains(stdout, `"id":"removed"`) {
		t.Fatalf("unreported exclusions: %s", stdout)
	}
	// Boundary conditions, including code points rather than bytes/graphemes.
	for _, n := range []int{0, 319, 320, 321} {
		s := strings.Repeat("é", n)
		got, cut := agentText(s, "compact")
		if utf8.RuneCountInString(got) != min(n, 320) || cut != (n > 320) {
			t.Fatalf("n=%d got=%q cut=%v", n, got, cut)
		}
	}
}
func TestAgentIdentityAndOfflineEvidence(t *testing.T) {
	dir := seedLocalReadStore(t)
	stdout, _, err := runAgentTest(t, "--store", dir, "--agent", "contacts", "resolve", localReadLID, "999999@lid", localReadPN)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeAgentTest(t, stdout)
	var result agentResolutions
	if err := json.Unmarshal(env.Data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Resolutions) != 3 || !result.Resolutions[0].Resolved || result.Resolutions[0].JID != localReadPN || result.Resolutions[1].Resolved || result.Resolutions[1].Phone != "" || result.Resolutions[1].JID != "" || result.Resolutions[1].LID != "999999@lid" {
		t.Fatalf("invented/lost identity: %s", stdout)
	}
	for _, dir := range []string{dir, t.TempDir()} {
		stdout, _, err := runAgentTest(t, "--store", dir, "--agent", "auth", "status")
		if err != nil {
			t.Fatal(err)
		}
		env := decodeAgentTest(t, stdout)
		var status agentAuth
		if err := json.Unmarshal(env.Data, &status); err != nil {
			t.Fatal(err)
		}
		if status.Connected != "unknown" || env.Meta.Freshness != "unknown" {
			t.Fatalf("misleading status: %s", stdout)
		}
	}
	// A stale heartbeat is activity only, even while another process holds LOCK.
	stale := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, "HEARTBEAT"), []byte(stale.Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	stdout, _, err = runAgentTest(t, "--store", dir, "--agent", "--detail", "full", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	env = decodeAgentTest(t, stdout)
	var report agentDoctor
	if err := json.Unmarshal(env.Data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Auth.Connected != "unknown" || report.Full == nil || report.Full.LastMessageAt == nil || (report.LastActivityAt == nil || !report.LastActivityAt.Equal(stale)) || !report.LockHeld {
		t.Fatalf("misleading doctor: %s", stdout)
	}
}
func TestAgentLimitsAndPayloadSizes(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 220; i++ {
		text := "fixture " + strings.Repeat("ação🙂 ", 200)
		err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: fmt.Sprintf("size-%03d", i), SenderJID: localReadLID, Timestamp: time.Unix(200+int64(i), 0), Text: text, DisplayText: text})
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	sizes := make(map[string]int)
	for _, mode := range []string{"compact", "full", "legacy"} {
		args := []string{"--store", dir, "--json", "messages", "list", "--limit", "20"}
		if mode != "legacy" {
			args = append(args, "--agent", "--detail", mode)
		}
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" {
			t.Fatalf("%v %s", err, stderr)
		}
		sizes[mode] = len(stdout)
		if mode != "legacy" {
			env := decodeAgentTest(t, stdout)
			var data agentMessages
			_ = json.Unmarshal(env.Data, &data)
			if len(data.Messages) != 20 || env.Meta.Limit != 20 {
				t.Fatalf("limit ignored: %s", stdout)
			}
		}
	}
	t.Logf("representative 20-row payload bytes: compact=%d full=%d legacy=%d", sizes["compact"], sizes["full"], sizes["legacy"])
	if sizes["compact"] >= sizes["full"] || sizes["compact"] >= sizes["legacy"] {
		t.Fatalf("compact not smaller: %v", sizes)
	}
	stdout, _, err := runAgentTest(t, "--store", dir, "--agent", "messages", "list")
	if err != nil {
		t.Fatal(err)
	}
	env := decodeAgentTest(t, stdout)
	var data agentMessages
	_ = json.Unmarshal(env.Data, &data)
	if len(data.Messages) != 20 || env.Meta.Limit != 20 {
		t.Fatalf("bad default: %s", stdout)
	}
	stdout, _, err = runAgentTest(t, "--store", dir, "--agent", "messages", "list", "--limit", "200")
	if err != nil {
		t.Fatal(err)
	}
	env = decodeAgentTest(t, stdout)
	_ = json.Unmarshal(env.Data, &data)
	if len(data.Messages) != 200 {
		t.Fatalf("bad maximum: %d", len(data.Messages))
	}
}
func TestAgentPreflightErrorsBeforeEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		args []string
		code string
	}{
		{[]string{"send", "text", "--agent"}, "unsupported_command"},
		{[]string{"--agent", "auth"}, "unsupported_command"},
		{[]string{"sync", "--agent"}, "unsupported_command"},
		{[]string{"--agent", "doctor", "--connect"}, "unsupported_command"},
		{[]string{"--agent", "groups"}, "unsupported_command"},
		{[]string{"--agent", "completion", "bash"}, "unsupported_command"},
		{[]string{"--agent", "messages", "purge"}, "unsupported_command"},
		{[]string{"--agent", "messages", "purge", "--dry-run"}, "unsupported_command"},
		{[]string{"chats", "cleanup", "--dry-run", "--agent"}, "unsupported_command"},
		{[]string{"--agent", "groups", "prune", "--dry-run"}, "unsupported_command"},
		{[]string{"store", "cleanup", "--agent", "--dry-run"}, "unsupported_command"},
		{[]string{"contacts", "alias", "set", "--agent"}, "unsupported_command"},
		{[]string{"--agent", "messages", "show"}, "invalid_arguments"},
		{[]string{"messages", "show", "--agent", "--unknown"}, "invalid_arguments"},
		{[]string{"--unknown", "messages", "list", "--agent"}, "invalid_arguments"},
		{[]string{"--unknown", "--agent", "messages", "list"}, "invalid_arguments"},
		{[]string{"unknown", "--agent"}, "invalid_arguments"},
		{[]string{"__complete", "messages", "--agent"}, "invalid_arguments"},
		{[]string{"--agent=bad", "messages", "list"}, "invalid_arguments"},

		{[]string{"messages", "list", "--agent", "extra"}, "invalid_arguments"},
		{[]string{"messages", "list", "--agent", "--limit", "0"}, "invalid_arguments"},
		{[]string{"messages", "list", "--agent", "--limit", "-1"}, "invalid_arguments"},
		{[]string{"messages", "list", "--agent", "--limit", "201"}, "invalid_arguments"},
		{[]string{"messages", "list", "--agent", "--limit", "garbage"}, "invalid_arguments"},
		{[]string{"messages", "list", "--agent", "--after", "bad"}, "invalid_arguments"},
		{[]string{"messages", "search", "fixture", "--agent", "--type", "bad"}, "invalid_arguments"},
		{[]string{"messages", "context", "--agent", "--chat", localReadPN, "--id", "x", "--before", "100", "--after", "100"}, "invalid_arguments"},
		{[]string{"contacts", "resolve", "--agent", "invalid@g.us"}, "invalid_arguments"},
		{[]string{"--agent", "--detail", "bad", "messages", "list"}, "invalid_arguments"},
		{[]string{"--agent", "--events", "messages", "list"}, "invalid_arguments"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir}, tc.args...)...)
			env := decodeAgentTest(t, stderr)
			if stdout != "" || err == nil || commandExitCode(err) != 2 || env.Error.Code != tc.code {
				t.Fatalf("stdout=%s stderr=%s err=%v exit=%d", stdout, stderr, err, commandExitCode(err))
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight created store: %v", err)
			}
		})
	}
	inputs := make([]string, 201)
	for i := range inputs {
		inputs[i] = localReadPN
	}
	stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir, "--agent", "contacts", "resolve"}, inputs...)...)
	if stdout != "" || commandExitCode(err) != 2 || decodeAgentTest(t, stderr).Error.Code != "invalid_arguments" {
		t.Fatalf("unbounded results: %v %s", err, stderr)
	}
}
func TestAgentFlagIntentAndLegacyBoundaries(t *testing.T) {
	dir := seedLocalReadStore(t)
	for _, args := range [][]string{
		{"--store", dir, "messages", "search", "--", "--agent"},
		{"--store", dir, "messages", "search", "text contains --agent"},
		{"--store", dir, "messages", "search", "--agent=false", "fixture"},
	} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--json"}, args...)...)
		if err != nil || stderr != "" || strings.Contains(stdout, "schema_version") {
			t.Fatalf("mistaken agent detection: %v %s %s", err, stdout, stderr)
		}
	}
	for _, args := range [][]string{
		{"--store", "--agent", "--bogus", "messages", "list"},
		{"messages", "search", "--chat=--agent", "--bogus"},
		{"--store", dir, "messages", "search", "--", "--agent", "--bogus"},
	} {
		_, stderr, err := runAgentTest(t, args...)
		if err == nil || strings.Contains(stderr, "schema_version") {
			t.Fatalf("value/terminator misdetected: %v %s", err, stderr)
		}
	}
	_, stderr, err := runAgentTest(t, "--detail", "full", "messages", "list", "--store", dir)
	if err == nil || !strings.Contains(stderr, "--detail requires --agent") {
		t.Fatalf("detail silently ignored: %v %s", err, stderr)
	}
	for _, args := range [][]string{{"--agent", "--help"}, {"--agent", "version"}, {"--agent", "--version"}} {
		stdout, stderr, err := runAgentTest(t, args...)
		if err != nil || stderr != "" || stdout == "" || strings.Contains(stdout, "schema_version") {
			t.Fatalf("help/version changed: %v %s %s", err, stdout, stderr)
		}
	}
	// Preserve legacy message field spelling and envelope, including null error.
	stdout, _, err := runAgentTest(t, "--store", dir, "--json", "messages", "show", "--chat", localReadPN, "--id", "m1")
	if err != nil || !strings.Contains(stdout, `"MsgID":"m1"`) || !strings.Contains(stdout, `"error":null`) || strings.Contains(stdout, "schema_version") {
		t.Fatalf("legacy changed: %v %s", err, stdout)
	}
}

// Exercise the actual main exit boundary in a subprocess of the test binary.
func TestAgentMainProcess(t *testing.T) {
	if os.Getenv("WACLI_AGENT_TEST_PROCESS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"wacli"}, os.Args[i+1:]...)
				main()
				return
			}
		}
		t.Fatal("missing subprocess arguments")
	}
	dir := seedLocalReadStore(t)
	oversizedDB, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := oversizedDB.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "oversized", Text: strings.Repeat("x", 5<<20), Timestamp: time.Unix(101, 0)}); err != nil {
		t.Fatal(err)
	}
	_ = oversizedDB.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		exit int
		code string
	}{
		{[]string{"--agent", "messages", "list"}, 0, ""},
		{[]string{"messages", "list", "--unknown", "--agent"}, 2, "invalid_arguments"},
		{[]string{"--agent", "send", "text"}, 2, "unsupported_command"},
		{[]string{"--agent", "--detail", "full", "messages", "show", "--chat", localReadPN, "--id", "oversized"}, 1, "payload_too_large"},
		{[]string{"--agent", "messages", "show", "--chat", localReadPN, "--id", "absent"}, 3, "not_found"},
		{[]string{"--agent", "--store", filepath.Join(dir, "absent"), "messages", "list"}, 4, "store_unavailable"},
	}
	for _, tc := range cases {
		cmd := exec.Command(exe, append([]string{"-test.run=^TestAgentMainProcess$", "--", "--store", dir}, tc.args...)...)
		cmd.Env = append(os.Environ(), "WACLI_AGENT_TEST_PROCESS=1")
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exit := 0
		if err != nil {
			var failed *exec.ExitError
			if !errors.As(err, &failed) {
				t.Fatal(err)
			}
			exit = failed.ExitCode()
		}
		if exit != tc.exit {
			t.Fatalf("%v exit=%d stderr=%s", tc.args, exit, stderr.String())
		}
		if tc.exit != 0 {
			env := decodeAgentTest(t, stderr.String())
			if env.Error.Code != tc.code || stdout.Len() != 0 {
				t.Fatalf("bad failure: %s %s", stdout.String(), stderr.String())
			}
		}
	}
	unknown := classifyAgentError(errors.New("unclassified fixture failure"))
	if commandExitCode(unknown) != 1 || unknown.Code != "internal_error" {
		t.Fatalf("bad fallback: %+v", unknown)
	}
	// Also keep the lock-free read boundary under a live fixture writer.
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lk, err := lock.AcquireWithTimeout(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "live-wal", Text: "live fixture", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "messages", "list")
	if err != nil || stderr != "" || !strings.Contains(stdout, `"id":"live-wal"`) {
		t.Fatalf("read failed under writer lock: %v %s %s", err, stdout, stderr)
	}
}

func TestAgentStoreCompatibilityErrors(t *testing.T) {
	for _, change := range []string{
		"UPDATE schema_migrations SET version = -version WHERE version = (SELECT MAX(version) FROM schema_migrations)",
		"INSERT INTO schema_migrations(version,name,applied_at) SELECT MAX(version)+1,'future fixture',1 FROM schema_migrations",
	} {
		dir := seedLocalReadStore(t)
		db, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(change); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		before := snapshotLocalStore(t, dir)
		for _, command := range [][]string{{"messages", "list"}, {"doctor"}, {"contacts", "search", "fixture"}} {
			stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir, "--agent"}, command...)...)
			if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
				t.Fatalf("bad state error: %v %s %s", err, stdout, stderr)
			}
			if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
				t.Fatal("compatibility read modified store")
			}
		}
	}
	for _, content := range []string{"", "not a sqlite fixture"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "wacli.db"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "messages", "list")
		if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" {
			t.Fatalf("bad empty/corrupt state: %v %s", err, stderr)
		}
	}
	// An initialized empty archive has unknown freshness, null timestamps and
	// zero counts; it does not imply that history was synchronized completely.
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	stdout, stderr, err := runAgentTest(t, "--store", dir, "--agent", "--detail", "full", "doctor")
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	env := decodeAgentTest(t, stdout)
	var doctor agentDoctor
	_ = json.Unmarshal(env.Data, &doctor)
	if doctor.Full == nil || doctor.Full.Messages != 0 || doctor.Full.LastMessageAt != nil || doctor.LastActivityAt != nil || env.Meta.Freshness != "unknown" {
		t.Fatalf("invented empty freshness: %s", stdout)
	}
}

func TestAgentGuardRunsBeforeOriginalArgs(t *testing.T) {
	flags := &rootFlags{agent: true, detail: "compact"}
	root := &cobra.Command{Use: "wacli"}
	called := false
	child := &cobra.Command{Use: "mutator", Args: func(*cobra.Command, []string) error { called = true; return nil }, RunE: func(*cobra.Command, []string) error { t.Fatal("executed mutator"); return nil }}
	root.AddCommand(child)
	installAgentGuards(root, flags)
	err := child.Args(child, nil)
	if called || commandExitCode(err) != 2 || classifyAgentError(err).Code != "unsupported_command" {
		t.Fatalf("guard followed original Args: called=%v err=%v", called, err)
	}
}
func TestAgentAccountReferenceUsesSelectedStore(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.AccountsConfig{DefaultAccount: "fixture", Accounts: map[string]config.AccountEntry{"fixture": {Store: "archive", Label: "Fixture Label"}}}
	if err := config.SaveAccountsConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	// Explicit/default named accounts and manual stores need no session data.
	for _, flags := range []*rootFlags{{agent: true, account: "fixture"}, {agent: true}, {agent: true, storeDir: filepath.Join(dir, "manual")}} {
		selected, err := resolveStoreDirWithConfig(flags, configPath)
		if err != nil {
			t.Fatal(err)
		}
		if flags.agentAccount.StoreRef == nil || *flags.agentAccount.StoreRef != selected {
			t.Fatalf("unidentified store: %+v", flags.agentAccount)
		}
		if flags.storeDir == "" && (flags.agentAccount.Name != "fixture" || selected != filepath.Join(dir, "archive")) {
			t.Fatalf("lost named selection: %+v", flags.agentAccount)
		}
		if flags.storeDir != "" && flags.agentAccount.Name != "" {
			t.Fatalf("manual path attributed to default account: %+v", flags.agentAccount)
		}
	}
}
func TestAgentUnreadableIdentityState(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.WriteFile(filepath.Join(dir, "session.db"), []byte("invalid identity fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"contacts", "search", "fixture"}, {"contacts", "resolve", localReadLID}, {"auth", "status"}, {"doctor"}} {
		stdout, stderr, err := runAgentTest(t, append([]string{"--store", dir, "--agent"}, command...)...)
		if stdout != "" || commandExitCode(err) != 4 || decodeAgentTest(t, stderr).Error.Code != "store_unavailable" || strings.Contains(stderr, "session.db") {
			t.Fatalf("bad identity-state failure: %v %s", err, stderr)
		}
	}
}

func TestAgentErrorsKeepInternalCausesPrivate(t *testing.T) {
	raw := errors.New("SECRET_INTERNAL_FIXTURE /private/session.db authenticated-media-url key-bytes")
	for _, typed := range []*out.AgentError{agentStoreError(raw), classifyAgentError(raw)} {
		var recovered *out.AgentError
		if !errors.As(typed, &recovered) || !errors.Is(typed, raw) {
			t.Fatal("lost typed cause")
		}
		stderr := captureRootStderr(t, func() { writeRootError(rootFlags{agent: true, detail: "full"}, typed) })
		if strings.Contains(stderr, "SECRET_INTERNAL_FIXTURE") || strings.Contains(stderr, "/private/session.db") || strings.Contains(stderr, "key-bytes") {
			t.Fatalf("raw failure leaked: %s", stderr)
		}
		env := decodeAgentTest(t, stderr)
		if env.Error.Code != typed.Code || env.Meta.Recovery != "" {
			t.Fatalf("misleading recovery: %s", stderr)
		}
	}
}

func TestAgentSelectionStaysWithResolvedAccount(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.AccountsConfig{DefaultAccount: "fixture", Accounts: map[string]config.AccountEntry{"fixture": {Store: "first"}}}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{agent: true}
	first, err := resolveStoreDirWithConfig(flags, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Accounts["fixture"] = config.AccountEntry{Store: "second"}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	opened, err := resolveStoreDirWithConfig(flags, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if opened != first || flags.agentAccount.StoreRef == nil || *flags.agentAccount.StoreRef != opened {
		t.Fatalf("selection changed between envelope and opener: first=%s opened=%s", first, opened)
	}
}

func TestAgentMessageRawWhitespaceAndLegacyJSON(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	raw := "\t\u00a0ação🙂\r\n"
	caption := "\ncaption\t "
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: "raw", Timestamp: time.Now(), Text: raw, MediaCaption: caption, MediaType: "document", DisplayText: " Sent document "}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	before := snapshotLocalStore(t, dir)
	args := []string{"--store", dir, "--read-only", "messages", "show", "--chat", localReadLID, "--id", "raw"}
	for _, detail := range []string{"full", "compact"} {
		stdout, stderr, err := runAgentTest(t, append(args, "--agent", "--detail", detail)...)
		if err != nil {
			t.Fatal(err, stderr)
		}
		env := decodeAgentTest(t, stdout)
		var dto agentMessage
		if err := json.Unmarshal(env.Data, &dto); err != nil {
			t.Fatal(err)
		}
		if dto.Text != "Sent document" || dto.TextTruncated {
			t.Error("presentation changed")
		}
		if detail == "full" {
			if dto.Full == nil {
				t.Fatal("full content missing")
			}
			if !bytes.Equal([]byte(raw), []byte(dto.Full.Content)) || !bytes.Equal([]byte(caption), []byte(dto.Full.Caption)) {
				t.Error("full content/caption lost raw whitespace")
			}
		} else if dto.Full != nil {
			t.Error("compact output gained raw fields")
		}
	}
	stdout, stderr, err := runAgentTest(t, append(args, "--json")...)
	if err != nil {
		t.Fatal(err, stderr)
	}
	var legacy struct {
		Success bool
		Data    store.Message
	}
	if err := json.Unmarshal([]byte(stdout), &legacy); err != nil || !legacy.Success {
		t.Fatal("invalid legacy envelope", err)
	}
	if !bytes.Equal([]byte(raw), []byte(legacy.Data.Text)) || !bytes.Equal([]byte(caption), []byte(legacy.Data.MediaCaption)) {
		t.Error("legacy JSON lost raw whitespace")
	}
	if messageText(legacy.Data) != "Sent document" || messageRawText(legacy.Data) != strings.TrimSpace(raw) {
		t.Error("human presentation changed")
	}
	if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
		t.Error("read under writer LOCK changed archive")
	}
}
