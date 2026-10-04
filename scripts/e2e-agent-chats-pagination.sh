#!/usr/bin/env bash
set -euo pipefail

# Run after pnpm build. An optional argument selects a plain production binary.
workspace_dir="$(pwd -P)"
mkdir -p dist/.tmp
fixture_dir="$(mktemp -d "$workspace_dir/dist/.tmp/chat-page.XXXXXX")"
store_dir="$fixture_dir/store"
# Retain the synthetic fixture for review; no recursive deletion or real store access.
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
 db,err:=store.Open(filepath.Join(os.Args[1],"wacli.db"));if err!=nil {panic(err)}
 defer db.Close()
 for i:=0;i<13;i++ {
  jid:=fmt.Sprintf("tie%02d@g.us",i)
  if err:=db.UpsertChat(jid,"group",`Synthetic %_\`,time.Unix(100,0));err!=nil {panic(err)}
  if err:=db.SetChatPinned(jid,true);err!=nil {panic(err)}
  if err:=db.SetChatUnreadCount(jid,i%2);err!=nil {panic(err)}
  if i%3==0 {if err:=db.SetChatMutedUntil(jid,-1);err!=nil {panic(err)}}
 }
 for _,jid:=range []string{"123@s.whatsapp.net","456@lid"} {
  if err:=db.UpsertChatMetadata(jid,"dm",`Synthetic %_\`);err!=nil {panic(err)}
 }
}
GO
gofmt -w "$fixture_dir/fixture.go"
CGO_ENABLED=1 CGO_CFLAGS="${CGO_CFLAGS:+$CGO_CFLAGS }-Wno-error=missing-braces" go run -tags sqlite_fts5 "$fixture_dir/fixture.go" "$store_dir"
node --input-type=module - "$store_dir" "${1:-./dist/wacli}" <<'JS'
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {readFileSync,readdirSync,statSync,chmodSync} from 'node:fs';
import {join} from 'node:path';
const store=process.argv[2],binary=process.argv[3];
const db=join(store,'wacli.db');
// Production CLI has only a read-only fixture file and no session/credentials.
chmodSync(db,0o444);
const before=readFileSync(db),mode=statSync(db).mode;
function run(args,exit=0,selected=store) {
 const p=spawnSync(binary,['--store',selected,'--read-only',...args],{encoding:'utf8'});
 assert.equal(p.status,exit,p.stderr);
 assert.equal(exit===0?p.stderr:p.stdout,'');
 return JSON.parse(exit===0?p.stdout:p.stderr);
}
const all=[...Array.from({length:13},(_,i)=>`tie${String(i).padStart(2,'0')}@g.us`),'123@s.whatsapp.net','456@lid'];
for(const filters of [[],['--unread'],['--no-unread'],['--pinned'],['--no-pinned'],['--muted'],['--no-muted'],['--no-archived'],['--unread','--pinned','--muted','--no-archived']]) {
 let token,count=0;const got=[];
 do {
  const args=['--agent','chats','list','--query','%_\\',...filters,'--limit',count?'2':'3','--detail',count?'full':'compact'];
  if(token)args.push('--cursor',token);
  const env=run(args),page=env.meta.page;
  assert.equal(env.schema_version,1);
  assert.equal(env.meta.completeness,'unknown');assert.equal(env.meta.freshness,'unknown');
  assert.equal(page.returned,env.data.chats.length);assert.equal(page.has_more,page.next_cursor!==null);
  assert.equal(env.account.store_ref,store);
  if(count)assert.ok(env.data.chats.every(c=>c.full));
  got.push(...env.data.chats.map(c=>c.jid));token=page.next_cursor;
  assert.ok(++count<20);
 } while(token);
 const expected=all.filter(jid=> {
  const i=all.indexOf(jid),pin=i<13,unread=pin&&i%2===1,muted=pin&&i%3===0;
  return filters.every(f=>({'--unread':unread,'--no-unread':!unread,'--pinned':pin,'--no-pinned':!pin,'--muted':muted,'--no-muted':!muted,'--no-archived':true})[f]);
 });
 assert.deepEqual(got,expected);
 if(!filters.length)assert.ok(count>2);
}
const token=run(['--agent','chats','list','--limit','1']).meta.page.next_cursor;
for(const args of [
 ['chats','list','--cursor',token,'--query','other'],
 ['chats','list','--cursor',token,'--no-unread'],
 ['messages','list','--cursor',token],
 ['messages','search','Synthetic','--sort','time','--cursor',token],
]) assert.equal(run(['--agent',...args],2).error.code,'invalid_cursor');
for(const bad of ['', 'PRIVATE_JID_SQL_TOKEN', 'x'.repeat(16385),Buffer.from(Buffer.from(token,'base64url').toString().replace('"v":1','"v":2')).toString('base64url')]) {
 const env=run(['--agent','chats','list','--cursor',bad],2,join(store,'missing'));
 assert.equal(env.error.code,'invalid_cursor');if(bad)assert.ok(!JSON.stringify(env).includes(bad));
}
assert.deepEqual(run(['--agent','chats','list','--limit','15']).meta.page,{returned:15,has_more:false,next_cursor:null});
assert.deepEqual(run(['--agent','chats','list','--archived']).meta.page,{returned:0,has_more:false,next_cursor:null});
const legacy=run(['--json','chats','list']);
assert.equal(legacy.schema_version,undefined);assert.equal(legacy.error,null);
assert.equal(legacy.data.length,15);
assert.ok(legacy.data.slice(0,13).every(c=>c.pinned&&c.last_message_ts==='1970-01-01T00:01:40Z'));
const p=spawnSync(binary,['--store',join(store,'missing'),'chats','list','--cursor',token],{encoding:'utf8'});
assert.equal(p.status,2);assert.match(p.stderr,/--cursor requires --agent/);assert.equal(p.stdout,'');
assert.deepEqual(readFileSync(db),before);assert.equal(statSync(db).mode,mode);
assert.deepEqual(readdirSync(store),['wacli.db']);
console.log('Synthetic production chat pagination passed: ties, filters, literal query, detail/limit, typed errors, legacy, immutable archive.');
JS
printf 'Retained synthetic fixture: %s\n' "$fixture_dir"
