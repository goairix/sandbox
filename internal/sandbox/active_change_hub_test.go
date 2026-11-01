package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type hubTestRepository struct {
	state.ActiveChangeRepository
	events chan state.ActiveChangeBatch
	calls  atomic.Int64
}

func (r *hubTestRepository) WatchActiveChanges(ctx context.Context, _ int64) (<-chan state.ActiveChangeBatch, error) {
	r.calls.Add(1)
	return r.events, nil
}
func hubTestNext(t *testing.T, ch <-chan state.ActiveChangeBatch) state.ActiveChangeBatch {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("missing notification")
		return state.ActiveChangeBatch{}
	}
}
func newHubTest(t *testing.T) (*hubTestRepository, *activeChangeHub) {
	t.Helper()
	repo := &hubTestRepository{events: make(chan state.ActiveChangeBatch)}
	hub := newActiveChangeHub(context.Background(), repo)
	t.Cleanup(hub.Stop)
	return repo, hub
}

func TestActiveChangeHubSubscribeRace(t *testing.T) {
	repo, hub := newHubTest(t)
	initial, stop := hub.Subscribe("a", 1)
	defer stop()
	require.True(t, hubTestNext(t, initial).Reset, "unknown history must request authoritative reread")
	repo.events <- state.ActiveChangeBatch{Revision: 10, Changes: []state.ActiveChange{{SandboxID: "a", RecordChanged: true}}}
	require.EqualValues(t, 10, hubTestNext(t, initial).Revision)
	// Event happened after the waiter read but before it subscribed.
	late, lateStop := hub.Subscribe("a", 10)
	defer lateStop()
	got := hubTestNext(t, late)
	require.True(t, got.Reset || got.Changes[0].RecordChanged)
	require.EqualValues(t, 1, repo.calls.Load())
}

func TestActiveChangeHubBoundsHistory(t *testing.T) {
	repo, hub := newHubTest(t)
	ch, stop := hub.Subscribe("a", 1)
	defer stop()
	hubTestNext(t, ch)
	for i := int64(1); i <= 300; i++ {
		repo.events <- state.ActiveChangeBatch{Revision: i, Changes: []state.ActiveChange{{SandboxID: "a", OperationChanged: true}}}
		hubTestNext(t, ch)
	}
	late, done := hub.Subscribe("a", 1)
	defer done()
	require.True(t, hubTestNext(t, late).Reset)
	hub.mu.Lock()
	require.LessOrEqual(t, len(hub.history), 256)
	hub.mu.Unlock()
}

func TestActiveChangeHubCoalescesControllerNoise(t *testing.T) {
	repo, hub := newHubTest(t)
	var stops []func()
	for i := 0; i < 100; i++ {
		ch, stop := hub.Subscribe("a", 1)
		hubTestNext(t, ch)
		stops = append(stops, stop)
	}
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	ch, stop := hub.Subscribe("a", 1)
	defer stop()
	hubTestNext(t, ch)
	repo.events <- state.ActiveChangeBatch{Revision: 5, Changes: []state.ActiveChange{{SandboxID: "other", RecordChanged: true}}}
	repo.events <- state.ActiveChangeBatch{Revision: 6, Changes: []state.ActiveChange{{SandboxID: "a", RecordChanged: true}}}
	got := hubTestNext(t, ch)
	require.EqualValues(t, 6, got.Revision)
	require.EqualValues(t, 1, repo.calls.Load())
}
