package etcd

import (
	"context"
	"fmt"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// SandboxReleasedReadResult is a current authenticated observation that the
// named sandbox's original runtime and ownership were released. It grants no
// TaskClaim, physical operation, quota settlement or workspace availability
// authority, including when that workspace has a newer owner.
type SandboxReleasedReadResult struct {
	SandboxID string
	Task      TaskReference
	Revision  int64
}

// ReadSandboxReleased observes only retained metadata and signed termination
// history. A non-nil result proves this ID was released at the final read; nil
// means absent placement or a valid current sandbox and grants no terminal
// conclusion. Partial/corrupt state and concurrent replacement return errors.
// The operation makes at most eight bounded read-only RPCs, shares the snapshot
// reader's 256-call admission, and remains joined by Close until its client
// calls actually return. It never retries, scans, issues leases or caches proof.
func (b *Backend) ReadSandboxReleased(ctx context.Context, sandboxID string) (*SandboxReleasedReadResult, error) {
	if err := b.readViewInput(ctx); err != nil {
		return nil, err
	}
	placementKey, err := b.namespace.placementKey(sandboxID)
	if err != nil {
		return nil, err
	}
	if b.requestTimeout <= 0 || b.requestTimeout > 3*time.Second {
		return nil, ErrInvalidConfiguration
	}
	if err = releasedReadBudget(); err != nil {
		return nil, err
	}

	// Bound only this API's admitted lifetime. Original snapshot callers retain
	// their existing deadline behavior, and cancellation never substitutes for
	// the original client's actual return when Close joins active readers.
	bounded, cancel := context.WithTimeout(ctx, 8*b.requestTimeout+time.Second)
	defer cancel()
	call, err := b.registerSnapshotRead(bounded)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	result, err := b.readSandboxReleased(call.context, sandboxID, placementKey)
	if err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return nil, context.Canceled
	}
	if err := call.context.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type sandboxReleasedRoute struct {
	placement   SandboxPlacementRecord
	placementKV *mvccpb.KeyValue
	keys        [4]string
	current     []*mvccpb.KeyValue
	owner       *WorkspaceOwnerRecord
	fence       WorkspaceFenceRecord
	link        TaskLinkRecord
}

func (b *Backend) readSandboxReleased(ctx context.Context, sandboxID, placementKey string) (*SandboxReleasedReadResult, error) {
	points, _, err := b.readReleasedPoints(ctx, releasedPlacement, []string{placementKey}, nil)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(points[0], sandboxID)
	if err != nil || placement == nil {
		return nil, err
	}
	if err = readViewCanonical(points[0], placement); err != nil {
		return nil, err
	}
	route := &sandboxReleasedRoute{placement: *placement, placementKV: points[0]}
	route.keys, err = b.readViewKeys(placement.WorkspaceHash, sandboxID)
	if err != nil {
		return nil, err
	}
	linkKey, err := b.namespace.Key("p", fmt.Sprintf("%02x", placement.Partition), "sandboxes", sandboxID, "cleanup-task")
	if err != nil {
		return nil, err
	}
	pins, err := releasedPointPins([]*mvccpb.KeyValue{route.placementKV})
	if err != nil {
		return nil, err
	}
	current, revision, err := b.readReleasedPoints(ctx, releasedCurrent, []string{route.keys[0], route.keys[1], route.keys[3], linkKey}, pins)
	if err != nil {
		return nil, err
	}
	route.current = current
	active, err := b.decodeReleasedRoute(route, revision)
	if err != nil || active {
		return nil, err
	}
	cohort, cohortPoints, err := b.readReleasedCohort(ctx, route)
	if err != nil {
		return nil, err
	}
	proof, err := b.readReleasedProof(ctx, cohort.record.Reference)
	if err != nil {
		return nil, err
	}
	if err = matchReleasedCohort(route, cohort, proof); err != nil {
		return nil, err
	}
	return b.confirmSandboxReleased(ctx, route, cohortPoints, proof)
}

// Validate current records before deciding whether this is a terminal route.
// Only a complete old quartet can return the nonterminal observation. A missing
// old control with an old owner is corruption; a higher-generation owner is
// validated separately and never followed to select another old history.
func (b *Backend) decodeReleasedRoute(route *sandboxReleasedRoute, revision int64) (bool, error) {
	points, placement := route.current, route.placement
	if points[1] == nil || decodeDomainRecord(points[1], &route.fence) != nil || readViewCanonical(points[1], &route.fence) != nil {
		return false, ErrCorruptRecord
	}
	if err := b.domainEpoch(route.fence.RestoreEpoch); err != nil {
		return false, err
	}
	if route.fence.WorkspaceHash != placement.WorkspaceHash || route.fence.Generation < placement.Generation {
		return false, ErrCorruptRecord
	}
	if points[0] != nil {
		route.owner = new(WorkspaceOwnerRecord)
		if err := decodeDomainRecord(points[0], route.owner); err != nil {
			return false, err
		}
		if err := readViewCanonical(points[0], route.owner); err != nil {
			return false, err
		}
		if err := b.domainEpoch(route.owner.RestoreEpoch); err != nil {
			return false, err
		}
		if route.owner.WorkspaceHash != placement.WorkspaceHash || route.owner.Generation != route.fence.Generation {
			return false, ErrCorruptRecord
		}
	}
	if points[3] != nil {
		if !enrolledImmutableKV(points[3]) || decodeTaskLinkRecord(points[3], &route.link) != nil || b.validateTaskReference(route.link.Reference) != nil || route.link.Reference.SandboxID != placement.SandboxID || route.link.Reference.Partition != placement.Partition {
			return false, ErrCorruptRecord
		}
		wire, err := encodeTaskLinkRecord(route.link)
		if err != nil || wire != string(points[3].Value) {
			return false, ErrCorruptRecord
		}
	}
	if points[2] != nil {
		_, err := b.decodeReadViewPoints(route.keys, []*mvccpb.KeyValue{points[0], points[1], route.placementKV, points[2]}, revision)
		return err == nil, err
	}
	if points[3] == nil {
		return false, ErrCorruptRecord
	}
	if route.owner != nil && (route.owner.SandboxID == placement.SandboxID || route.owner.Generation <= placement.Generation) {
		return false, ErrCorruptRecord
	}
	return false, nil
}

func (b *Backend) readReleasedCohort(ctx context.Context, route *sandboxReleasedRoute) (*cleanupBirthCohort, []*mvccpb.KeyValue, error) {
	ref := route.link.Reference
	key, err := b.namespace.cleanupIntentKey(ref)
	if err != nil {
		return nil, nil, err
	}
	points, _, err := b.readReleasedPoints(ctx, releasedIntent, []string{key}, nil)
	if err != nil {
		return nil, nil, err
	}
	routing, err := b.decodeCleanupBirthRoute(ref.Partition, ref.TaskID, points[0])
	if err != nil || routing.Task.Reference != ref {
		return nil, nil, ErrCorruptRecord
	}
	keys, _, err := b.cleanupBirthCohortKeys(ref, routing.Attempt)
	if err != nil {
		return nil, nil, err
	}
	pins, err := releasedPointPins([]*mvccpb.KeyValue{points[0], route.current[3]})
	if err != nil {
		return nil, nil, err
	}
	cohortPoints, revision, err := b.readReleasedPoints(ctx, releasedCohort, keys[:4], pins)
	if err != nil {
		return nil, nil, err
	}
	cohort, err := b.cleanupBirthCohortFromPoints(points[0], cohortPoints, revision)
	return cohort, cohortPoints, err
}
