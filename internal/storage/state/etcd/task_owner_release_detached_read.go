package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type taskOwnerReleaseProfile uint8

const (
	taskOwnerReleaseLocal taskOwnerReleaseProfile = iota + 1
	taskOwnerReleaseDetached
)

// Only a complete actual native history can produce this private detached
// proof. Its historical sender is not today's live Claim or release authority.
type detachedOwnerReleaseProof struct {
	record      detachedOwnerReleaseRecord
	kv          *mvccpb.KeyValue
	termination p.TaskRuntimeTerminationIntent
	history     *detachedRuntimeHistorySnapshot
}

// Exactly one branch may be present. The local branch retains its original
// strict v1 codec and nine-point history, without synthetic archive fields.
type taskOwnerReleaseProof struct {
	local    *localOwnerReleaseProof
	detached *detachedOwnerReleaseProof
}

// The discriminator only routes complete strict codecs. It grants no authority
// and cannot turn a foreign task, unsupported version or mixed tuple into proof.
func decodeTaskOwnerReleaseProfile(task TaskReference, seal *mvccpb.KeyValue) (taskOwnerReleaseProfile, error) {
	if task.Validate() != nil || !enrolledImmutableKV(seal) || len(seal.Value) == 0 || len(seal.Value) > detachedOwnerReleaseBytes {
		return 0, ErrCorruptRecord
	}
	key := task.Namespace + fmt.Sprintf("p/%02x/tasks/%s/owner-release", task.Partition, task.TaskID)
	if string(seal.Key) != key {
		return 0, ErrCorruptRecord
	}
	var version struct {
		Version uint32 `json:"version"`
	}
	if json.Unmarshal(seal.Value, &version) != nil {
		return 0, ErrCorruptRecord
	}
	switch version.Version {
	case 1:
		r, err := decodeLocalOwnerRelease(seal)
		if err != nil || r.Task != task {
			return 0, errors.Join(ErrCorruptRecord, err)
		}
		return taskOwnerReleaseLocal, nil
	case 2:
		r, err := decodeDetachedOwnerRelease(seal)
		if err != nil || r.Task != task {
			return 0, errors.Join(ErrCorruptRecord, err)
		}
		return taskOwnerReleaseDetached, nil
	default:
		return 0, ErrCorruptRecord
	}
}

// Paid readers supply their original actual Task point and fixed native read
// set here. This pure join has no hidden RPCs or effect dependencies. It copies
// the bounded points only after checking profile dimensions; a returned proof
// owns its bytes and preserves every companion for later native pinning.
func (b *Backend) decodeTaskOwnerReleaseProof(task TaskReference, actualTask, seal, termination, provider *mvccpb.KeyValue, history []*mvccpb.KeyValue) (*taskOwnerReleaseProof, error) {
	if b == nil || b.validateTaskReference(task) != nil {
		return nil, ErrInvalidRecord
	}
	profile, err := decodeTaskOwnerReleaseProfile(task, seal)
	if err != nil {
		return nil, err
	}
	caps := localRuntimeHistoryCaps()
	if profile == taskOwnerReleaseDetached {
		caps = detachedRuntimeHistoryCaps()
	}
	if len(history) != len(caps) || termination == nil || len(termination.Value) > 16384 || provider == nil || len(provider.Value) > 16384 {
		return nil, ErrCorruptRecord
	}
	for i, point := range history {
		if point == nil || len(point.Value) == 0 || len(point.Value) > caps[i] {
			return nil, ErrCorruptRecord
		}
	}
	if actualTask != nil && (actualTask.Version != history[0].Version || !ownTaskFence(actualTask).matches(history[0])) {
		return nil, ErrCorruptRecord
	}
	if profile == taskOwnerReleaseLocal {
		snapshot := &localRuntimeHistorySnapshot{termination: cloneOwnerReleasePoint(termination), provider: cloneOwnerReleasePoint(provider)}
		for i, point := range history {
			snapshot.history[i] = cloneOwnerReleasePoint(point)
		}
		local, err := b.decodeLocalOwnerReleaseProof(task, cloneOwnerReleasePoint(seal), snapshot)
		if err != nil {
			return nil, err
		}
		local.history = snapshot
		return &taskOwnerReleaseProof{local: local}, nil
	}
	if actualTask == nil {
		return nil, ErrCorruptRecord
	}
	snapshot := &detachedRuntimeHistorySnapshot{termination: cloneOwnerReleasePoint(termination), provider: cloneOwnerReleasePoint(provider)}
	for i, point := range history {
		snapshot.history[i] = cloneOwnerReleasePoint(point)
	}
	r, err := b.decodeDetachedRuntimeHistory(task, actualTask, snapshot)
	if err != nil {
		return nil, err
	}
	record, err := decodeDetachedOwnerRelease(seal)
	if err != nil {
		return nil, err
	}
	if err = matchDetachedOwnerReleaseHistory(record, snapshot.termination); err != nil {
		return nil, err
	}
	return &taskOwnerReleaseProof{detached: &detachedOwnerReleaseProof{record: record, kv: cloneOwnerReleasePoint(seal), termination: r, history: snapshot}}, nil
}

func cloneOwnerReleasePoint(point *mvccpb.KeyValue) *mvccpb.KeyValue {
	owned := *point
	owned.Key, owned.Value = bytes.Clone(point.Key), bytes.Clone(point.Value)
	return &owned
}

// The caller owns an original bounded runtime owner/read slot. Standalone v2
// locates Task alongside the original two-point head, whereas a paid caller
// can reuse its actual cohort Task without an extra RPC. Neither path reads
// deleted owner/control/index or current E/S. Absent seal remains independent
// absence, never permission to resend a previously attempted native release.
func (b *Backend) readTaskOwnerReleaseProof(ctx context.Context, task TaskReference, actualTask *mvccpb.KeyValue) (*taskOwnerReleaseProof, error) {
	if b == nil || ctx == nil || b.validateTaskReference(task) != nil {
		return nil, ErrInvalidRecord
	}
	tk, pk, rk, err := b.namespace.localRuntimeReleaseKeys(task)
	if err != nil {
		return nil, err
	}
	seal, err := b.readTaskOwnerReleasePoints(ctx, []string{rk}, []int{detachedOwnerReleaseBytes})
	if err != nil || seal[0] == nil {
		return nil, err
	}
	profile, err := decodeTaskOwnerReleaseProfile(task, seal[0])
	if err != nil {
		return nil, err
	}
	headKeys, headCaps := []string{tk, pk}, []int{16384, 16384}
	if profile == taskOwnerReleaseDetached && actualTask == nil {
		key, err := b.namespace.taskKey(task)
		if err != nil {
			return nil, err
		}
		headKeys, headCaps = append(headKeys, key), append(headCaps, 4096)
	}
	head, err := b.readTaskOwnerReleasePoints(ctx, headKeys, headCaps)
	if err != nil {
		return nil, err
	}
	var intent p.TaskRuntimeTerminationIntent
	if profile == taskOwnerReleaseLocal {
		intent, err = b.decodeLocalRuntimeHistoryHead(task, &localRuntimeHistorySnapshot{termination: head[0], provider: head[1]})
		if err == nil && intent.Version != 1 {
			err = ErrCorruptRecord
		}
	} else {
		intent, err = b.decodeDetachedRuntimeHistoryHead(task, &detachedRuntimeHistorySnapshot{termination: head[0], provider: head[1]})
		if actualTask == nil {
			actualTask = head[2]
		}
	}
	if err != nil {
		return nil, err
	}
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(intent, actualTask)
	if err != nil {
		return nil, err
	}
	history, err := b.readTaskOwnerReleasePoints(ctx, keys, caps)
	if err != nil {
		return nil, err
	}
	return b.decodeTaskOwnerReleaseProof(task, actualTask, seal[0], head[0], head[1], history)
}

// The same flat bounded transport serves each finite read. Its13-point native
// history projection includes identity/restore in both branches and preserves
// the consumer's128KiB request /256KiB response budget, not just codec caps.
func (b *Backend) readTaskOwnerReleasePoints(ctx context.Context, keys []string, caps []int) ([]*mvccpb.KeyValue, error) {
	if len(keys) == 0 || len(keys) != len(caps) || len(keys) > 13 {
		return nil, ErrInvalidRecord
	}
	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	values := []string{b.identityValue, b.restoreEpoch}
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) || caps[i] <= 0 || caps[i] > maxRecordBytes {
			return nil, ErrInvalidRecord
		}
		values = append(values, cleanupWirePadding(caps[i]))
	}
	reads := taskProtoPoints(all)
	response := taskProtoResponse(all, values)
	projection := cleanupWireProjection{name: "owner-release-history", request: &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: reads, Failure: reads}, success: response, failure: response}
	if err := validateCleanupWire(projection); err != nil {
		return nil, err
	}
	return b.readEnrolledPoints(ctx, keys, caps)
}
