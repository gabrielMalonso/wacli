package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

func TestAgentSearchSenderNormalizationAcrossOrders(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, sender string }{{"pn-sender", localReadPN}, {"lid-sender", localReadLID}} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: localReadLID, MsgID: m.id, SenderJID: m.sender, Text: "senderfixture", Timestamp: time.Unix(100, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, sortBy := range []string{"relevance", "time"} {
		for _, sender := range []string{"15550000001", localReadPN, "  " + localReadPN + "  ", "15550000001:4@s.whatsapp.net"} {
			stdout, stderr, err := runAgentTest(t, "--read-only", "--store", dir, "--agent", "messages", "search", "senderfixture", "--sort", sortBy, "--from", sender)
			if err != nil || stderr != "" {
				t.Fatalf("sort=%s sender=%q: %v %s", sortBy, sender, err, stderr)
			}
			var data agentMessages
			if err := json.Unmarshal(decodeAgentTest(t, stdout).Data, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Messages) != 1 || data.Messages[0].ID != "pn-sender" {
				t.Fatalf("sort=%s sender=%q: %s", sortBy, sender, stdout)
			}
		}
	}
	// Legacy search retains its exact stored-JID filter.
	for _, sender := range []string{"15550000001", localReadPN} {
		stdout, err := runLocalRead(t, dir, []string{"messages", "search", "senderfixture", "--from", sender})
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Data struct{ Messages []store.Message } `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatal(err)
		}
		want := 0
		if sender == localReadPN {
			want = 1
		}
		if len(env.Data.Messages) != want {
			t.Fatal(stdout)
		}
	}
	if !reflect.DeepEqual(snapshotLocalStore(t, dir), before) {
		t.Fatal("search changed archive/session/permissions")
	}
}

func TestMessageContextCLIUsesVerifiedAliasesAndKeepsCaps(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(localReadPN, "dm", "Synthetic", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, jid string }{{"before", localReadLID}, {"target", localReadPN}, {"after", localReadLID}} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: m.jid, MsgID: m.id, Text: "fixture", Timestamp: time.Unix(0, 0), Revoked: m.id == "target"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	for _, chat := range []string{localReadPN, localReadLID} {
		for _, detail := range []string{"compact", "full"} {
			stdout, stderr, err := runAgentTest(t, "--read-only", "--store", dir, "--agent", "--detail", detail, "messages", "context", "--chat", chat, "--id", "target", "--before", "1", "--after", "1")
			if err != nil || stderr != "" {
				t.Fatalf("%v %s", err, stderr)
			}
			env := decodeAgentTest(t, stdout)
			var data agentMessages
			if err := json.Unmarshal(env.Data, &data); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range data.Messages {
				ids = append(ids, m.ID)
				if m.Timestamp != nil {
					t.Fatal("nonpositive public timestamp must stay null")
				}
			}
			if !reflect.DeepEqual(ids, []string{"before", "target", "after"}) || !data.Messages[1].Revoked || env.Meta.Limit != 3 || !reflect.DeepEqual(env.Meta.Excluded, []string{"tombstones"}) {
				t.Fatal(stdout)
			}
		}
	}
	stdout, err := runLocalRead(t, dir, []string{"messages", "context", "--chat", localReadLID, "--id", "target", "--before", "1", "--after", "1"})
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct{ Data []store.Message }
	if err := json.Unmarshal([]byte(stdout), &legacy); err != nil || len(legacy.Data) != 3 || !legacy.Data[1].Timestamp.IsZero() {
		t.Fatalf("legacy context: %s %v", stdout, err)
	}
	for _, beforeCount := range []int{199, 200} {
		stdout, _, err := runAgentTest(t, "--read-only", "--store", dir, "--agent", "messages", "context", "--chat", localReadPN, "--id", "target", "--before", strconv.Itoa(beforeCount), "--after", "0")
		if beforeCount == 199 {
			if err != nil || decodeAgentTest(t, stdout).Meta.Limit != 200 {
				t.Fatalf("inclusive cap: %s %v", stdout, err)
			}
		} else if err == nil || stdout != "" {
			t.Fatalf("excessive context should fail: %s %v", stdout, err)
		}
	}
	if !reflect.DeepEqual(snapshotLocalStore(t, dir), before) {
		t.Fatal("context changed archive/session/permissions")
	}
}

func TestChatsListLegacyPinnedDisplayOrder(t *testing.T) {
	dir := seedLocalReadStore(t)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(localReadPN, "dm", "Pinned synthetic", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChatPinned(localReadPN, true); err != nil {
		t.Fatal(err)
	}
	const recent = "recent@g.us"
	if err := db.UpsertChat(recent, "group", "Recent synthetic", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalStore(t, dir)
	stdout, err := runLocalRead(t, dir, []string{"chats", "list"})
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Data []store.Chat }
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Data) == 0 || env.Data[0].JID != localReadPN || !env.Data[0].Pinned {
		t.Fatalf("legacy JSON pin order: %s %v", stdout, err)
	}
	for _, c := range env.Data[1:] {
		if c.JID == localReadPN || c.JID == localReadLID {
			t.Fatal("mapped duplicate was not fused")
		}
	}
	stdout = captureRootStdout(t, func() {
		err = execute([]string{"--read-only", "--store", dir, "chats", "list"})
	})
	if err != nil || strings.Index(stdout, localReadPN) < 0 || strings.Index(stdout, localReadPN) > strings.Index(stdout, recent) {
		t.Fatalf("legacy table pin order: %s %v", stdout, err)
	}
	if !reflect.DeepEqual(snapshotLocalStore(t, dir), before) {
		t.Fatal("chat display changed archive/session/permissions")
	}
}
