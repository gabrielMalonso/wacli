#!/usr/bin/env bash
set -euo pipefail

# Requires pnpm build first; optional first argument selects a plain test binary. Everything below is a synthetic, workspace-local archive.
workspace_dir="$(pwd -P)"
mkdir -p dist/.tmp
fixture_dir="$(mktemp -d "$workspace_dir/dist/.tmp/page.XXXXXX")"
store_dir="$fixture_dir/store"
cleanup_fixture() {
  local target
  target="$(cd "$fixture_dir" && pwd -P)"
  printf 'Cleaning synthetic fixture: %s\n' "$target"
  case "$target" in
    "$workspace_dir"/dist/.tmp/page.*) ;;
    *) echo 'refusing cleanup outside workspace fixture' >&2; return 1 ;;
  esac
  if [ -z "$target" ] || [ "$target" = / ] || [ "$target" = "$HOME" ] || [[ "$HOME/" == "$target/"* ]]; then
    echo 'refusing cleanup of protected path' >&2
    return 1
  fi
  rm -rf -- "$target"
}
trap cleanup_fixture EXIT

cat >"$fixture_dir/fixture.go" <<'GO'
package main

import (
 "fmt"
 "os"
 "path/filepath"
 "time"
 "github.com/openclaw/wacli/internal/store"
)

func main() {
 db, err := store.Open(filepath.Join(os.Args[1], "wacli.db")); if err != nil {panic(err)}
 defer db.Close()
 if err := db.UpsertChat("123@s.whatsapp.net", "dm", "Synthetic pagination", time.Unix(100,0)); err != nil {panic(err)}
 for i:=0;i<11;i++ {
  if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID:"123@s.whatsapp.net",MsgID:fmt.Sprintf("m%d",i),Timestamp:time.Unix(100,0),Text:"Synthetic message"}); err != nil {panic(err)}
 }
}
GO
CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-Wno-error=missing-braces" go run -tags sqlite_fts5 "$fixture_dir/fixture.go" "$store_dir"
node --input-type=module - "$store_dir" "${1:-./dist/wacli}" <<'JS'
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {readFileSync,readdirSync} from 'node:fs';
import {join} from 'node:path';
const store=process.argv[2];
const binary=process.argv[3];
const before=readFileSync(join(store,'wacli.db'));
function run(args,exit=0) {
 const p=spawnSync(binary,['--store',store,'--read-only',...args],{encoding:'utf8'});
 assert.equal(p.status,exit,p.stderr);
 assert.equal(exit===0?p.stderr:p.stdout,'');
 return JSON.parse(exit===0?p.stdout:p.stderr);
}
const fts=run(['--json','messages','search','Synthetic']).data.fts;
const mode=fts?'fts5':'like';
for(const operation of ['list','search']) for(const asc of [false,true]) {
 let token;const ids=[];let count=0;
 do {
  const args=['--agent','messages',operation,...(operation==='search'?['Synthetic','--sort','time']:[]),'--limit',count===0?'3':'2','--detail',count===0?'compact':'full'];
  if(asc)args.push('--asc');
  if(token)args.push('--cursor',token);
  const envelope=run(args);const page=envelope.meta.page;
  assert.equal(envelope.schema_version,1);
  if(operation==='search') { assert.equal(envelope.data.search_mode,mode);assert.equal(envelope.data.order,asc?'time_asc':'time_desc'); }
  assert.equal(envelope.meta.completeness,'unknown');
  assert.equal(envelope.meta.freshness,'unknown');
  assert.equal(page.returned,envelope.data.messages.length);
  assert.equal(page.has_more,page.next_cursor!==null);
  if(token)assert.ok(envelope.data.messages.every(m=>m.full));
  ids.push(...envelope.data.messages.map(m=>m.id));token=page.next_cursor;
  assert.ok(++count<10);
 } while(token);
 assert.ok(count>2);
 const want=Array.from({length:11},(_,i)=>`m${asc?i:10-i}`);
 assert.deepEqual(ids,want);
}
const first=run(['--agent','--cursor=','messages','list'],2);
assert.equal(first.error.code,'invalid_cursor');
const token=run(['--agent','messages','list','--limit','1']).meta.page.next_cursor;
assert.equal(run(['--agent','messages','list','--cursor',token,'--asc'],2).error.code,'invalid_cursor');
for(const bad of ['secret-token-fragment','x'.repeat(513)]) {
 const e=run(['--agent','messages','list','--cursor',bad],2);
 assert.equal(e.error.code,'invalid_cursor');assert.ok(!JSON.stringify(e).includes(bad));
}
assert.equal(run(['--agent','send','text','--cursor',token],2).error.code,'invalid_arguments');
const exact=run(['--agent','messages','list','--limit','11']);
assert.deepEqual(exact.meta.page,{returned:11,has_more:false,next_cursor:null});
const empty=run(['--agent','messages','list','--chat','999@s.whatsapp.net']);
assert.deepEqual(empty.meta.page,{returned:0,has_more:false,next_cursor:null});
const legacy=run(['--json','messages','list']);
assert.equal(legacy.schema_version,undefined);assert.equal(legacy.error,null);
assert.deepEqual(legacy.data.messages.map(m=>m.MsgID),Array.from({length:11},(_,i)=>`m${10-i}`));
const searchToken=run(['--agent','messages','search','Synthetic','--sort','time','--limit','1']).meta.page.next_cursor;
for(const args of [
 ['messages','search','other','--sort','time','--cursor',searchToken],
 ['messages','search','Synthetic','--sort','time','--cursor',token],
 ['messages','list','--cursor',searchToken],
 ['messages','search','Synthetic','--sort','time','--cursor',searchToken,'--has-media'],
 ['messages','search','Synthetic','--sort','time','--cursor',searchToken,'--asc'],
]) assert.equal(run(['--agent',...args],2).error.code,'invalid_cursor');
for(const args of [
 ['messages','search','Synthetic','--cursor',searchToken],
 ['messages','search','Synthetic','--sort','relevance','--asc'],
 ['messages','search','Synthetic','--sort','rank'],
]) assert.equal(run(['--agent',...args],2).error.code,'invalid_arguments');
for(const bad of ['', 'secret-token-fragment','x'.repeat(513)]) {
 const e=run(['--agent','messages','search','Synthetic','--sort','time','--cursor',bad],2);
 assert.equal(e.error.code,'invalid_cursor');if(bad)assert.ok(!JSON.stringify(e).includes(bad));
}
for(const args of [['--sort','time'],['--asc'],['--asc=false']]) {
 const p=spawnSync(binary,['--store',join(store,'missing'),'messages','search','Synthetic',...args],{encoding:'utf8'});
 assert.equal(p.status,1);assert.equal(p.stdout,'');assert.match(p.stderr,/require --agent/);
}
const relevance=run(['--agent','messages','search','Synthetic']);
assert.equal(relevance.meta.page,undefined);assert.equal(relevance.data.search_mode,mode);assert.equal(relevance.data.order,fts?'relevance':'time_desc');
const legacySearch=run(['--json','messages','search','Synthetic']);
assert.equal(legacySearch.schema_version,undefined);
assert.deepEqual(relevance.data.messages.map(m=>m.id),legacySearch.data.messages.map(m=>m.MsgID));
assert.ok(legacySearch.data.messages.every(m=>fts?m.Snippet.includes('[Synthetic]'):m.Snippet===''));
for(const literal of ['--sort','--asc','--agent','--cursor']) {
 assert.equal(run(['--json','messages','search','--',literal]).schema_version,undefined);
 assert.deepEqual(run(['--agent','messages','search','--sort','time','--',literal]).meta.page,{returned:0,has_more:false,next_cursor:null});
}
assert.deepEqual(run(['--agent','messages','search','Synthetic','--sort','time','--limit','11']).meta.page,{returned:11,has_more:false,next_cursor:null});
assert.deepEqual(run(['--agent','messages','search','Absent','--sort','time']).meta.page,{returned:0,has_more:false,next_cursor:null});
assert.deepEqual(readFileSync(join(store,'wacli.db')),before);
assert.deepEqual(readdirSync(store),['wacli.db']);
console.log('Synthetic production CLI list/search pagination e2e passed (asc/desc, ties, limits, errors, legacy, unchanged archive).');
JS
