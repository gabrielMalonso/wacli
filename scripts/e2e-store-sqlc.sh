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

    "github.com/openclaw/wacli/internal/store"
)

func main() {
    db, err := store.Open(filepath.Join(os.Args[1], "wacli.db"))
    if err != nil { panic(err) }
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
