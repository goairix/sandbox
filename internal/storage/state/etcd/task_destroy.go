package etcd

import (
	"context"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type BeginDestroyInput struct {
	SandboxID, RequestID    string
	ExpectedControlRevision int64
}

// PrepareDestroy fixes a metadata-only destroy attempt against the exact active
// control revision. Preparing does not close admission; only a committed Stage
// establishes that transaction. Neither its Stage nor diagnostic TaskReference
// proves that the runtime gate is closed or permits physical cleanup.
func (b *Backend) PrepareDestroy(ctx context.Context, in BeginDestroyInput, ttl time.Duration) (*Stage, TaskReference, error) {
	if ctx == nil || b == nil || !validDomainSegment(in.SandboxID) || !validDomainSegment(in.RequestID) || in.ExpectedControlRevision <= 0 {
		return nil, TaskReference{}, ErrInvalidRecord
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return nil, TaskReference{}, ErrInvalidMutation
	}
	if err := ctx.Err(); err != nil {
		return nil, TaskReference{}, err
	}
	bundle, err := b.loadOperationControl(ctx, in.SandboxID)
	if err != nil {
		return nil, TaskReference{}, err
	}
	if bundle.KVs[1].ModRevision != in.ExpectedControlRevision {
		return nil, TaskReference{}, ErrConflict
	}
	// Expiry is retained, not used as a fresh admission clock: manual cleanup of
	// an expired active object must still bind its exact current runtime.
	ref := TaskReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, TaskID: uuid.NewString(), SandboxID: in.SandboxID, Partition: bundle.Partition}
	stage, err := b.beginStageWithBuilder(ctx, ref.Partition, in.RequestID, "destroy", ttl, func(attempt StageAttemptLocator) (Mutation, error) {
		return buildTaskDestroyMutation(b.namespace, bundle, ref, attempt)
	})
	return stage, ref, err
}

// Only PrepareDestroy supplies this already validated, coherently owned bundle.
// The native Stage copies the resulting mutation before any Lease allocation.
func buildTaskDestroyMutation(n Namespace, bundle *operationControlBundle, ref TaskReference, attempt StageAttemptLocator) (Mutation, error) {
	control := bundle.Control
	runtime := *control.Runtime
	control.Runtime = &runtime
	task := TaskRecord{Version: 1, Reference: ref, Kind: "cleanup", WorkspaceHash: control.WorkspaceHash, CreationIntentID: control.IntentID, Generation: control.Generation, DataGateEpoch: control.DataGateEpoch, ControlRevision: bundle.KVs[1].ModRevision, Runtime: runtime, Snapshot: control.Snapshot, ExpiresAt: control.ExpiresAt}
	control.Phase = PhaseDestroying
	controlValue, err := encodeDomainRecord(control)
	if err != nil {
		return Mutation{}, err
	}
	taskValue, err := encodeTaskRecord(task)
	if err != nil {
		return Mutation{}, err
	}
	intentValue, err := encodeCleanupIntentRecord(CleanupIntentRecord{Version: 1, Task: task, Attempt: attempt})
	if err != nil {
		return Mutation{}, err
	}
	linkValue, err := encodeTaskLinkRecord(TaskLinkRecord{Version: 1, Reference: ref})
	if err != nil {
		return Mutation{}, err
	}
	checkpointValue, err := encodeTaskCheckpointRecord(TaskCheckpointRecord{Version: 1, Reference: ref, State: TaskCheckpointPending, Attempt: attempt})
	if err != nil {
		return Mutation{}, err
	}
	mutation := Mutation{Writes: []Write{{Key: bundle.Keys[1], Value: []byte(controlValue)}}}
	for i, key := range bundle.Keys {
		kv := bundle.KVs[i]
		mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.Value(key), "=", string(kv.Value)), clientv3.Compare(clientv3.ModRevision(key), "=", kv.ModRevision), clientv3.Compare(clientv3.CreateRevision(key), "=", kv.CreateRevision), clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
	}
	for _, record := range []struct {
		key   func(TaskReference) (string, error)
		value string
	}{{n.taskKey, taskValue}, {n.cleanupIntentKey, intentValue}, {n.taskLinkKey, linkValue}, {n.taskCheckpointKey, checkpointValue}} {
		key, err := record.key(ref)
		if err != nil {
			return Mutation{}, err
		}
		mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
		mutation.Writes = append(mutation.Writes, Write{Key: key, Value: []byte(record.value)})
	}
	return mutation, nil
}
