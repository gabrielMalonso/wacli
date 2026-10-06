package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The fake exercises application invocations, never SDK frames or remote ACKs.
type outboundFake struct {
	account             store.DraftIdentity
	uploads, sends, ids int
	uploadType          whatsmeow.MediaType
	sent                *waE2E.Message
	bytes               []byte
	id                  string
	onSend              func(context.Context, types.JID, string, *waE2E.Message) (whatsmeow.SendResponse, error)
	onUpload            func(context.Context, []byte) (whatsmeow.UploadResponse, error)
}

func (f *outboundFake) LinkedJID() string { return f.account.PN }
func (f *outboundFake) LinkedLID() string { return f.account.LID }
func (f *outboundFake) GenerateOutboundMessageID() (string, error) {
	f.ids++
	return "3EB0NORMALFIXTURE", nil
}
func (f *outboundFake) SendOutbound(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	f.sends++
	f.id = id
	f.sent = proto.Clone(msg).(*waE2E.Message)
	if f.onSend != nil {
		return f.onSend(ctx, to, id, msg)
	}
	return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("90001", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
}
func (f *outboundFake) Upload(ctx context.Context, b []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads++
	f.uploadType = kind
	f.bytes = bytes.Clone(b)
	if f.onUpload != nil {
		return f.onUpload(ctx, b)
	}
	d := sha256.Sum256(b)
	return whatsmeow.UploadResponse{URL: "https://synthetic.invalid/media", DirectPath: "/fixture", FileLength: uint64(len(b)), FileSHA256: d[:], FileEncSHA256: make([]byte, 32), MediaKey: make([]byte, 32)}, nil
}

func outboundAppFixture(t *testing.T, kind store.DraftKind) (*App, outboundRunner, OutboundSendRequest, *outboundFake, store.DraftRevision) {
	t.Helper()
	a := draftAppFixture(t)
	input := DraftInput{To: "90002@lid", Message: draftTextPointer("literal\\n\n👋")}
	if kind == store.DraftContactKind {
		input.Message = nil
		contact, err := store.NewDraftContact("Fixture", "15550000003")
		if err != nil {
			t.Fatal(err)
		}
		input.Contact = &contact
	}
	if kind == store.DraftDocumentKind {
		input.Message = nil
		input.File = filepath.Join(a.StoreDir(), "source.txt")
		if err := os.WriteFile(input.File, []byte("same bytes\x00\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == store.DraftImageKind {
		input.Message = nil
		input.Image = &DraftImageInput{Path: filepath.Join(a.StoreDir(), "source.png")}
		var data bytes.Buffer
		if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 1))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input.Image.Path, data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		input.Caption = " literal\\n\n🔷 "
		input.ReplyTo = "image-quoted"
		if err := a.DB().UpsertChat(input.To, "dm", "fixture", time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: input.To, MsgID: input.ReplyTo, SenderJID: input.To, Text: " frozen\n quote ", Timestamp: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if kind == store.DraftVoiceKind {
		input, _ = voiceDraftInput(t, a)
	}
	e, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, input), os.Open)
	if err != nil {
		t.Fatal(err)
	}
	requestID, _ := store.NewDraftID()
	p := e.Revision.Payload().Data()
	r := OutboundSendRequest{Version: 1, RequestID: requestID, StoreRef: a.StoreDir(), OwnPN: p.Account.PN, DraftID: e.Record.ID, RevisionID: e.Revision.ID(), Hash: e.Revision.Payload().Hash(), Key: "fixture-key"}
	f := &outboundFake{account: p.Account}
	x := outboundRunner{repository: a.DB().Outbound(), readDraft: a.DB().ReadDraft, identities: a.ReadDraftIdentities, open: func() (wa.OutboundClient, error) { return f, nil }, connect: func(context.Context) error { return nil }, document: func(ctx context.Context, r store.DraftRevision) ([]byte, error) {
		return readOutboundDocument(ctx, a.StoreDir(), r)
	}, history: a.persistOutboundHistory, finalBudget: time.Second}
	return a, x, r, f, e.Revision
}

func TestOutboundTypedPayloadAndRetainedDuplicate(t *testing.T) {
	for _, kind := range []store.DraftKind{store.DraftTextKind, store.DraftContactKind, store.DraftDocumentKind, store.DraftImageKind, store.DraftVoiceKind} {
		t.Run(string(kind), func(t *testing.T) {
			a, x, r, f, rev := outboundAppFixture(t, kind)
			if kind == store.DraftImageKind {
				if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "90002@lid", MsgID: "image-quoted", SenderJID: "90002@lid", Text: "edited after preparation", Timestamp: time.Unix(2, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			if kind == store.DraftVoiceKind {
				if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "90002@lid", MsgID: "voice-quoted", SenderJID: "90002@lid", Text: "edited after voice preparation", Timestamp: time.Unix(2, 0)}); err != nil {
					t.Fatal(err)
				}
			}
			admissions := 0
			result, err := x.send(t.Context(), r, func(context.Context) error { admissions++; return nil })
			if err != nil || result.Entry.Operation.Result != store.OutboundAccepted || result.Persistence != "confirmed" || result.KnownACK == nil {
				t.Fatal(result, err)
			}
			if f.sends != 1 || f.ids != 1 || f.id != result.Entry.Operation.MessageID || admissions != 1 {
				t.Fatal("invocation/ID", f, admissions)
			}
			to, want, err := prepareOutboundPayload(rev.Payload().Data(), nil)
			if err != nil || to.String() != "90002@lid" {
				t.Fatal(to, err)
			}
			if kind == store.DraftDocumentKind {
				if !bytes.Equal(f.bytes, []byte("same bytes\x00\n")) || f.uploads != 1 || f.sent.GetDocumentMessage().GetFileLength() != uint64(len(f.bytes)) {
					t.Fatal(f)
				}
				media, err := a.DB().GetMediaDownloadInfo(result.Entry.Operation.Recipient.JID, f.id)
				if err != nil || media.DirectPath != "/fixture" || len(media.MediaKey) != 32 || media.FileLength != uint64(len(f.bytes)) {
					t.Fatal("secondary document projection", media, err)
				}
			} else if kind == store.DraftImageKind {
				value := rev.Payload().Data().Image
				msg := f.sent.GetImageMessage()
				if f.uploadType != whatsmeow.MediaImage || msg.GetWidth() != value.Width || msg.GetHeight() != value.Height || msg.GetCaption() != value.Caption || !bytes.Equal(msg.GetJPEGThumbnail(), value.JPEGThumbnail) || msg.GetFileLength() != uint64(value.Size) {
					t.Fatal("image wire", msg)
				}
				quote := msg.GetContextInfo()
				if quote.GetStanzaID() != "image-quoted" || quote.GetParticipant() != "90002@lid" || quote.GetRemoteJID() != "90002@lid" || quote.GetQuotedMessage().GetConversation() != " frozen\n quote " {
					t.Fatal("image quote followed current history", quote)
				}
				info, err := a.DB().GetMediaDownloadInfo(result.Entry.Operation.Recipient.JID, f.id)
				if err != nil || info.MediaType != "image" || info.MimeType != value.MIME {
					t.Fatal(info, err)
				}
			} else if kind == store.DraftVoiceKind {
				msg := f.sent.GetAudioMessage()
				value := rev.Payload().Data().Voice
				if f.uploadType != whatsmeow.MediaAudio || !msg.GetPTT() || msg.Seconds != nil || msg.Waveform != nil || msg.GetMimetype() != value.MIME || msg.GetFileLength() != uint64(value.Size) || !bytes.Equal(f.bytes, voiceFixtureBytes(t)) {
					t.Fatal("voice wire", msg)
				}
				if msg.GetContextInfo().GetQuotedMessage().GetConversation() != " frozen\n voice quote " {
					t.Fatal("voice quote followed current history")
				}
				info, err := a.DB().GetMediaDownloadInfo(result.Entry.Operation.Recipient.JID, f.id)
				if err != nil || info.MediaType != "audio" || info.MimeType != value.MIME {
					t.Fatal(info, err)
				}
				if err := os.Rename(filepath.Join(a.StoreDir(), rev.Review().SnapshotPath), filepath.Join(a.StoreDir(), rev.Review().SnapshotPath)+".retained"); err != nil {
					t.Fatal(err)
				}
			} else if !proto.Equal(f.sent, want) || f.uploads != 0 {
				t.Fatal("payload", f.sent, want)
			}
			if result.Entry.Evidence.Accepted != "observed" || result.Entry.Evidence.Delivered != "unknown" || result.Entry.Evidence.Read != "unknown" {
				t.Fatal("invented delivery", result.Entry.Evidence)
			}
			if _, err := a.DB().GetChat(result.Entry.Operation.Recipient.JID); err != nil {
				t.Fatal("secondary chat projection", err)
			}
			if _, err := a.DB().DiscardDraft(t.Context(), r.DraftID, r.RevisionID); err != nil {
				t.Fatal(err)
			}
			x.readDraft = func(context.Context, string, string) (store.DraftEntry, error) {
				t.Fatal("duplicate revision read")
				return store.DraftEntry{}, nil
			}
			x.identities = func(context.Context, []string) ([]HistoryIdentity, error) {
				t.Fatal("duplicate identity read")
				return nil, nil
			}
			x.open = func() (wa.OutboundClient, error) { t.Fatal("duplicate WA"); return nil, nil }
			again, err := x.send(t.Context(), r, func(context.Context) error { t.Fatal("duplicate pacing"); return nil })
			if err != nil || !again.Duplicate || again.Entry.Operation.ID != result.Entry.Operation.ID || f.sends != 1 {
				t.Fatal(again, err)
			}
			r.Hash = strings.Repeat("a", 64)
			_, err = x.send(t.Context(), r, nil)
			var fail *OutboundSendError
			if !errors.As(err, &fail) || fail.Code != "idempotency_conflict" {
				t.Fatal(err)
			}
		})
	}
}

func TestOutboundMilestonesFailuresAndNoApplicationRetry(t *testing.T) {
	cases := []struct {
		name           string
		phase          store.OutboundPhase
		lostCommit     bool
		sendError      bool
		uploadError    bool
		cancel         bool
		want           store.OutboundResult
		uploads, sends int
	}{
		{name: "preparing", phase: store.OutboundPreparing, want: store.OutboundNotDispatched},
		{name: "upload possible", phase: store.OutboundUploadPossible, want: store.OutboundNotDispatched},
		{name: "upload failure", uploadError: true, want: store.OutboundNotDispatched, uploads: 1},
		{name: "upload returned", phase: store.OutboundUploadReturned, want: store.OutboundNotDispatched, uploads: 1},
		{name: "dispatch refused", phase: store.OutboundDispatchPossible, want: store.OutboundNotDispatched, uploads: 1},
		{name: "dispatch commit response lost", phase: store.OutboundDispatchPossible, lostCommit: true, want: store.OutboundUncertain, uploads: 1},
		{name: "send response lost", sendError: true, want: store.OutboundUncertain, uploads: 1, sends: 1},
		{name: "caller canceled in send", cancel: true, want: store.OutboundUncertain, uploads: 1, sends: 1},
	}
	for _, kind := range []store.DraftKind{store.DraftDocumentKind, store.DraftImageKind, store.DraftVoiceKind} {
		for _, c := range cases {
			t.Run(string(kind)+"/"+c.name, func(t *testing.T) {
				_, x, r, f, _ := outboundAppFixture(t, kind)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				original := x.repository.Checkpoint
				x.repository.Checkpoint = func(ctx context.Context, ch store.OutboundCheckpoint) (store.OutboundOperation, error) {
					if ch.Phase == c.phase {
						if c.lostCommit {
							_, err := original(ctx, ch)
							if err != nil {
								return store.OutboundOperation{}, err
							}
						}
						return store.OutboundOperation{}, errors.New("synthetic commit failure")
					}
					return original(ctx, ch)
				}
				if c.uploadError {
					f.onUpload = func(context.Context, []byte) (whatsmeow.UploadResponse, error) {
						return whatsmeow.UploadResponse{}, errors.New("upload uncertain")
					}
				}
				if c.sendError || c.cancel {
					f.onSend = func(context.Context, types.JID, string, *waE2E.Message) (whatsmeow.SendResponse, error) {
						if c.cancel {
							cancel()
							return whatsmeow.SendResponse{}, context.Canceled
						}
						return whatsmeow.SendResponse{}, errors.New("lost ACK")
					}
				}
				result, err := x.send(ctx, r, nil)
				if err == nil || result.Persistence != "confirmed" || result.Entry.Operation.Result != c.want || f.uploads != c.uploads || f.sends != c.sends {
					t.Fatal(result, err, f.uploads, f.sends)
				}
				duplicate, err := x.send(t.Context(), r, nil)
				if err != nil || !duplicate.Duplicate || f.uploads != c.uploads || f.sends != c.sends {
					t.Fatal("replayed", duplicate, err)
				}
			})
		}
	}
}

func TestOutboundConcurrentObserveAckFinalizationAndHistoryWarning(t *testing.T) {
	_, x, r, f, _ := outboundAppFixture(t, store.DraftTextKind)
	base := x.repository.Checkpoint
	conflict := true
	x.repository.Checkpoint = func(ctx context.Context, ch store.OutboundCheckpoint) (store.OutboundOperation, error) {
		if ch.Phase == store.OutboundFinalized && conflict {
			conflict = false
			at := time.Now().UTC()
			_, err := x.repository.Observe(ctx, ch.ID, ch.Account, ch.MessageID, store.OutboundObservation{Fact: store.OutboundDelivered, Source: store.OutboundLiveReceipt, ChatJID: "15550000002@s.whatsapp.net", ActorJID: "90002@lid", EventAt: &at, ObservedAt: at})
			if err != nil {
				return store.OutboundOperation{}, err
			}
		}
		return base(ctx, ch)
	}
	x.history = func(store.DraftPayloadData, store.OutboundOperation, whatsmeow.SendResponse, *whatsmeow.UploadResponse) error {
		return errors.New("history unavailable")
	}
	result, err := x.send(t.Context(), r, nil)
	if err != nil || result.KnownACK == nil || !result.HistoryWarning || result.Entry.Evidence.Accepted != "observed" || result.Entry.Evidence.Delivered != "observed" || f.sends != 1 {
		t.Fatal(result, err, f.sends)
	}
}

func TestOutboundAckKnownWhenPersistenceUnavailable(t *testing.T) {
	_, x, r, f, _ := outboundAppFixture(t, store.DraftTextKind)
	base := x.repository.Checkpoint
	x.repository.Checkpoint = func(ctx context.Context, ch store.OutboundCheckpoint) (store.OutboundOperation, error) {
		if ch.Phase == store.OutboundFinalized {
			return store.OutboundOperation{}, errors.New("disk unavailable")
		}
		return base(ctx, ch)
	}
	result, err := x.send(t.Context(), r, nil)
	var typed *OutboundSendError
	if !errors.As(err, &typed) || typed.Code != "persistence_unconfirmed" || result.KnownResult != store.OutboundAccepted || result.KnownACK == nil || result.Persistence != "unconfirmed" || f.sends != 1 {
		t.Fatal(result, err)
	}
	again, err := x.send(t.Context(), r, nil)
	if err != nil || !again.Duplicate || again.KnownResult != store.OutboundPending || f.sends != 1 {
		t.Fatal("pending replay", again, err)
	}
}

func TestOutboundIdentityAndFrozenQuote(t *testing.T) {
	_, x, r, f, rev := outboundAppFixture(t, store.DraftTextKind)
	p := rev.Payload().Data()
	p.Text.Mentions = []store.DraftRecipient{p.Recipient}
	p.Reply = &store.DraftReply{ChatJID: p.Recipient.JID, ID: "frozen-quote", Sender: p.Recipient, Text: "literal quote"}
	_, msg, err := prepareOutboundPayload(p, nil)
	ci := msg.GetExtendedTextMessage().GetContextInfo()
	if err != nil || ci.GetStanzaID() != "frozen-quote" || ci.GetQuotedMessage().GetConversation() != "literal quote" || ci.GetParticipant() != "90002@lid" || len(ci.GetMentionedJID()) != 1 {
		t.Fatal(msg, err)
	}
	for _, change := range []func(*HistoryIdentity){func(i *HistoryIdentity) { i.AliasJID = "90003@lid" }, func(i *HistoryIdentity) { i.AccountJID = "15550000009@s.whatsapp.net" }, func(i *HistoryIdentity) { i.AccountAliasJID = "90009@lid" }} {
		i := HistoryIdentity{InputJID: p.Recipient.JID, ChatJID: p.Recipient.PN, AliasJID: p.Recipient.LID, AccountJID: p.Account.PN, AccountAliasJID: p.Account.LID}
		change(&i)
		if validateOutboundIdentities(p, []HistoryIdentity{i}) == nil {
			t.Fatal("accepted changed identity")
		}
	}
	x.identities = func(context.Context, []string) ([]HistoryIdentity, error) {
		return nil, errors.New("public identity unavailable")
	}
	_, err = x.send(t.Context(), r, nil)
	if err == nil || f.ids != 0 || f.sends != 0 {
		t.Fatal("discovery/SDK on unavailable identity", err)
	}
	p.Recipient.LID = ""
	if _, _, err := prepareOutboundPayload(p, nil); err == nil {
		t.Fatal("PN silently discovered")
	}
}

func TestOutboundManagedSnapshotRejectsChangedBytesAndSymlink(t *testing.T) {
	a, _, _, _, rev := outboundAppFixture(t, store.DraftDocumentKind)
	rel, _ := store.DraftSnapshotRelativePath(rev.ID())
	path := filepath.Join(a.StoreDir(), rel)
	b, err := readOutboundDocument(t.Context(), a.StoreDir(), rev)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != rev.Payload().Data().Document.SHA256 {
		t.Fatal("digest")
	}
	b[0] ^= 1
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOutboundDocument(t.Context(), a.StoreDir(), rev); err == nil {
		t.Fatal("changed digest accepted")
	}
	moved := path + ".original"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Skip(err)
	}
	if _, err := readOutboundDocument(t.Context(), a.StoreDir(), rev); err == nil {
		t.Fatal("symlink accepted")
	}
	parent := filepath.Join(a.StoreDir(), store.DraftMediaDirectory)
	if err := os.Rename(parent, parent+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent+".retained", parent); err != nil {
		t.Skip(err)
	}
	if _, err := readOutboundDocument(t.Context(), a.StoreDir(), rev); err == nil {
		t.Fatal("symlink directory accepted")
	}
}

func TestOutboundOldRevisionAndDelayedReceiptBeforeACK(t *testing.T) {
	a, x, r, f, rev := outboundAppFixture(t, store.DraftTextKind)
	p := rev.Payload().Data()
	p.Text.Text = "new head must not be sent"
	payload, err := store.NewDraftPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := store.NewDraftID()
	newRev, err := store.NewDraftRevision(r.DraftID, rid, time.Now().UTC(), payload, rev.Review())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().WriteDraft(t.Context(), newRev, r.RevisionID); err != nil {
		t.Fatal(err)
	}
	f.onSend = func(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
		e, err := x.repository.Read(ctx, "", r.Key, r.OwnPN, 1, "")
		if err != nil {
			return whatsmeow.SendResponse{}, err
		}
		done := make(chan error, 1)
		go func() {
			at := time.Now().UTC()
			_, err := x.repository.Observe(ctx, e.Operation.ID, e.Operation.Account, id, store.OutboundObservation{Fact: store.OutboundRead, Source: store.OutboundLiveReceipt, ChatJID: e.Operation.Recipient.JID, ActorJID: e.Operation.Recipient.LID, EventAt: &at, ObservedAt: at})
			done <- err
		}()
		if err := <-done; err != nil {
			return whatsmeow.SendResponse{}, err
		}
		return whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("90001", types.HiddenUserServer), Timestamp: time.Now().UTC()}, nil
	}
	result, err := x.send(t.Context(), r, nil)
	if err != nil || result.Entry.Operation.RevisionID != rev.ID() || f.sent.GetConversation() != rev.Payload().Data().Text.Text || result.Entry.Evidence.Read != "observed" || result.KnownACK == nil || f.sends != 1 {
		t.Fatal(result, err, f.sent)
	}
}

func TestOutboundLostFinalCommitRetainsKnownAcceptance(t *testing.T) {
	_, x, r, f, _ := outboundAppFixture(t, store.DraftTextKind)
	base := x.repository.Checkpoint
	lost := false
	x.repository.Checkpoint = func(ctx context.Context, ch store.OutboundCheckpoint) (store.OutboundOperation, error) {
		o, err := base(ctx, ch)
		if err == nil && ch.Phase == store.OutboundFinalized && !lost {
			lost = true
			return store.OutboundOperation{}, &store.OutboundError{Code: "write_uncertain", Cause: errors.New("lost final commit response")}
		}
		return o, err
	}
	result, err := x.send(t.Context(), r, nil)
	if err != nil || result.Persistence != "confirmed" || result.KnownACK == nil || result.Entry.Operation.Result != store.OutboundAccepted || f.sends != 1 {
		t.Fatal(result, err)
	}
}

func TestOutboundReservationLostAndPacingDeadlineNeverCrossEffect(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "pacing", true: "reserve lost"}[lost], func(t *testing.T) {
			_, x, r, f, _ := outboundAppFixture(t, store.DraftTextKind)
			if lost {
				base := x.repository.Reserve
				x.repository.Reserve = func(ctx context.Context, res store.OutboundReservation) (store.OutboundOperation, error) {
					_, err := base(ctx, res)
					if err != nil {
						return store.OutboundOperation{}, err
					}
					return store.OutboundOperation{}, &store.OutboundError{Code: "write_uncertain"}
				}
			}
			result, err := x.send(t.Context(), r, func(context.Context) error { return context.DeadlineExceeded })
			if err == nil || f.sends != 0 || f.uploads != 0 {
				t.Fatal(result, err)
			}
			if lost {
				var typed *OutboundSendError
				if !errors.As(err, &typed) || typed.OperationID == "" || typed.MessageID == "" {
					t.Fatal("lost candidate IDs", err)
				}
			} else if result.Entry.Operation.Result != store.OutboundNotDispatched {
				t.Fatal(result)
			}
			again, err := x.send(t.Context(), r, nil)
			if err != nil || !again.Duplicate || f.sends != 0 {
				t.Fatal(again, err)
			}
		})
	}
}

func TestOutboundResponseContradictionsAndUploadDigest(t *testing.T) {
	for _, bad := range []string{"id", "chat", "sender", "timestamp", "upload digest", "server rejection"} {
		t.Run(bad, func(t *testing.T) {
			kind := store.DraftTextKind
			if bad == "upload digest" {
				kind = store.DraftDocumentKind
			}
			_, x, r, f, _ := outboundAppFixture(t, kind)
			if bad == "upload digest" {
				f.onUpload = func(context.Context, []byte) (whatsmeow.UploadResponse, error) {
					return whatsmeow.UploadResponse{URL: "fixture", DirectPath: "fixture", FileLength: 12, FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32), MediaKey: make([]byte, 32)}, nil
				}
			} else {
				f.onSend = func(_ context.Context, to types.JID, id string, _ *waE2E.Message) (whatsmeow.SendResponse, error) {
					response := whatsmeow.SendResponse{ID: id, Chat: to, Sender: types.NewJID("90001", types.HiddenUserServer), Timestamp: time.Now().UTC()}
					switch bad {
					case "id":
						response.ID = "other"
					case "chat":
						response.Chat = types.NewJID("90003", types.HiddenUserServer)
					case "sender":
						response.Sender = to
					case "timestamp":
						response.Timestamp = time.Time{}
					case "server rejection":
						return response, whatsmeow.ErrServerReturnedError
					}
					return response, nil
				}
			}
			result, err := x.send(t.Context(), r, nil)
			want := store.OutboundUncertain
			if bad == "upload digest" {
				want = store.OutboundNotDispatched
			} else if bad == "server rejection" {
				want = store.OutboundRejected
			}
			if err == nil || result.Entry.Operation.Result != want || result.KnownACK != nil || f.sends > 1 || bad == "upload digest" && f.sends != 0 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestOutboundRequestCanonicalBoundedScope(t *testing.T) {
	_, _, r, _, _ := outboundAppFixture(t, store.DraftTextKind)
	raw, _ := json.Marshal(r)
	var decoded OutboundSendRequest
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != r {
		t.Fatal(decoded, err)
	}
	for _, bad := range [][]byte{[]byte(strings.Replace(string(raw), `"version":1`, `"version": 1`, 1)), []byte(strings.Replace(string(raw), `"version":1`, `"version":2`, 1)), []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"unknown":true`, 1)), []byte(strings.Replace(string(raw), r.Key, strings.Repeat("x", 8193), 1))} {
		if json.Unmarshal(bad, &decoded) == nil {
			t.Fatal("accepted noncanonical/oversized request")
		}
	}
	wrong := r
	wrong.StoreRef = "relative"
	if wrong.Validate() == nil {
		t.Fatal("relative scope")
	}
	wrong = r
	wrong.Hash = strings.ToUpper(r.Hash)
	if wrong.Validate() == nil {
		t.Fatal("hash normalization")
	}
}

func TestOutboundIdentityChangedDuringUploadStopsDispatch(t *testing.T) {
	_, x, r, f, _ := outboundAppFixture(t, store.DraftDocumentKind)
	base := x.identities
	x.identities = func(ctx context.Context, inputs []string) ([]HistoryIdentity, error) {
		ids, err := base(ctx, inputs)
		if f.uploads > 0 && len(ids) > 0 {
			ids[0].AliasJID = "90003@lid"
		}
		return ids, err
	}
	result, err := x.send(t.Context(), r, nil)
	if err == nil || result.Entry.Operation.Result != store.OutboundNotDispatched || result.Entry.Operation.UploadReturnedAt == nil || result.Entry.Operation.DispatchPossibleAt != nil || f.uploads != 1 || f.sends != 0 {
		t.Fatal(result, err, f.uploads, f.sends)
	}
}

func TestOutboundContactHistoryAndRetainedDuplicate(t *testing.T) {
	for _, name := range []string{"Fixture", "A\\B;C,D\r\nTEL:+999\rFN:injected\n尾"} {
		t.Run(name, func(t *testing.T) {
			a, x, r, f, rev := outboundAppFixture(t, store.DraftContactKind)
			if name != "Fixture" {
				contact, err := store.NewDraftContact(name, "15550000003")
				if err != nil {
					t.Fatal(err)
				}
				entry, err := a.WriteLocalDraft(t.Context(), draftAppRequest(t, a, DraftInput{To: "90002@lid", Contact: &contact}), os.Open)
				if err != nil {
					t.Fatal(err)
				}
				rev = entry.Revision
				r.DraftID, r.RevisionID, r.Hash = entry.Record.ID, rev.ID(), rev.Payload().Hash()
			}
			historyCalls := 0
			history := x.history
			x.history = func(p store.DraftPayloadData, o store.OutboundOperation, response whatsmeow.SendResponse, uploaded *whatsmeow.UploadResponse) error {
				historyCalls++
				return history(p, o, response, uploaded)
			}
			result, err := x.send(t.Context(), r, nil)
			if err != nil || result.HistoryWarning || result.KnownResult != store.OutboundAccepted || result.Persistence != "confirmed" || result.Entry.Evidence.Accepted != "observed" || result.Entry.Evidence.Delivered != "unknown" {
				t.Fatal(result, err)
			}
			card := rev.Payload().Data().Contact
			wire := f.sent.GetContactMessage()
			if wire.GetDisplayName() != card.DisplayName || wire.GetVcard() != card.VCard {
				t.Fatal("wire changed from frozen contact")
			}
			op := result.Entry.Operation
			m, err := a.DB().GetMessage(op.Recipient.JID, op.MessageID)
			want := "Contact: " + name + " (+15550000003)"
			if err != nil || m.Text != want || m.DisplayText != "" || m.MediaType != "" || !m.FromMe || m.SenderJID != "90001@lid" {
				t.Fatalf("contact projection: text=%q, err=%v", m.Text, err)
			}
			found, err := a.DB().SearchMessages(store.SearchMessagesParams{ChatJID: op.Recipient.JID, Query: card.Phone, Type: "text"})
			if err != nil || len(found) != 1 || found[0].MsgID != op.MessageID || found[0].Text != want {
				t.Fatal("contact phone not searchable", err)
			}
			again, err := x.send(t.Context(), r, nil)
			count, countErr := a.DB().CountMessages()
			if err != nil || !again.Duplicate || again.Entry.Operation.ID != op.ID || again.Entry.Operation.Generation != op.Generation || f.sends != 1 || f.ids != 1 || historyCalls != 1 || countErr != nil || count != 1 {
				t.Fatal("duplicate changed operation, send or history", err, countErr)
			}
			if a.wa != nil {
				t.Fatal("real WA client opened")
			}
		})
	}
}

func TestOutboundContactHistoryFailureRemainsAccepted(t *testing.T) {
	_, x, r, f, _ := outboundAppFixture(t, store.DraftContactKind)
	historyCalls := 0
	x.history = func(store.DraftPayloadData, store.OutboundOperation, whatsmeow.SendResponse, *whatsmeow.UploadResponse) error {
		historyCalls++
		return errors.New("synthetic history failure")
	}
	result, err := x.send(t.Context(), r, nil)
	if err != nil || !result.HistoryWarning || result.KnownResult != store.OutboundAccepted || result.Persistence != "confirmed" || result.Entry.Evidence.Accepted != "observed" {
		t.Fatal("secondary history failure changed acceptance", err)
	}
	again, err := x.send(t.Context(), r, nil)
	if err != nil || !again.Duplicate || again.Entry.Operation.ID != result.Entry.Operation.ID || again.Entry.Operation.Generation != result.Entry.Operation.Generation || f.sends != 1 || f.ids != 1 || historyCalls != 1 {
		t.Fatal("history failure triggered resend or persistence replay", err)
	}
}

func TestOutboundRawWhitespaceHistoryAndFrozenRevision(t *testing.T) {
	for _, kind := range []store.DraftKind{store.DraftTextKind, store.DraftDocumentKind} {
		t.Run(string(kind), func(t *testing.T) {
			a, _, r, f, old := outboundAppFixture(t, kind)
			raw := strings.Repeat("á", 77) + "\n"
			input := DraftInput{To: "90002@lid", Message: draftTextPointer(raw)}
			if kind == store.DraftDocumentKind {
				raw = "\t\u00a0caption🙂\r\n"
				input.Message = nil
				input.File = filepath.Join(a.StoreDir(), "source.txt")
				input.Caption = raw
			}
			rid, err := store.NewDraftID()
			if err != nil {
				t.Fatal(err)
			}
			entry, err := a.WriteLocalDraft(t.Context(), DraftWriteRequest{Version: 1, Action: "update", DraftID: r.DraftID, RevisionID: rid, ExpectedRevision: r.RevisionID, StoreRef: a.StoreDir(), Input: &input}, os.Open)
			if err != nil {
				t.Fatal(err)
			}
			r.RevisionID, r.Hash = rid, entry.Revision.Payload().Hash()
			a.opts.WAFactory = func(wa.Options) (WAClient, error) {
				return &outboundLifecycleFake{fakeWA: newFakeWA(), adapter: f}, nil
			}
			result, err := a.SendOutbound(t.Context(), r, nil)
			if err != nil {
				t.Fatal(err)
			}
			sent := f.sent.GetConversation()
			if kind == store.DraftDocumentKind {
				sent = f.sent.GetDocumentMessage().GetCaption()
			}
			if !bytes.Equal([]byte(raw), []byte(sent)) {
				t.Error("fake argument differs from frozen content")
			}
			m, err := a.DB().GetMessage(result.Entry.Operation.Recipient.JID, f.id)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal([]byte(raw), []byte(m.Text)) {
				t.Errorf("history text: expected %d raw bytes, observed %d", len(raw), len(m.Text))
			}
			if kind == store.DraftDocumentKind && !bytes.Equal([]byte(raw), []byte(m.MediaCaption)) {
				t.Errorf("history caption: expected %d raw bytes, observed %d", len(raw), len(m.MediaCaption))
			}
			// Exercise normal ingestion independently of the synthetic send acknowledgement.
			chat := types.NewJID("15550000002", types.DefaultUserServer)
			evt := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: types.NewJID("15550000001", types.DefaultUserServer), IsFromMe: true}, ID: f.id, Timestamp: time.Now()}, Message: f.sent}
			pm := wa.ParseLiveMessage(evt)
			if !bytes.Equal([]byte(raw), []byte(pm.Text)) {
				t.Error("parser lost raw text")
			}
			if err := a.storeParsedMessage(t.Context(), pm); err != nil {
				t.Fatal(err)
			}
			synced, err := a.DB().GetMessage(chat.String(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal([]byte(raw), []byte(synced.Text)) || kind == store.DraftDocumentKind && !bytes.Equal([]byte(raw), []byte(synced.MediaCaption)) {
				t.Error("ingestion lost raw whitespace")
			}
			frozen, err := a.DB().ReadDraft(t.Context(), r.DraftID, r.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := a.DB().ReadDraft(t.Context(), old.DraftID(), old.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(entry.Revision.Payload().CanonicalJSON(), frozen.Revision.Payload().CanonicalJSON()) || entry.Revision.Review() != frozen.Revision.Review() || r.Hash != frozen.Revision.Payload().Hash() || !bytes.Equal(old.Payload().CanonicalJSON(), previous.Revision.Payload().CanonicalJSON()) || old.Payload().Hash() != previous.Revision.Payload().Hash() {
				t.Error("frozen revisions/review/hash changed")
			}
			again, err := a.SendOutbound(t.Context(), r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !again.Duplicate || f.sends != 1 || again.Entry.Operation.ID != result.Entry.Operation.ID || again.Entry.Operation.MessageID != f.id || again.Entry.Operation.Key != r.Key || again.Entry.Operation.Hash != r.Hash || again.Entry.Operation.RevisionID != r.RevisionID {
				t.Error("retained binding changed or replay sent again")
			}
			if result.Entry.Evidence.Accepted != "observed" || result.Entry.Evidence.Delivered != "unknown" || result.Entry.Evidence.Read != "unknown" {
				t.Error("synthetic acknowledgement invented delivery/read")
			}
		})
	}
}
