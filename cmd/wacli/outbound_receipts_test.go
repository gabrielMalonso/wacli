package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The production App receives SDK event types. Unexpected WA operations hit the
// absent embedded interface; this fixture never connects, authenticates or sends.
type outboundQueryFixtureWA struct {
	app.WAClient
	account store.DraftIdentity
	handler func(any)
}

func (f *outboundQueryFixtureWA) LinkedJID() string { return f.account.PN }
func (f *outboundQueryFixtureWA) LinkedLID() string { return f.account.LID }
func (f *outboundQueryFixtureWA) AddEventHandler(h func(any)) uint32 {
	f.handler = h
	return 1
}
func (f *outboundQueryFixtureWA) RemoveEventHandler(uint32) { f.handler = nil }
func (f *outboundQueryFixtureWA) Disconnect()               {}
func (f *outboundQueryFixtureWA) Close()                    {}
func (f *outboundQueryFixtureWA) CheckPublicPair(context.Context, types.JID, types.JID) (wa.PublicPairResult, error) {
	return wa.PublicPairUnverified, errors.New("fixture forbids alias discovery")
}

func exerciseOutboundReceiptQueries(t *testing.T, binary string) {
	t.Helper()
	dir := t.TempDir()
	db, o := outboundCLISeed(t, dir, 1)
	for _, phase := range []store.OutboundPhase{store.OutboundPreparing, store.OutboundUploadPossible, store.OutboundUploadReturned, store.OutboundDispatchPossible, store.OutboundFinalized} {
		result, code := store.OutboundPending, ""
		if phase == store.OutboundFinalized {
			result, code = store.OutboundUncertain, "transport_error"
		}
		var err error
		o, err = db.Outbound().Checkpoint(t.Context(), store.OutboundCheckpoint{ID: o.ID, Account: o.Account, MessageID: o.MessageID, Generation: o.Generation, Phase: phase, Result: result, ErrorCode: code, At: o.UpdatedAt.Add(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	owner, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	f := &outboundQueryFixtureWA{account: o.Account}
	a, err := app.New(app.Options{StoreDir: dir, WAFactory: func(wa.Options) (app.WAClient, error) { return f, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() // writer/observer closes before LOCK release
	if err := a.OpenWA(); err != nil {
		t.Fatal(err)
	}
	chat, _ := types.ParseJID(o.Recipient.LID)
	peer, _ := types.ParseJID(o.Recipient.PN)
	own, _ := types.ParseJID(o.Account.PN)
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: chat, Sender: chat, SenderAlt: peer}, MessageIDs: []string{o.MessageID}, Type: types.ReceiptTypeRead, Timestamp: time.Unix(1700000010, 0)}
	f.handler(receipt)
	f.handler(receipt)
	f.handler(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: own, IsFromMe: true}, ID: o.MessageID}, Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("fixture.txt")}}})
	for _, detail := range []string{"compact", "full"} {
		for _, selection := range [][]string{{"show", o.ID}, {"list", "--account-jid", o.Account.PN}} {
			args := append([]string{"--agent", "--read-only", "--store", dir, "outbound"}, selection...)
			args = append(args, "--detail", detail, "--limit", "1")
			raw, stderr, err := runDraftBinary(t, binary, args, true)
			if err != nil || stderr != "" || !json.Valid([]byte(raw)) || len(raw) > 1<<20 {
				t.Fatal("one bounded envelope", err, stderr, raw)
			}
			e := decodeAgentTest(t, raw)
			var data struct {
				Operation  outboundDTO   `json:"operation"`
				Operations []outboundDTO `json:"operations"`
			}
			if err := json.Unmarshal(e.Data, &data); err != nil {
				t.Fatal(err)
			}
			dto := data.Operation
			if selection[0] == "list" {
				if len(data.Operations) != 1 {
					t.Fatal(data)
				}
				dto = data.Operations[0]
			} else if !e.Meta.Page.HasMore {
				t.Fatal("show page omitted later echo")
			}
			if dto.Status != "read" || dto.AttemptResult != store.OutboundUncertain || !dto.Evidence.OwnEcho || dto.Evidence.Accepted != "observed" || (dto.Checkpoints != nil) != (detail == "full") || e.Meta.Source != "local" {
				t.Fatal(dto)
			}
			if strings.Contains(raw, "snapshot_path") || strings.Contains(raw, "media_key") {
				t.Fatal("private data", raw)
			}
		}
	}
}

func TestOutboundReceiptReadonlyQueries(t *testing.T) {
	exerciseOutboundReceiptQueries(t, "")
}

func TestOutboundProductionBinaryReceiptReadonlyQueries(t *testing.T) {
	binary := os.Getenv("WACLI_OUTBOUND_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_OUTBOUND_E2E_BINARY to a freshly built local binary")
	}
	exerciseOutboundReceiptQueries(t, binary)
}
