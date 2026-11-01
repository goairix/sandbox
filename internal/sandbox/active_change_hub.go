package sandbox

import (
	"context"
	"math"
	"sync"

	"github.com/goairix/sandbox/internal/storage/state"
)

type activeChangeSubscription struct {
	id string
	ch chan state.ActiveChangeBatch
}

// activeChangeHub multiplexes one prefix watch. Missing bounded history always
// produces Reset; a wakeup is only a reason to reread authoritative state.
type activeChangeHub struct {
	mu            sync.Mutex
	history       []state.ActiveChangeBatch
	floor         int64
	subscriptions map[*activeChangeSubscription]struct{}
	cancel        context.CancelFunc
	done          chan struct{}
	stopped       bool
}

func newActiveChangeHub(ctx context.Context, repo state.ActiveChangeRepository) *activeChangeHub {
	watchCtx, cancel := context.WithCancel(ctx)
	h := &activeChangeHub{floor: math.MaxInt64, subscriptions: make(map[*activeChangeSubscription]struct{}), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		stream, err := repo.WatchActiveChanges(watchCtx, 0)
		if err != nil {
			h.dispatch(state.ActiveChangeBatch{Reset: true})
			return
		}
		for {
			select {
			case <-watchCtx.Done():
				return
			case batch, ok := <-stream:
				if !ok {
					h.dispatch(state.ActiveChangeBatch{Reset: true})
					return
				}
				h.dispatch(batch)
			}
		}
	}()
	return h
}

func (h *activeChangeHub) Subscribe(id string, afterRevision int64) (<-chan state.ActiveChangeBatch, func()) {
	sub := &activeChangeSubscription{id: id, ch: make(chan state.ActiveChangeBatch, 1)}
	h.mu.Lock()
	if h.stopped || afterRevision < h.floor {
		sub.ch <- state.ActiveChangeBatch{Reset: true}
	} else {
		for _, batch := range h.history {
			if batch.Revision >= afterRevision {
				h.notify(sub, batch)
			}
		}
	}
	if !h.stopped {
		h.subscriptions[sub] = struct{}{}
	}
	h.mu.Unlock()
	var once sync.Once
	return sub.ch, func() { once.Do(func() { h.mu.Lock(); delete(h.subscriptions, sub); h.mu.Unlock() }) }
}

func (h *activeChangeHub) dispatch(batch state.ActiveChangeBatch) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	if batch.Reset {
		h.history = nil
		h.floor = math.MaxInt64
	} else {
		if h.floor == math.MaxInt64 {
			h.floor = batch.Revision
		}
		h.history = append(h.history, batch)
		if len(h.history) > 256 {
			h.floor = h.history[0].Revision + 1
			copy(h.history, h.history[1:])
			h.history = h.history[:256]
		}
	}
	for sub := range h.subscriptions {
		h.notify(sub, batch)
	}
}

// notify runs under mu. A full one-slot queue coalesces flags and revision
// rather than blocking the watcher or silently losing a lifecycle change.
func (h *activeChangeHub) notify(sub *activeChangeSubscription, batch state.ActiveChangeBatch) {
	next := state.ActiveChangeBatch{Revision: batch.Revision, Reset: batch.Reset}
	for _, change := range batch.Changes {
		if change.SandboxID == sub.id {
			next.Changes = append(next.Changes, change)
		}
	}
	if !next.Reset && len(next.Changes) == 0 {
		return
	}
	select {
	case old := <-sub.ch:
		next.Reset = next.Reset || old.Reset
		next.Revision = max(next.Revision, old.Revision)
		if len(old.Changes) > 0 {
			if len(next.Changes) == 0 {
				next.Changes = old.Changes
			} else {
				next.Changes[0].RecordChanged = next.Changes[0].RecordChanged || old.Changes[0].RecordChanged
				next.Changes[0].OperationChanged = next.Changes[0].OperationChanged || old.Changes[0].OperationChanged
			}
		}
	default:
	}
	sub.ch <- next
}

func (h *activeChangeHub) Stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.cancel()
	<-h.done
}
