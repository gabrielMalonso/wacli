package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
)

// Reads and replies stay in memory; no owner, socket, store or WA client is opened.
type boundedSendConn struct {
	input     *bytes.Reader
	output    bytes.Buffer
	readBytes int
}

func (c *boundedSendConn) Read(p []byte) (int, error) {
	n, e := c.input.Read(p)
	c.readBytes += n
	return n, e
}
func (c *boundedSendConn) Write(p []byte) (int, error)      { return c.output.Write(p) }
func (c *boundedSendConn) Close() error                     { return nil }
func (c *boundedSendConn) LocalAddr() net.Addr              { return nil }
func (c *boundedSendConn) RemoteAddr() net.Addr             { return nil }
func (c *boundedSendConn) SetDeadline(time.Time) error      { return nil }
func (c *boundedSendConn) SetReadDeadline(time.Time) error  { return nil }
func (c *boundedSendConn) SetWriteDeadline(time.Time) error { return nil }

func boundedSendRequests() []sendDelegateRequest {
	return []sendDelegateRequest{
		{Version: 1, Kind: outboundSendKind, Outbound: &app.OutboundSendRequest{Version: 1, RequestID: strings.Repeat("f", 32), StoreRef: "/fixture", OwnPN: "15550000001@s.whatsapp.net", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), Hash: strings.Repeat("c", 64), Key: "fixture-key"}},
		{Version: 1, Kind: agentChatStateKind, AgentChatState: &app.ChatStateRequest{Version: 1, StoreRef: "/fixture", Requested: "15550000002@s.whatsapp.net", Action: app.ChatStateArchive}},
		{Version: 1, Kind: draftCleanupKind, DraftCleanup: &app.DraftCleanupRequest{Version: 1, StoreRef: "/fixture", Selection: store.DraftCleanupSelection{DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), ExpectedHeadID: strings.Repeat("b", 32), Hash: strings.Repeat("c", 64)}}},
		{Version: 1, Kind: draftWriteKind, Draft: &app.DraftWriteRequest{Version: 1, Action: "discard", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), ExpectedRevision: strings.Repeat("b", 32), StoreRef: "/fixture", AccountName: ""}},
		fixtureBackfillRequest("15550000002@s.whatsapp.net"),
		{Version: 1, Kind: "text", To: "15550000002@s.whatsapp.net", Message: "fixture"},
	}
}

func runBoundedSend(t *testing.T, raw []byte) (int, int, sendDelegateResponse) {
	t.Helper()
	c := &boundedSendConn{input: bytes.NewReader(raw)}
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	calls := 0
	handleSendDelegateConn(t.Context(), c, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		calls++
		return sendDelegateResponse{OK: true}, nil
	}, slot, newSendPacer(sendSpacing{}))
	var resp sendDelegateResponse
	if err := json.Unmarshal(c.output.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if c.readBytes > sendDelegateMaxRequestBytes+1 {
		t.Fatalf("read %d bytes beyond cap plus sentinel", c.readBytes)
	}
	return c.readBytes, calls, resp
}

func padSendObject(raw []byte, size int) []byte {
	const padding = `,"padding":"` + `"}`
	result := append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"padding":"`)...)
	result = append(result, bytes.Repeat([]byte("x"), size-(len(raw)-1)-len(padding))...)
	return append(result, []byte(`"}`)...)
}

func TestSendIPCCommonLimitInclusiveAndReadAhead(t *testing.T) {
	for _, req := range boundedSendRequests()[3:] {
		raw, _ := json.Marshal(req)
		for _, delta := range []int{-1, 0, 1} {
			t.Run(req.Kind+"/"+[]string{"limit-1", "limit", "limit+1"}[delta+1], func(t *testing.T) {
				object := padSendObject(raw, sendDelegateMaxRequestBytes+delta)
				// A large trailing value remains ignored in these families. Read-ahead
				// must not turn a within-limit first object into an oversized request.
				frame := append(object, bytes.Repeat([]byte(" "), sendDelegateMaxRequestBytes)...)
				frame = append(frame, []byte("{}\n")...)
				read, calls, resp := runBoundedSend(t, frame)
				want := 1
				if delta > 0 {
					want = 0
				}
				if calls != want || resp.OK != (want == 1) {
					t.Fatalf("calls=%d response=%+v", calls, resp)
				}
				if read < min(len(object), sendDelegateMaxRequestBytes+1) {
					t.Fatalf("read=%d did not consume the object or overflow sentinel", read)
				}
			})
		}
		t.Run(req.Kind+"/leading-whitespace-counts", func(t *testing.T) {
			_, calls, _ := runBoundedSend(t, append([]byte(" "), padSendObject(raw, sendDelegateMaxRequestBytes)...))
			if calls != 0 {
				t.Fatal("leading whitespace bypassed limit")
			}
		})
	}
}

func TestSendIPCCommonLimitIndependentOfPrefix(t *testing.T) {
	for _, req := range boundedSendRequests() {
		raw, _ := json.Marshal(req)
		huge := padSendObject(raw, sendDelegateMaxRequestBytes+512)
		frames := map[string][]byte{
			"payload-first": huge,
			"whitespace":    append([]byte(" "), huge...),
			"version-first": []byte(`{"version":1,` + strings.Replace(string(huge[1:]), `,"version":1,"kind":`, `,"kind":`, 1)),
			"unknown-first": []byte(`{"padding":"` + strings.Repeat("x", sendDelegateMaxRequestBytes) + `",` + string(raw[1:])),
		}
		for name, frame := range frames {
			t.Run(req.Kind+"/"+name, func(t *testing.T) {
				read, calls, resp := runBoundedSend(t, frame)
				if calls != 0 || resp.OK {
					t.Fatal("oversized request executed")
				}
				if name != "payload-first" && read != sendDelegateMaxRequestBytes+1 {
					t.Fatalf("read=%d want cap+1", read)
				}
				if name == "payload-first" && (req.Kind == outboundSendKind || req.Kind == agentChatStateKind || req.Kind == draftCleanupKind) && read > 20<<10 {
					t.Fatalf("smaller family bound lost: read=%d", read)
				}
			})
		}
	}
}

func TestSendIPCFamilyFramingPreserved(t *testing.T) {
	for _, req := range boundedSendRequests() {
		raw, _ := json.Marshal(req)
		frames := map[string][]byte{
			"canonical":         append(append([]byte{}, raw...), '\n'),
			"no-newline":        raw,
			"duplicate-version": []byte(strings.Replace(string(raw), `"version":1,"kind"`, `"version":1,"version":1,"kind"`, 1) + "\n"),
			"whitespace":        []byte(strings.Replace(string(raw), `"kind":`, `"kind" :`, 1) + "\n"),
			"trailing-object":   append(append([]byte{}, raw...), []byte("{}\n")...),
			"unknown":           append(append([]byte{}, raw[:len(raw)-1]...), []byte(",\"unused\":true}\n")...),
			"reordered":         []byte(`{"version":1,` + strings.Replace(string(raw[1:]), `,"version":1,"kind":`, `,"kind":`, 1) + "\n"),
		}
		for name, frame := range frames {
			t.Run(req.Kind+"/"+name, func(t *testing.T) {
				_, calls, _ := runBoundedSend(t, frame)
				want := 1
				switch req.Kind {
				case outboundSendKind:
					if name == "unknown" || name == "reordered" {
						want = 0
					}
				case agentChatStateKind:
					if name == "unknown" || name == "reordered" || name == "trailing-object" || name == "no-newline" {
						want = 0
					}
				case draftCleanupKind:
					if name != "canonical" {
						want = 0
					}
				}
				if calls != want {
					t.Fatalf("calls=%d want=%d", calls, want)
				}
			})
		}
	}
}

func TestSendIPCMaximumDraftAndFilePath(t *testing.T) {
	text := strings.Repeat("\x01", (store.MaxDraftPayloadBytes-128)/6)
	r := app.DraftWriteRequest{Version: 1, Action: "create", DraftID: strings.Repeat("a", 32), RevisionID: strings.Repeat("b", 32), StoreRef: "/" + strings.Repeat("s", 4095), Input: &app.DraftInput{To: "15550000002@s.whatsapp.net", Message: &text}}
	raw, _ := json.Marshal(r)
	r.AccountName = strings.Repeat("a", store.MaxDraftPayloadBytes+16384-len(raw))
	raw, _ = json.Marshal(r)
	if len(raw) != store.MaxDraftPayloadBytes+16384 || r.Validate() != nil {
		t.Fatalf("invalid maximum draft: size=%d error=%v", len(raw), r.Validate())
	}
	req := sendDelegateRequest{Version: 1, Kind: draftWriteKind, Draft: &r, TimeoutMS: 1000}
	outer, _ := json.Marshal(req)
	_, calls, resp := runBoundedSend(t, outer)
	if calls != 1 || !resp.OK || len(outer) <= 16<<10 || len(outer) >= sendDelegateMaxRequestBytes {
		t.Fatalf("maximum draft rejected: size=%d response=%+v", len(outer), resp)
	}
	t.Logf("maximum canonical draft=%d outer=%d", len(raw), len(outer))
	// A 100 MiB document is streamed separately; IPC carries only its path.
	r.Input = &app.DraftInput{To: "15550000002@s.whatsapp.net", File: "/" + strings.Repeat("&", 4095)}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	outer, _ = json.Marshal(req)
	_, calls, resp = runBoundedSend(t, outer)
	if calls != 1 || !resp.OK {
		t.Fatal("escaped file path rejected", resp)
	}
	t.Logf("escaped draft file request=%d", len(outer))
}

func TestSendIPCIncompleteReadDeadlineAndOwnerCancellation(t *testing.T) {
	for _, mode := range []string{"incomplete-eof", "read-deadline", "owner-cancel"} {
		t.Run(mode, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				// Mirror the production server's cancellation close without a listener.
				stop := context.AfterFunc(parent, func() { _ = server.Close() })
				defer stop()
				handleSendDelegateConn(parent, &boundedDeadlineConn{Conn: server, ready: ready, short: mode == "read-deadline"}, unexpectedExecute(t), make(chan struct{}, 1), newSendPacer(sendSpacing{}))
			}()
			<-ready
			if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(client, `{"version":1,"kind":"text","message":"unfinished`); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "incomplete-eof":
				_ = client.Close()
			case "owner-cancel":
				cancel()
			default:
				var resp sendDelegateResponse
				if err := json.NewDecoder(client).Decode(&resp); err != nil || resp.OK {
					t.Fatalf("deadline refusal: %+v %v", resp, err)
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("decoder did not exit")
			}
		})
	}
}

type boundedDeadlineConn struct {
	net.Conn
	ready chan struct{}
	short bool
}

func (c *boundedDeadlineConn) SetDeadline(d time.Time) error {
	// Exercise the handler's initial decoding deadline without waiting five minutes.
	if c.short {
		err := c.Conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		close(c.ready)
		return err
	}
	err := c.Conn.SetDeadline(d)
	close(c.ready)
	return err
}
