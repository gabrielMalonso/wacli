package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The existing exact retry fixture runs production CLI/App with fake WA and
// explicit byte transport. AppState notification is injected during Connect.
func TestAgentMediaRetryPreservesAppStateReplayBeforeConnection(t *testing.T) {
	dir, data := seedAgentRetry(t, false)
	retryFixtureSQL(t, dir, "UPDATE chats SET archived=0,pinned=1")
	f := &mediaRetryCLIWA{authed: true}
	f.connectHook = func() error {
		reader, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		pending, err := reader.DB().AppStateRecoveryCollections()
		reader.Close()
		if err != nil || len(pending) != 3 {
			t.Fatalf("missing pre-connect debt: %v %v", pending, err)
		}
		chat, err := types.ParseJID(mediaFixtureChat)
		if err != nil {
			t.Fatal(err)
		}
		f.emit(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}})
		f.emit(&events.Pin{JID: chat, Action: &waSyncAction.PinAction{Pinned: proto.Bool(false)}})
		return nil
	}
	var output bytes.Buffer
	err := retryFixtureRunner(t, dir, f, &output, func(context.Context, store.MediaDownloadInfo, string, bool) ([]byte, error) { return data, nil })
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	row, err := a.DB().GetChat(mediaFixtureChat)
	if err != nil {
		t.Fatal(err)
	}
	debt, err := a.DB().AppStateRecoveryRequired("regular_low")
	if err != nil {
		t.Fatal(err)
	}
	if row.Archived || !row.Pinned || !debt {
		t.Fatalf("archive=%v pin=%v debt=%v", row.Archived, row.Pinned, debt)
	}
	if f.connects.Load() != 1 || f.receipts.Load() != 1 {
		t.Fatal("fixture skipped network-required recovery")
	}
	t.Logf("retry completed with one connection/receipt; mirror=(%v,%v), debt=%v with durable debt for unobserved notifications", row.Archived, row.Pinned, debt)
}

func TestAgentMediaRetryLocalReuseAddsNoAppStateDebt(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache", true: "destination"}[existing], func(t *testing.T) {
			dir, data := seedAgentRetry(t, true)
			if existing {
				if err := os.WriteFile(filepath.Join(dir, "media", "output"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			f := &mediaRetryCLIWA{authed: true}
			var output bytes.Buffer
			if err := retryFixtureRunner(t, dir, f, &output, nil); err != nil {
				t.Fatal(err)
			}
			reader, err := app.New(app.Options{StoreDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			pending, err := reader.DB().AppStateRecoveryCollections()
			if err != nil || len(pending) != 0 || f.opens.Load() != 0 || f.connects.Load() != 0 || f.receipts.Load() != 0 {
				t.Fatalf("local reuse added debt/WA: %v %v opens=%d connects=%d receipts=%d", pending, err, f.opens.Load(), f.connects.Load(), f.receipts.Load())
			}
		})
	}
}

func TestAgentMediaRetryIntentFailurePreventsConnection(t *testing.T) {
	dir, _ := seedAgentRetry(t, false)
	retryFixtureSQL(t, dir, `CREATE TRIGGER fail_app_state_intent BEFORE INSERT ON app_state_recovery_intents BEGIN SELECT RAISE(ABORT,'synthetic marker failure'); END`)
	f := &mediaRetryCLIWA{authed: true}
	var output bytes.Buffer
	err := retryFixtureRunner(t, dir, f, &output, nil)
	if err == nil || classifyMediaRetryCommandError(err).Code != "store_failed" || f.connects.Load() != 0 || f.receipts.Load() != 0 {
		t.Fatalf("connection crossed failed intent: %v connects=%d receipts=%d", err, f.connects.Load(), f.receipts.Load())
	}
}
