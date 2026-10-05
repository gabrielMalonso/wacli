package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

const (
	sendDelegateVersion       = 1
	sendDelegateSocketName    = ".send.sock"
	sendDelegateResponseGrace = 5 * time.Second
	// sendDelegateReplyMargin is reserved before the caller's deadline so a
	// refusal can reach the caller before it gives up on the connection. An
	// explicit "not sent" is only useful if it arrives.
	sendDelegateReplyMargin = 500 * time.Millisecond
)

var errSendDelegateUnavailable = errors.New("send delegate unavailable")

type sendDelegateRequest struct {
	DraftCleanup         *app.DraftCleanupRequest `json:"draft_cleanup,omitempty"`
	AgentChatState       *app.ChatStateRequest    `json:"agent_chat_state,omitempty"`
	Outbound             *app.OutboundSendRequest `json:"outbound,omitempty"`
	Draft                *app.DraftWriteRequest   `json:"draft,omitempty"`
	Backfill             *backfillDelegateOptions `json:"backfill,omitempty"`
	Version              int                      `json:"version"`
	Kind                 string                   `json:"kind"`
	To                   string                   `json:"to,omitempty"`
	Pick                 int                      `json:"pick,omitempty"`
	Message              string                   `json:"message,omitempty"`
	Mentions             []string                 `json:"mentions,omitempty"`
	ReplyTo              string                   `json:"reply_to,omitempty"`
	ReplyToSender        string                   `json:"reply_to_sender,omitempty"`
	NoPreview            bool                     `json:"no_preview,omitempty"`
	AllowSelf            bool                     `json:"allow_self,omitempty"`
	Ephemeral            bool                     `json:"ephemeral,omitempty"`
	EphemeralDuration    string                   `json:"ephemeral_duration,omitempty"`
	EphemeralDurationSet bool                     `json:"ephemeral_duration_set,omitempty"`
	File                 string                   `json:"file,omitempty"`
	Filename             string                   `json:"filename,omitempty"`
	Caption              string                   `json:"caption,omitempty"`
	MIME                 string                   `json:"mime,omitempty"`
	As                   string                   `json:"as,omitempty"`
	PTT                  bool                     `json:"ptt,omitempty"`
	ID                   string                   `json:"id,omitempty"`
	Reaction             string                   `json:"reaction,omitempty"`
	Sender               string                   `json:"sender,omitempty"`
	Label                string                   `json:"label,omitempty"`
	ButtonID             string                   `json:"button_id,omitempty"`
	SelectIndex          int                      `json:"select_index,omitempty"`
	Type                 string                   `json:"type,omitempty"`
	Latitude             float64                  `json:"latitude,omitempty"`
	Longitude            float64                  `json:"longitude,omitempty"`
	Name                 string                   `json:"name,omitempty"`
	Question             string                   `json:"question,omitempty"`
	Options              []string                 `json:"options,omitempty"`
	Selectable           int                      `json:"selectable,omitempty"`
	PresenceState        string                   `json:"presence_state,omitempty"`
	PresenceMedia        string                   `json:"presence_media,omitempty"`
	Read                 *bool                    `json:"read,omitempty"`
	Receipts             bool                     `json:"receipts,omitempty"`
	ChatStateAction      string                   `json:"chat_state_action,omitempty"`
	MuteDurationMS       int64                    `json:"mute_duration_ms,omitempty"`
	PostSendWaitMS       int64                    `json:"post_send_wait_ms,omitempty"`
	TimeoutMS            int64                    `json:"timeout_ms,omitempty"`
	DeadlineUnixMS       int64                    `json:"deadline_unix_ms,omitempty"`
}

type sendDelegateResponse struct {
	DraftCleanup     *draftCleanupReply      `json:"draft_cleanup,omitempty"`
	AgentChatState   *agentChatStateReply    `json:"agent_chat_state,omitempty"`
	Outbound         *outboundDelegateResult `json:"outbound,omitempty"`
	DraftResult      *draftDelegateResult    `json:"draft_result,omitempty"`
	DraftFailure     *store.DraftError       `json:"draft_failure,omitempty"`
	DraftRequestHash string                  `json:"draft_request_hash,omitempty"`
	HistoryFailure   *app.HistoryFailure     `json:"history_failure,omitempty"`
	Backfill         *app.BackfillResult     `json:"backfill,omitempty"`
	OK               bool                    `json:"ok"`
	Error            string                  `json:"error,omitempty"`
	Sent             bool                    `json:"sent,omitempty"`
	To               string                  `json:"to,omitempty"`
	ID               string                  `json:"id,omitempty"`
	Target           string                  `json:"target,omitempty"`
	Reaction         string                  `json:"reaction,omitempty"`
	Question         string                  `json:"question,omitempty"`
	Options          []string                `json:"options,omitempty"`
	Selected         []string                `json:"selected,omitempty"`
	SelectedOption   *selectOption           `json:"selected_option,omitempty"`
	File             map[string]string       `json:"file,omitempty"`
	StoreWarning     string                  `json:"store_warning,omitempty"`
	Chat             string                  `json:"chat,omitempty"`
	Action           string                  `json:"action,omitempty"`
	Receipts         *int                    `json:"receipts,omitempty"`
	ReceiptType      string                  `json:"receipt_type,omitempty"`
}

type sendDelegateExecutor func(context.Context, sendDelegateRequest) (sendDelegateResponse, error)

func sendDelegateSocketPath(storeDir string) string {
	return filepath.Join(storeDir, sendDelegateSocketName)
}

func delegateSend(ctx context.Context, flags *rootFlags, req sendDelegateRequest) (sendDelegateResponse, error) {
	req.Version = sendDelegateVersion
	req.TimeoutMS = durationMillis(flags.timeout)
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	path := sendDelegateSocketPath(storeDir)

	deadline := time.Now().Add(commandTimeout(flags))
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return sendDelegateResponse{}, fmt.Errorf("%w: %v", errSendDelegateUnavailable, err)
	}
	defer conn.Close()
	if req.Kind == historyBackfillKind || req.Kind == draftWriteKind || req.Kind == outboundSendKind || req.Kind == agentChatStateKind || req.Kind == draftCleanupKind {
		// Closing the client transport interrupts its wait, not the owner's
		// operation: this protocol has no cancellation acknowledgement.
		stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stopClose()
	}

	req.DeadlineUnixMS = deadline.UnixMilli()
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return sendDelegateResponse{}, historyTransportError(req, err)
	}
	var resp sendDelegateResponse
	var responseReader io.Reader = conn
	if req.Kind == draftCleanupKind {
		frame, err := bufio.NewReader(io.LimitReader(conn, draftCleanupMaxFrame+1)).ReadBytes('\n')
		if err != nil || len(frame) > draftCleanupMaxFrame {
			return sendDelegateResponse{}, draftCleanupIPCUncertain(req.DraftCleanup, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(frame))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&resp); err != nil {
			return sendDelegateResponse{}, draftCleanupIPCUncertain(req.DraftCleanup, err)
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err != io.EOF || req.DraftCleanup == nil {
			return sendDelegateResponse{}, draftCleanupIPCUncertain(req.DraftCleanup, err)
		}
		// Require a canonical frame, rejecting duplicate keys as well as extras.
		canonical, err := json.Marshal(resp)
		if err != nil || !bytes.Equal(bytes.TrimSuffix(frame, []byte("\n")), canonical) {
			return sendDelegateResponse{}, draftCleanupIPCUncertain(req.DraftCleanup, err)
		}
		_, err = validateDraftCleanupDelegate(*req.DraftCleanup, resp)
		return resp, err
	}
	if req.Kind == agentChatStateKind {
		// A single bounded newline frame, matching Encoder.Encode. Closing this
		// connection does not acknowledge cancellation of the owner's action.
		frame, err := bufio.NewReader(io.LimitReader(conn, agentChatStateMaxFrame+1)).ReadBytes('\n')
		if err != nil || len(frame) > agentChatStateMaxFrame {
			return sendDelegateResponse{}, agentChatStateUncertain(req.AgentChatState, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(frame))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&resp); err != nil {
			return sendDelegateResponse{}, agentChatStateUncertain(req.AgentChatState, err)
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err != io.EOF {
			return sendDelegateResponse{}, agentChatStateUncertain(req.AgentChatState, err)
		}
		if req.AgentChatState == nil {
			return sendDelegateResponse{}, agentChatStateUncertain(nil, nil)
		}
		if _, err := validateAgentChatStateDelegate(*req.AgentChatState, resp); err != nil {
			return sendDelegateResponse{}, err
		}
		return resp, nil
	}
	if req.Kind == draftWriteKind || req.Kind == outboundSendKind {
		responseReader = io.LimitReader(conn, 1<<20)
	}
	responseDecoder := json.NewDecoder(responseReader)
	if req.Kind == outboundSendKind {
		responseDecoder.DisallowUnknownFields()
	}
	if err := responseDecoder.Decode(&resp); err != nil {
		if req.Kind == historyBackfillKind || req.Kind == draftWriteKind || req.Kind == outboundSendKind {
			return sendDelegateResponse{}, historyTransportError(req, err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// The request reached the daemon, which may have started it just
			// before the deadline. Say so, because a blind retry can send twice.
			return sendDelegateResponse{}, fmt.Errorf("no reply from the running sync process before the timeout; the %s may still have gone through, so check before retrying: %w", req.Kind, err)
		}
		return sendDelegateResponse{}, historyTransportError(req, err)
	}
	if req.Kind == outboundSendKind {
		if req.Outbound == nil {
			return sendDelegateResponse{}, outboundIPCUncertain(nil, nil)
		}
		if _, err := validateOutboundDelegate(*req.Outbound, resp); err != nil {
			return sendDelegateResponse{}, err
		}
		return resp, nil
	}
	if req.Kind == draftWriteKind {
		if !resp.OK {
			return sendDelegateResponse{}, draftIPCFailure(req, resp)
		}
		if req.Draft == nil {
			return sendDelegateResponse{}, draftIPCUncertain(nil, nil)
		}
		if _, err := validateDraftDelegateResult(*req.Draft, resp.DraftResult); err != nil {
			return sendDelegateResponse{}, err
		}
		return resp, nil
	}
	if !resp.OK {
		if req.Kind == historyBackfillKind {
			return sendDelegateResponse{}, historyIPCError(req, resp.HistoryFailure, resp.Error)
		}
		return sendDelegateResponse{}, errors.New(resp.Error)
	}
	if req.Kind == historyBackfillKind && (req.Backfill == nil || resp.Backfill == nil || resp.Backfill.AttemptID != req.Backfill.AttemptID) {
		return sendDelegateResponse{}, historyIPCUncertain(req, fmt.Errorf("history recovery result correlation not confirmed; history may already have been persisted; check before retrying"))
	}
	return resp, nil
}

func tryDelegateSend(ctx context.Context, flags *rootFlags, lockErr error, req sendDelegateRequest) (sendDelegateResponse, bool, error) {
	if !lock.IsLocked(lockErr) {
		return sendDelegateResponse{}, false, lockErr
	}
	resp, err := delegateSend(ctx, flags, req)
	if err != nil {
		if errors.Is(err, errSendDelegateUnavailable) {
			return sendDelegateResponse{}, false, lockErr
		}
		return sendDelegateResponse{}, true, err
	}
	return resp, true, nil
}

func startSendDelegateServer(ctx context.Context, a *app.App, spacing sendSpacing) (func(), error) {
	return startSendDelegateServerForStore(ctx, a.StoreDir(), spacing, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return executeDelegatedSend(ctx, a, req)
	})
}

func startSendDelegateServerForStore(ctx context.Context, storeDir string, spacing sendSpacing, execute sendDelegateExecutor) (func(), error) {
	path := sendDelegateSocketPath(storeDir)
	if err := removeStaleSendDelegateSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}

	serverCtx, cancelServer := context.WithCancel(ctx)
	var connections sync.WaitGroup
	done := make(chan struct{})
	// One slot serializes delegated operations. Waiting for it is bounded by
	// each caller's deadline, paced or not, so an operation still queued when
	// its caller gives up is refused instead of running late (#446).
	sendSlot := make(chan struct{}, 1)
	sendSlot <- struct{}{}
	// One pacer shared across connections: it spaces the serialized delegated
	// sends so a burst of `wacli send` processes delegating to this daemon
	// leaves the wire paced instead of back-to-back. Disabled = no-op.
	pacer := newSendPacer(spacing)
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				stopClose := context.AfterFunc(serverCtx, func() { _ = conn.Close() })
				defer stopClose()
				handleSendDelegateConn(serverCtx, conn, execute, sendSlot, pacer)
			}()
		}
	}()

	stop := func() {
		cancelServer()
		_ = ln.Close()
		<-done
		connections.Wait()
		_ = os.Remove(path)
	}
	return stop, nil
}

func removeStaleSendDelegateSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	return os.Remove(path)
}

func handleSendDelegateConn(ctx context.Context, conn net.Conn, execute sendDelegateExecutor, sendSlot chan struct{}, pacer *sendPacer) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	var req sendDelegateRequest
	// Typed envelopes start with their payload field. Bound their decoding
	// while preserving existing legacy decoder semantics.
	buffered := bufio.NewReader(conn)
	prefix := []byte(`{"outbound":`)
	head, _ := buffered.Peek(len(prefix))
	outboundEnvelope := bytes.Equal(head, prefix)
	cleanupPrefix := []byte(`{"draft_cleanup":`)
	cleanupEnvelope := bytes.Equal(head, cleanupPrefix[:len(prefix)])
	var cleanupFrame []byte
	chatStatePrefix := []byte(`{"agent_chat_state":`)
	chatStateEnvelope := bytes.Equal(head, chatStatePrefix[:len(prefix)])
	var requestReader io.Reader = buffered
	if outboundEnvelope {
		requestReader = io.LimitReader(buffered, 16384)
	}
	if cleanupEnvelope {
		frame, err := bufio.NewReader(io.LimitReader(buffered, draftCleanupMaxFrame+1)).ReadBytes('\n')
		if err != nil || len(frame) > draftCleanupMaxFrame {
			_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "invalid bounded cleanup frame"})
			return
		}
		cleanupFrame = frame
		requestReader = bytes.NewReader(frame)
	}
	if chatStateEnvelope {
		frame, err := bufio.NewReader(io.LimitReader(buffered, agentChatStateMaxFrame+1)).ReadBytes('\n')
		if err != nil || len(frame) > agentChatStateMaxFrame {
			_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "invalid bounded agent chat state frame"})
			return
		}
		requestReader = bytes.NewReader(frame)
	}
	requestDecoder := json.NewDecoder(requestReader)
	if outboundEnvelope || chatStateEnvelope || cleanupEnvelope {
		requestDecoder.DisallowUnknownFields()
	}
	if err := requestDecoder.Decode(&req); err != nil {
		if chatStateEnvelope {
			_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "invalid agent chat state frame"})
			return
		}
		_ = json.NewEncoder(conn).Encode(sendDelegateResponse{OK: false, Error: err.Error()})
		return
	}
	if chatStateEnvelope {
		var extra json.RawMessage
		if err := requestDecoder.Decode(&extra); err != io.EOF {
			_ = json.NewEncoder(conn).Encode(sendDelegateResponse{Error: "invalid agent chat state frame"})
			return
		}
	}
	if cleanupEnvelope || req.DraftCleanup != nil || req.Kind == draftCleanupKind {
		canonical, err := json.Marshal(req)
		if !cleanupEnvelope || !validateDraftCleanupEnvelope(req) || err != nil || !bytes.Equal(bytes.TrimSuffix(cleanupFrame, []byte("\n")), canonical) {
			_ = json.NewEncoder(conn).Encode(draftCleanupRefusal(req, store.DraftCleanupInvalidArguments))
			return
		}
	}
	// A typed chat-state payload can never opt into another executor, even
	// when the envelope names a legacy kind or reorders fields.
	if (chatStateEnvelope || req.AgentChatState != nil) && req.Kind != agentChatStateKind {
		_ = json.NewEncoder(conn).Encode(agentChatStateRefusal(req, "invalid_arguments"))
		return
	}

	// Every request gets one budget: its own timeout, capped by the caller's
	// absolute deadline less the reply margin. Queueing, pacing and the
	// operation itself all share it.
	deadline := time.Now().Add(millisDuration(req.TimeoutMS, 5*time.Minute))
	if req.DeadlineUnixMS > 0 {
		callerDeadline := time.UnixMilli(req.DeadlineUnixMS)
		if callerDeadline.Before(deadline) {
			deadline = callerDeadline
		}
	}
	// Reserve at most a tenth of the remaining budget for the reply, so
	// sub-second requests still have time to execute.
	if remaining := time.Until(deadline); remaining > 0 {
		deadline = deadline.Add(-min(sendDelegateReplyMargin, remaining/10))
	}
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	deadline, _ = requestCtx.Deadline()
	// The fixed initial deadline only protects request decoding. A queued or
	// paced request may intentionally run longer than five minutes, so keep the
	// transport alive through its budget and the final response write.
	_ = conn.SetDeadline(deadline.Add(sendDelegateResponseGrace))

	if req.Kind == historyBackfillKind {
		if req.Version != sendDelegateVersion || req.Backfill == nil {
			_ = json.NewEncoder(conn).Encode(historyRefusal(req, "invalid_arguments", "invalid history backfill request before dispatch; no history was requested"))
			return
		}
		if _, err := req.Backfill.options(); err != nil {
			_ = json.NewEncoder(conn).Encode(historyRefusal(req, "invalid_arguments", fmt.Sprintf("invalid history backfill request before dispatch; no history was requested: %v", err)))
			return
		}
	}

	if req.Kind == draftWriteKind {
		if req.Version != sendDelegateVersion || req.Draft == nil {
			_ = json.NewEncoder(conn).Encode(draftRefusal(req, draftIPCUncertain(req.Draft, nil)))
			return
		}
		if err := req.Draft.Validate(); err != nil {
			_ = json.NewEncoder(conn).Encode(draftRefusal(req, err))
			return
		}
	}

	if req.Kind == outboundSendKind {
		if req.Version != sendDelegateVersion {
			_ = json.NewEncoder(conn).Encode(outboundRefusal(req, "outcome_uncertain"))
			return
		}
		raw, encodingErr := json.Marshal(req)
		if !outboundEnvelope || encodingErr != nil || len(raw) > 16384 || req.Outbound == nil || req.Outbound.Validate() != nil {
			_ = json.NewEncoder(conn).Encode(outboundRefusal(req, "invalid_arguments"))
			return
		}
	}
	if req.Kind == agentChatStateKind {
		if !chatStateEnvelope || !validateAgentChatStateEnvelope(req) {
			_ = json.NewEncoder(conn).Encode(agentChatStateRefusal(req, "invalid_arguments"))
			return
		}
		if requestCtx.Err() != nil {
			_ = json.NewEncoder(conn).Encode(agentChatStateRefusal(req, "not_dispatched"))
			return
		}
	}
	if req.Kind == chatStateKind || req.Kind == agentChatStateKind {
		// App-state writes are serialized by the app and can wait minutes on
		// recovery, so they must not hold the send queue.
		if requestCtx.Err() != nil {
			if req.Kind == agentChatStateKind {
				_ = json.NewEncoder(conn).Encode(agentChatStateRefusal(req, "not_dispatched"))
				return
			}
			_ = json.NewEncoder(conn).Encode(sendDelegateResponse{OK: false, Error: "request deadline passed before dispatch; it was not sent"})
			return
		}
		resp, err := execute(requestCtx, req)
		writeDelegateResult(conn, requestCtx, req, resp, err)
		return
	}

	refuse := func() {
		msg := "request timed out in the send queue before dispatch; it was not sent"
		if req.Kind == historyBackfillKind {
			msg = "history backfill expired in the operation queue before dispatch; no history was requested"
		} else if pacer.enabled() {
			msg = "send spacing exceeded request timeout before dispatch; it was not sent"
		}
		resp := sendDelegateResponse{OK: false, Error: msg}
		if req.Kind == outboundSendKind {
			resp = outboundRefusal(req, "not_dispatched")
		}
		if req.Kind == draftCleanupKind {
			resp = draftCleanupRefusal(req, store.DraftCleanupCanceled)
		}
		if req.Kind == draftWriteKind {
			resp = draftRefusal(req, store.DraftFailure("local_write_not_dispatched", "", "", "", nil))
		}
		if req.Kind == historyBackfillKind {
			resp = historyRefusal(req, "backfill_not_dispatched", msg)
		}
		_ = json.NewEncoder(conn).Encode(resp)
	}

	select {
	case <-requestCtx.Done():
		refuse()
		return
	case <-sendSlot:
		defer func() { sendSlot <- struct{}{} }()
	}
	// select picks at random when the slot frees up at the same moment the
	// deadline passes. Never start an operation after its caller gave up.
	if requestCtx.Err() != nil || !time.Now().Before(deadline) {
		refuse()
		return
	}

	// Space this send from the previous one while serialized. Bound the wait by
	// the same deadline. Disabled spacing leaves the path untouched.
	if req.Kind != historyBackfillKind && req.Kind != draftWriteKind && req.Kind != draftCleanupKind && req.Kind != outboundSendKind && pacer.enabled() {
		if !pacer.wait(requestCtx) {
			refuse()
			return
		}
	}

	// Timer delivery can lag wall-clock expiry, including while pacing.
	if requestCtx.Err() != nil || !time.Now().Before(deadline) {
		refuse()
		return
	}

	if req.Kind == outboundSendKind {
		requestCtx = context.WithValue(requestCtx, outboundPacerKey{}, pacer)
	}
	resp, err := execute(requestCtx, req)
	if req.Kind != historyBackfillKind && req.Kind != draftWriteKind && req.Kind != draftCleanupKind && req.Kind != outboundSendKind && pacer.enabled() {
		// Record completion, not handler entry: recipient resolution, media
		// preparation, and the actual wire send all happen inside execute.
		// Starting the gap here prevents a slow operation from consuming it.
		pacer.record()
	}
	writeDelegateResult(conn, requestCtx, req, resp, err)
}

func writeDelegateResult(conn net.Conn, requestCtx context.Context, req sendDelegateRequest, resp sendDelegateResponse, err error) {
	if err != nil {
		if req.Kind == draftCleanupKind {
			failure := draftCleanupIPCUncertain(req.DraftCleanup, err)
			if req.DraftCleanup != nil {
				resp = sendDelegateResponse{DraftCleanup: &draftCleanupReply{Capability: draftCleanupKind, RequestHash: draftCleanupRequestHash(*req.DraftCleanup), Result: failure.Result, Failure: failure.Failure}}
			}
			_ = json.NewEncoder(conn).Encode(resp)
			return
		}

		if req.Kind == agentChatStateKind {
			_ = json.NewEncoder(conn).Encode(agentChatStateRefusal(req, "chat_state_outcome_uncertain"))
			return
		}
		if req.Kind == outboundSendKind {
			_ = json.NewEncoder(conn).Encode(outboundRefusal(req, "outcome_uncertain"))
			return
		}
		if req.Kind == draftWriteKind {
			_ = json.NewEncoder(conn).Encode(draftRefusal(req, err))
			return
		}
		if req.Kind == historyBackfillKind {
			err = fmt.Errorf("delegated history backfill failed after dispatch; already persisted history may remain; check before retrying: %w", err)
		} else if requestCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			err = fmt.Errorf("delegated %s failed after dispatch and may still have gone through; check before retrying: %w", req.Kind, err)
		}
		resp = sendDelegateResponse{OK: false, Error: err.Error()}
		if req.Kind == historyBackfillKind {
			var typed *app.BackfillError
			if errors.As(err, &typed) {
				copy := typed.History
				resp.HistoryFailure = &copy
			} else {
				uncertain := historyIPCUncertain(req, err).(*app.BackfillError)
				resp.HistoryFailure = &uncertain.History
			}
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

func executeDelegatedSend(parent context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.Version != sendDelegateVersion {
		return sendDelegateResponse{}, fmt.Errorf("unsupported send delegate version %d", req.Version)
	}
	ctx, cancel := context.WithTimeout(parent, millisDuration(req.TimeoutMS, 5*time.Minute))
	defer cancel()

	switch req.Kind {
	case draftCleanupKind:
		return executeDelegatedDraftCleanup(ctx, a, req)
	case agentChatStateKind:
		return executeDelegatedAgentChatState(ctx, a, req)
	case outboundSendKind:
		return executeDelegatedOutbound(ctx, a, req)
	case draftWriteKind:
		return executeDelegatedDraft(ctx, a, req)
	case historyBackfillKind:
		return executeDelegatedBackfill(ctx, a, req)
	case "text":
		return executeDelegatedText(ctx, a, req)
	case "file", "voice":
		return executeDelegatedFile(ctx, a, req)
	case "sticker":
		return executeDelegatedSticker(ctx, a, req)
	case "react":
		return executeDelegatedReact(ctx, a, req)
	case "location":
		return executeDelegatedLocation(ctx, a, req)
	case "poll":
		return executeDelegatedPoll(ctx, a, req)
	case "poll_vote":
		return executeDelegatedPollVote(ctx, a, req)
	case "button_list_select":
		return executeDelegatedButtonListSelect(ctx, a, req)
	case "presence":
		return executeDelegatedPresence(ctx, a, req)
	case "edit":
		return executeDelegatedEdit(ctx, a, req)
	case markReadKind:
		return executeDelegatedMarkRead(ctx, a, req)
	case markReadReceiptsKind:
		// Its own kind, so daemons without receipt support reject it here
		// instead of marking the chat read and dropping the unread count.
		req.Receipts = true
		return executeDelegatedMarkRead(ctx, a, req)
	case chatStateKind:
		return executeDelegatedChatState(ctx, a, req)
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
}

type delegatedMarkReadApp interface {
	recipientResolverApp
	MarkChatRead(context.Context, types.JID, bool) error
	MarkChatReadWithReceipts(context.Context, types.JID) (int, types.ReceiptType, error)
}

func executeDelegatedMarkRead(ctx context.Context, a delegatedMarkReadApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	read := true
	if req.Read != nil {
		read = *req.Read
	}
	if req.Receipts && !read {
		return sendDelegateResponse{}, fmt.Errorf("--receipts only applies to mark-read")
	}
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	// Receipt mode never enters app-state recovery.
	var receipts *int
	var receiptType string
	if req.Receipts {
		n, kind, err := a.MarkChatReadWithReceipts(ctx, toJID)
		if err != nil {
			return sendDelegateResponse{}, err
		}
		receipts = &n
		receiptType = string(kind)
	} else if err := a.MarkChatRead(ctx, toJID, read); err != nil {
		return sendDelegateResponse{}, err
	}
	action := "mark-read"
	if !read {
		action = "mark-unread"
	}
	return sendDelegateResponse{OK: true, Chat: toJID.String(), Action: action, Receipts: receipts, ReceiptType: receiptType}, nil
}

type delegatedChatStateApp interface {
	recipientResolverApp
	ArchiveChat(context.Context, types.JID, bool) error
	PinChat(context.Context, types.JID, bool) error
	MuteChat(context.Context, types.JID, bool, time.Duration) error
}

func executeDelegatedChatState(ctx context.Context, a delegatedChatStateApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	var run func(types.JID) error
	switch req.ChatStateAction {
	case "archive", "unarchive":
		run = func(jid types.JID) error { return a.ArchiveChat(ctx, jid, req.ChatStateAction == "archive") }
	case "pin", "unpin":
		run = func(jid types.JID) error { return a.PinChat(ctx, jid, req.ChatStateAction == "pin") }
	case "mute":
		run = func(jid types.JID) error { return a.MuteChat(ctx, jid, true, millisDuration(req.MuteDurationMS, 0)) }
	case "unmute":
		run = func(jid types.JID) error { return a.MuteChat(ctx, jid, false, 0) }
	default:
		return sendDelegateResponse{}, fmt.Errorf("unsupported chat state action %q", req.ChatStateAction)
	}
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := run(toJID); err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Chat: toJID.String(), Action: req.ChatStateAction}, nil
}

func executeDelegatedPresence(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	state, err := presenceStateFromString(req.PresenceState)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID, err := wa.ParseUserOrJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID = warmupDelegatedRecipient(ctx, a, toJID)
	chatMedia, err := presenceMediaFromString(req.PresenceMedia)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := sendPresenceWithRetry(ctx, reconnectForSend(a), func(ctx context.Context) error {
		return a.WA().SendChatPresence(ctx, toJID, state, chatMedia)
	}); err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Sent: true, To: toJID.String()}, nil
}

func executeDelegatedEdit(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, req.To, req.ID)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := validateMessageCanEdit(msg, time.Now().UTC()); err != nil {
		return sendDelegateResponse{}, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().EditMessage(ctx, chatJID, types.MessageID(msg.MsgID), req.Message)
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := a.DB().UpdateMessageText(msg.ChatJID, msg.MsgID, req.Message); err != nil {
		return sendDelegateResponse{}, fmt.Errorf("store edited message text: %w", err)
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return sendDelegateResponse{OK: true, Sent: true, To: chatJID.String(), ID: string(sentID), Target: msg.MsgID}, nil
}

func executeDelegatedText(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	ephemeral := textEphemeralOptions{
		Enabled:     req.Ephemeral,
		Duration:    req.EphemeralDuration,
		DurationSet: req.EphemeralDurationSet,
	}
	if err := validateTextEphemeralOptions(ephemeral); err != nil {
		return sendDelegateResponse{}, err
	}
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if !req.AllowSelf {
		if err := validateTextRecipient(a.WA(), toJID); err != nil {
			return sendDelegateResponse{}, err
		}
	}
	toJID = warmupDelegatedRecipient(ctx, a, toJID)
	mentionedJIDs, err := parseMentionedJIDs(req.Mentions)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	preview := fetchLinkPreview(ctx, req.Message, req.NoPreview)
	msgID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return sendTextMessage(ctx, a, toJID, req.Message, req.ReplyTo, req.ReplyToSender, preview, mentionedJIDs, ephemeral, textSendOptions{allowSelf: req.AllowSelf})
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	now := time.Now().UTC()
	storeErr := persistOutboundText(ctx, a, toJID, string(msgID), req.Message, now)
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	resp := sendDelegateResponse{OK: true, Sent: true, To: toJID.String(), ID: string(msgID)}
	if storeErr != nil {
		resp.StoreWarning = storeErr.Error()
	}
	return resp, nil
}

func executeDelegatedFile(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	mediaAs, err := validateSendFileMediaOptions(req.As, req.PTT || req.Kind == "voice")
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID = warmupDelegatedRecipient(ctx, a, toJID)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	res, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (sendDelegateResponse, error) {
		outcome, err := sendFile(ctx, a, toJID, req.File, sendFileOptions{
			filename:      req.Filename,
			caption:       req.Caption,
			mimeOverride:  req.MIME,
			mediaAs:       mediaAs,
			replyTo:       req.ReplyTo,
			replyToSender: req.ReplyToSender,
			ptt:           req.PTT || req.Kind == "voice",
		})
		if err != nil {
			return sendDelegateResponse{}, err
		}
		resp := sendDelegateResponse{OK: true, Sent: true, To: toJID.String(), ID: outcome.id, File: outcome.meta}
		if outcome.storeWarning != nil {
			resp.StoreWarning = outcome.storeWarning.Error()
		}
		return resp, nil
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return res, nil
}

func executeDelegatedSticker(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	toJID = warmupDelegatedRecipient(ctx, a, toJID)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	res, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (sendDelegateResponse, error) {
		outcome, err := sendSticker(ctx, a, toJID, req.File, sendStickerOptions{
			replyTo:       req.ReplyTo,
			replyToSender: req.ReplyToSender,
		})
		if err != nil {
			return sendDelegateResponse{}, err
		}
		resp := sendDelegateResponse{OK: true, Sent: true, To: toJID.String(), ID: outcome.id, File: outcome.meta}
		if outcome.storeWarning != nil {
			resp.StoreWarning = outcome.storeWarning.Error()
		}
		return resp, nil
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return res, nil
}

func executeDelegatedReact(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	chat, senderJID, err := reactionTarget(req.To, req.Sender)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	chat = warmupDelegatedRecipient(ctx, a, chat)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return sendDelegateResponse{}, err
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendReaction(ctx, chat, senderJID, types.MessageID(req.ID), req.Reaction)
	})
	if err != nil {
		return sendDelegateResponse{}, err
	}
	now := time.Now().UTC()
	chatName := a.WA().ResolveChatName(ctx, chat, "")
	storeErr := upsertSentReaction(a.DB(), chat, chatName, sentID, req.ID, req.Reaction, now)
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	resp := sendDelegateResponse{OK: true, Sent: true, To: chat.String(), ID: string(sentID), Target: req.ID, Reaction: req.Reaction}
	if storeErr != nil {
		resp.StoreWarning = storeErr.Error()
	}
	return resp, nil
}

func writeDelegatedSendOutput(flags *rootFlags, kind string, resp sendDelegateResponse) error {
	warnSendStoreFailureMsg(os.Stderr, resp.ID, resp.StoreWarning)
	if flags.asJSON {
		body := map[string]any{"sent": resp.Sent, "to": resp.To, "id": resp.ID}
		if resp.File != nil {
			body["file"] = resp.File
		}
		if resp.StoreWarning != "" {
			body["store_warning"] = resp.StoreWarning
		}
		if kind == "react" {
			body["target"] = resp.Target
			body["reaction"] = resp.Reaction
		}
		if kind == "poll" {
			body["question"] = resp.Question
			body["options"] = resp.Options
		}
		if kind == "poll_vote" {
			body["target"] = resp.Target
			body["selected"] = resp.Selected
		}
		if kind == "button_list_select" {
			body["target"] = resp.Target
			body["selected"] = resp.SelectedOption
		}
		return out.WriteJSON(os.Stdout, body)
	}
	switch kind {
	case "file":
		fmt.Fprintf(os.Stdout, "Sent %s to %s (id %s)\n", resp.File["name"], resp.To, resp.ID)
	case "sticker":
		fmt.Fprintf(os.Stdout, "Sent sticker to %s (id %s)\n", resp.To, resp.ID)
	case "location":
		fmt.Fprintf(os.Stdout, "Sent location to %s (id %s)\n", resp.To, resp.ID)
	case "voice":
		fmt.Fprintf(os.Stdout, "Sent voice note to %s (id %s)\n", resp.To, resp.ID)
	case "react":
		if resp.Reaction == "" {
			fmt.Fprintf(os.Stdout, "Removed reaction from %s (id %s)\n", resp.Target, resp.ID)
		} else {
			fmt.Fprintf(os.Stdout, "Reacted %s to %s (id %s)\n", resp.Reaction, resp.Target, resp.ID)
		}
	case "poll":
		fmt.Fprintf(os.Stdout, "Sent poll to %s (id %s)\n", resp.To, resp.ID)
	case "poll_vote":
		fmt.Fprintf(os.Stdout, "Voted on %s in %s (id %s)\n", resp.Target, resp.To, resp.ID)
	case "button_list_select":
		label := ""
		if resp.SelectedOption != nil {
			label = resp.SelectedOption.DisplayText
		}
		fmt.Fprintf(os.Stdout, "Selected %q on %s in %s (id %s)\n", label, resp.Target, resp.To, resp.ID)
	default:
		fmt.Fprintf(os.Stdout, "Sent to %s (id %s)\n", resp.To, resp.ID)
	}
	return nil
}

func warmupDelegatedRecipient(ctx context.Context, a *app.App, jid types.JID) types.JID {
	return warmupRecipient(ctx, a.WA(), jid, os.Stderr)
}

func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

func millisDuration(ms int64, fallback time.Duration) time.Duration {
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func commandTimeout(flags *rootFlags) time.Duration {
	if flags == nil || flags.timeout <= 0 {
		return 5 * time.Minute
	}
	return flags.timeout
}

func historyTransportError(req sendDelegateRequest, err error) error {
	if req.Kind == draftCleanupKind {
		return draftCleanupIPCUncertain(req.DraftCleanup, err)
	}
	if req.Kind == agentChatStateKind {
		return agentChatStateUncertain(req.AgentChatState, err)
	}
	if req.Kind == outboundSendKind {
		return outboundIPCUncertain(req.Outbound, err)
	}
	if req.Kind == draftWriteKind {
		return draftIPCUncertain(req.Draft, err)
	}
	if req.Kind == historyBackfillKind {
		return historyIPCUncertain(req, delegateTransportError(req.Kind, err))
	}
	return delegateTransportError(req.Kind, err)
}
func historyRefusal(req sendDelegateRequest, code, message string) sendDelegateResponse {
	id := ""
	if req.Backfill != nil && app.ValidateHistoryAttemptID(req.Backfill.AttemptID) == nil {
		id = req.Backfill.AttemptID
	}
	return sendDelegateResponse{Error: message, HistoryFailure: &app.HistoryFailure{AttemptID: id, Phase: store.HistoryPreparing, Outcome: "not_dispatched", Code: code}}
}
