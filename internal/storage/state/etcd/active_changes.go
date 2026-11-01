package etcd

import (
	"context"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.ActiveChangeRepository = (*ActiveSandboxRepository)(nil)

func (r *ActiveSandboxRepository) ObserveOperations(ctx context.Context, id string) (state.ActiveOperationObservation, error) {
	if !validActiveID(id) {
		return state.ActiveOperationObservation{}, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	started := time.Now()
	read, err := r.store.client.Get(requestCtx, r.operationPrefix(id), clientv3.WithPrefix(), clientv3.WithCountOnly())
	r.store.observeCoordinationStage(requestCtx, "observe", "read", err, started)
	if err != nil {
		return state.ActiveOperationObservation{}, err
	}
	return state.ActiveOperationObservation{Count: read.Count, Revision: read.Header.Revision}, nil
}

// WatchActiveChanges uses one prefix stream, replaying only complete revisions.
// Lost history emits Reset rather than claiming that nothing changed.
func (r *ActiveSandboxRepository) WatchActiveChanges(ctx context.Context, revision int64) (<-chan state.ActiveChangeBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(chan state.ActiveChangeBatch)
	prefix := r.recordKey("")
	go func() {
		defer close(out)
		send := func(batch state.ActiveChangeBatch) bool {
			select {
			case out <- batch:
				return true
			case <-ctx.Done():
				return false
			}
		}
		next := revision
		backoff := 100 * time.Millisecond
		for ctx.Err() == nil {
			watchCtx, cancel := context.WithCancel(ctx)
			watchStarted := time.Now()
			stream := r.store.client.Watch(watchCtx, prefix, clientv3.WithPrefix(), clientv3.WithRev(next), clientv3.WithProgressNotify(), clientv3.WithCreatedNotify())
			for response := range stream {
				if response.Created {
					r.store.observeCoordinationStage(ctx, "watch", "stream", nil, watchStarted)
				}
				if r.store.coordinationWatchObserver != nil {
					var bytes int64
					for _, event := range response.Events {
						bytes += int64((*mvccpb.Event)(event).Size())
					}
					r.store.coordinationWatchObserver(ctx, int64(len(response.Events)), bytes)
				}
				if response.Err() != nil {
					if !send(state.ActiveChangeBatch{Revision: response.CompactRevision, Reset: true}) {
						cancel()
						return
					}
					if response.CompactRevision > 0 {
						next = 0
					}
					break
				}
				backoff = 100 * time.Millisecond
				// Preserve each event revision, even when a response contains several.
				for start := 0; start < len(response.Events); {
					rev := response.Events[start].Kv.ModRevision
					changes := make(map[string]state.ActiveChange)
					end := start
					for end < len(response.Events) && response.Events[end].Kv.ModRevision == rev {
						event := response.Events[end]
						suffix := strings.TrimPrefix(string(event.Kv.Key), prefix)
						parts := strings.Split(suffix, "/")
						if validActiveID(parts[0]) {
							c := changes[parts[0]]
							c.SandboxID = parts[0]
							if len(parts) == 1 {
								c.RecordChanged = true
							}
							if len(parts) == 3 && parts[1] == "operation" && event.Type == clientv3.EventTypeDelete {
								c.OperationChanged = true
							}
							if c.RecordChanged || c.OperationChanged {
								changes[c.SandboxID] = c
							}
						}
						end++
					}
					if len(changes) > 0 {
						batch := state.ActiveChangeBatch{Revision: rev}
						for _, c := range changes {
							batch.Changes = append(batch.Changes, c)
						}
						sort.Slice(batch.Changes, func(i, j int) bool { return batch.Changes[i].SandboxID < batch.Changes[j].SandboxID })
						if !send(batch) {
							cancel()
							return
						}
					}
					next = rev + 1
					start = end
				}
				if response.IsProgressNotify() && response.Header.Revision >= next {
					next = response.Header.Revision + 1
				}
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			// Stream closure may have skipped a notification; consumers reread.
			if !send(state.ActiveChangeBatch{Reset: true}) {
				return
			}
			timer := time.NewTimer(backoff + time.Duration(rand.Int64N(int64(backoff/10)+1)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(5*time.Second, backoff*2)
		}
	}()
	return out, nil
}
