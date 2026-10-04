package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type BackfillOptions struct {
	AttemptID      string
	ChatJID        string
	Count          int
	Requests       int
	WaitPerRequest time.Duration
	IdleExit       time.Duration
}

const (
	DefaultBackfillCount    = 50
	DefaultBackfillRequests = 1
	MaxBackfillCount        = 500
	MaxBackfillRequests     = 100
)

// BackfillStopReason describes why the requested batches stopped. None of these
// reasons proves that all historical messages exist in the local archive.
type BackfillStopReason string

const (
	BackfillStopRequestedBatchLimit BackfillStopReason = "requested_batch_limit"
	BackfillStopNoProgress          BackfillStopReason = "no_progress"
	BackfillStopEmptyResponse       BackfillStopReason = "empty_response"
	BackfillStopPrimaryNoMore       BackfillStopReason = "primary_no_more_messages"
)

type BackfillResult struct {
	AttemptID     string
	Evidence      *store.HistoryAttempt `json:"evidence,omitempty"`
	ChatJID       string
	RequestsSent  int
	ResponsesSeen int
	// MessagesAdded is net distinct local growth for the selected conversation
	// during the post-connect counting window, including concurrent activity.
	MessagesAdded int64
	// MessagesSynced is the global Sync counter, including other chats and
	// updates/replays. Connected mode reports its delta during this window;
	// standalone reports its enclosing Sync total. Manual blobs stay separate.
	MessagesSynced int64
	StopReason     BackfillStopReason
}

type onDemandResponse struct {
	chatJID       string
	observedAt    time.Time
	conversations int
	messages      int
	endType       waHistorySync.Conversation_EndOfHistoryTransferType
}

func (a *App) BackfillHistory(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	return a.backfillHistory(ctx, opts, nil)
}

// BackfillHistoryConnected uses the existing follow owner without changing its
// connection or history download settings. The IPC operation slot serializes
// callers; the observer also refuses overlapping operations within this App.
func (a *App) BackfillHistoryConnected(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	a.historyMu.Lock()
	runtime := a.historyRuntime
	a.historyMu.Unlock()
	if runtime == nil || runtime.ctx.Err() != nil {
		return BackfillResult{}, historyFailure(opts.AttemptID, store.HistoryPreparing, "backfill_not_dispatched", false, false, fmt.Errorf("history backfill requires an active sync follow owner"))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(runtime.ctx, cancel)
	defer stop()
	return a.backfillHistory(ctx, opts, runtime)
}

// PrepareBackfillOptions validates and bounds options before opening a writer
// or sending an IPC request. Nonpositive values retain the legacy defaults.
func PrepareBackfillOptions(opts BackfillOptions) (BackfillOptions, error) {
	chatStr := strings.TrimSpace(opts.ChatJID)
	if chatStr == "" {
		return BackfillOptions{}, fmt.Errorf("--chat is required")
	}
	chat, err := ParseHistoryJID(chatStr)
	if err != nil {
		return BackfillOptions{}, fmt.Errorf("parse chat JID: %w", err)
	}
	opts.ChatJID = chat.ToNonAD().String()
	if opts.AttemptID != "" {
		if err := ValidateHistoryAttemptID(opts.AttemptID); err != nil {
			return BackfillOptions{}, err
		}
	}
	opts = normalizeBackfillOptions(opts)
	return opts, validateBackfillOptions(opts)
}

func (a *App) backfillHistory(ctx context.Context, opts BackfillOptions, runtime *historyRuntime) (result BackfillResult, runErr error) {
	if a.opts.ReadOnly {
		return BackfillResult{}, fmt.Errorf("read-only mode: history backfill would modify the store")
	}
	opts, err := PrepareBackfillOptions(opts)
	if err != nil {
		return BackfillResult{}, err
	}
	chat, _ := types.ParseJID(opts.ChatJID) // validated above
	if err := ctx.Err(); err != nil {
		return BackfillResult{}, err
	}
	a.historyMu.Lock()
	if a.historyBackfillActive {
		a.historyMu.Unlock()
		return BackfillResult{}, fmt.Errorf("history backfill is already running")
	}
	a.historyBackfillActive = true
	a.historyMu.Unlock()
	defer func() { a.historyMu.Lock(); a.historyBackfillActive = false; a.historyMu.Unlock() }()
	if opts.AttemptID == "" {
		opts.AttemptID, err = NewHistoryAttemptID()
		if err != nil {
			return BackfillResult{}, err
		}
	}
	now := nowUTC()
	rec := store.HistoryAttempt{RequestedChatJID: opts.ChatJID, AttemptID: opts.AttemptID, StartedAt: now, CheckpointAt: now,
		State: store.HistoryUnfinalized, Phase: store.HistoryPreparing, ExecutionMode: "standalone", Count: opts.Count, Requests: opts.Requests,
		WaitMS: max(1, opts.WaitPerRequest.Milliseconds()), IdleMS: max(1, opts.IdleExit.Milliseconds())}
	if runtime != nil {
		rec.ExecutionMode = "sync_owner"
	}
	if err := a.saveHistoryAttempt(ctx, rec, true); err != nil {
		return BackfillResult{}, historyFailure(opts.AttemptID, rec.Phase, "store_state", false, false, err)
	}
	defer a.finishHistoryAttempt(ctx, &rec, &result, &runErr)
	if runtime == nil {
		if err := a.EnsureAuthed(ctx); err != nil {
			return BackfillResult{}, err
		}
	} else if !a.wa.IsConnected() || !a.wa.IsAuthed() {
		return BackfillResult{}, fmt.Errorf("sync follow owner is not connected and authenticated")
	}

	var mu sync.Mutex
	var waitCh chan onDemandResponse
	var windowActive bool
	var windowChat, windowAlias string
	var storeErr error
	var observedResponse onDemandResponse
	var primaryObservedAt *time.Time
	var primaryResponseChat string
	checkpoint := func(ctx context.Context) error {
		mu.Lock()
		observed := observedResponse
		if !observed.observedAt.IsZero() {
			rec.ResponseObservedAt = new(observed.observedAt)
			rec.ResponseChatJID = observed.chatJID
		}
		if primaryObservedAt != nil {
			rec.PrimaryNoMoreObservedAt = new(*primaryObservedAt)
			rec.PrimaryResponseChatJID = primaryResponseChat
		}
		mu.Unlock()
		rec.CheckpointAt = nowUTC()
		err := a.saveHistoryAttempt(ctx, rec, false)
		if err != nil {
			return historyFailure(rec.AttemptID, rec.Phase, "store_state", rec.DispatchPossible, true, err)
		}
		return nil
	}
	observeStoreError := func(jid types.JID, err error) {
		mu.Lock()
		defer mu.Unlock()
		identity := jid.ToNonAD().String()
		if !windowActive || (identity != windowChat && identity != windowAlias) {
			return
		}
		if storeErr == nil {
			storeErr = fmt.Errorf("persist on-demand history for %s: %w", jid, err)
		}
	}
	persistenceError := func() error {
		mu.Lock()
		defer mu.Unlock()
		return storeErr
	}

	handleOnDemand := func(hs *events.HistorySync) {
		if hs == nil || hs.Data == nil || hs.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			return
		}
		for _, conv := range hs.Data.GetConversations() {
			jid, err := types.ParseJID(strings.TrimSpace(conv.GetID()))
			if err != nil {
				continue
			}
			resp := onDemandResponse{
				chatJID: strings.TrimSpace(conv.GetID()), observedAt: nowUTC(),
				conversations: len(hs.Data.GetConversations()),
				messages:      len(conv.GetMessages()),
				endType:       conv.GetEndOfHistoryTransferType(),
			}
			mu.Lock()
			if !windowActive {
				mu.Unlock()
				return
			}
			// Match the immutable window scope, not a resolver whose map or
			// context may change while persistence/cancellation is in flight.
			identity := jid.ToNonAD().String()
			if identity != windowChat && identity != windowAlias {
				mu.Unlock()
				continue
			}
			observedResponse = resp
			if resp.endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
				primaryObservedAt = new(resp.observedAt)
				primaryResponseChat = resp.chatJID
			}
			ch := waitCh
			mu.Unlock()
			if ch == nil {
				return
			}
			select {
			case ch <- resp:
			default:
			}
			return
		}
	}
	var requestsSent int
	var responsesSeen int
	errResponseTimeout := errors.New("timed out waiting for on-demand history sync response")
	request := func(ctx context.Context, anchor store.MessageInfo, requestChat types.JID) (onDemandResponse, error) {
		if err := ctx.Err(); err != nil {
			return onDemandResponse{}, err
		}
		ch := make(chan onDemandResponse, 4)
		mu.Lock()
		waitCh = ch
		mu.Unlock()
		defer func() {
			mu.Lock()
			waitCh = nil
			mu.Unlock()
		}()

		storeChat := a.canonicalStoreJID(ctx, chat).String()
		if rec.FirstAnchorID == "" {
			rec.FirstAnchorID = anchor.MsgID
		}
		rec.LastAnchorID = anchor.MsgID
		rec.PreparedRequestChatJID = requestChat.String()
		previousPhase := rec.Phase
		rec.Phase = store.HistoryDispatchPossible
		// Commit uncertainty before invoking the transport. A failed checkpoint
		// prevents this call, and cannot undo prior dispatch uncertainty.
		previousPossible := rec.DispatchPossible
		rec.DispatchPossible = true
		if err := checkpoint(ctx); err != nil {
			// This invocation did not occur. Prior calls remain uncertain; a
			// failed checkpoint never makes earlier dispatch certain again.
			rec.DispatchPossible = previousPossible
			rec.Phase = previousPhase
			return onDemandResponse{}, historyFailure(rec.AttemptID, rec.Phase, "store_state", previousPossible, true, err)
		}
		requestsSent++
		rec.RequestsSent = requestsSent
		a.emitOrPrint("backfill_requesting", map[string]any{
			"chat_jid":         storeChat,
			"request_chat_jid": requestChat.String(),
			"count":            opts.Count,
			"request":          requestsSent,
			"anchor_msg_id":    anchor.MsgID,
		}, "Requesting %d older messages for %s...\n", opts.Count, storeChat)
		reqInfo := types.MessageInfo{
			MessageSource: types.MessageSource{Chat: requestChat, IsFromMe: anchor.FromMe},
			ID:            types.MessageID(anchor.MsgID),
			Timestamp:     anchor.Timestamp,
		}
		if _, err := a.wa.RequestHistorySyncOnDemand(ctx, reqInfo, opts.Count); err != nil {
			return onDemandResponse{}, err
		}
		timer := time.NewTimer(opts.WaitPerRequest)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return onDemandResponse{}, ctx.Err()
		case resp := <-ch:
			responsesSeen++
			rec.ResponsesSeen = responsesSeen
			if err := checkpoint(ctx); err != nil {
				return resp, err
			}
			return resp, nil
		case <-timer.C:
			return onDemandResponse{}, fmt.Errorf("%w (anchor %s)", errResponseTimeout, anchor.MsgID)
		}
	}

	// A mapped 1:1 chat has two identities. The primary device files some
	// chats under the LID and others under the phone number, and answers only
	// requests addressed to the one it uses (#444). Ask by LID first, retry the
	// same anchor by phone number when that goes unanswered, and keep using
	// the identity of the successful attempt. Late replies have no request ID,
	// so either identity remains eligible for fallback in later batches. Resolve on every
	// request: sync can learn a mapping while connecting.
	var preferPN bool
	requestIdentities := func(ctx context.Context) []types.JID {
		lidChat := a.wa.ResolvePNToLID(ctx, chat)
		pnChat := a.wa.ResolveLIDToPN(ctx, lidChat)
		if pnChat == lidChat {
			return []types.JID{lidChat}
		}
		if preferPN {
			return []types.JID{pnChat, lidChat}
		}
		return []types.JID{lidChat, pnChat}
	}
	requestAnchor := func(ctx context.Context, anchor store.MessageInfo) (onDemandResponse, error) {
		ids := requestIdentities(ctx)
		resp, err := request(ctx, anchor, ids[0])
		if len(ids) < 2 || !errors.Is(err, errResponseTimeout) || ctx.Err() != nil {
			return resp, err
		}
		a.emitWarning("backfill_identity_retry",
			fmt.Sprintf("warning: no history response for anchor %s from %s; retrying with %s", anchor.MsgID, ids[0], ids[1]),
			map[string]any{
				"chat_jid":               a.canonicalStoreJID(ctx, chat).String(),
				"anchor_msg_id":          anchor.MsgID,
				"request_chat_jid":       ids[0].String(),
				"retry_request_chat_jid": ids[1].String(),
			})
		resp, err = request(ctx, anchor, ids[1])
		if err == nil {
			preferPN = ids[1].Server == types.DefaultUserServer
		}
		return resp, err
	}

	var beforeCount int64
	var beforeStored int64
	var stopReason BackfillStopReason
	countIdentities := func(ctx context.Context) (string, string) {
		storeChat := a.canonicalStoreJID(ctx, chat)
		alias := a.wa.ResolvePNToLID(ctx, storeChat)
		return storeChat.String(), canonicalJIDString(alias)
	}
	checkIdentities := func(ctx context.Context) error {
		currentChat, currentAlias := countIdentities(ctx)
		if currentChat != windowChat || currentAlias != windowAlias {
			return fmt.Errorf("backfill conversation identities changed during the counting window; result could not be measured reliably; already persisted messages may remain")
		}
		return nil
	}
	stop := func(reason BackfillStopReason, legacyReason, message string) error {
		stopReason = reason
		rec.StopReason = string(reason)
		rec.Phase = store.HistoryFinalizing
		if err := checkpoint(ctx); err != nil {
			return err
		}
		a.emitOrPrint("backfill_stopped", map[string]any{
			"chat_jid": windowChat, "reason": legacyReason, "stop_reason": reason,
		}, "%s\n", message)
		return nil
	}

	runRequests := func(ctx context.Context) error {
		// Sync can learn mappings and migrate old LID rows while connecting.
		// Resolve the local identity only after that migration has completed.
		windowChat, windowAlias = countIdentities(ctx)
		var err error
		beforeCount, err = a.db.CountConversationMessages(windowChat, windowAlias)
		if err != nil {
			return historyFailure(rec.AttemptID, rec.Phase, "store_state", rec.DispatchPossible, true, fmt.Errorf("count backfill conversation before requests: %w", err))
		}
		rec.AccountJID = publicHistoryAccount(a.wa.LinkedJID())
		rec.WindowChatJID, rec.WindowAliasJID = windowChat, windowAlias
		if rec.WindowAliasJID == rec.WindowChatJID {
			rec.WindowAliasJID = ""
		}
		rec.BaselineCount = new(beforeCount)
		rec.Phase = store.HistoryObserving
		if err := checkpoint(ctx); err != nil {
			return err
		}
		if runtime != nil {
			beforeStored = runtime.messagesStored.Load()
		}
		mu.Lock()
		windowActive = true
		mu.Unlock()
		chatStr := windowChat
		for i := 0; i < opts.Requests; i++ {
			if err := checkIdentities(ctx); err != nil {
				return err
			}
			oldest, err := a.db.GetOldestMessageInfo(chatStr)
			if err != nil {
				if err == sql.ErrNoRows {
					return historyFailure(rec.AttemptID, rec.Phase, "no_local_anchor", rec.DispatchPossible, true, fmt.Errorf("no messages for %s in local DB; run `wacli sync` first", chatStr))
				}
				return historyFailure(rec.AttemptID, rec.Phase, "store_state", rec.DispatchPossible, true, err)
			}

			resp, err := requestAnchor(ctx, oldest)
			if errors.Is(err, errResponseTimeout) && ctx.Err() == nil {
				next, nextErr := a.db.GetNextMessageInfo(chatStr, oldest.MsgID)
				if nextErr != nil && !errors.Is(nextErr, sql.ErrNoRows) {
					return nextErr
				}
				if nextErr == nil {
					a.emitWarning("backfill_anchor_retry",
						fmt.Sprintf("warning: no history response for anchor %s; retrying once with next local anchor %s", oldest.MsgID, next.MsgID),
						map[string]any{"chat_jid": chatStr, "anchor_msg_id": oldest.MsgID, "retry_anchor_msg_id": next.MsgID})
					resp, err = requestAnchor(ctx, next)
				}
			}
			if err != nil {
				return err
			}

			if err := ctx.Err(); err != nil {
				return err
			}
			if err := persistenceError(); err != nil {
				return err
			}
			if err := checkIdentities(ctx); err != nil {
				return err
			}
			a.emitOrPrint("backfill_response", map[string]any{
				"chat_jid":       chatStr,
				"conversations":  resp.conversations,
				"messages":       resp.messages,
				"responses_seen": responsesSeen,
			}, "On-demand history sync: %d conversations, %d messages.\n", resp.conversations, resp.messages)

			newOldest, err := a.db.GetOldestMessageInfo(chatStr)
			if err != nil {
				return fmt.Errorf("read oldest backfill message after response: %w", err)
			}
			// Explicit primary evidence wins even for empty or duplicate replies.
			if resp.endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
				return stop(BackfillStopPrimaryNoMore, "start_of_history_reached", "Primary device reports no more messages available (stopping).")
			}
			if resp.messages <= 0 {
				return stop(BackfillStopEmptyResponse, "no_messages_returned", "No messages returned (stopping).")
			}
			// A retry's newer anchor is not progress past the original oldest row.
			if newOldest.MsgID == oldest.MsgID {
				return stop(BackfillStopNoProgress, "no_older_messages_added", "No older messages were added (stopping).")
			}
		}
		return stop(BackfillStopRequestedBatchLimit, "requested_batch_limit", "Requested batch limit reached (stopping).")
	}
	var syncRes SyncResult
	if runtime == nil {
		syncRes, err = a.Sync(ctx, SyncOptions{
			Mode: SyncModeOnce, AllowQR: false, IdleExit: opts.IdleExit,
			afterHistorySync: handleOnDemand, historyStoreError: observeStoreError,
			AfterConnect: runRequests,
		})
	} else {
		observer := newHistoryObserver(handleOnDemand, observeStoreError)
		if err := a.registerHistoryObserver(runtime, observer); err != nil {
			return BackfillResult{}, err
		}
		defer a.removeHistoryObserver(observer)
		err = runRequests(ctx)
		if err == nil {
			observer.mu.Lock()
			observer.last = time.Now()
			observer.mu.Unlock()
			err = observer.waitIdle(ctx, opts.IdleExit)
		}
		a.removeHistoryObserver(observer)
		syncRes.MessagesStored = runtime.messagesStored.Load() - beforeStored
	}
	// Close the observer's operation-local window before reading its final error.
	// Late callbacks cannot mutate the result or affect a later Sync operation.
	mu.Lock()
	windowActive = false
	finalStoreErr := storeErr
	if !observedResponse.observedAt.IsZero() {
		rec.ResponseObservedAt = new(observedResponse.observedAt)
		rec.ResponseChatJID = observedResponse.chatJID
	}
	if primaryObservedAt != nil {
		rec.PrimaryNoMoreObservedAt = new(*primaryObservedAt)
		rec.PrimaryResponseChatJID = primaryResponseChat
	}
	mu.Unlock()
	if err != nil {
		return BackfillResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return BackfillResult{}, err
	}
	if finalStoreErr != nil {
		return BackfillResult{}, finalStoreErr
	}
	if err := checkIdentities(ctx); err != nil {
		return BackfillResult{}, err
	}
	afterCount, err := a.db.CountConversationMessages(windowChat, windowAlias)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("count backfill conversation after sync: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return BackfillResult{}, err
	}
	if err := checkIdentities(ctx); err != nil {
		return BackfillResult{}, err
	}
	if afterCount < beforeCount {
		return BackfillResult{}, fmt.Errorf("backfill conversation count decreased during the counting window; result could not be measured reliably; already persisted messages may remain")
	}

	return BackfillResult{
		ChatJID:        windowChat,
		RequestsSent:   requestsSent,
		ResponsesSeen:  responsesSeen,
		MessagesAdded:  afterCount - beforeCount,
		MessagesSynced: syncRes.MessagesStored,
		StopReason:     stopReason,
	}, nil
}

func normalizeBackfillOptions(opts BackfillOptions) BackfillOptions {
	if opts.Count <= 0 {
		opts.Count = DefaultBackfillCount
	}
	if opts.Requests <= 0 {
		opts.Requests = DefaultBackfillRequests
	}
	if opts.WaitPerRequest <= 0 {
		opts.WaitPerRequest = 60 * time.Second
	}
	if opts.IdleExit <= 0 {
		opts.IdleExit = 5 * time.Second
	}
	return opts
}

func validateBackfillOptions(opts BackfillOptions) error {
	if opts.WaitPerRequest > 5*time.Minute || opts.IdleExit > 5*time.Minute {
		return fmt.Errorf("--wait and --idle-exit must be <= 5m")
	}
	if opts.Count > MaxBackfillCount {
		return fmt.Errorf("--count must be <= %d (got %d)", MaxBackfillCount, opts.Count)
	}
	if opts.Requests > MaxBackfillRequests {
		return fmt.Errorf("--requests must be <= %d (got %d)", MaxBackfillRequests, opts.Requests)
	}
	return nil
}
