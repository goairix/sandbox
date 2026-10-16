package etcd

import (
	"bytes"
	"context"
	"errors"
	"strings"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type taskLocalRuntimeExecution struct {
	a          *taskLocalRuntimeAuthority
	provider   *k.PreparedTaskRuntimeCleanup
	cleanup    taskRuntimeProviderCleanupRecord
	providerKV *mvccpb.KeyValue
	intent     p.TaskRuntimeTerminationIntent
	intentKV   *mvccpb.KeyValue
}

func localRuntimeProviderBinding(r p.TaskRuntimeTerminationIntent, contract string) k.TaskRuntimeCleanupBinding {
	c := r.Current
	return k.TaskRuntimeCleanupBinding{Authority: p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, Runtime: c.Runtime, Generation: c.Generation, DataGateEpoch: c.DataGateEpoch, TaskID: c.TaskID, LogicalID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, CreationIntentID: r.CreationIntentID, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, EnrollmentDigest: r.EnrollmentDigest, ContractDigest: contract}
}

func localRuntimeProviderMatches(r taskRuntimeProviderCleanupRecord, intent p.TaskRuntimeTerminationIntent, contract string) bool {
	return r.Task == localRuntimeTerminationTask(intent) && r.TaskBirth == intent.TaskBirth && r.Binding == localRuntimeProviderBinding(intent, contract)
}

func localRuntimeProviderComplete(r taskRuntimeProviderCleanupRecord) bool {
	for i, s := range r.Slots {
		if s.State != "complete" && !(i >= 2 && i <= 5 && s.State == "skipped") {
			return false
		}
	}
	return true
}

// The only writes here target the two fixed keys of this private execution.
// A lost response is returned once; neither this helper nor a caller retries.
func (x *taskLocalRuntimeExecution) put(ctx context.Context, provider bool, wire string) (*mvccpb.KeyValue, error) {
	a, b := x.a, x.a.backend
	key, prior := a.terminationKey, x.intentKV
	var cmps []clientv3.Cmp
	if provider {
		key, prior = a.providerKey, x.providerKV
		cmps = a.providerComparisons(prior, x.intentKV)
	} else if prior != nil {
		cmps = a.terminationComparisons(prior, x.providerKV)
	} else {
		cmps = a.terminationReserveComparisons(x.providerKV)
	}
	if err := a.preflightWrite(cmps, key, wire); err != nil {
		return nil, err
	}
	if err := a.local(ctx); err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(cmps...).Then(clientv3.OpPut(key, wire)).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if b.operationResponseHeader(response) != nil || taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(response)}) != nil {
		return nil, ErrOutcomeUnknown
	}
	if !response.Succeeded {
		return nil, ErrConflict
	}
	if len(response.Responses) != 1 || response.Responses[0] == nil || response.Responses[0].GetResponsePut() == nil || response.Header.Revision <= a.claim.reference.CreateRevision {
		return nil, ErrOutcomeUnknown
	}
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: response.Header.Revision, ModRevision: response.Header.Revision}
	if prior != nil {
		if response.Header.Revision <= prior.ModRevision {
			return nil, ErrOutcomeUnknown
		}
		kv.Version, kv.CreateRevision = prior.Version+1, prior.CreateRevision
	}
	if err := a.local(ctx); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	return kv, nil
}

func (x *taskLocalRuntimeExecution) storeProvider(ctx context.Context) error {
	wire, err := encodeTaskRuntimeProviderCleanup(x.cleanup)
	if err != nil {
		return err
	}
	kv, err := x.put(ctx, true, wire)
	if err == nil {
		x.providerKV = kv
	}
	return err
}

func (x *taskLocalRuntimeExecution) storeTermination(ctx context.Context) error {
	wire, err := p.EncodeTaskRuntimeTerminationIntent(x.intent)
	if err != nil {
		return err
	}
	kv, err := x.put(ctx, false, string(wire))
	if err == nil {
		x.intentKV = kv
	}
	return err
}

func (x *taskLocalRuntimeExecution) preflightProvider() error {
	projections, err := taskRuntimeProviderWireProjections(x.a.wirePins(), x.cleanup, localRuntimeClaimWire(x.a.claim), x.providerKV, x.intentKV)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(x.a.backend.namespace, projections)
}

// Preserve the original canonical seven-slot schema check. The cleanup gate
// additionally uses the whole record cap, so this representative RV string is
// not mistaken for a maximum JSON-escaped ResourceVersion or descriptor.
func taskRuntimeProviderWireProjections(pins taskLocalRuntimeWirePins, projected taskRuntimeProviderCleanupRecord, claim taskCloseClaimWire, provider, termination *mvccpb.KeyValue) ([]cleanupWireProjection, error) {
	for i, step := range taskRuntimeCleanupSteps {
		kind := "Pod"
		if i == 2 || i == 3 {
			kind = "NetworkPolicy"
		} else if i == 4 || i == 5 {
			kind = "CiliumNetworkPolicy"
		}
		projected.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "complete", Kind: kind, Name: strings.Repeat("n", 128), UID: "ffffffff-ffff-4fff-8fff-ffffffffffff", ResourceVersion: strings.Repeat("r", 128), EffectID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Claim: &claim}
	}
	wire, err := encodeTaskRuntimeProviderCleanup(projected)
	if err != nil {
		return nil, err
	}
	return []cleanupWireProjection{taskRuntimeWriteProjection(pins.providerComparisons(provider, termination), pins.providerKey, wire)}, nil
}

func (x *taskLocalRuntimeExecution) prepareProvider(ctx context.Context) error {
	var saved []byte
	if x.providerKV != nil {
		saved = x.cleanup.Descriptor
	}
	if err := x.a.guard(ctx, x.a.base()); err != nil {
		return err
	}
	handle, err := x.a.backend.localRuntimeRelease.Provider.Prepare(ctx, localRuntimeProviderBinding(x.intent, x.a.backend.enrolledPublication.ContractDigest), saved)
	if err != nil {
		return err
	}
	x.provider = handle
	descriptor := handle.Descriptor()
	if len(descriptor) == 0 || len(descriptor) > 16384 || saved != nil && !bytes.Equal(saved, descriptor) {
		return ErrCorruptRecord
	}
	if x.providerKV == nil {
		x.cleanup = taskRuntimeProviderCleanupRecord{Version: 1, Task: x.a.task.Reference, TaskBirth: x.a.claim.birth, Binding: localRuntimeProviderBinding(x.intent, x.a.backend.enrolledPublication.ContractDigest), Descriptor: descriptor}
		for i, step := range taskRuntimeCleanupSteps {
			x.cleanup.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "unstarted"}
		}
	}
	if err := x.preflightProvider(); err != nil {
		return err
	}
	// Check both future terminal encodings before installing the finalizer.
	projection := x.providerKV
	if projection == nil {
		wire, err := encodeTaskRuntimeProviderCleanup(x.cleanup)
		if err != nil {
			return err
		}
		projection = &mvccpb.KeyValue{Key: []byte(x.a.providerKey), Value: []byte(wire), Version: 1, CreateRevision: 1, ModRevision: 1}
	}
	original := x.intent
	original.Phase, original.TerminalProof, original.CleanupDigest = "reserved", nil, ""
	if err := x.a.preflightTerminationPhases(original, projection); err != nil {
		return err
	}
	if x.providerKV == nil {
		return x.storeProvider(ctx)
	}
	return nil
}

func (x *taskLocalRuntimeExecution) applyProvider(ctx context.Context, i int) error {
	slot := x.cleanup.Slots[i]
	if slot.State == "complete" || slot.State == "skipped" {
		return nil
	}
	if slot.State != "unstarted" {
		return ErrOutcomeUnknown // an existing intent is never a resend grant
	}
	observation, err := x.provider.ObserveStep(ctx, slot.Step)
	if err != nil || observation.Step != slot.Step {
		return errors.Join(ErrCorruptRecord, err)
	}
	if i >= 2 && i <= 5 && observation.Absent {
		slot.State = "skipped" // point-read absence, explicitly not a send receipt
		x.cleanup.Slots[i] = slot
		return x.storeProvider(ctx)
	}
	// Production enrolled Pods are born retained. Step zero still records its
	// intent and consumes Apply's exact preflight; an already retained Pod uses
	// its guarded no-op and the same real readback below. Existing intents or
	// unknown results remain rejected above and never receive a resend grant.
	if observation.Absent || observation.UID == "" || observation.ResourceVersion == "" || i == 0 && observation.Deleting || i == 1 && (!observation.Retained || observation.Deleting) || i == 6 && (!observation.Retained || !observation.Deleting) {
		return ErrConflict
	}
	claim := localRuntimeClaimWire(x.a.claim)
	slot = taskRuntimeCleanupSlot{Step: slot.Step, State: "intent", Kind: observation.Kind, Name: observation.Name, UID: observation.UID, ResourceVersion: observation.ResourceVersion, EffectID: uuid.NewString(), Claim: &claim}
	x.cleanup.Slots[i] = slot
	if err := x.storeProvider(ctx); err != nil {
		return err
	}
	if err := x.a.guard(ctx, x.a.providerComparisons(x.providerKV, x.intentKV)); err != nil {
		return err
	}
	_, err = x.provider.Apply(ctx, slot.Step)
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	// A mutation ACK is not a readback. Observe the fixed original step again.
	result, err := x.provider.ObserveStep(ctx, slot.Step)
	if err != nil || result.Step != slot.Step {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if !result.Absent && (result.Kind != slot.Kind || result.Name != slot.Name || result.UID != slot.UID) || i == 0 && (result.Absent || !result.Retained) || i == 1 && (result.Absent || !result.Deleting || !result.Retained) || i >= 2 && i <= 5 && (!result.Absent || !result.Terminal) || i == 6 && result.Retained {
		return ErrOutcomeUnknown
	}
	slot.State = "complete"
	x.cleanup.Slots[i] = slot
	return x.storeProvider(ctx)
}

func (x *taskLocalRuntimeExecution) finishProvider(ctx context.Context) error {
	for i := 2; i < len(taskRuntimeCleanupSteps); i++ {
		if err := x.applyProvider(ctx, i); err != nil {
			return err
		}
	}
	if !localRuntimeProviderComplete(x.cleanup) {
		return ErrOutcomeUnknown
	}
	pod, err := x.provider.Observe(ctx)
	if err != nil || !pod.Absent {
		return errors.Join(ErrConflict, err)
	}
	for i := 2; i <= 5; i++ {
		observation, err := x.provider.ObserveStep(ctx, taskRuntimeCleanupSteps[i])
		if err != nil || observation.Step != taskRuntimeCleanupSteps[i] || !observation.Absent {
			return errors.Join(ErrConflict, err)
		}
	}
	if err := x.a.guard(ctx, x.a.terminationComparisons(x.intentKV, x.providerKV)); err != nil {
		return err
	}
	x.intent.Phase, x.intent.CleanupDigest = "cleanup-complete", workspaceSyncIntentDigest(string(x.providerKV.Value))
	return x.storeTermination(ctx)
}
