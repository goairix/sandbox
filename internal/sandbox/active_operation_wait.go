package sandbox

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

type activeOperationWait struct {
	mu          sync.Mutex
	observation state.ActiveOperationObservation
	err         error
	ready       bool
	updated     chan struct{}
	done        chan struct{}
	cancel      context.CancelFunc
	refs        int // guarded by Manager.activeWaitsMu
}

func (m *Manager) waitDistributedOperations(ctx context.Context, id string, exclusive *state.ActiveSandboxOperation) error {
	target := int64(0)
	interval := 25 * time.Millisecond
	if exclusive != nil {
		target = 1
		interval = 10 * time.Millisecond
	}
	repo, ok := m.activeSandboxes.(state.ActiveChangeRepository)
	if !ok || m.activeChanges == nil {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			count, err := m.activeSandboxes.LiveOperations(ctx, id)
			if err != nil {
				return err
			}
			if count == target {
				return nil
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-ticker.C:
			}
		}
	}
	m.activeWaitsMu.Lock()
	if m.activeWaits == nil {
		m.activeWaits = make(map[string]*activeOperationWait)
	}
	waiter := m.activeWaits[id]
	if waiter == nil {
		workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		waiter = &activeOperationWait{updated: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		m.activeWaits[id] = waiter
		go m.runOperationWait(workerCtx, repo, id, waiter)
	}
	waiter.refs++
	m.activeWaitsMu.Unlock()
	defer func() {
		m.activeWaitsMu.Lock()
		waiter.refs--
		last := waiter.refs == 0
		if last && m.activeWaits[id] == waiter {
			delete(m.activeWaits, id)
			waiter.cancel()
		}
		m.activeWaitsMu.Unlock()
		if last {
			<-waiter.done
		}
	}()
	for {
		waiter.mu.Lock()
		observation, err, ready, updated := waiter.observation, waiter.err, waiter.ready, waiter.updated
		waiter.mu.Unlock()
		if ready {
			if err != nil {
				return err
			}
			if observation.Count == target {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-updated:
		}
	}
}

func (m *Manager) runOperationWait(ctx context.Context, repo state.ActiveChangeRepository, id string, w *activeOperationWait) {
	defer close(w.done)
	publish := func(observation state.ActiveOperationObservation, err error) {
		w.mu.Lock()
		w.observation = observation
		w.err = err
		w.ready = true
		close(w.updated)
		w.updated = make(chan struct{})
		w.mu.Unlock()
	}
	observed, err := repo.ObserveOperations(ctx, id)
	publish(observed, err)
	if err != nil {
		return
	}
	changes, unsubscribe := m.activeChanges.Subscribe(id, observed.Revision+1)
	defer unsubscribe()
	timer := time.NewTimer(time.Second + time.Duration(rand.Int64N(int64(100*time.Millisecond)+1)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-timer.C:
		}
		observed, err = repo.ObserveOperations(ctx, id)
		publish(observed, err)
		if err != nil {
			return
		}
		timer.Reset(time.Second + time.Duration(rand.Int64N(int64(100*time.Millisecond)+1)))
	}
}
