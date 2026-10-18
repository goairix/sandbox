package etcd

import (
	"fmt"
	"math"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (d *cleanupDetachedWireBuilder) runtimePins() (taskLocalRuntimeWirePins, error) {
	w := &d.w
	keys, err := d.sourceKeys()
	if err != nil {
		return taskLocalRuntimeWirePins{}, err
	}
	cmps := cleanupWireAppend(d.b.baseComparisons(), w.live...)
	for _, key := range []string{w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey, w.finalKey, w.issuerKey, keys[2], keys[4], keys[5]} {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64))
	}
	return taskLocalRuntimeWirePins{comparisons: cmps, prefix: w.prefix, enrollment: [2]taskFence{cleanupWireFence(keys[7], 8192), cleanupWireFence(keys[8], 8192)}, terminationKey: w.terminationKey, providerKey: w.providerKey, settlement: cleanupWireFence(d.factKey, 32768)}, nil
}

func (d *cleanupDetachedWireBuilder) runtimeProjection(family, variant string) (cleanupWireProjection, error) {
	w := &d.w
	pins, err := d.runtimePins()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	termination, provider := cleanupWireKV(w.terminationKey, 16384), cleanupWireKV(w.providerKey, 16384)
	if family == "owner-release" {
		return d.ownerProjection(variant, pins, termination)
	}
	if family == "provider" {
		var cmps []clientv3.Cmp
		switch variant {
		case "provider-initial":
			cmps = pins.providerComparisons(nil, nil)
		case "provider-before-termination":
			cmps = pins.providerComparisons(provider, nil)
		case "provider-after-termination":
			cmps = pins.providerComparisons(provider, termination)
		default:
			return cleanupWireProjection{}, ErrInvalidRecord
		}
		return taskRuntimeWriteProjection(cmps, w.providerKey, string(provider.Value)), nil
	}
	switch variant {
	case "authority-history-read.domain":
		keys := make([]string, 8)
		for i, f := range d.in.bundle.fences {
			keys[i] = f.key
		}
		return w.enrolledRead(keys, 4096, 4096, 2048, 4096, 4096, 4096, 4096, 4096), nil
	case "authority-history-read.publication-initial":
		return d.sourceProjection("publication-initial")
	case "authority-history-read.publication-complete":
		return d.sourceProjection("publication-complete")
	case "authority-history-read.none-locate":
		return d.noEffectProjection("locate")
	case "authority-history-read.none-quiescence":
		return d.noEffectProjection("quiescence")
	case "authority-history-read.none-source":
		return d.noEffectProjection("source")
	case "authority-history-read.supplement":
		keys, err := d.sourceKeys()
		if err != nil {
			return cleanupWireProjection{}, err
		}
		return w.enrolledRead([]string{w.reservationKey, w.issuerKey, keys[7], keys[8]}, 8192, 8192, 8192, 8192), nil
	case "termination-read":
		return w.enrolledRead([]string{w.terminationKey, w.providerKey, w.releaseKey}, 16384, 16384, 8192), nil
	case "provider-read":
		return w.enrolledRead([]string{w.terminationKey, w.providerKey}, 16384, 16384), nil
	case "reserve":
		return taskRuntimeWriteProjection(pins.terminationReserveComparisons(provider), w.terminationKey, string(termination.Value)), nil
	case "reserved-guard", "terminated-guard", "cleanup-complete-guard":
		return w.guard(pins.terminationComparisons(termination, provider), true), nil
	case "terminated-write", "cleanup-complete-write":
		return taskRuntimeWriteProjection(pins.terminationComparisons(termination, provider), w.terminationKey, string(termination.Value)), nil
	}
	return cleanupWireProjection{}, ErrInvalidRecord
}

func (d *cleanupDetachedWireBuilder) ownerProjection(variant string, pins taskLocalRuntimeWirePins, termination *mvccpb.KeyValue) (cleanupWireProjection, error) {
	w := &d.w
	switch variant {
	case "release-proof-read", "release-arbitration":
		return w.enrolledRead([]string{w.releaseKey}, 8192), nil
	case "release-history-read.head-with-task":
		return w.enrolledRead([]string{w.terminationKey, w.providerKey, d.in.bundle.fences[0].key}, 16384, 16384, 4096), nil
	case "release-history-read.head-paid-task":
		return w.enrolledRead([]string{w.terminationKey, w.providerKey}, 16384, 16384), nil
	case "release-history-read.retained13":
		// This is only the original immutable Task's dimensional byte view for
		// the pure key router, never a reconstructed surviving-history proof.
		f := d.in.bundle.fences[0]
		keys, caps, err := d.b.taskOwnerReleaseHistoryKeys(d.terminationSchema(), &mvccpb.KeyValue{Key: []byte(f.key), Value: []byte(f.value), Version: 1, CreateRevision: d.in.bundle.birth, ModRevision: d.in.bundle.birth})
		if err != nil {
			return cleanupWireProjection{}, err
		}
		return w.enrolledRead(keys, caps...), nil
	case "release":
		r := d.terminationSchema()
		r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", make([]byte, 4096), cleanupWirePaddingDigest()
		seal, err := newDetachedOwnerReleaseRecord(r, math.MaxInt64-3, math.MaxInt64-1, taskCloseClaimWire{w.claim.Task, w.claim.ClaimID, w.claim.WorkerID, w.claim.CreateRevision, w.claim.LeaseID})
		if err != nil {
			return cleanupWireProjection{}, err
		}
		a := &taskLocalRuntimeAuthority{backend: d.b, comparisons: pins.comparisons, prefix: pins.prefix, terminationKey: pins.terminationKey, providerKey: pins.providerKey, releaseKey: w.releaseKey, detached: &taskDetachedRuntimeAuthority{settlement: pins.settlement}}
		_, request, response, err := a.detachedReleaseProjection(termination, [3]string{seal.Deleted[0].Key, seal.Deleted[1].Key, seal.Deleted[2].Key}, cleanupWirePadding(8192))
		if err != nil {
			return cleanupWireProjection{}, err
		}
		return cleanupWireProjection{request: request, success: response, failure: taskProtoResponse(nil, nil)}, nil
	}
	return cleanupWireProjection{}, ErrInvalidRecord
}

// Provider state aliases all use the original single-write constructor. Unknown
// is the already written intent's same-byte read-only state, never a new send.
func (d *cleanupDetachedWireBuilder) emitProviderStep(step int, phase string, emit func(cleanupDetachedWireEmission) error) error {
	if err := checkCleanupDetachedWireAllocation(cleanupDetachedWireAllocation{keyBytes: 64 << 10, valueBytes: 576 << 10, schemaBytes: 176128, nodes: 64, keys: 64}); err != nil {
		return err
	}
	pins, err := d.runtimePins()
	if err != nil {
		return err
	}
	provider, termination := cleanupWireKV(d.w.providerKey, 16384), cleanupWireKV(d.w.terminationKey, 16384)
	if step == 0 {
		termination = nil
	}
	cmps := pins.providerComparisons(provider, termination)
	projection := taskRuntimeWriteProjection(cmps, d.w.providerKey, string(provider.Value))
	if phase == "guard" {
		projection = d.w.guard(cmps, true)
	}
	return d.publishProjection("provider", fmt.Sprintf("step-%d-%s", step, phase), projection, emit)
}

// Validate the future seven-slot codec once with all independently bounded
// scalar fields. The descriptor is opaque protocol data, not a provider handle.
func (d *cleanupDetachedWireBuilder) providerSchema() error {
	r := d.terminationSchema()
	provider := taskRuntimeProviderCleanupRecord{Version: 1, Task: d.in.bundle.task.Reference, TaskBirth: d.in.bundle.birth, Binding: localRuntimeProviderBinding(r, d.b.detachedCleanup.contractDigest), Descriptor: []byte("{}")}
	claim := taskCloseClaimWire{d.w.claim.Task, d.w.claim.ClaimID, d.w.claim.WorkerID, d.w.claim.CreateRevision, d.w.claim.LeaseID}
	for i, step := range taskRuntimeCleanupSteps {
		kind := "Pod"
		if i == 2 || i == 3 {
			kind = "NetworkPolicy"
		}
		if i == 4 || i == 5 {
			kind = "CiliumNetworkPolicy"
		}
		provider.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "complete", Kind: kind, Name: strings.Repeat("n", 128), UID: "ffffffff-ffff-4fff-8fff-ffffffffffff", ResourceVersion: strings.Repeat("r", 128), EffectID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Claim: &claim}
	}
	_, err := encodeTaskRuntimeProviderCleanup(provider)
	return err
}
