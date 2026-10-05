package store

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMessageContextEffectiveIdentitiesAndRawBounds(t *testing.T) {
	for _, targetTS := range []int64{-1, 0, 100} {
		t.Run(time.Unix(targetTS, 0).String(), func(t *testing.T) {
			db := openTestDB(t)
			const pn, lid, third = "15550000001@s.whatsapp.net", "100000000001@lid", "15550000002@s.whatsapp.net"
			for _, jid := range []string{pn, lid, third} {
				if err := db.UpsertChat(jid, "dm", "Synthetic", time.Time{}); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range []struct {
				jid, id string
				delta   int64
				deleted bool
			}{
				{pn, "older", -1, false},
				{lid, "tie-before", 0, false},
				{third, "third-before", 0, false},
				{lid, "deleted-before", 0, true},
				{pn, "target", 0, true},
				{lid, "tie-after", 0, false},
				{lid, "deleted-after", 0, true},
				{third, "third-after", 0, false},
				{pn, "newer", 1, false},
			} {
				if err := db.UpsertMessage(UpsertMessageParams{ChatJID: m.jid, MsgID: m.id, Text: m.id, Timestamp: time.Unix(targetTS+m.delta, 0), Revoked: m.deleted}); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				before, after int
				want          []string
			}{
				{1, 1, []string{"tie-before", "target", "tie-after"}},
				{0, 0, []string{"target"}},
				{0, 2, []string{"target", "tie-after", "newer"}},
				{2, 0, []string{"older", "tie-before", "target"}},
				{99, 99, []string{"older", "tie-before", "target", "tie-after", "newer"}},
			} {
				msgs, err := db.MessageContextForChats([]string{pn, lid, pn}, "target", tc.before, tc.after)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, m := range msgs {
					ids = append(ids, m.MsgID)
					if m.MsgID == "target" && (!m.Revoked || m.rowTS != targetTS || m.Timestamp.IsZero() != (targetTS <= 0)) {
						t.Fatalf("target changed: %+v raw=%d", m, m.rowTS)
					}
				}
				if !reflect.DeepEqual(ids, tc.want) {
					t.Fatalf("before=%d after=%d: got=%v want=%v", tc.before, tc.after, ids, tc.want)
				}
			}
			// Single-chat callers use the same raw anchor, with no alias expansion.
			msgs, err := db.MessageContext(pn, "target", 1, 1)
			if err != nil || len(msgs) != 3 || msgs[0].MsgID != "older" || msgs[1].MsgID != "target" || msgs[2].MsgID != "newer" {
				t.Fatalf("single chat context: %+v %v", msgs, err)
			}
		})
	}
}

func TestMessageContextTargetIdentityPriority(t *testing.T) {
	db := openTestDB(t)
	for _, jid := range []string{"pn", "lid"} {
		if err := db.UpsertChat(jid, "dm", "Synthetic", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertMessage(UpsertMessageParams{ChatJID: jid, MsgID: "conflict", Text: jid, Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range [][]string{{"pn", "lid"}, {"lid", "pn"}, {"absent", "lid", "pn"}} {
		msgs, err := db.MessageContextForChats(order, "conflict", 0, 0)
		want := order[0]
		if want == "absent" {
			want = "lid"
		}
		if err != nil || len(msgs) != 1 || msgs[0].ChatJID != want {
			t.Fatalf("order=%v context=%+v error=%v", order, msgs, err)
		}
	}
	for _, order := range [][]string{nil, {"absent"}, {"pn", "lid"}} {
		if _, err := db.MessageContextForChats(order, "missing", 1, 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("order=%v missing target error=%v", order, err)
		}
	}
}
