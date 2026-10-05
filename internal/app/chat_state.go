package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	appStateRetryInitialDelay = 250 * time.Millisecond
	appStateRetryMaxDelay     = 5 * time.Second
	appStateRecoveryMaxWait   = 5 * time.Minute
)

// AddChatStatePersistenceHandler captures app-state events that arrive while a
// one-shot chat-state command is connected. Remove it only after closing the
// connection so whatsmeow cannot advance another collection without persisting it.
func (a *App) AddChatStatePersistenceHandler(ctx context.Context) (func(), error) {
	if err := a.OpenWA(); err != nil {
		return nil, err
	}
	waClient := a.WA()
	a.waMu.Lock()
	a.appStateReplayOnClose = true
	lifetimeObserver := a.outboundEvents
	a.waMu.Unlock()
	handlerID := waClient.AddEventHandler(func(evt any) {
		eventCtx := ctx
		if lifetimeObserver != nil {
			persistCtx, done, ok := lifetimeObserver.enter(ctx)
			if !ok {
				return
			}
			defer done()
			eventCtx = persistCtx
		}
		switch v := evt.(type) {
		case *events.AppState, *events.Star, *events.DeleteForMe,
			*events.Archive, *events.Pin, *events.Mute, *events.MarkChatAsRead:
			a.handleAppStatePersistenceEvent(eventCtx, evt, nil)
		case *wa.AppStateKeyUnavailable:
			a.warnEmptyAppStateKey(v)
		}
	})
	a.waMu.Lock()
	a.appStateUnobserved = false
	a.waMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			waClient.RemoveEventHandler(handlerID)
		})
	}, nil
}

func (a *App) ArchiveChat(ctx context.Context, jid types.JID, archive bool) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularLow)
	if err != nil {
		return err
	}
	defer release()
	chatJID := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	_, _, err = a.archiveChatResolved(ctx, jid, chatJID, archive, nil)
	return err
}

// Resolved helpers run under beginChatStateWrite. The legacy wrappers retain
// their resolver; agent actions supply one frozen strict observation instead.
func (a *App) archiveChatResolved(ctx context.Context, jid types.JID, chatJID string, archive bool, beforeSend func() error) (ChatStateOutcome, ChatStateMirror, error) {
	return a.applyLocalChatStateWrite(ctx, beforeSend, func(boundary func()) ([]any, error) {
		return a.wa.ArchiveChat(ctx, jid, archive, nowUTC(), nil, boundary)
	}, func() error {
		settings, err := a.currentChatSettings(context.WithoutCancel(ctx), jid)
		if err != nil {
			return err
		}
		return a.persistCachedChatFlags(chatJID, settings)
	})
}

func (a *App) PinChat(ctx context.Context, jid types.JID, pin bool) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularLow)
	if err != nil {
		return err
	}
	defer release()
	chatJID := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	pending, err := a.beginLocalAppStateWrite(appstate.WAPatchRegularLow)
	if err != nil {
		return err
	}
	postSendEvents, err := a.wa.PinChat(ctx, jid, pin, func() { pending.reserve(a) })
	if err != nil {
		return errors.Join(err, a.failLocalAppStateWrite(ctx, &pending, postSendEvents))
	}
	if !pending.reserved {
		return fmt.Errorf("WhatsApp app state send completed without an apply boundary")
	}
	return a.completeLocalAppStateWrite(ctx, &pending, postSendEvents, func() error {
		settings, err := a.currentChatSettings(context.WithoutCancel(ctx), jid)
		if err != nil {
			return err
		}
		return a.persistCachedChatFlags(chatJID, settings)
	})
}

func (a *App) MuteChat(ctx context.Context, jid types.JID, mute bool, duration time.Duration) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularHigh)
	if err != nil {
		return err
	}
	defer release()
	chatJID := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	mutedUntil := mutedUntilUnix(mute, duration, nowUTC())
	pending, err := a.beginLocalAppStateWrite(appstate.WAPatchRegularHigh)
	if err != nil {
		return err
	}
	postSendEvents, err := a.wa.MuteChat(ctx, jid, mute, duration, func() { pending.reserve(a) })
	if err != nil {
		return errors.Join(err, a.failLocalAppStateWrite(ctx, &pending, postSendEvents))
	}
	if !pending.reserved {
		return fmt.Errorf("WhatsApp app state send completed without an apply boundary")
	}
	return a.completeLocalAppStateWrite(ctx, &pending, postSendEvents, func() error {
		return a.db.SetChatMutedUntil(chatJID, mutedUntil)
	})
}

func (a *App) MarkChatRead(ctx context.Context, jid types.JID, read bool) error {
	release, err := a.beginChatStateWrite(ctx, appstate.WAPatchRegularLow)
	if err != nil {
		return err
	}
	defer release()
	chatJID := canonicalJIDString(a.canonicalStoreJID(ctx, jid))
	_, _, err = a.markChatReadResolved(ctx, jid, chatJID, read, nil)
	return err
}

func (a *App) markChatReadResolved(ctx context.Context, jid types.JID, chatJID string, read bool, beforeSend func() error) (ChatStateOutcome, ChatStateMirror, error) {
	lastTS, lastKey := a.latestMessageRange(chatJID)
	return a.applyLocalChatStateWrite(ctx, beforeSend, func(boundary func()) ([]any, error) {
		return a.wa.MarkChatAsRead(ctx, jid, read, lastTS, lastKey, boundary)
	}, func() error {
		if !read {
			return a.db.SetChatUnread(chatJID, true)
		}
		var ids []string
		if lastKey != nil {
			ids = []string{lastKey.GetID()}
		}
		return a.db.ClearChatUnreadThrough(chatJID, lastTS, ids)
	})
}

func (a *App) applyLocalChatStateWrite(ctx context.Context, beforeSend func() error, send func(func()) ([]any, error), persist func() error) (ChatStateOutcome, ChatStateMirror, error) {
	pending, err := a.beginLocalAppStateWrite(appstate.WAPatchRegularLow)
	if err != nil {
		return ChatStateNotDispatched, ChatStateMirrorUnknown, err
	}
	if beforeSend != nil {
		if err := beforeSend(); err != nil {
			return ChatStateNotDispatched, ChatStateMirrorUnknown, err
		}
	}
	// Crossing the mutating call is conservative: beforeApply reserves local
	// persistence before SDK preparation, and does not prove a frame was sent.
	postSendEvents, err := send(func() { pending.reserve(a) })
	if err != nil {
		return ChatStateUncertain, ChatStateMirrorUnknown, errors.Join(err, a.failLocalAppStateWrite(ctx, &pending, postSendEvents))
	}
	if !pending.reserved {
		return ChatStateSDKCompleted, ChatStateMirrorUnconfirmed, fmt.Errorf("WhatsApp app state send completed without an apply boundary")
	}
	if err := a.completeLocalAppStateWrite(ctx, &pending, postSendEvents, persist); err != nil {
		return ChatStateSDKCompleted, ChatStateMirrorUnconfirmed, err
	}
	return ChatStateSDKCompleted, ChatStateMirrorPersisted, nil
}

func (a *App) acquireChatStateSync(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for chat state synchronization: %w", ctx.Err())
	case <-a.chatStateSync:
	}
	release := func() { a.chatStateSync <- struct{}{} }
	if err := ctx.Err(); err != nil {
		release()
		return nil, fmt.Errorf("wait for chat state synchronization: %w", err)
	}
	return release, nil
}

func (a *App) beginChatStateWrite(ctx context.Context, collection appstate.WAPatchName) (func(), error) {
	release, err := a.acquireChatStateSync(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.syncChatStateBeforeWrite(ctx, collection); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (a *App) syncChatStateBeforeWrite(ctx context.Context, collection appstate.WAPatchName) error {
	markerGeneration, recoveryRequired, err := a.db.BeginAppStateRecovery(string(collection))
	if err != nil {
		return fmt.Errorf("begin WhatsApp app state recovery for %s: %w", collection, err)
	}
	tracker := &appStatePersistenceTracker{}
	if recoveryRequired {
		return a.replayRequiredAppState(ctx, collection, markerGeneration, tracker)
	}

	for {
		err, persistenceErr := a.fetchAndPersistAppState(ctx, collection, false, tracker)
		if persistenceErr != nil {
			return fmt.Errorf("persist fetched app state %s: %w", collection, persistenceErr)
		}
		if err == nil {
			return a.clearCompletedAppStateRecovery(collection, markerGeneration)
		}

		if errors.Is(err, appstate.ErrMismatchingLTHash) || errors.Is(err, wa.ErrEmptyAppStateKeyShare) {
			return a.replayRequiredAppState(ctx, collection, markerGeneration, tracker)
		} else if errors.Is(err, appstate.ErrKeyNotFound) {
			return a.replayRequiredAppState(ctx, collection, markerGeneration, tracker)
		} else {
			return fmt.Errorf("sync WhatsApp chat state before update: %w", err)
		}
	}
}

func (a *App) replayRequiredAppState(ctx context.Context, collection appstate.WAPatchName, markerGeneration int64, tracker *appStatePersistenceTracker) error {
	ctx, cancel := context.WithTimeout(ctx, appStateRecoveryMaxWait)
	defer cancel()
	retryDelay := appStateRetryInitialDelay
	for {
		fetchErr, persistenceErr := a.fetchAndPersistAppState(ctx, collection, true, tracker)
		if persistenceErr != nil {
			return fmt.Errorf("persist replayed app state %s: %w", collection, persistenceErr)
		}
		if fetchErr == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return a.clearCompletedAppStateRecovery(collection, markerGeneration)
		}
		if errors.Is(fetchErr, appstate.ErrMismatchingLTHash) || errors.Is(fetchErr, wa.ErrEmptyAppStateKeyShare) {
			return a.recoverMismatchingAppState(ctx, collection, markerGeneration, tracker, nil)
		}
		if !errors.Is(fetchErr, appstate.ErrKeyNotFound) {
			return fmt.Errorf("replay WhatsApp app state recovery for %s: %w", collection, fetchErr)
		}
		// whatsmeow requests the missing keys when patch decoding returns
		// ErrKeyNotFound. Keep the connection alive for that delivery, bounded
		// so one absent key cannot hold all chat-state writes forever.

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for missing WhatsApp app state key during %s recovery: %w", collection, ctx.Err())
		case <-timer.C:
		}
		if retryDelay < appStateRetryMaxDelay {
			retryDelay *= 2
			if retryDelay > appStateRetryMaxDelay {
				retryDelay = appStateRetryMaxDelay
			}
		}
	}
}

func (a *App) recoverMismatchingAppState(ctx context.Context, collection appstate.WAPatchName, markerGeneration int64, tracker *appStatePersistenceTracker, onRequested func(types.MessageID)) error {
	ticket := a.appStatePersist.reserve()
	eventsToPersist, recoveryErr := a.waitForPrimaryAppStateRecovery(ctx, collection, onRequested)
	persistCtx := context.WithoutCancel(ctx)
	result := make(chan error, 1)
	frontier := a.appStatePersist.complete(ticket, func() {
		if recoveryErr != nil {
			result <- nil
			return
		}
		result <- a.persistFetchedAppStateEvents(persistCtx, eventsToPersist, tracker)
	})
	if err := a.appStatePersist.waitThrough(persistCtx, frontier); err != nil {
		return err
	}
	if persistenceErr := <-result; persistenceErr != nil {
		return fmt.Errorf("persist recovered app state %s: %w", collection, persistenceErr)
	}
	if recoveryErr != nil {
		return recoveryErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.clearCompletedAppStateRecovery(collection, markerGeneration)
}

func (a *App) waitForPrimaryAppStateRecovery(ctx context.Context, collection appstate.WAPatchName, onRequested func(types.MessageID)) ([]any, error) {
	completed := make(chan []any, 1)
	var mu sync.Mutex
	var captured []any
	finished := false
	// whatsmeow dispatches recovery mutations synchronously and emits this
	// collection's AppStateSyncComplete last, so the sentinel closes the drain.
	handlerID := a.wa.AddEventHandler(func(evt any) {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		if syncComplete, ok := evt.(*events.AppStateSyncComplete); ok {
			if syncComplete != nil && syncComplete.Recovery && syncComplete.Name == collection {
				finished = true
				completed <- append([]any(nil), captured...)
			}
			return
		}
		if slices.Contains(appStateCollectionsForEvent(evt), collection) {
			captured = append(captured, evt)
		}
	})
	defer a.wa.RemoveEventHandler(handlerID)

	requestID, err := a.wa.RequestAppStateRecovery(ctx, string(collection))
	if err != nil {
		mu.Lock()
		finished = true
		mu.Unlock()
		return nil, fmt.Errorf("request WhatsApp app state recovery for %s: %w", collection, err)
	}
	if onRequested != nil {
		onRequested(requestID)
	}
	select {
	case eventsToPersist := <-completed:
		return eventsToPersist, nil
	case <-ctx.Done():
		mu.Lock()
		finished = true
		mu.Unlock()
		return nil, fmt.Errorf("wait for WhatsApp app state recovery for %s: %w", collection, ctx.Err())
	}
}

func (a *App) fetchAndPersistAppState(ctx context.Context, collection appstate.WAPatchName, fullSync bool, tracker *appStatePersistenceTracker) (fetchErr, persistenceErr error) {
	ticket := a.appStatePersist.reserve()
	var eventsToPersist []any
	func() {
		releaseFetch := a.beginManualAppStateFetch(collection)
		defer releaseFetch()
		eventsToPersist, fetchErr = a.wa.FetchAppStateEvents(ctx, string(collection), fullSync, false)
	}()
	result := make(chan error, 1)
	frontier := a.appStatePersist.complete(ticket, func() {
		result <- a.persistFetchedAppStateEvents(ctx, eventsToPersist, tracker)
	})
	if waitErr := a.appStatePersist.waitThrough(ctx, frontier); waitErr != nil {
		return fetchErr, waitErr
	}
	return fetchErr, <-result
}

func (a *App) beginManualAppStateFetch(collection appstate.WAPatchName) func() {
	name := string(collection)
	a.manualFetchMu.Lock()
	if a.manualFetches == nil {
		a.manualFetches = make(map[string]int)
	}
	a.manualFetches[name]++
	a.manualFetchMu.Unlock()
	return func() {
		a.manualFetchMu.Lock()
		a.manualFetches[name]--
		if a.manualFetches[name] == 0 {
			delete(a.manualFetches, name)
		}
		a.manualFetchMu.Unlock()
	}
}

func (a *App) ownsManualAppStateFetch(collection appstate.WAPatchName) bool {
	a.manualFetchMu.Lock()
	defer a.manualFetchMu.Unlock()
	return a.manualFetches[string(collection)] > 0
}

func (a *App) clearCompletedAppStateRecovery(collection appstate.WAPatchName, markerGeneration int64) error {
	a.waMu.Lock()
	defer a.waMu.Unlock()
	// A read can finish after Sync removed coverage, while the detached SDK
	// socket still advances state. Keep debt until a covered later startup.
	if a.appStateUnobserved {
		return nil
	}
	cleared, err := a.db.ClearAppStateRecoveryGeneration(string(collection), markerGeneration)
	if err != nil {
		return fmt.Errorf("clear WhatsApp app state recovery for %s: %w", collection, err)
	}
	if !cleared {
		required, checkErr := a.db.AppStateRecoveryRequired(string(collection))
		if checkErr != nil {
			return fmt.Errorf("check completed WhatsApp app state recovery for %s: %w", collection, checkErr)
		}
		if required {
			return fmt.Errorf("WhatsApp app state recovery changed during %s synchronization; retry the command", collection)
		}
	}
	return nil
}

type pendingLocalAppStateWrite struct {
	collection appstate.WAPatchName
	generation int64
	once       sync.Once
	ticket     uint64
	reserved   bool
}

func (p *pendingLocalAppStateWrite) reserve(a *App) {
	p.once.Do(func() {
		p.ticket = a.appStatePersist.reserve()
		p.reserved = true
	})
}

func (a *App) beginLocalAppStateWrite(collection appstate.WAPatchName) (pendingLocalAppStateWrite, error) {
	generation, err := a.db.MarkAppStateRecoveryGeneration(string(collection))
	if err != nil {
		return pendingLocalAppStateWrite{}, fmt.Errorf("mark local WhatsApp app state recovery for %s: %w", collection, err)
	}
	// Keep this exact intent until a later pre-write replay. Other queued work
	// clears only its own generation and cannot erase an unfinished send.
	return pendingLocalAppStateWrite{collection: collection, generation: generation}, nil
}

func (a *App) failLocalAppStateWrite(ctx context.Context, pending *pendingLocalAppStateWrite, postSendEvents []any) error {
	if !pending.reserved {
		return nil
	}
	result := make(chan error, 1)
	persistCtx := context.WithoutCancel(ctx)
	frontier := a.appStatePersist.complete(pending.ticket, func() {
		tracker := &appStatePersistenceTracker{}
		result <- a.persistFetchedAppStateEvents(persistCtx, postSendEvents, tracker)
	})
	if err := a.appStatePersist.waitThrough(persistCtx, frontier); err != nil {
		return err
	}
	return <-result
}

func (a *App) completeLocalAppStateWrite(ctx context.Context, pending *pendingLocalAppStateWrite, postSendEvents []any, persist func() error) error {
	result := make(chan error, 1)
	persistCtx := context.WithoutCancel(ctx)
	frontier := a.appStatePersist.complete(pending.ticket, func() {
		localErr := persist()
		tracker := &appStatePersistenceTracker{}
		eventsErr := a.persistFetchedAppStateEvents(persistCtx, postSendEvents, tracker)
		persistErr := localErr
		if persistErr == nil {
			persistErr = eventsErr
		}
		result <- persistErr
	})
	if err := a.appStatePersist.waitThrough(persistCtx, frontier); err != nil {
		return err
	}
	// Another whatsmeow fetch can advance the cursor and dispatch later. Keep
	// replay debt durable until the next pre-write full replay.
	return <-result
}

func (a *App) persistFetchedAppStateEvents(ctx context.Context, eventsToPersist []any, tracker *appStatePersistenceTracker) error {
	tracker.begin()
	for _, evt := range eventsToPersist {
		a.handleAppStatePersistenceEvent(ctx, evt, tracker)
	}
	return tracker.end()
}

func (a *App) latestMessageRange(chatJID string) (time.Time, *waCommon.MessageKey) {
	info, err := a.db.GetLatestMessageInfo(chatJID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.emitWarning(
				"chat_state_latest_message_failed",
				fmt.Sprintf("warning: failed to load latest message for chat state patch: %v", err),
				map[string]any{"chat_jid": chatJID, "error": err.Error()},
			)
		}
		return time.Time{}, nil
	}
	return info.Timestamp, messageKeyFromStore(info)
}

func messageKeyFromStore(info store.MessageInfo) *waCommon.MessageKey {
	if strings.TrimSpace(info.ChatJID) == "" || strings.TrimSpace(info.MsgID) == "" {
		return nil
	}
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(info.ChatJID),
		FromMe:    proto.Bool(info.FromMe),
		ID:        proto.String(info.MsgID),
	}
	if sender := strings.TrimSpace(info.SenderJID); sender != "" && sender != info.ChatJID {
		key.Participant = proto.String(sender)
	}
	return key
}

func (a *App) currentChatSettings(ctx context.Context, jid types.JID) (types.LocalChatSettings, error) {
	reader, ok := a.WA().(interface {
		GetChatSettings(context.Context, types.JID) (types.LocalChatSettings, error)
	})
	if !ok {
		return types.LocalChatSettings{}, fmt.Errorf("WhatsApp chat settings unavailable")
	}
	settings, err := reader.GetChatSettings(ctx, jid)
	if err != nil {
		return types.LocalChatSettings{}, fmt.Errorf("read WhatsApp chat settings: %w", err)
	}
	if !settings.Found {
		return types.LocalChatSettings{}, fmt.Errorf("WhatsApp chat settings not found")
	}
	return settings, nil
}

func (a *App) persistCachedChatFlags(chatJID string, settings types.LocalChatSettings) error {
	if err := a.db.SetChatArchived(chatJID, settings.Archived); err != nil {
		return err
	}
	// Archive also unpins in the SDK. Persist both cached flags even when the
	// corresponding Pin callback has not arrived yet. Unread has another source.
	return a.db.SetChatPinned(chatJID, settings.Pinned)
}

func (a *App) handleChatStateEvent(ctx context.Context, evt any) error {
	switch v := evt.(type) {
	case *events.Archive:
		if v == nil || v.JID.IsEmpty() || v.Action == nil {
			return nil
		}
		// SendAppState dispatches after advancing the SDK cache and may arrive
		// after a newer replay. Read the exact event JID before resolving the
		// archive identity; never apply an old payload or guess an alias.
		settings, err := a.currentChatSettings(ctx, v.JID)
		if err == nil {
			chat := a.canonicalStoreJID(ctx, v.JID)
			err = a.persistCachedChatFlags(canonicalJIDString(chat), settings)
		}
		if err != nil {
			a.emitChatStateWarning("archive", v.JID, err)
			return err
		}
	case *events.Pin:
		if v == nil || v.JID.IsEmpty() || v.Action == nil {
			return nil
		}
		settings, err := a.currentChatSettings(ctx, v.JID)
		if err == nil {
			chat := a.canonicalStoreJID(ctx, v.JID)
			err = a.persistCachedChatFlags(canonicalJIDString(chat), settings)
		}
		if err != nil {
			a.emitChatStateWarning("pin", v.JID, err)
			return err
		}
	case *events.Mute:
		if v == nil || v.JID.IsEmpty() || v.Action == nil {
			return nil
		}
		chat := a.canonicalStoreJID(ctx, v.JID)
		if err := a.db.SetChatMutedUntil(canonicalJIDString(chat), mutedUntilFromAction(v.Action)); err != nil {
			a.emitChatStateWarning("mute", v.JID, err)
			return err
		}
	case *events.MarkChatAsRead:
		if v == nil || v.JID.IsEmpty() || v.Action == nil {
			return nil
		}
		chat := a.canonicalStoreJID(ctx, v.JID)
		var err error
		if v.Action.GetRead() {
			through, ids := markChatAsReadPosition(v)
			err = a.db.ClearChatUnreadThrough(canonicalJIDString(chat), through, ids)
		} else {
			err = a.db.SetChatUnread(canonicalJIDString(chat), true)
		}
		if err != nil {
			a.emitChatStateWarning("mark_read", v.JID, err)
			return err
		}
	}
	return nil
}

func mutedUntilFromAction(action *waSyncAction.MuteAction) int64 {
	if action == nil || !action.GetMuted() {
		return 0
	}
	ms := action.GetMuteEndTimestamp()
	if ms < 0 {
		return -1
	}
	if ms > 0 {
		return time.UnixMilli(ms).Unix()
	}
	return -1
}

func mutedUntilUnix(mute bool, duration time.Duration, base time.Time) int64 {
	if !mute {
		return 0
	}
	if duration <= 0 {
		return -1
	}
	return base.Add(duration).Unix()
}

func (a *App) emitChatStateWarning(kind string, jid types.JID, err error) {
	a.emitWarning(
		"chat_state_store_failed",
		fmt.Sprintf("warning: failed to store %s chat state for %s: %v", kind, jid, err),
		map[string]any{"kind": kind, "jid": jid.String(), "error": err.Error()},
	)
}
