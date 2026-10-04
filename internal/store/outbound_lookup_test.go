package store

import (
	"strings"
	"testing"
)

func TestOutboundByMessageIndexedFrozenSummaryAndIsolation(t *testing.T) {
	db := openTestDB(t)
	var ops []OutboundOperation
	for i, account := range []string{"15550000001@s.whatsapp.net", "15550000009@s.whatsapp.net"} {
		_, r := outboundFixture(t, db, i+1, DraftTextKind, account)
		r.MessageID = "SAME-ID"
		o, err := db.Outbound().Reserve(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, o)
	}
	for _, want := range ops {
		got, err := db.Outbound().ByMessage(t.Context(), want.Account.PN, want.MessageID)
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	_, err := db.Outbound().ByMessage(t.Context(), "15550000008@s.whatsapp.net", "SAME-ID")
	assertOutboundCode(t, err, "not_found")
	_, err = db.Outbound().ByMessage(t.Context(), ops[0].Account.PN, "MISSING")
	assertOutboundCode(t, err, "not_found")
	_, err = db.Outbound().ByMessage(t.Context(), ops[0].Account.PN, "invalid / id")
	assertOutboundCode(t, err, "invalid_arguments")
	rows, err := db.sql.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT "+outboundColumns+outboundFrom+"WHERE o.account_jid=? AND o.message_id=?", ops[0].Account.PN, "SAME-ID")
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH o USING INDEX") && strings.Contains(detail, "account_jid=? AND message_id=?") {
			indexed = true
		}
		if strings.Contains(detail, "SCAN o") || strings.Contains(detail, "outbound_observations") {
			t.Fatal("catalogue/facts scan", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !indexed {
		t.Fatal("account/message UNIQUE index not used")
	}
	// The event lookup validates the summary but does not load a full payload.
	// Corrupt summary data must be a store error, not an absent match.
	if _, err := db.sql.Exec(`DROP TRIGGER draft_revision_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE draft_revisions SET payload_json='{}' WHERE id=?`, ops[0].RevisionID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.Outbound().ByMessage(t.Context(), ops[0].Account.PN, ops[0].MessageID); err != nil || got.ID != ops[0].ID {
		t.Fatal("event lookup loaded full payload", got, err)
	}
	_, err = db.Outbound().Read(t.Context(), ops[0].ID, "", "", 1, "")
	assertOutboundCode(t, err, "store_error")
	if _, err := db.sql.Exec(`UPDATE draft_revisions SET summary_json='{}' WHERE id=?`, ops[0].RevisionID); err != nil {
		t.Fatal(err)
	}
	_, err = db.Outbound().ByMessage(t.Context(), ops[0].Account.PN, ops[0].MessageID)
	assertOutboundCode(t, err, "store_error")
}
