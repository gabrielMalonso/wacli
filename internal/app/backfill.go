package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type BackfillOptions struct {
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
	ChatJID       string
	RequestsSent  int
	ResponsesSeen int
	// MessagesAdded is net distinct local growth for the selected conversation
	// during the post-connect counting window, including concurrent activity.
	MessagesAdded  int64
	MessagesSynced int64 // global Sync counter, including updates/replays
	StopReason     BackfillStopReason
}

type onDemandResponse struct {
	conversations int
	messages      int
	endType       waHistorySync.Conversation_EndOfHistoryTransferType
}

func (a *App) BackfillHistory(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	chatStr := strings.TrimSpace(opts.ChatJID)
	if chatStr == "" {
		return BackfillResult{}, fmt.Errorf("--chat is required")
	}
	chat, err := types.ParseJID(chatStr)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("parse chat JID: %w", err)
	}
	chatStr = chat.String()

	opts = normalizeBackfillOptions(opts)
	if err := validateBackfillOptions(opts); err != nil {
		return BackfillResult{}, err
	}

	if err := a.EnsureAuthed(ctx); err != nil {
		return BackfillResult{}, err
	}
	if err := a.OpenWA(); err != nil {
		return BackfillResult{}, err
	}
	a.wa.SetManualHistorySyncDownload(true)
	defer a.wa.SetManualHistorySyncDownload(false)

	var mu sync.Mutex
	var waitCh chan onDemandResponse
	var windowActive bool
	var storeErr error
	observeStoreError := func(jid types.JID, err error) {
		if a.canonicalStoreJID(ctx, jid) != a.canonicalStoreJID(ctx, chat) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if windowActive && storeErr == nil {
			storeErr = fmt.Errorf("persist on-demand history for %s: %w", jid, err)
		}
	}
	persistenceError := func() error {
		mu.Lock()
		defer mu.Unlock()
		return storeErr
	}
	var manualMessagesStored atomic.Int64
	var manualLastEvent atomic.Int64
	manualLastEvent.Store(nowUTC().UnixNano())
	handleOnDemand := func(hs *events.HistorySync) {
		if hs == nil || hs.Data == nil || hs.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			return
		}
		for _, conv := range hs.Data.GetConversations() {
			if a.canonicalStoreJIDString(ctx, strings.TrimSpace(conv.GetID())) != a.canonicalStoreJID(ctx, chat).String() {
				continue
			}
			mu.Lock()
			ch := waitCh
			mu.Unlock()
			if ch == nil {
				return
			}
			resp := onDemandResponse{
				conversations: len(hs.Data.GetConversations()),
				messages:      len(conv.GetMessages()),
				endType:       conv.GetEndOfHistoryTransferType(),
			}
			select {
			case ch <- resp:
			default:
			}
			return
		}
	}
	handlerID := a.wa.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			notif := historySyncNotificationFromMessage(v)
			if notif == nil || notif.GetSyncType() != waE2E.HistorySyncType_ON_DEMAND {
				return
			}
			data, err := a.wa.DownloadHistorySync(ctx, notif)
			if err != nil {
				a.emitWarning(
					"on_demand_history_download_failed",
					fmt.Sprintf("warning: failed to download on-demand history sync: %v", err),
					map[string]any{"error": err.Error()},
				)
				return
			}
			if data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
				return
			}
			hs := &events.HistorySync{Data: data}
			a.handleHistorySync(ctx, SyncOptions{historyStoreError: observeStoreError}, hs, &manualMessagesStored, &manualLastEvent, func(string, string) {})
			handleOnDemand(hs)
		}
	})
	defer a.wa.RemoveEventHandler(handlerID)

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
		requestsSent++
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
	var windowChat, windowAlias string
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
	stop := func(reason BackfillStopReason, legacyReason, message string) {
		stopReason = reason
		a.emitOrPrint("backfill_stopped", map[string]any{
			"chat_jid": windowChat, "reason": legacyReason, "stop_reason": reason,
		}, "%s\n", message)
	}

	syncRes, err := a.Sync(ctx, SyncOptions{
		Mode:              SyncModeOnce,
		AllowQR:           false,
		IdleExit:          opts.IdleExit,
		afterHistorySync:  handleOnDemand,
		historyStoreError: observeStoreError,
		AfterConnect: func(ctx context.Context) error {
			// Sync can learn mappings and migrate old LID rows while connecting.
			// Resolve the local identity only after that migration has completed.
			windowChat, windowAlias = countIdentities(ctx)
			var err error
			beforeCount, err = a.db.CountConversationMessages(windowChat, windowAlias)
			if err != nil {
				return fmt.Errorf("count backfill conversation before requests: %w", err)
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
						return fmt.Errorf("no messages for %s in local DB; run `wacli sync` first", chatStr)
					}
					return err
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
					stop(BackfillStopPrimaryNoMore, "start_of_history_reached", "Primary device reports no more messages available (stopping).")
					return nil
				}
				if resp.messages <= 0 {
					stop(BackfillStopEmptyResponse, "no_messages_returned", "No messages returned (stopping).")
					return nil
				}
				// A retry's newer anchor is not progress past the original oldest row.
				if newOldest.MsgID == oldest.MsgID {
					stop(BackfillStopNoProgress, "no_older_messages_added", "No older messages were added (stopping).")
					return nil
				}
			}
			stop(BackfillStopRequestedBatchLimit, "requested_batch_limit", "Requested batch limit reached (stopping).")
			return nil
		},
	})
	// Close the observer's operation-local window before reading its final error.
	// Late callbacks cannot mutate the result or affect a later Sync operation.
	mu.Lock()
	windowActive = false
	finalStoreErr := storeErr
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
	if opts.Count > MaxBackfillCount {
		return fmt.Errorf("--count must be <= %d (got %d)", MaxBackfillCount, opts.Count)
	}
	if opts.Requests > MaxBackfillRequests {
		return fmt.Errorf("--requests must be <= %d (got %d)", MaxBackfillRequests, opts.Requests)
	}
	return nil
}
