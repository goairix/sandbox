package etcd

import (
	"fmt"
	"math"
	"time"
)

// This closed table names individual native variants, including recovery
// aliases. A completed traversal is dimensional evidence only. No caller can
// supply a different table, use an archive family, or retain a protobuf plan.
var cleanupDetachedWireManifest = [...]struct{ name, variants string }{
	{"source", "snapshot-placement creation-intent publication-initial publication-complete"},
	{"stage-begin", "generic-begin stage-grant stage-revoke"},
	{"stage-commit", "close-commit checkpoint-commit"},
	{"stage-resolve", "close-resolve checkpoint-resolve quiescence-resolve"},
	{"quiescence-stage", "reserved-begin reserved-commit"},
	{"task-checkpoint", "claim-create claim-post-fence checkpoint-mutation"},
	{"task-renew", "renew-before renew-after task-grant task-keepalive"},
	{"task-release", "task-revoke"},
	{"close", "issuer-read issuer-register close-pre-guard close-committed-guard close-intent-read close-stage-receipt-read"},
	{"close-query", "query-close-history query-close-stage-receipt query-close-issuer"},
	{"quiescence", "reservation-read reservation-write quiescence-pre-guard quiescence-committed-guard quiescence-intent-read.initial quiescence-intent-read.prior-initial quiescence-intent-read.prior-complete quiescence-receipt-read"},
	{"quiescence-query", "query-quiescence-history.initial query-quiescence-history.prior-initial query-quiescence-history.prior-complete query-quiescence-stage-receipt query-quiescence-issuer"},
	{"dual-drain", "operations-page empty-before empty-after"},
	{"final-sync-none", "final-sync-read final-sync-reserve"},
	{"execution", "point bundle observe claim-observe initialize rotate reserve progress claim claim-postcheck joined settle terminal-history"},
	{"detached-noeffect", "locate quiescence source arbitration write"},
	{"detached-recovery", "new-run arbitration"},
	{"termination", "authority-history-read.domain authority-history-read.publication-initial authority-history-read.publication-complete authority-history-read.none-locate authority-history-read.none-quiescence authority-history-read.none-source authority-history-read.supplement termination-read provider-read reserve reserved-guard terminated-write terminated-guard cleanup-complete-write cleanup-complete-guard"},
	{"provider", "provider-initial provider-before-termination provider-after-termination"},
	{"owner-release", "release-proof-read release-history-read.head-with-task release-history-read.head-paid-task release-history-read.retained13 release release-arbitration"},
}

type cleanupDetachedWireBuilder struct {
	b         *Backend
	in        cleanupDetachedWireInput
	w         cleanupLocalWireBuilder // Only common Task/close/Q dimensions are populated.
	execution cleanupExecutionRecord
	factKey   string
	roots     [84]string
	rootCount int
	report    cleanupDetachedWireReport
}

func (b *Backend) prepareDetachedWireBuilder(in cleanupDetachedWireInput, dimensions cleanupLocalWireDimensions) (*cleanupDetachedWireBuilder, error) {
	// Check the actual caller-owned fields before a validator/constructor can
	// hash JSON, clone a certificate, or copy a fence into client comparisons.
	// Native managed reads enforce these same individual point caps; that does
	// not excuse this private dimensional entry from enforcing its own boundary.
	if err := cleanupDetachedWireInputBounds(in); err != nil {
		return nil, err
	}
	if b == nil || b.detachedCleanup == nil || in.bundle == nil || in.bundle.checkpoint == nil || len(in.bundle.fences) != 8 || in.publication == nil || in.bundle.task.Validate() != nil || in.snapshot.Validate() != nil || in.intent.Validate() != nil || in.publication.Record.Validate() != nil || in.issuer.Record.Validate() != nil || in.claimTTL < 30*time.Second || in.claimTTL > 300*time.Second || in.claimTTL%time.Second != 0 || !validDomainSegment(in.workerID) || dimensions.worker.Validate() != nil || !validPreparationUUID(dimensions.runID) || !validPreparationUUID(dimensions.claimID) {
		return nil, ErrInvalidRecord
	}
	task := in.bundle.task
	if in.snapshot.Version != 1 || in.snapshot.LocalCleanup != nil || in.workspace.Kind() != WorkspaceIdentityDetached || in.workspace.Hash() != task.WorkspaceHash || in.workspace.Partition() != task.Reference.Partition || in.snapshot.Snapshot != task.Snapshot || in.snapshot.SandboxID != task.Reference.SandboxID || in.intent.IntentID != task.CreationIntentID || in.intent.WorkspaceHash != task.WorkspaceHash || in.publication.Record.WorkspaceHash != task.WorkspaceHash || in.publication.Record.Runtime != task.Runtime || in.publication.Record.Attempt.Namespace != b.namespace.Root() || in.issuer.Record.Namespace != b.namespace.Root() || in.issuer.Record.RestoreEpoch != b.restoreEpoch {
		return nil, ErrInvalidRecord
	}
	// Check the worst single-frame allocation before creating keys, padding,
	// client comparison byte copies, schema encodings or protobuf objects.
	if err := checkCleanupDetachedWireAllocation(cleanupDetachedWireAllocation{keyBytes: 64 << 10, valueBytes: 576 << 10, schemaBytes: 176128, nodes: 64, keys: 64}); err != nil {
		return nil, err
	}
	d := &cleanupDetachedWireBuilder{b: b, in: in}
	d.w = cleanupLocalWireBuilder{b: b, in: cleanupLocalWireInput{bundle: in.bundle, snapshot: in.snapshot, publication: in.publication, issuer: in.issuer, workerID: in.workerID, claimTTL: in.claimTTL}, dimensions: dimensions}
	if err := d.prepareCommon(); err != nil {
		return nil, err
	}
	if err := d.validateSchemas(); err != nil {
		return nil, err
	}
	return d, nil
}

// No archive, sync-plan, local descriptor, destination or TLS object is built.
// All future revisions retain maximal protobuf widths without being authority.
func (d *cleanupDetachedWireBuilder) prepareCommon() error {
	b, w, task := d.b, &d.w, d.in.bundle.task
	ref := task.Reference
	w.claim = TaskClaimReference{Task: ref, ClaimID: w.dimensions.claimID, WorkerID: d.in.workerID, CreateRevision: math.MaxInt64 - 20, LeaseID: math.MaxInt64}
	var err error
	w.claimKey, err = b.namespace.taskClaimKey(ref)
	if err != nil {
		return err
	}
	w.guardKey, err = b.namespace.taskGuardKey(ref, w.claim.ClaimID)
	if err != nil {
		return err
	}
	w.claimValue, err = encodeTaskClaimRecord(taskClaimRecord{Version: 1, Task: ref, ClaimID: w.claim.ClaimID, WorkerID: w.claim.WorkerID, LeaseID: w.claim.LeaseID})
	if err != nil {
		return err
	}
	w.issuerKey, err = b.namespace.commandIssuerKey(d.in.issuer.Record.CertificateID)
	if err != nil {
		return err
	}
	w.issuerValue, err = encodeCommandIssuerRecord(d.in.issuer.Record)
	if err != nil {
		return err
	}
	w.closeKey, err = b.namespace.taskCloseDataKey(ref)
	if err != nil {
		return err
	}
	w.quiescenceKey, err = b.namespace.taskQuiescenceKey(ref)
	if err != nil {
		return err
	}
	w.reservationKey, err = b.namespace.taskQuiescenceAttemptKey(ref)
	if err != nil {
		return err
	}
	w.finalKey, err = b.namespace.taskFinalSyncAttemptKey(ref)
	if err != nil {
		return err
	}
	w.terminationKey, w.providerKey, w.releaseKey, err = b.namespace.localRuntimeReleaseKeys(ref)
	if err != nil {
		return err
	}
	w.prefix, err = b.taskOperationsPrefix(ref)
	if err != nil {
		return err
	}
	d.factKey, err = b.namespace.taskDetachedNoEffectKey(ref)
	if err != nil {
		return err
	}
	for _, spec := range []struct {
		name string
		ref  *StageReference
		key  *string
	}{
		{"task_close_data", &w.closeStage, &w.closeReceiptKey},
		{"task_quiesce_users", &w.quiescenceStage, &w.quiescenceReceiptKey},
	} {
		*spec.ref = (StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: w.claim.ClaimID, StageID: spec.name, AttemptID: w.dimensions.runID}).reference(cleanupWirePaddingDigest())
		_, *spec.key, err = b.stageKeys(*spec.ref)
		if err != nil {
			return err
		}
	}
	r := cleanupExecutionRecord{Version: 1, Domain: cleanupExecutionDomain, Task: ref, TaskBirth: d.in.bundle.birth, InitialAttempt: d.in.bundle.initial, TaskDigest: workspaceSyncIntentDigest(d.in.bundle.fences[0].value), InitialMutationDigest: cleanupWirePaddingDigest(), InitialReceiptDigest: cleanupWirePaddingDigest(), Phase: "selecting", Confirmed: "queued"}
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	r.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(r))
	d.execution = r
	keys, err := d.sourceKeys()
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err = d.addRoot(key); err != nil {
			return err
		}
	}
	for _, f := range d.in.bundle.fences {
		if err = d.addRoot(f.key); err != nil {
			return err
		}
	}
	for _, key := range []string{string(d.in.bundle.checkpoint.Key), w.claimKey, w.guardKey, w.issuerKey, w.closeKey, w.closeReceiptKey, w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey, w.finalKey, w.prefix, w.terminationKey, w.providerKey, w.releaseKey, d.factKey} {
		if err = d.addRoot(key); err != nil {
			return err
		}
	}
	// Validate the actual retained native-key union before comparison builders
	// can copy any of the eight actual fence values.
	w.live = taskClaimWireComparisons(d.in.bundle.fences, w.claim, w.claimKey, w.guardKey, w.claimValue)
	w.close = taskCloseWireComparisons(w.live, w.issuerKey, w.issuerValue, math.MaxInt64-30)
	w.quiescence = cleanupWireAppend(w.close, cleanupWireFence(w.closeKey, 16384).comparisons()...)
	w.quiescence = append(w.quiescence, taskQuiescenceReservationPin{key: w.closeReceiptKey, revision: math.MaxInt64 - 12}.comparison())
	return nil
}

func (d *cleanupDetachedWireBuilder) addRoot(key string) error {
	for _, existing := range d.roots[:d.rootCount] {
		if key == existing {
			return nil
		}
	}
	if d.rootCount == len(d.roots) || len(key) > maxNamespaceKeyBytes {
		return ErrInvalidConfiguration
	}
	d.roots[d.rootCount] = key
	d.rootCount++
	return nil
}

func (d *cleanupDetachedWireBuilder) sourceKeys() ([]string, error) {
	b, task := d.b, d.in.bundle.task
	jk, pk, err := b.namespace.runtimePublicationKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	ek, ak, err := b.namespace.runtimeEnrollmentKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	receipt, err := b.runtimeDispatchReceiptKey(d.in.publication.Record.Attempt)
	if err != nil {
		return nil, err
	}
	history, err := b.enrolledHistoryKeys(task.Reference.Partition, task.CreationIntentID, d.in.publication.Record)
	if err != nil {
		return nil, err
	}
	_, snapshot, err := b.namespace.sandboxKeys(task.Reference.Partition, task.Reference.SandboxID, task.Snapshot.Version)
	if err != nil {
		return nil, err
	}
	placement, err := b.namespace.placementKey(task.Reference.SandboxID)
	if err != nil {
		return nil, err
	}
	intent, err := b.namespace.intentKey(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	return append([]string{b.identityKey, b.restoreKey, snapshot, placement, intent, jk, pk, ek, ak, receipt}, history...), nil
}

func (d *cleanupDetachedWireBuilder) emitVariant(family, variant string, emit func(cleanupDetachedWireEmission) error) error {
	// The same scalar bound precedes every variant's first materialization.
	if err := checkCleanupDetachedWireAllocation(cleanupDetachedWireAllocation{keyBytes: 64 << 10, valueBytes: 576 << 10, schemaBytes: 176128, nodes: 64, keys: 64}); err != nil {
		return err
	}
	if kind := cleanupLocalLeaseKind(family, variant); kind != "" {
		lease := cleanupDetachedLeaseProjection(variant, kind)
		if err := validateCleanupLeaseWire(lease); err != nil {
			return err
		}
		if err := emit(cleanupDetachedWireEmission{family: family, variant: variant, lease: &lease}); err != nil {
			return err
		}
		d.report.variants++
		return nil
	}
	projection, err := d.projection(family, variant)
	if err != nil {
		return fmt.Errorf("%w: detached wire %s/%s: %w", ErrInvalidConfiguration, family, variant, err)
	}
	return d.publishProjection(family, variant, projection, emit)
}

// The current projection is borrowed only during this call. Count its actual
// retained key union with source/bundle/common roots, not just source+nodes.
func (d *cleanupDetachedWireBuilder) publishProjection(family, variant string, p cleanupWireProjection, emit func(cleanupDetachedWireEmission) error) error {
	if err := validateCleanupWire(p); err != nil {
		return fmt.Errorf("%s/%s: %w", family, variant, err)
	}
	roots, count := d.roots, d.rootCount
	add := func(key []byte) error {
		for _, old := range roots[:count] {
			if old == string(key) {
				return nil
			}
		}
		if count == len(roots) {
			return ErrInvalidConfiguration
		}
		roots[count] = string(key)
		count++
		return nil
	}
	for _, cmp := range p.request.Compare {
		if err := add(cmp.Key); err != nil {
			return err
		}
	}
	if err := cleanupDetachedProjectionKeys(p, add); err != nil {
		return err
	}
	d.report.retainedKeys = max(d.report.retainedKeys, count)
	d.report.maxNodes = max(d.report.maxNodes, len(p.request.Compare)+len(p.request.Success)+len(p.request.Failure))
	d.report.requestBytes = max(d.report.requestBytes, p.request.Size())
	d.report.successBytes = max(d.report.successBytes, p.success.Size())
	d.report.failureBytes = max(d.report.failureBytes, p.failure.Size())
	p.name = variant
	if err := emit(cleanupDetachedWireEmission{family: family, variant: variant, txn: &p}); err != nil {
		return err
	}
	d.report.variants++
	return nil
}
