package app

import (
	"context"
	"fmt"
	"sync"

	"go.mau.fi/whatsmeow/appstate"
)

var mirroredAppStateCollections = []appstate.WAPatchName{
	appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow, appstate.WAPatchRegular,
}

// RequireAppStateReplay protects connections without an archive persistence
// handler. It must succeed before connecting; it never imports unrelated rows.
// Close reaffirms this debt after the session store and known writers close.
func (a *App) RequireAppStateReplay(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.waMu.Lock()
	defer a.waMu.Unlock()
	if a.opts.ReadOnly {
		return fmt.Errorf("read-only mode: command would mark app state recovery")
	}
	if a.closed {
		return fmt.Errorf("application is closed")
	}
	names := make([]string, len(mirroredAppStateCollections))
	for i, name := range mirroredAppStateCollections {
		names[i] = string(name)
	}
	if _, err := a.db.MarkAppStateRecoveryGenerations(names); err != nil {
		return fmt.Errorf("mark WhatsApp app state replay: %w", err)
	}
	a.appStateReplayOnClose = true
	return nil
}

func (a *App) restoreAppStateReplay() error {
	var missing []string
	for _, name := range mirroredAppStateCollections {
		required, err := a.db.AppStateRecoveryRequired(string(name))
		if err != nil {
			return err
		}
		if !required {
			missing = append(missing, string(name))
		}
	}
	_, err := a.db.MarkAppStateRecoveryGenerations(missing)
	return err
}

type appStatePersistenceTask struct {
	ready         bool
	blocksEarlier bool
	run           func()
}

type appStatePersistenceSequencer struct {
	mu      sync.Mutex
	next    uint64
	serving uint64
	running bool
	changed chan struct{}
	tasks   map[uint64]*appStatePersistenceTask
}

func (s *appStatePersistenceSequencer) reserve() uint64 {
	return s.reserveTask(false)
}

func (s *appStatePersistenceSequencer) reserveLive() uint64 {
	return s.reserveTask(true)
}

func (s *appStatePersistenceSequencer) reserveTask(blocksEarlier bool) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	ticket := s.next
	s.next++
	s.tasks[ticket] = &appStatePersistenceTask{blocksEarlier: blocksEarlier}
	return ticket
}

func (s *appStatePersistenceSequencer) complete(ticket uint64, run func()) uint64 {
	return s.completeTask(ticket, run, true)
}

func (s *appStatePersistenceSequencer) completeOne(ticket uint64, run func()) uint64 {
	return s.completeTask(ticket, run, false)
}

func (s *appStatePersistenceSequencer) completeTask(ticket uint64, run func(), includeSuccessors bool) uint64 {
	s.mu.Lock()
	s.initLocked()
	task := s.tasks[ticket]
	task.ready = true
	task.run = run
	waitFrontier := ticket
	drainFrontier := ticket
	drainBlocked := false
	for candidate := ticket + 1; candidate < s.next; candidate++ {
		nextTask := s.tasks[candidate]
		if nextTask == nil {
			break
		}
		if nextTask.ready {
			if !drainBlocked {
				drainFrontier = candidate
			}
			if includeSuccessors {
				waitFrontier = candidate
			}
			continue
		}
		if includeSuccessors && nextTask.blocksEarlier {
			waitFrontier = candidate
			drainBlocked = true
			continue
		}
		break
	}
	start := !s.running && ticket == s.serving
	if start {
		s.running = true
	}
	s.mu.Unlock()
	if start {
		s.drainThrough(drainFrontier)
	}
	return waitFrontier
}

func (s *appStatePersistenceSequencer) waitThrough(ctx context.Context, frontier uint64) error {
	for {
		s.mu.Lock()
		s.initLocked()
		if s.serving > frontier {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for live app state persistence: %w", ctx.Err())
		case <-changed:
		}
	}
}

func (s *appStatePersistenceSequencer) waitIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		s.initLocked()
		if s.serving == s.next {
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for app state persistence queue: %w", ctx.Err())
		case <-changed:
		}
	}
}

func (s *appStatePersistenceSequencer) drainThrough(frontier uint64) {
	for {
		s.mu.Lock()
		if s.serving > frontier {
			s.running = false
			nextFrontier := s.next - 1
			nextTask := s.tasks[s.serving]
			handoff := nextTask != nil && nextTask.ready
			if handoff {
				s.running = true
			}
			s.mu.Unlock()
			if handoff {
				go s.drainThrough(nextFrontier)
			}
			return
		}
		task := s.tasks[s.serving]
		if task == nil || !task.ready {
			s.running = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		task.run()

		s.mu.Lock()
		delete(s.tasks, s.serving)
		s.serving++
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
}

func (s *appStatePersistenceSequencer) initLocked() {
	if s.tasks == nil {
		s.tasks = make(map[uint64]*appStatePersistenceTask)
	}
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
}

type appStatePersistenceTracker struct {
	mu     sync.Mutex
	active bool
	err    error
}

func (t *appStatePersistenceTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = true
	t.err = nil
}

func (t *appStatePersistenceTracker) record(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active && t.err == nil {
		t.err = err
	}
}

func (t *appStatePersistenceTracker) end() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = false
	return t.err
}
