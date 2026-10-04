#!/usr/bin/env bash
set -euo pipefail

workspace_dir="$(pwd -P)"
mkdir -p dist/.tmp
fixture_dir="$(mktemp -d "$workspace_dir/dist/.tmp/wacli-sqlc-e2e.XXXXXX")"
store_dir="$fixture_dir/store"
cleanup_fixture() {
  local target
  target="$(cd "$fixture_dir" && pwd -P)"
  printf 'Cleaning synthetic fixture: %s\n' "$target"
  case "$target" in
    "$workspace_dir"/dist/.tmp/wacli-sqlc-e2e.*) ;;
    *) echo "refusing cleanup outside workspace fixture" >&2; return 1 ;;
  esac
  if [ -z "$target" ] || [ "$target" = / ] || [ "$target" = "$HOME" ] || [[ "$HOME/" == "$target/"* ]]; then
    echo "refusing cleanup of protected path" >&2
    return 1
  fi
  rm -rf -- "$target"
}
trap cleanup_fixture EXIT

mkdir -p dist
CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-Wno-error=missing-braces" go build -tags sqlite_fts5 -o dist/wacli ./cmd/wacli

# Reads must not bootstrap an absent store. Initialize only a synthetic fixture.
if ./dist/wacli --store "$store_dir" --json store stats >"$fixture_dir/missing.json" 2>"$fixture_dir/missing.err"; then
  echo "store stats unexpectedly initialized a missing store" >&2
  exit 1
fi
test ! -e "$store_dir"
cat >"$fixture_dir/fixture.go" <<'GO'
package main

import (
    "os"
    "path/filepath"
    "time"

    "github.com/openclaw/wacli/internal/store"
)

func main() {
    db, err := store.Open(filepath.Join(os.Args[1], "wacli.db"))
    if err != nil { panic(err) }
    if len(os.Args) > 2 {
        old := time.Now().UTC().AddDate(0, 0, -400)
        jid := "synthetic@g.us"
        if err := db.UpsertChat(jid, "group", "Synthetic e2e group", old); err != nil { panic(err) }
        if err := db.UpsertGroup(jid, "Synthetic e2e group", "", old); err != nil { panic(err) }
        if err := db.MarkGroupLeft(jid, old); err != nil { panic(err) }
        if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: jid, MsgID: "deleted", Text: "Synthetic retained payload", Timestamp: old}); err != nil { panic(err) }
        if err := db.MarkMessageDeletedForMePreserveMedia(jid, "deleted"); err != nil { panic(err) }
    }
    if err := db.Close(); err != nil { panic(err) }
}
GO
CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-Wno-error=missing-braces" go run -tags sqlite_fts5 "$fixture_dir/fixture.go" "$store_dir"
cp "$store_dir/wacli.db" "$fixture_dir/before.db"
stats_json="$(./dist/wacli --store "$store_dir" --json store stats)"
cmp "$fixture_dir/before.db" "$store_dir/wacli.db"
test ! -e "$store_dir/session.db"
test ! -e "$store_dir/LOCK"

case "$stats_json" in
  *'"success":true'*'"data"'*'"chats":0'*'"groups":0'*'"left_groups":0'*'"messages":0'*) ;;
  *) echo "unexpected store stats JSON: $stats_json" >&2; exit 1 ;;
esac

# Exercise the production binary's maintenance previews against synthetic data.
CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-Wno-error=missing-braces" go run -tags sqlite_fts5 "$fixture_dir/fixture.go" "$store_dir" maintenance
cp "$store_dir/wacli.db" "$fixture_dir/before-maintenance.db"
preview() {
  local expected="$1"
  shift
  local result
  result="$(./dist/wacli --store "$store_dir" --json --read-only "$@" --dry-run)"
  case "$result" in
    *'"success":true'*"$expected"*) ;;
    *) echo "unexpected maintenance preview: $result" >&2; exit 1 ;;
  esac
  cmp "$fixture_dir/before-maintenance.db" "$store_dir/wacli.db"
  test ! -e "$store_dir/session.db"
  test ! -e "$store_dir/LOCK"
  if ./dist/wacli --store "$store_dir" --read-only "$@" --confirm >"$fixture_dir/blocked.out" 2>"$fixture_dir/blocked.err"; then
    echo "read-only maintenance execution unexpectedly succeeded" >&2
    exit 1
  fi
  grep -q 'read-only mode' "$fixture_dir/blocked.err"
  cmp "$fixture_dir/before-maintenance.db" "$store_dir/wacli.db"
  test ! -e "$store_dir/session.db"
  test ! -e "$store_dir/LOCK"
}
preview '"would_delete":1' chats cleanup
preview '"message_count":1' chats cleanup --jid synthetic@g.us
preview '"would_delete":1' groups prune
preview '"would_delete_chats":1' store cleanup
preview '"would_purge":1' messages purge --chat synthetic@g.us --id deleted

# Real execution remains writable, offline, and confined to this fixture.
./dist/wacli --store "$store_dir" --json messages purge --chat synthetic@g.us --id deleted --confirm >"$fixture_dir/purged.json"
grep -q '"purged":1' "$fixture_dir/purged.json"
./dist/wacli --store "$store_dir" --json groups prune --confirm >"$fixture_dir/pruned.json"
grep -q '"deleted":1' "$fixture_dir/pruned.json"
./dist/wacli --store "$store_dir" --json store stats >"$fixture_dir/after.json"
grep -q '"chats":0' "$fixture_dir/after.json"
grep -q '"messages":0' "$fixture_dir/after.json"
