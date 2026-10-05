package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
)

const mediaFixtureChat = "123456789@s.whatsapp.net"

func seedAgentMedia(t *testing.T, kind string, cache bool) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	data := []byte("synthetic " + kind + " media bytes")
	sum := sha256.Sum256(data)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(mediaFixtureChat, "dm", "Fixture", time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	err = db.UpsertMessage(store.UpsertMessageParams{ChatJID: mediaFixtureChat, MsgID: "media-1", Timestamp: time.Unix(1000, 0), MediaType: kind, MimeType: "application/octet-stream", Filename: "fixture.bin", FileSHA256: sum[:], FileLength: uint64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if cache {
		path := filepath.Join(dir, "media", "cache.bin")
		if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkMediaDownloaded(mediaFixtureChat, "media-1", path, time.Unix(1001, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, data
}
func mediaFixtureArgs(dir, command string) []string {
	return []string{"--store", dir, "--agent", "media", command, "--chat", mediaFixtureChat, "--id", "media-1"}
}
func decodeMediaFixture(t *testing.T, raw string) agentMediaStatus {
	t.Helper()
	env := decodeAgentTest(t, raw)
	var d agentMediaStatus
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAgentMediaStatusAndCacheDownloadReadonlyUnderLock(t *testing.T) {
	for _, kind := range []string{"audio", "document", "image", "gif", "sticker"} {
		t.Run(kind, func(t *testing.T) {
			dir, data := seedAgentMedia(t, kind, true)
			lk, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lk.Release()
			before := snapshotLocalStore(t, dir)
			for _, detail := range []string{"compact", "full"} {
				args := append(mediaFixtureArgs(dir, "status"), "--detail", detail)
				raw, stderr, err := runAgentTest(t, args...)
				if err != nil || stderr != "" {
					t.Fatalf("%v %s", err, stderr)
				}
				d := decodeMediaFixture(t, raw)
				if d.Local.State != "existing" || d.Local.Verification != "not_checked" || d.Binding != "sha256_present" || d.DownloadMetadata != "incomplete" || d.DeclaredBytes == nil {
					t.Fatalf("stat claimed proof: %s", raw)
				}
				raw, stderr, err = runAgentTest(t, append(args, "--verify")...)
				if err != nil || stderr != "" {
					t.Fatalf("%v %s", err, stderr)
				}
				d = decodeMediaFixture(t, raw)
				if d.Local.State != "verified" || d.Status != "cached" || len(d.Local.Checks) != 2 || d.Remote.Current != "unknown" {
					t.Fatalf("%s", raw)
				}
			}
			output := filepath.Join(t.TempDir(), "nested", "out.bin")
			args := append(mediaFixtureArgs(dir, "download"), "--output", output)
			for _, ro := range []bool{false, true} {
				t.Setenv("WACLI_READONLY", "0")
				if ro {
					t.Setenv("WACLI_READONLY", "1")
				}
				raw, stderr, err := runAgentTest(t, args...)
				if err != nil || stderr != "" {
					t.Fatalf("%v %s", err, stderr)
				}
				d := decodeMediaFixture(t, raw)
				expected := "cached"
				if ro {
					expected = "existing"
				}
				if d.Status != expected || d.Recorded || d.Output == nil || d.Output.State != "verified" || d.Output.Bytes == nil || *d.Output.Bytes != int64(len(data)) {
					t.Fatalf("%s", raw)
				}
				if env := decodeAgentTest(t, raw); env.Meta.Source != "live" {
					t.Fatalf("source=%s", env.Meta.Source)
				}
			}
			if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%q %v", got, err)
			}
			if got := snapshotLocalStore(t, dir); !reflect.DeepEqual(got, before) {
				t.Fatalf("archive mutated: %v %v", before, got)
			}
			if _, err := os.Stat(filepath.Join(dir, "session.db")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("session opened: %v", err)
			}
		})
	}
}

func TestAgentMediaHistoricalUnavailableCoexistsWithLocalBytes(t *testing.T) {
	dir, _ := seedAgentMedia(t, "audio", true)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMediaUnavailable(context.Background(), mediaFixtureChat, "media-1", time.Unix(900, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "status"), "--verify")...)
	if err != nil || stderr != "" {
		t.Fatalf("%v %s", err, stderr)
	}
	d := decodeMediaFixture(t, raw)
	if d.Status != "cached" || d.Remote.Current != "unknown" || d.Remote.Observation == nil || d.Remote.Observation.ObservedAt == nil || d.Remote.Observation.ObservedAt.Unix() != 900 {
		t.Fatalf("%s", raw)
	}
}

func TestAgentMediaMissingBindingDiffersFromNetworkMetadata(t *testing.T) {
	dir, _ := seedAgentMedia(t, "audio", true)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: mediaFixtureChat, MsgID: "unbound", Timestamp: time.Unix(1000, 0), MediaType: "audio"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMediaDownloaded(mediaFixtureChat, "unbound", filepath.Join(dir, "media", "cache.bin"), time.Unix(1001, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	args := []string{"--store", dir, "--agent", "media", "status", "--chat", mediaFixtureChat, "--id", "unbound", "--verify"}
	raw, _, err := runAgentTest(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	d := decodeMediaFixture(t, raw)
	if d.Local.State != "existing" || d.Local.Verification != "unknown" || d.DeclaredBytes != nil {
		t.Fatalf("%s", raw)
	}
	args[4] = "download"
	args = append(args[:len(args)-1], "--output", filepath.Join(t.TempDir(), "out"))
	stdout, stderr, err := runAgentTest(t, args...)
	if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != "media_binding_unverified" {
		t.Fatalf("%v %s %s", err, stdout, stderr)
	}
	_, stderr, err = runAgentTest(t, append(mediaFixtureArgs(dir, "download"), "--id", "missing", "--output", filepath.Join(t.TempDir(), "out"))...)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "not_found" {
		t.Fatalf("%v %s", err, stderr)
	}
}

func TestAgentMediaGuardsAndSanitizedParseSource(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		source, code string
	}{
		{[]string{"--agent", "media", "download"}, "live", "invalid_arguments"},
		{[]string{"--agent", "media", "download", "--chat", mediaFixtureChat, "--id", "x"}, "live", "invalid_arguments"},
		{[]string{"--agent", "media", "download", "--timeout", "https://SECRET_KEY/path"}, "live", "invalid_arguments"},
		{[]string{"--agent", "media", "status", "--verify=SECRET_KEY"}, "local", "invalid_arguments"},
		{[]string{"--agent", "media", "retry"}, "live", "invalid_arguments"},
		{[]string{"--agent", "media", "backfill"}, "local", "unsupported_command"},
		{[]string{"--agent", "media", "status", "--chat", "123", "--id", "x"}, "local", "invalid_arguments"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "not-created")
			args := append([]string{"--store", dir}, tc.args...)
			stdout, stderr, err := runAgentTest(t, args...)
			if err == nil || stdout != "" {
				t.Fatalf("%v %s", err, stdout)
			}
			e := decodeAgentTest(t, stderr)
			if e.Meta.Source != tc.source || e.Error.Code != tc.code || strings.Contains(stderr, "SECRET_KEY") {
				t.Fatalf("%s", stderr)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("guard accessed store: %v", err)
			}
		})
	}
}

func TestAgentMediaTombstoneMetadataAndOutputConflicts(t *testing.T) {
	for _, kind := range []string{"tombstone", "missing_network", "existing_mismatch", "directory_link", "store_control", "missing_cache", "hash_mismatch"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := seedAgentMedia(t, "audio", true)
			output := filepath.Join(t.TempDir(), "out")
			code := ""
			switch kind {
			case "tombstone":
				db, err := store.Open(filepath.Join(dir, "wacli.db"))
				if err != nil {
					t.Fatal(err)
				}
				if err := db.MarkMessageRevoked(mediaFixtureChat, "media-1"); err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
				code = "media_deleted"
			case "missing_network", "missing_cache":
				if err := os.Rename(filepath.Join(dir, "media", "cache.bin"), filepath.Join(dir, "media", "previous.bin")); err != nil {
					t.Fatal(err)
				}
				code = "media_metadata_incomplete"
			case "existing_mismatch":
				if err := os.WriteFile(output, []byte("keep me"), 0o600); err != nil {
					t.Fatal(err)
				}
				code = "output_conflict"
			case "hash_mismatch":
				if err := os.WriteFile(filepath.Join(dir, "media", "cache.bin"), []byte("different"), 0o600); err != nil {
					t.Fatal(err)
				}
				code = "media_metadata_incomplete"
			case "directory_link":
				link := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(filepath.Dir(output), link); err != nil {
					t.Skip(err)
				}
				output = filepath.Join(link, "out")
				code = "path_not_allowed"
			case "store_control":
				output = filepath.Join(dir, "session.db")
				code = "path_not_allowed"
			}
			stdout, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "download"), "--output", output)...)
			if err == nil || stdout != "" || decodeAgentTest(t, stderr).Error.Code != code {
				t.Fatalf("want %s: %v %s %s", code, err, stdout, stderr)
			}
			if kind == "existing_mismatch" {
				got, _ := os.ReadFile(output)
				if string(got) != "keep me" {
					t.Fatal("destination overwritten")
				}
			}
		})
	}
}

func TestAgentMediaRootConfinementAndDirectoryChange(t *testing.T) {
	dir, _ := seedAgentMedia(t, "audio", true)
	t.Setenv(mediaRootsEnv, filepath.Join(dir, "media"))
	_, stderr, err := runAgentTest(t, append(mediaFixtureArgs(dir, "download"), "--output", filepath.Join(t.TempDir(), "out"))...)
	if err == nil || decodeAgentTest(t, stderr).Error.Code != "path_not_allowed" {
		t.Fatalf("%v %s", err, stderr)
	}
	loc, err := openAgentMediaLocation(filepath.Join(dir, "media", "out"), dir, []string{filepath.Join(dir, "media")}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer loc.close()
	if err := os.Rename(filepath.Join(dir, "media"), filepath.Join(dir, "old-media")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "media"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := loc.Check(); err == nil {
		t.Fatal("directory replacement accepted")
	}
	// A DB path cannot authorize reading another local file, even if roots do.
	if _, err := openAgentMediaLocation(filepath.Join(t.TempDir(), "secret"), dir, nil, true, false); err == nil {
		t.Fatal("arbitrary cache path accepted")
	}
}

func TestAgentMediaRejectsControlHardlinkAndHonorsCancelledContext(t *testing.T) {
	dir, _ := seedAgentMedia(t, "audio", true)
	alias := filepath.Join(dir, "media", "control-alias")
	if err := os.Link(filepath.Join(dir, "wacli.db"), alias); err != nil {
		t.Skip(err)
	}
	if loc, err := openAgentMediaLocation(alias, dir, nil, true, false); err == nil {
		loc.close()
		t.Fatal("control hardlink accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flags := &rootFlags{storeDir: dir, agent: true, agentCapability: agentMediaDownload, timeout: time.Second}
	cmd := newMediaCmd(flags)
	cmd.SetContext(ctx)
	err := runAgentMedia(cmd, flags, mediaFixtureChat, "media-1", filepath.Join(t.TempDir(), "out"), true, true)
	if err == nil || classifyAgentError(err).Code != "cancelled" {
		t.Fatalf("cancellation: %v", err)
	}
}

type brokenMediaOutput struct{}

func (brokenMediaOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestAgentMediaOutputFailureRetainsPublication(t *testing.T) {
	for _, status := range []string{"cached", "downloaded", "existing"} {
		err := writeAgentMediaDownload(brokenMediaOutput{}, &rootFlags{agent: true, agentCapability: agentMediaDownload}, agentMediaStatus{ChatJID: mediaFixtureChat, ID: "media-1", Status: status, Output: &app.MediaArtifact{State: "verified"}})
		if err == nil {
			t.Fatal("output failure ignored")
		}
		e := classifyAgentError(err)
		want := "written"
		if status == "existing" {
			want = "not_written"
		}
		if e.Media == nil || e.Media.FilePublication != want || e.Media.Recorded {
			t.Fatalf("%+v", e)
		}
	}
}
