package etcd

import (
	"context"
	"crypto/sha256"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

type cleanupManagedSourcePin struct {
	key                        string
	birth, mod, version, lease int64
	digest                     [32]byte
}

func managedSourcePin(kv *mvccpb.KeyValue) cleanupManagedSourcePin {
	return cleanupManagedSourcePin{key: strings.Clone(string(kv.Key)), birth: kv.CreateRevision, mod: kv.ModRevision, version: kv.Version, lease: kv.Lease, digest: sha256.Sum256(kv.Value)}
}

// cleanupManagedSource verifies actual bounded source points on the original
// paid actor. Its ordinary identity and compact observations grant no managed
// Claim, runtime send, slot or mutation authority. No TLS constructor runs here.
func (b *Backend) cleanupManagedSource(ctx context.Context, j *cleanupLocalJob) (bool, error) {
	s := j.worker.managedSource
	if s == nil || s.job != j || j.owner == nil || j.bundle == nil || j.bundle.task.Reference != j.task {
		return false, ErrInvalidRecord
	}
	task := j.bundle.task
	keys, err := b.snapshotReadKeys(snapshotCacheKey{partition: task.Reference.Partition, sandboxID: task.Reference.SandboxID, version: task.Snapshot.Version, digest: task.Snapshot.Digest})
	if err != nil {
		return false, err
	}
	points, err := b.readSnapshotPoints(ctx, keys)
	if err != nil {
		return false, err
	}
	placement, err := b.decodePlacement(points[1], task.Reference.SandboxID)
	if err != nil {
		return false, err
	}
	snapshot, err := decodeSnapshot(points[0], task.Reference.Partition, task.Reference.SandboxID, task.Snapshot, placement)
	if err != nil || snapshot == nil {
		return false, ErrCorruptRecord
	}
	if snapshot.Version == 2 && snapshot.LocalCleanup != nil {
		// The original local descriptor still selects the original local path.
		// That path retains all of its existing physical profile checks.
		return false, nil
	}
	if snapshot.Version != 1 || snapshot.LocalCleanup != nil || points[0].Lease != 0 {
		return false, ErrCorruptRecord
	}
	s.pins[0] = managedSourcePin(points[0])
	key, err := b.namespace.intentKey(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return false, err
	}
	intents, err := b.readEnrolledPoints(ctx, []string{key}, []int{4096})
	if err != nil {
		return false, err
	}
	var intent CreationIntentRecord
	if intents[0] == nil || intents[0].Lease != 0 || decodeDomainRecord(intents[0], &intent) != nil {
		return false, ErrCorruptRecord
	}
	canonical, err := encodeDomainRecord(&intent)
	if err != nil || canonical != string(intents[0].Value) {
		return false, ErrCorruptRecord
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return false, err
	}
	workspace, err := detachedIdentityFromRequestHash(identity.StorageID, intent.RequestHash)
	if err != nil {
		return false, err
	}
	if _, err = b.decodeCreationIntent(intents[0], workspace, task.CreationIntentID); err != nil {
		return false, err
	}
	s.pins[1] = managedSourcePin(intents[0])
	publication, err := b.readRuntimePublicationEnrolledAt(ctx, historyLocator(workspace), task.CreationIntentID, func(values []*mvccpb.KeyValue) {
		for i, kv := range values {
			s.pins[i+2] = managedSourcePin(kv)
		}
		s.count = 16
	})
	if err != nil {
		return false, err
	}
	if publication == nil || publication.Publication == nil {
		return false, ErrCorruptRecord
	}
	if _, err = b.detachedCleanupSource(task, *snapshot, intent, publication.Publication); err != nil {
		return false, err
	}
	// All native source authentication ran, but the complete detached wire
	// manifest and physical phase have not been admitted. This diagnostic must
	// never be interpreted as a capability by a later job or consumer.
	j.result.Reason = "detached-source-verified"
	return true, ErrInvalidConfiguration
}
