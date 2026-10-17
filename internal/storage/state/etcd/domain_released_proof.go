package etcd

import (
	"context"
	"errors"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Read only the three fixed proof profiles using the terminal reader's strict
// flat-envelope transport. The original physical loaders reuse the same pure
// validators with their existing transport and admission; no provider or
// issuer configuration is needed to authenticate this historical observation.
func (b *Backend) readReleasedProof(ctx context.Context, task TaskReference) (*localOwnerReleaseProof, error) {
	if b.enrolledVerifier == nil || b.enrolledPublication == nil || !validHexDigest(b.enrolledPublication.ContractDigest) {
		return nil, ErrInvalidConfiguration
	}
	tk, pk, rk, err := b.namespace.localRuntimeReleaseKeys(task)
	if err != nil {
		return nil, err
	}
	release, _, err := b.readReleasedPoints(ctx, releasedSeal, []string{rk}, nil)
	if err != nil {
		return nil, err
	}
	r, err := decodeLocalOwnerRelease(release[0])
	if err != nil || r.Task != task {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	head, _, err := b.readReleasedPoints(ctx, releasedTermination, []string{tk, pk}, nil)
	if err != nil {
		return nil, err
	}
	snapshot := &localRuntimeHistorySnapshot{termination: head[0], provider: head[1]}
	termination, err := b.decodeLocalRuntimeHistoryHead(task, snapshot)
	if err != nil {
		return nil, err
	}
	keys, err := b.localRuntimeHistoryKeys(termination, task)
	if err != nil {
		return nil, err
	}
	history, _, err := b.readReleasedPoints(ctx, releasedHistory, keys, nil)
	if err != nil {
		return nil, err
	}
	copy(snapshot.history[:], history)
	proof, err := b.decodeLocalOwnerReleaseProof(task, release[0], snapshot)
	if err != nil {
		return nil, err
	}
	proof.history = snapshot
	return proof, nil
}

func matchReleasedCohort(route *sandboxReleasedRoute, cohort *cleanupBirthCohort, proof *localOwnerReleaseProof) error {
	if cohort == nil || proof == nil {
		return ErrCorruptRecord
	}
	task, placement, termination := cohort.record, route.placement, proof.termination
	if task.Reference != route.link.Reference || task.WorkspaceHash != placement.WorkspaceHash || task.Reference.SandboxID != placement.SandboxID || task.Reference.Partition != placement.Partition || task.Reference.RestoreEpoch != placement.RestoreEpoch || task.CreationIntentID != placement.IntentID || task.Generation != placement.Generation {
		return ErrCorruptRecord
	}
	if proof.record.TaskBirth != cohort.birth || termination.TaskBirth != cohort.birth || termination.Authority.Task != localRuntimeFenceStamp(cohort.task) || termination.Authority.CleanupIntent != localRuntimeFenceStamp(cohort.intent) || termination.Authority.TaskLink != localRuntimeFenceStamp(cohort.link) || termination.Authority.Placement != localRuntimeStamp(route.placementKV) {
		return ErrCorruptRecord
	}
	return nil
}

// A permanent fence is never deleted by safe release. A higher generation can
// therefore coexist with the old signed death and release without requiring a
// recursive proof about the unknown number of successor lifetimes. That result
// says only that the old ID is released, never that this workspace is available.
func validateReleasedFence(route *sandboxReleasedRoute, proof *localOwnerReleaseProof) error {
	fence, signed := route.current[1], proof.termination.Authority.Fence
	if route.fence.Generation == route.placement.Generation {
		if route.owner != nil || localRuntimeStamp(fence) != signed {
			return ErrCorruptRecord
		}
		return nil
	}
	if route.fence.Generation < route.placement.Generation || fence.CreateRevision != signed.CreateRevision || fence.ModRevision <= proof.kv.CreateRevision {
		return ErrCorruptRecord
	}
	return nil
}

func (b *Backend) confirmSandboxReleased(ctx context.Context, route *sandboxReleasedRoute, cohort []*mvccpb.KeyValue, proof *localOwnerReleaseProof) (*SandboxReleasedReadResult, error) {
	if err := validateReleasedFence(route, proof); err != nil {
		return nil, err
	}
	retained := append([]*mvccpb.KeyValue{}, cohort...)
	retained = append(retained, proof.kv, proof.history.termination, proof.history.provider)
	retained = append(retained, proof.history.history[:]...)
	retained = append(retained, route.placementKV, route.current[1])
	if route.current[0] != nil {
		retained = append(retained, route.current[0])
	}
	pins, err := releasedPointPins(retained)
	if err != nil {
		return nil, err
	}
	index, err := b.namespace.runtimeIndexKey(proof.record.Runtime.UID)
	if err != nil {
		return nil, err
	}
	keys := []string{route.keys[2], route.keys[1], route.keys[0], route.keys[3], index}
	profile := releasedFinal
	var successor [4]string
	if route.owner != nil {
		successor, err = b.readViewKeys(route.owner.WorkspaceHash, route.owner.SandboxID)
		if err != nil {
			return nil, err
		}
		keys = append(keys, successor[2], successor[3])
		profile = releasedSuccessorFinal
	}
	points, revision, err := b.readReleasedPoints(ctx, profile, keys, pins)
	if err != nil {
		return nil, err
	}
	// Metadata was read together after every retained proof pin passed. The
	// exact old placement survives; its old control and runtime index must not.
	if points[0] == nil || localRuntimeStamp(points[0]) != proof.termination.Authority.Placement || points[1] == nil || points[3] != nil || points[4] != nil {
		return nil, ErrCorruptRecord
	}
	if route.owner == nil {
		if points[2] != nil {
			return nil, ErrCorruptRecord
		}
	} else {
		if _, err = b.decodeReadViewPoints(successor, []*mvccpb.KeyValue{points[2], points[1], points[5], points[6]}, revision); err != nil {
			return nil, err
		}
		// Owner/control may have later legitimate modifications. Their common
		// creation, along with placement, must still be the successor's fence
		// advance after the old release. Recreating just one point is rejected.
		birth := points[1].ModRevision
		for _, point := range []*mvccpb.KeyValue{points[2], points[5], points[6]} {
			if point.CreateRevision != birth || birth <= proof.kv.CreateRevision {
				return nil, ErrCorruptRecord
			}
		}
	}
	return &SandboxReleasedReadResult{SandboxID: route.placement.SandboxID, Task: proof.record.Task, Revision: revision}, nil
}
