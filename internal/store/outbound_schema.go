package store

func migrateOutbound(d *DB) error {
	_, err := d.sql.Exec(`
CREATE TABLE IF NOT EXISTS outbound_operations (
 id TEXT PRIMARY KEY CHECK(length(id)=32 AND id NOT GLOB '*[^0-9a-f]*'),
 version INTEGER NOT NULL CHECK(version=1),
 account_jid TEXT NOT NULL CHECK(length(account_jid) BETWEEN 1 AND 128),
 idempotency_key TEXT NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 128 AND idempotency_key NOT GLOB '*[^!-~]*'),
 draft_id TEXT NOT NULL, revision_id TEXT NOT NULL,
 payload_hash TEXT NOT NULL CHECK(length(payload_hash)=64 AND payload_hash NOT GLOB '*[^0-9a-f]*'),
 message_id TEXT NOT NULL CHECK(length(message_id) BETWEEN 1 AND 128 AND message_id NOT GLOB '*[^A-Za-z0-9_.-]*'),
 phase TEXT NOT NULL CHECK(phase IN ('reserved','preparing','upload_possible','upload_returned','dispatch_possible','finalized')),
 attempt_result TEXT NOT NULL CHECK(attempt_result IN ('pending','accepted','rejected','not_dispatched','uncertain')),
 generation INTEGER NOT NULL CHECK(generation>0),
 created_at INTEGER NOT NULL CHECK(created_at>0), updated_at INTEGER NOT NULL CHECK(updated_at>=created_at),
 preparing_at INTEGER,upload_possible_at INTEGER,upload_returned_at INTEGER,dispatch_possible_at INTEGER,finalized_at INTEGER,
 error_code TEXT NOT NULL CHECK(error_code IN ('','canceled','deadline','preparation_failed','identity_changed','upload_error','transport_error','server_error','store_error')),
 FOREIGN KEY(draft_id,revision_id) REFERENCES draft_revisions(draft_id,id),
 UNIQUE(account_jid,idempotency_key), UNIQUE(account_jid,message_id),
 CHECK(preparing_at IS NULL OR preparing_at BETWEEN created_at AND updated_at),
 CHECK(upload_possible_at IS NULL OR (preparing_at IS NOT NULL AND upload_possible_at BETWEEN preparing_at AND updated_at)),
 CHECK(upload_returned_at IS NULL OR (upload_possible_at IS NOT NULL AND upload_returned_at BETWEEN upload_possible_at AND updated_at)),
 CHECK(dispatch_possible_at IS NULL OR (preparing_at IS NOT NULL AND dispatch_possible_at BETWEEN COALESCE(upload_returned_at,preparing_at) AND updated_at)),
 CHECK(finalized_at IS NULL OR finalized_at BETWEEN COALESCE(dispatch_possible_at,upload_returned_at,upload_possible_at,preparing_at,created_at) AND updated_at),
 CHECK((phase='finalized' AND finalized_at IS NOT NULL AND attempt_result!='pending') OR (phase!='finalized' AND finalized_at IS NULL AND attempt_result='pending' AND error_code='')),
 CHECK(attempt_result NOT IN ('accepted','rejected') OR dispatch_possible_at IS NOT NULL),
 CHECK(attempt_result!='not_dispatched' OR dispatch_possible_at IS NULL),
 CHECK(attempt_result!='uncertain' OR dispatch_possible_at IS NOT NULL OR upload_possible_at IS NOT NULL),
 CHECK(phase!='reserved' OR (preparing_at IS NULL AND upload_possible_at IS NULL AND dispatch_possible_at IS NULL)),
 CHECK(phase!='preparing' OR (preparing_at IS NOT NULL AND upload_possible_at IS NULL AND dispatch_possible_at IS NULL)),
 CHECK(phase!='upload_possible' OR (upload_possible_at IS NOT NULL AND upload_returned_at IS NULL AND dispatch_possible_at IS NULL)),
 CHECK(phase!='upload_returned' OR (upload_returned_at IS NOT NULL AND dispatch_possible_at IS NULL)),
 CHECK(phase!='dispatch_possible' OR dispatch_possible_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_outbound_created_id ON outbound_operations(created_at,id);
CREATE INDEX IF NOT EXISTS idx_outbound_account_created_id ON outbound_operations(account_jid,created_at,id);
CREATE TRIGGER IF NOT EXISTS outbound_initial_checkpoint BEFORE INSERT ON outbound_operations WHEN NEW.phase!='reserved' OR NEW.attempt_result!='pending' OR NEW.generation!=1 OR NEW.updated_at!=NEW.created_at BEGIN SELECT RAISE(ABORT,'outbound starts with a reservation'); END;
CREATE TRIGGER IF NOT EXISTS outbound_revision_binding BEFORE INSERT ON outbound_operations WHEN NOT EXISTS (SELECT 1 FROM drafts d JOIN draft_revisions r ON r.draft_id=d.id WHERE d.id=NEW.draft_id AND r.id=NEW.revision_id AND d.state='active' AND d.account_jid=NEW.account_jid AND r.payload_hash=NEW.payload_hash AND r.payload_version=1) BEGIN SELECT RAISE(ABORT,'invalid outbound revision binding'); END;
CREATE TRIGGER IF NOT EXISTS outbound_scope_immutable BEFORE UPDATE OF id,version,account_jid,idempotency_key,draft_id,revision_id,payload_hash,message_id,created_at ON outbound_operations BEGIN SELECT RAISE(ABORT,'outbound binding is immutable'); END;
CREATE TRIGGER IF NOT EXISTS outbound_retained BEFORE DELETE ON outbound_operations BEGIN SELECT RAISE(ABORT,'outbound operations are retained'); END;
CREATE TRIGGER IF NOT EXISTS outbound_cas BEFORE UPDATE ON outbound_operations WHEN NEW.generation!=OLD.generation+1 OR NEW.updated_at<OLD.updated_at BEGIN SELECT RAISE(ABORT,'outbound CAS is required'); END;
CREATE TRIGGER IF NOT EXISTS outbound_transition BEFORE UPDATE ON outbound_operations WHEN
 (NEW.phase=OLD.phase AND (NEW.attempt_result!=OLD.attempt_result OR NEW.error_code!=OLD.error_code OR NEW.preparing_at IS NOT OLD.preparing_at OR NEW.upload_possible_at IS NOT OLD.upload_possible_at OR NEW.upload_returned_at IS NOT OLD.upload_returned_at OR NEW.dispatch_possible_at IS NOT OLD.dispatch_possible_at OR NEW.finalized_at IS NOT OLD.finalized_at)) OR
 (NEW.phase!=OLD.phase AND NOT ((OLD.phase='reserved' AND NEW.phase='preparing') OR (OLD.phase='preparing' AND NEW.phase IN ('upload_possible','dispatch_possible')) OR (OLD.phase='upload_possible' AND NEW.phase='upload_returned') OR (OLD.phase='upload_returned' AND NEW.phase='dispatch_possible') OR (OLD.phase!='finalized' AND NEW.phase='finalized'))) OR
 (OLD.preparing_at IS NOT NULL AND NEW.preparing_at IS NOT OLD.preparing_at) OR
 (OLD.upload_possible_at IS NOT NULL AND NEW.upload_possible_at IS NOT OLD.upload_possible_at) OR
 (OLD.upload_returned_at IS NOT NULL AND NEW.upload_returned_at IS NOT OLD.upload_returned_at) OR
 (OLD.dispatch_possible_at IS NOT NULL AND NEW.dispatch_possible_at IS NOT OLD.dispatch_possible_at) OR
 (OLD.finalized_at IS NOT NULL AND NEW.finalized_at IS NOT OLD.finalized_at)
 BEGIN SELECT RAISE(ABORT,'invalid outbound transition'); END;
CREATE TABLE IF NOT EXISTS outbound_observations (
 id INTEGER PRIMARY KEY,
 operation_id TEXT NOT NULL REFERENCES outbound_operations(id),
 fact TEXT NOT NULL CHECK(fact IN ('ack','own_echo','delivered','read','server_error')),
 source TEXT NOT NULL CHECK(source IN ('send_response','live_receipt','live_echo','history_echo')),
 chat_jid TEXT NOT NULL CHECK(length(chat_jid) BETWEEN 1 AND 128),
 actor_jid TEXT NOT NULL CHECK(length(actor_jid)<=128),actor_alias TEXT NOT NULL CHECK(length(actor_alias)<=128),
 device INTEGER NOT NULL CHECK(device BETWEEN 0 AND 65535),
 event_at INTEGER CHECK(event_at>0), observed_at INTEGER NOT NULL CHECK(observed_at>0),
 error_code TEXT NOT NULL CHECK(error_code IN ('','canceled','deadline','preparation_failed','identity_changed','upload_error','transport_error','server_error','store_error')),
 CHECK((fact='ack' AND source='send_response') OR (fact='own_echo' AND source IN ('live_echo','history_echo')) OR (fact IN ('delivered','read','server_error') AND source='live_receipt')),
 CHECK(fact='server_error' OR error_code=''),
 CHECK(fact IN ('ack','server_error') OR length(actor_jid)>0)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbound_observation_semantics ON outbound_observations(operation_id,fact,source,chat_jid,actor_jid,actor_alias,device,COALESCE(event_at,0),error_code);
CREATE TRIGGER IF NOT EXISTS outbound_accepted_ack BEFORE UPDATE ON outbound_operations WHEN NEW.attempt_result='accepted' AND NOT EXISTS (SELECT 1 FROM outbound_observations WHERE operation_id=NEW.id AND fact='ack') BEGIN SELECT RAISE(ABORT,'accepted result requires retained ack'); END;
CREATE TRIGGER IF NOT EXISTS outbound_observation_immutable BEFORE UPDATE ON outbound_observations BEGIN SELECT RAISE(ABORT,'outbound facts are immutable'); END;
CREATE TRIGGER IF NOT EXISTS outbound_observation_retained BEFORE DELETE ON outbound_observations BEGIN SELECT RAISE(ABORT,'outbound facts are retained'); END;
`)
	return err
}
