package etcd

import (
	"bytes"
	"encoding/json"
	"math"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

var taskRuntimeCleanupSteps = [7]k.TaskRuntimeCleanupStep{
	k.TaskRuntimeCleanupInstallRetainFinalizer,
	k.TaskRuntimeCleanupDeletePod,
	k.TaskRuntimeCleanupDeleteNetworkPolicyLogical,
	k.TaskRuntimeCleanupDeleteNetworkPolicyRuntime,
	k.TaskRuntimeCleanupDeleteCiliumPolicyLogical,
	k.TaskRuntimeCleanupDeleteCiliumPolicyRuntime,
	k.TaskRuntimeCleanupRemoveRetainFinalizer,
}

type taskRuntimeCleanupSlot struct {
	Step            k.TaskRuntimeCleanupStep `json:"step"`
	State           string                   `json:"state"`
	Kind            string                   `json:"kind"`
	Name            string                   `json:"name"`
	UID             string                   `json:"uid"`
	ResourceVersion string                   `json:"resource_version"`
	EffectID        string                   `json:"effect_id"`
	Claim           *taskCloseClaimWire      `json:"claim"`
}

type taskRuntimeProviderCleanupRecord struct {
	Version    uint32                      `json:"version"`
	Task       TaskReference               `json:"task"`
	TaskBirth  int64                       `json:"task_birth"`
	Binding    k.TaskRuntimeCleanupBinding `json:"binding"`
	Descriptor []byte                      `json:"descriptor"`
	Slots      [7]taskRuntimeCleanupSlot   `json:"slots"`
}

func (r taskRuntimeProviderCleanupRecord) Validate() error {
	b := r.Binding
	if r.Version != 1 || r.Task.Validate() != nil || r.TaskBirth <= 0 || b.Authority.Namespace != r.Task.Namespace || b.Authority.RestoreEpoch != r.Task.RestoreEpoch || !validDomainSegment(b.Authority.AuthorityID) || !validDomainSegment(b.Authority.Target) || b.TaskID != r.Task.TaskID || b.LogicalID != r.Task.SandboxID || !validHexDigest(b.WorkspaceHash) || !validPreparationUUID(b.CreationIntentID) || !validPreparationUUID(b.NodeUID) || !validPreparationUUID(b.NodeBootID) || !validPreparationUUID(b.Runtime.UID) || !validPreparationUUID(b.Runtime.BootID) || !validDomainSegment(b.Runtime.ID) || b.Generation <= 0 || b.DataGateEpoch <= 0 || !validHexDigest(b.EnrollmentDigest) || !validHexDigest(b.ContractDigest) || len(r.Descriptor) == 0 || len(r.Descriptor) > 16384 || !json.Valid(r.Descriptor) {
		return ErrInvalidRecord
	}
	for i, s := range r.Slots {
		if s.Step != taskRuntimeCleanupSteps[i] {
			return ErrInvalidRecord
		}
		switch s.State {
		case "unstarted", "skipped":
			if s.Kind != "" || s.Name != "" || s.UID != "" || s.ResourceVersion != "" || s.EffectID != "" || s.Claim != nil {
				return ErrInvalidRecord
			}
		case "intent", "unknown", "complete":
			if !validDomainSegment(s.Name) || len(s.ResourceVersion) == 0 || len(s.ResourceVersion) > 128 || !validPreparationUUID(s.UID) || !validPreparationUUID(s.EffectID) || s.Claim == nil || !validWorkspaceSyncFinalizationClaim(*s.Claim, r.Task) {
				return ErrInvalidRecord
			}
			kind := "Pod"
			if i == 2 || i == 3 {
				kind = "NetworkPolicy"
			} else if i == 4 || i == 5 {
				kind = "CiliumNetworkPolicy"
			}
			if s.Kind != kind {
				return ErrInvalidRecord
			}
		default:
			return ErrInvalidRecord
		}
	}
	return nil
}

func encodeTaskRuntimeProviderCleanup(r taskRuntimeProviderCleanupRecord) (string, error) {
	if r.Validate() != nil {
		return "", ErrInvalidRecord
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > 16384 {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func validLocalRuntimeMutableKV(kv *mvccpb.KeyValue) bool {
	return kv != nil && kv.Lease == 0 && kv.CreateRevision > 0 && kv.Version > 0 && kv.Version < math.MaxInt64 && (kv.Version == 1 && kv.ModRevision == kv.CreateRevision || kv.Version > 1 && kv.ModRevision > kv.CreateRevision)
}

func decodeTaskRuntimeProviderCleanup(kv *mvccpb.KeyValue) (taskRuntimeProviderCleanupRecord, error) {
	var r taskRuntimeProviderCleanupRecord
	if !validLocalRuntimeMutableKV(kv) || len(kv.Value) == 0 || len(kv.Value) > 16384 || json.Unmarshal(kv.Value, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeTaskRuntimeProviderCleanup(r)
	if err != nil || wire != string(kv.Value) {
		return taskRuntimeProviderCleanupRecord{}, ErrCorruptRecord
	}
	return r, nil
}

func decodeLocalRuntimeTermination(kv *mvccpb.KeyValue) (p.TaskRuntimeTerminationIntent, error) {
	if !validLocalRuntimeMutableKV(kv) {
		return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
	}
	r, err := p.DecodeTaskRuntimeTerminationIntent(kv.Value)
	if err != nil || kv.CreateRevision <= r.Current.ClaimCreateRevision {
		return p.TaskRuntimeTerminationIntent{}, ErrCorruptRecord
	}
	return r, nil
}

func sameLocalRuntimeTerminationOrigin(a, b p.TaskRuntimeTerminationIntent) bool {
	a.Phase, b.Phase = "reserved", "reserved"
	a.TerminalProof, b.TerminalProof = nil, nil
	a.CleanupDigest, b.CleanupDigest = "", ""
	x, err := p.EncodeTaskRuntimeTerminationIntent(a)
	y, other := p.EncodeTaskRuntimeTerminationIntent(b)
	return err == nil && other == nil && bytes.Equal(x, y)
}

func (n Namespace) localRuntimeReleaseKeys(task TaskReference) (termination, provider, release string, err error) {
	parts, err := n.taskKeyParts(task)
	if err != nil {
		return "", "", "", err
	}
	termination, err = n.Key(append(parts, "runtime-termination")...)
	if err == nil {
		provider, err = n.Key(append(parts, "runtime-provider-cleanup")...)
	}
	if err == nil {
		release, err = n.Key(append(parts, "owner-release")...)
	}
	return
}
