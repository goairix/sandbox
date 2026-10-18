package etcd

import (
	"bytes"
	"context"
	"sync"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type releasedReadProfile uint8

const (
	releasedPlacement releasedReadProfile = iota
	releasedCurrent
	releasedIntent
	releasedCohort
	releasedSeal
	releasedTermination
	releasedHistory
	releasedFinal
	releasedSuccessorFinal

	// Detached profiles are independent transport dimensions. No existing
	// consumer selects them before the separate opaque-proof integration.
	releasedDetachedHistory
	releasedDetachedFinal
	releasedDetachedSuccessorFinal
)

// The original profiles retain their eighteen-pin allowance. Only the new
// detached final profiles can retain twenty-two native points; this dimension
// is checked before dispatch and grants no authority.
func releasedReadPinLimit(profile releasedReadProfile) int {
	switch profile {
	case releasedPlacement, releasedCurrent, releasedIntent, releasedCohort, releasedSeal, releasedTermination, releasedHistory, releasedFinal, releasedSuccessorFinal, releasedDetachedHistory:
		return 18
	case releasedDetachedFinal, releasedDetachedSuccessorFinal:
		return 22
	default:
		return 0
	}
}

// These are the complete finite profiles, in RPC order. The successor's extra
// two points belong to the same final transaction, never another discovery
// round. Caps include serialized JSON bytes, not just decoded payload lengths.
func releasedReadCaps(profile releasedReadProfile) []int {
	switch profile {
	case releasedPlacement, releasedIntent:
		return []int{4096}
	case releasedCurrent:
		return []int{4096, 1024, 4096, 2048}
	case releasedCohort:
		return []int{4096, 4096, 2048, 2048}
	case releasedSeal:
		return []int{8192}
	case releasedTermination:
		return []int{16384, 16384}
	case releasedHistory:
		return localRuntimeHistoryCaps()
	case releasedFinal:
		return []int{4096, 1024, 4096, 4096, 4096}
	case releasedSuccessorFinal:
		return []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}
	case releasedDetachedHistory:
		// Reuse the accepted thirteen-point allowance so routing and transport
		// cannot drift. The caps neither decode history nor prove release.
		return detachedRuntimeHistoryCaps()
	case releasedDetachedFinal:
		return []int{4096, 1024, 4096, 4096, 4096}
	case releasedDetachedSuccessorFinal:
		return []int{4096, 1024, 4096, 4096, 4096, 4096, 4096}
	default:
		return nil
	}
}

// This caches only the fixed protocol's dimension validation, never records,
// signed proofs or terminal conclusions. High-frequency active reads must not
// allocate all eight maximal reply projections on every request.
var releasedReadBudget = sync.OnceValue(checkReleasedReadBudget)

// The detached eight-RPC envelope has its own cached dimension check, never
// cached metadata or proof. The original v1 budget remains independent.
var releasedDetachedReadBudget = sync.OnceValue(checkReleasedDetachedReadBudget)

// Charge maximal keys/scalars and identity/restore on every reply, just as the
// original envelope does. Successor final dominates the alternative final in
// the same eighth RPC; neither history discovery nor a ninth RPC is added.
func checkReleasedDetachedReadBudget() error {
	profiles := []releasedReadProfile{
		releasedPlacement, releasedCurrent, releasedIntent, releasedCohort,
		releasedSeal, releasedTermination, releasedDetachedHistory, releasedDetachedSuccessorFinal,
	}
	total := 0
	for _, profile := range profiles {
		caps := releasedReadCaps(profile)
		if len(caps) == 0 {
			return ErrInvalidMutation
		}
		reply := cleanupBirthPointReply(append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, caps...))
		if reply.Size() > maxMutationBytes {
			return ErrInvalidMutation
		}
		total += reply.Size()
	}
	if total > 512*1024 {
		return ErrInvalidMutation
	}
	return nil
}

// Maximal 1024-byte keys, protobuf scalars and identity/restore bytes are
// charged for every reply, even when its successful branch omits metadata.
// This covers every prefix followed by a failed identity or conflict branch.
func checkReleasedReadBudget() error {
	profiles := []releasedReadProfile{releasedPlacement, releasedCurrent, releasedIntent, releasedCohort, releasedSeal, releasedTermination, releasedHistory, releasedSuccessorFinal}
	total := 0
	for _, profile := range profiles {
		caps := append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, releasedReadCaps(profile)...)
		reply := cleanupBirthPointReply(caps)
		if reply.Size() > maxMutationBytes {
			return ErrInvalidMutation
		}
		total += reply.Size()
	}
	if total > 512*1024 {
		return ErrInvalidMutation
	}
	return nil
}

func (b *Backend) readReleasedPoints(ctx context.Context, profile releasedReadProfile, keys []string, pins []clientv3.Cmp) ([]*mvccpb.KeyValue, int64, error) {
	caps := releasedReadCaps(profile)
	if len(caps) == 0 || len(keys) != len(caps) || len(pins) > releasedReadPinLimit(profile) {
		return nil, 0, ErrInvalidRecord
	}
	for _, key := range keys {
		if !validNamespaceKey(b.namespace, key) {
			return nil, 0, ErrInvalidRecord
		}
	}
	for _, pin := range pins {
		native := pb.Compare(pin)
		if pin.Target != pb.Compare_MOD || pin.Result != pb.Compare_EQUAL || len(pin.RangeEnd) != 0 || native.GetModRevision() <= 0 {
			return nil, 0, ErrInvalidRecord
		}
	}
	metadata := []string{b.identityKey, b.restoreKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(append(b.baseComparisons(), pins...)), Success: taskProtoPoints(keys), Failure: taskProtoPoints(metadata)}
	maximum := *request
	maximum.Compare = b.maximumCleanupBirthComparisons(request.Compare)
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request, &maximum}, []*pb.TxnResponse{cleanupBirthPointReply(caps), cleanupBirthPointReply([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes})}); err != nil {
		return nil, 0, err
	}
	switch profile {
	case releasedDetachedHistory, releasedDetachedFinal, releasedDetachedSuccessorFinal:
		// New profiles retain the stricter request budget independently of the
		// original v1 transport. Both actual and maximum identity/scalar forms
		// must fit before the original client is allowed to dispatch anything.
		if request.Size() > 128*1024 || maximum.Size() > 128*1024 {
			return nil, 0, ErrInvalidMutation
		}
		if err := releasedDetachedReadBudget(); err != nil {
			return nil, 0, err
		}
	}

	// readViewDispatch validates the flat response and every nested header
	// before protobuf Size. The global enrolled reader is intentionally not
	// used here: a recursive response must never reach generated Size methods.
	points, revision, err := b.readViewDispatch(ctx, keys, caps, pins)
	if err != nil {
		return nil, 0, err
	}
	for i, point := range points {
		if point != nil {
			owned := *point
			owned.Key, owned.Value = bytes.Clone(point.Key), bytes.Clone(point.Value)
			points[i] = &owned
		}
	}
	return points, revision, nil
}

// A point was already authenticated with its full envelope and canonical
// bytes. Its exact ModRevision detects rewriting, lease changes, deletion and
// recreation. Duplicate Task/history reads must agree, never replace a pin.
func releasedPointPins(points []*mvccpb.KeyValue) ([]clientv3.Cmp, error) {
	seen := make(map[string]int64)
	var pins []clientv3.Cmp
	for _, point := range points {
		if point == nil || point.ModRevision <= 0 {
			return nil, ErrCorruptRecord
		}
		key := string(point.Key)
		if previous, ok := seen[key]; ok {
			if previous != point.ModRevision {
				return nil, ErrConflict
			}
			continue
		}
		seen[key] = point.ModRevision
		pins = append(pins, clientv3.Compare(clientv3.ModRevision(key), "=", point.ModRevision))
	}
	return pins, nil
}
