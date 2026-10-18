package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Read only the three fixed proof profiles using the terminal reader's strict
// flat-envelope transport. The original physical loaders reuse the same pure
// validators with their existing transport and admission; no provider or
// issuer configuration is needed to authenticate this historical observation.
// The original cohort Task derives detached snapshot routing without another
// RPC. B's pure final join requires it to equal the repeated history Task.
func (b *Backend) readReleasedProof(ctx context.Context, task TaskReference, actualTask *mvccpb.KeyValue) (*taskOwnerReleaseProof, error) {
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
	variant, err := decodeTaskOwnerReleaseProfile(task, release[0])
	if err != nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	head, _, err := b.readReleasedPoints(ctx, releasedTermination, []string{tk, pk}, nil)
	if err != nil {
		return nil, err
	}
	var termination p.TaskRuntimeTerminationIntent
	historyProfile := releasedHistory
	switch variant {
	case taskOwnerReleaseLocal:
		termination, err = b.decodeLocalRuntimeHistoryHead(task, &localRuntimeHistorySnapshot{termination: head[0], provider: head[1]})
		if err == nil && termination.Version != 1 {
			err = ErrCorruptRecord
		}
	case taskOwnerReleaseDetached:
		termination, err = b.decodeDetachedRuntimeHistoryHead(task, &detachedRuntimeHistorySnapshot{termination: head[0], provider: head[1]})
		historyProfile = releasedDetachedHistory
	default:
		return nil, ErrCorruptRecord
	}
	if err != nil {
		return nil, err
	}
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(termination, actualTask)
	if err != nil {
		return nil, err
	}
	// Routing and the preflighted transport must agree on the entire finite
	// allowance. A discriminator selects reads only, never terminal permission.
	transportCaps := releasedReadCaps(historyProfile)
	if len(caps) != len(transportCaps) {
		return nil, ErrCorruptRecord
	}
	for i, cap := range caps {
		if cap != transportCaps[i] {
			return nil, ErrCorruptRecord
		}
	}
	history, _, err := b.readReleasedPoints(ctx, historyProfile, keys, nil)
	if err != nil {
		return nil, err
	}
	return b.decodeTaskOwnerReleaseProof(task, actualTask, release[0], head[0], head[1], history)
}

// This temporary observation borrows only authenticated historical data for
// cohort matching and final native pins. It is not an effect capability or a
// retained worker cache, and never converts a detached seal into a local seal.
type releasedTerminalObservation struct {
	task      TaskReference
	taskBirth int64
	runtime   RuntimeReference
	detached  bool

	termination p.TaskRuntimeTerminationIntent
	kv          *mvccpb.KeyValue
	history     []*mvccpb.KeyValue
}

// Exactly one fully decoded private branch supplies these borrowed facts.
// Every head/history point remains present for final native pinning; the
// observation adds no permission beyond the caller's authenticated proof.
func releasedTerminalObservationFor(proof *taskOwnerReleaseProof) (*releasedTerminalObservation, error) {
	if proof == nil || (proof.local == nil) == (proof.detached == nil) {
		return nil, ErrCorruptRecord
	}
	var observation releasedTerminalObservation
	if local := proof.local; local != nil {
		if local.record.Version != 1 || local.termination.Version != 1 || local.kv == nil || local.history == nil {
			return nil, ErrCorruptRecord
		}
		observation = releasedTerminalObservation{
			task: local.record.Task, taskBirth: local.record.TaskBirth, runtime: local.record.Runtime,
			termination: local.termination, kv: local.kv,
			history: append([]*mvccpb.KeyValue{local.history.termination, local.history.provider}, local.history.history[:]...),
		}
	} else {
		detached := proof.detached
		if detached.record.Version != 2 || detached.termination.Version != 2 || detached.termination.DetachedEffects == nil || detached.kv == nil || detached.history == nil {
			return nil, ErrCorruptRecord
		}
		observation = releasedTerminalObservation{
			task: detached.record.Task, taskBirth: detached.record.TaskBirth, runtime: detached.record.Runtime, detached: true,
			termination: detached.termination, kv: detached.kv,
			history: append([]*mvccpb.KeyValue{detached.history.termination, detached.history.provider}, detached.history.history[:]...),
		}
	}
	for _, point := range observation.history {
		if point == nil {
			return nil, ErrCorruptRecord
		}
	}
	return &observation, nil
}

func matchReleasedCohort(route *sandboxReleasedRoute, cohort *cleanupBirthCohort, ownerProof *taskOwnerReleaseProof) error {
	proof, err := releasedTerminalObservationFor(ownerProof)
	if err != nil || route == nil || cohort == nil {
		return ErrCorruptRecord
	}
	task, placement, termination := cohort.record, route.placement, proof.termination
	if task.Reference != route.link.Reference || task.WorkspaceHash != placement.WorkspaceHash || task.Reference.SandboxID != placement.SandboxID || task.Reference.Partition != placement.Partition || task.Reference.RestoreEpoch != placement.RestoreEpoch || task.CreationIntentID != placement.IntentID || task.Generation != placement.Generation {
		return ErrCorruptRecord
	}
	if proof.taskBirth != cohort.birth || termination.TaskBirth != cohort.birth || termination.Authority.Task != localRuntimeFenceStamp(cohort.task) || termination.Authority.CleanupIntent != localRuntimeFenceStamp(cohort.intent) || termination.Authority.TaskLink != localRuntimeFenceStamp(cohort.link) || termination.Authority.Placement != localRuntimeStamp(route.placementKV) {
		return ErrCorruptRecord
	}
	return nil
}

// A permanent fence is never deleted by safe release. A higher generation can
// therefore coexist with the old signed death and release without requiring a
// recursive proof about the unknown number of successor lifetimes. That result
// says only that the old ID is released, never that this workspace is available.
func validateReleasedFence(route *sandboxReleasedRoute, ownerProof *taskOwnerReleaseProof) error {
	proof, err := releasedTerminalObservationFor(ownerProof)
	if err != nil || route == nil {
		return ErrCorruptRecord
	}
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

func (b *Backend) confirmSandboxReleased(ctx context.Context, route *sandboxReleasedRoute, cohort []*mvccpb.KeyValue, ownerProof *taskOwnerReleaseProof) (*SandboxReleasedReadResult, error) {
	if err := validateReleasedFence(route, ownerProof); err != nil {
		return nil, err
	}
	proof, err := releasedTerminalObservationFor(ownerProof)
	if err != nil {
		return nil, err
	}
	retained := append([]*mvccpb.KeyValue{}, cohort...)
	retained = append(retained, proof.kv)
	retained = append(retained, proof.history...)
	retained = append(retained, route.placementKV, route.current[1])
	if route.current[0] != nil {
		retained = append(retained, route.current[0])
	}
	pins, err := releasedPointPins(retained)
	if err != nil {
		return nil, err
	}
	index, err := b.namespace.runtimeIndexKey(proof.runtime.UID)
	if err != nil {
		return nil, err
	}
	keys := []string{route.keys[2], route.keys[1], route.keys[0], route.keys[3], index}
	profile := releasedFinal
	if proof.detached {
		profile = releasedDetachedFinal
	}
	var successor [4]string
	if route.owner != nil {
		successor, err = b.readViewKeys(route.owner.WorkspaceHash, route.owner.SandboxID)
		if err != nil {
			return nil, err
		}
		keys = append(keys, successor[2], successor[3])
		profile = releasedSuccessorFinal
		if proof.detached {
			profile = releasedDetachedSuccessorFinal
		}
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
	return &SandboxReleasedReadResult{SandboxID: route.placement.SandboxID, Task: proof.task, Revision: revision}, nil
}
