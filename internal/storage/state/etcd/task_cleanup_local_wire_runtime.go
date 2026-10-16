package etcd

import (
	"fmt"
	"math"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Context construction is dimensional only. The original signer and verifier
// remain mandatory; these prospective records cannot be passed as evidence.
func (w *cleanupLocalWireBuilder) closeContext() p.TaskCloseDataContext {
	task, ref := w.in.bundle.task, w.claim.Task
	return p.TaskCloseDataContext{Namespace: ref.Namespace, AuthorityID: w.b.publicationAuthorityID, Target: w.b.publicationTarget, RestoreEpoch: ref.RestoreEpoch, IssuerCertificateID: w.in.issuer.Record.CertificateID, IssuerCertificateDigest: w.in.issuer.Record.CertificateDigest, CommandID: w.in.state.record.Run.ID, TaskID: ref.TaskID, TaskDigest: workspaceSyncIntentDigest(w.in.bundle.fences[0].value), ClaimID: w.claim.ClaimID, WorkerID: w.claim.WorkerID, SandboxID: ref.SandboxID, WorkspaceHash: task.WorkspaceHash, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, ControlRevision: w.in.bundle.birth, ClaimCreateRevision: w.claim.CreateRevision, LeaseID: w.claim.LeaseID, Runtime: p.RuntimeReference(task.Runtime)}
}

func cleanupWireKV(key string, size int) *mvccpb.KeyValue {
	return &mvccpb.KeyValue{Key: []byte(key), Value: make([]byte, size), CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64, Version: math.MaxInt64 - 1}
}

func (w *cleanupLocalWireBuilder) runtimeRequests() error {
	b, task := w.b, w.in.bundle.task
	ek, ak, err := b.namespace.runtimeEnrollmentKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return err
	}
	pins := taskLocalRuntimeWirePins{comparisons: w.archive, prefix: w.prefix, complete: cleanupWireFence(w.completeKey, 8192), enrollment: [2]taskFence{cleanupWireFence(ek, 8192), cleanupWireFence(ak, 8192)}, terminationKey: w.terminationKey, providerKey: w.providerKey}
	termination, provider := cleanupWireKV(w.terminationKey, 16384), cleanupWireKV(w.providerKey, 16384)
	r := w.terminationSchema()
	if _, err := taskRuntimeTerminationWireProjections(pins, r, provider); err != nil {
		return err
	}
	providerRecord := taskRuntimeProviderCleanupRecord{Version: 1, Task: task.Reference, TaskBirth: w.in.bundle.birth, Binding: localRuntimeProviderBinding(r, b.enrolledPublication.ContractDigest), Descriptor: []byte("{}")}
	for i, step := range taskRuntimeCleanupSteps {
		providerRecord.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "unstarted"}
	}
	writer := taskCloseClaimWire{w.claim.Task, w.claim.ClaimID, w.claim.WorkerID, w.claim.CreateRevision, w.claim.LeaseID}
	if _, err := taskRuntimeProviderWireProjections(pins, providerRecord, writer, provider, termination); err != nil {
		return err
	}
	// Whole encoded record caps cover all legal future proofs, descriptor
	// base64 and JSON escaping; no cap bytes enter a codec or physical API.
	w.add("termination", "reserve", taskRuntimeWriteProjection(pins.terminationReserveComparisons(provider), w.terminationKey, string(termination.Value)))
	for _, phase := range []string{"reserved", "terminated", "cleanup-complete"} {
		cmps := pins.terminationComparisons(termination, provider)
		w.add("termination", phase+"-guard", w.guard(cmps, true))
		if phase != "reserved" {
			w.add("termination", phase+"-write", taskRuntimeWriteProjection(cmps, w.terminationKey, string(termination.Value)))
		}
	}
	before := pins.providerComparisons(provider, nil)
	after := pins.providerComparisons(provider, termination)
	w.add("provider", "provider-initial", taskRuntimeWriteProjection(pins.providerComparisons(nil, nil), w.providerKey, string(provider.Value)))
	w.add("provider", "provider-before-termination", taskRuntimeWriteProjection(before, w.providerKey, string(provider.Value)))
	w.add("provider", "provider-after-termination", taskRuntimeWriteProjection(after, w.providerKey, string(provider.Value)))
	for step := range taskRuntimeCleanupSteps {
		cmps := after
		if step == 0 {
			cmps = before
		}
		write := taskRuntimeWriteProjection(cmps, w.providerKey, string(provider.Value))
		for _, phase := range []string{"intent", "unknown", "complete"} {
			// Unknown retains the already written intent. This same-byte alias
			// does not claim a new unknown write or authorize a resend.
			w.add("provider", fmt.Sprintf("step-%d-%s", step, phase), write)
		}
		w.add("provider", fmt.Sprintf("step-%d-guard", step), w.guard(cmps, true))
		if step >= 2 && step <= 5 {
			w.add("provider", fmt.Sprintf("step-%d-skipped", step), write)
		}
	}
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", make([]byte, 4096), cleanupWirePaddingDigest()
	released, err := newLocalOwnerReleaseRecord(r, math.MaxInt64-2, math.MaxInt64-1, writer)
	if err != nil {
		return err
	}
	if _, err := encodeLocalOwnerRelease(released); err != nil {
		return err
	}
	deleted := [3]string{released.Deleted[0].Key, released.Deleted[1].Key, released.Deleted[2].Key}
	cmps := cleanupWireAppend(pins.terminationComparisons(termination, nil), clientv3.Compare(clientv3.Version(w.releaseKey), "=", 0))
	_, request, response, err := taskOwnerReleaseWireProjection(b.namespace, cmps, w.releaseKey, deleted, string(make([]byte, 8192)))
	if err != nil {
		return err
	}
	w.add("owner-release", "release", cleanupWireProjection{request: request, success: response, failure: taskProtoResponse(nil, nil)})
	return w.runtimeReads(ek, ak)
}

func (w *cleanupLocalWireBuilder) terminationSchema() p.TaskRuntimeTerminationIntent {
	stamp := p.RecordStamp{CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64 - 1, SHA256: cleanupWirePaddingDigest()}
	f := w.in.bundle.fences
	publication := w.in.publication.Record
	return p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: w.closeContext(), Partition: w.claim.Task.Partition, TaskBirth: w.in.bundle.birth, CreationIntentID: w.in.bundle.task.CreationIntentID, EnrollmentDigest: publication.EnrollmentDigest, ObserverCertificateDigest: publication.ObserverCertificateDigest, NodeUID: publication.NodeUID, NodeBootID: publication.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty", Authority: p.TerminationAuthorityBindings{Task: localRuntimeFenceStamp(f[0]), CleanupIntent: localRuntimeFenceStamp(f[1]), TaskLink: localRuntimeFenceStamp(f[2]), Placement: localRuntimeFenceStamp(f[3]), Control: localRuntimeFenceStamp(f[4]), Owner: localRuntimeFenceStamp(f[5]), Fence: localRuntimeFenceStamp(f[6]), RuntimeIndex: localRuntimeFenceStamp(f[7]), Quiescence: stamp, QuiescenceReceipt: stamp, QuiescenceReservation: stamp, FinalSync: stamp, Issuer: localRuntimeFenceStamp(taskFence{key: w.issuerKey, value: w.issuerValue, create: w.in.issuer.Revision, mod: w.in.issuer.Revision}), Archive: stamp, LocalComplete: stamp, QuiescenceStage: p.TerminationQuiescenceStage{RequestID: w.quiescenceStage.RequestID, StageID: w.quiescenceStage.StageID, AttemptID: w.quiescenceStage.AttemptID, Digest: w.quiescenceStage.Digest}}}
}

// readEnrolledPoints adds these same two base points to each native business
// group. Keeping separate projections preserves source branch boundaries.
func (w *cleanupLocalWireBuilder) enrolledRead(keys []string, caps ...int) cleanupWireProjection {
	b := w.b
	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	values := []string{b.identityValue, b.restoreEpoch}
	for _, size := range caps {
		values = append(values, string(make([]byte, size)))
	}
	return cleanupWirePointProjection(b.baseComparisons(), all, values)
}

func (w *cleanupLocalWireBuilder) runtimeReads(ek, ak string) error {
	b, task := w.b, w.in.bundle.task
	keys := make([]string, len(w.in.bundle.fences))
	for i, f := range w.in.bundle.fences {
		keys[i] = f.key
	}
	w.add("termination", "authority-history-read.domain", w.enrolledRead(keys, 4096, 4096, 2048, 4096, 4096, 4096, 4096, 4096))
	w.add("termination", "authority-history-read.local-completion", w.enrolledRead([]string{w.progressKey, w.completeKey, string(w.in.bundle.checkpoint.Key)}, 16384, 8192, 4096))
	jk, pk, err := b.namespace.runtimePublicationKeys(task.Reference.Partition, task.CreationIntentID)
	if err != nil {
		return err
	}
	w.add("termination", "authority-history-read.publication-initial", w.enrolledRead([]string{jk, pk, ek, ak}, 8192, 16384, 8192, 8192))
	receipt, err := b.runtimeDispatchReceiptKey(w.in.publication.Record.Attempt)
	if err != nil {
		return err
	}
	history, err := b.enrolledHistoryKeys(task.Reference.Partition, task.CreationIntentID, w.in.publication.Record)
	if err != nil {
		return err
	}
	w.add("termination", "authority-history-read.publication-complete", w.enrolledRead(append([]string{jk, pk, ek, ak, receipt}, history...), append([]int{8192, 16384, 8192, 8192, 4096}, enrolledHistoryCaps()...)...))
	w.add("termination", "authority-history-read.enrollment", w.enrolledRead([]string{ek, ak, jk}, 8192, 8192, 8192))
	w.add("termination", "authority-history-read.archive-final", w.enrolledRead([]string{w.archiveKey, w.finalKey}, maxRecordBytes, 8192))
	w.add("termination", "authority-history-read.quiescence-pins", w.enrolledRead([]string{w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey, w.issuerKey}, 32768, 4096, 8192, 8192))
	// loadHistory also calls loadTaskQuiescence: its actual3/3/4/7 groups
	// are the exact query-family projections, already admitted in this plan.
	w.add("termination", "termination-read", w.enrolledRead([]string{w.terminationKey, w.providerKey, w.releaseKey}, 16384, 16384, 8192))
	terminalProvider := w.enrolledRead([]string{w.terminationKey, w.providerKey}, 16384, 16384)
	w.add("termination", "provider-read", terminalProvider)
	w.add("owner-release", "release-history-read.terminal-provider", terminalProvider)
	w.add("owner-release", "release-proof-read", w.enrolledRead([]string{w.releaseKey}, 8192))
	w.add("owner-release", "release-history-read.retained-history", w.enrolledRead([]string{keys[0], w.completeKey, w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey, w.finalKey, w.issuerKey, w.archiveKey, ek}, 4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192))
	return nil
}
