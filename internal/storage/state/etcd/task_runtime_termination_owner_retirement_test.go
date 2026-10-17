package etcd

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These are lifecycle-only holders, matching the existing PurposeOwners unit
// fixture. They grant no native E/S, source, provider or physical authority.
// Each begin below still creates its original real context/cancel/done owner.
func runtimeReleaseRetirementFixture(t *testing.T) (*Backend, *TaskClaim, *TaskClaim) {
	t.Helper()
	b := &Backend{archiveLife: &taskArchiveBackend{}, localRuntimeRelease: &LocalRuntimeReleaseOptions{MaxActive: 2, Timeout: 20 * time.Second, Provider: &k.TaskRuntimeCleanupClient{}}}
	seed := recoveryTestSeed(t)
	first := &TaskClaim{origin: b, parentCtx: context.Background(), reference: TaskClaimReference{Task: seed.Task, ClaimID: seed.ClaimID, CreateRevision: 2, LeaseID: 3}, deadline: time.Now().Add(time.Minute)}
	first.self = first
	task := seed.Task
	task.TaskID = uuid.NewString()
	second := &TaskClaim{origin: b, parentCtx: context.Background(), reference: TaskClaimReference{Task: task, ClaimID: uuid.NewString(), CreateRevision: 4, LeaseID: 5}, deadline: time.Now().Add(time.Minute)}
	second.self = second
	return b, first, second
}

func TestLocalRuntimeReleaseFinishedOwnerRetainsLatchWithoutContext(t *testing.T) {
	b, claim, _ := runtimeReleaseRetirementFixture(t)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	for _, purpose := range []string{"terminate", "release"} {
		owner, err := b.beginLocalRuntimeReleasePurpose(context.Background(), claim, purpose)
		require.NoError(t, err)
		t.Cleanup(func() {
			select {
			case <-owner.done:
			default:
				owner.finish()
			}
		})
		originalContext := owner.ctx
		require.NotNil(t, originalContext)
		owner.finish()
		require.ErrorIs(t, originalContext.Err(), context.Canceled)
		select {
		case <-owner.done:
		default:
			t.Fatal("finish did not publish its original join signal")
		}
		if purpose == "terminate" {
			require.Same(t, owner, claim.runtimeReleaseAttempt)
		} else {
			require.Same(t, owner, claim.ownerReleaseAttempt)
		}
		b.archiveLife.mu.Lock()
		retainedContext, retainedCancel := owner.ctx != nil, owner.cancel != nil
		active := b.archiveLife.runtimeReleases[owner.slot]
		b.archiveLife.mu.Unlock()
		require.Nil(t, active)
		require.False(t, retainedContext, "closed original owner must release its caller context graph")
		require.False(t, retainedCancel, "closed original owner must retain no executable cancel closure")
		_, err = b.beginLocalRuntimeReleasePurpose(context.Background(), claim, purpose)
		require.ErrorIs(t, err, ErrConflict, "retiring context cannot reopen this Claim's consumed purpose")
	}
}

// The first cancel blocks only after Close has copied the active owner set.
// Finish of slot one then retires its fields before Close resumes. Close must
// use its already copied cancel value and still wait for slot zero's real done;
// cancellation alone cannot substitute for that foreground owner's finish.
func TestLocalRuntimeReleaseCloseSnapshotSurvivesOtherOwnerFinish(t *testing.T) {
	b, firstClaim, secondClaim := runtimeReleaseRetirementFixture(t)
	first, err := b.beginLocalRuntimeReleasePurpose(context.Background(), firstClaim, "terminate")
	require.NoError(t, err)
	second, err := b.beginLocalRuntimeReleasePurpose(context.Background(), secondClaim, "release")
	require.NoError(t, err)
	require.Equal(t, 0, first.slot)
	require.Equal(t, 1, second.slot)
	firstContext, secondContext := first.ctx, second.ctx
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	var firstCancelEntered atomic.Bool
	secondCloseCancel := make(chan struct{})
	var secondCancelCalls atomic.Int64
	b.archiveLife.mu.Lock()
	firstCancel, secondCancel := first.cancel, second.cancel
	first.cancel = func() {
		if firstCancelEntered.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		firstCancel()
	}
	second.cancel = func() {
		if secondCancelCalls.Add(1) == 2 {
			close(secondCloseCancel)
		}
		secondCancel()
	}
	b.archiveLife.mu.Unlock()
	closed := make(chan struct{})
	var closeErr error
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				closeErr = fmt.Errorf("Close dereferenced a retired cancel: %v", failure)
			}
			close(closed)
		}()
		closeErr = b.Close()
	}()
	t.Cleanup(func() {
		unblock.Do(func() { close(release) })
		for _, owner := range []*taskLocalRuntimeReleaseOwner{first, second} {
			select {
			case <-owner.done:
			default:
				owner.finish()
			}
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Error("original Close goroutine failed to join after test cleanup")
		}
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Close did not reach the original slot-zero cancel")
	}
	second.finish()
	b.archiveLife.mu.Lock()
	secondContextRetained, secondCancelRetained := second.ctx != nil, second.cancel != nil
	b.archiveLife.mu.Unlock()
	unblock.Do(func() { close(release) })
	select {
	case <-secondCloseCancel:
	case <-closed:
		t.Fatalf("Close returned before invoking its copied slot-one cancel: %v", closeErr)
	case <-time.After(time.Second):
		t.Fatal("Close failed to invoke its copied slot-one cancel")
	}
	select {
	case <-closed:
		t.Fatal("Close confused cancellation with the unjoined slot-zero owner")
	default:
	}
	first.finish()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join both original foreground owners")
	}
	require.NoError(t, closeErr)
	require.ErrorIs(t, firstContext.Err(), context.Canceled)
	require.ErrorIs(t, secondContext.Err(), context.Canceled)
	require.EqualValues(t, 2, secondCancelCalls.Load(), "finish and Close each used an original idempotent cancel value")
	require.False(t, secondContextRetained, "slot one finished after Close's snapshot and must already release ctx")
	require.False(t, secondCancelRetained, "slot one finished after Close's snapshot and must already release cancel")
	require.Same(t, first, firstClaim.runtimeReleaseAttempt)
	require.Same(t, second, secondClaim.ownerReleaseAttempt)
}
