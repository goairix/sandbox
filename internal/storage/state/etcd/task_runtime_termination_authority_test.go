package etcd

import (
	"fmt"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestLocalRuntimeReleaseAuthorityBudgets(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	n := b.namespace
	termination, provider, release, err := n.localRuntimeReleaseKeys(d.record.Reference.Task)
	require.NoError(t, err)
	_, complete, err := n.workspaceSyncLocalKeys(d.record.Reference.Task)
	require.NoError(t, err)
	prefix, err := b.taskOperationsPrefix(d.record.Reference.Task)
	require.NoError(t, err)
	a := &taskLocalRuntimeAuthority{backend: b, prefix: prefix, complete: taskFence{key: complete, value: strings.Repeat("c", 8192), create: 5, mod: 5}, terminationKey: termination, providerKey: provider, releaseKey: release}
	for i := 0; i < 50; i++ {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(n.Root()+fmt.Sprintf("authority-%02d", i)), "=", strings.Repeat("x", 512)))
	}
	a.enrollment = [2]taskFence{{key: n.Root() + "enrollment", create: 2, mod: 2}, {key: n.Root() + "activation", create: 2, mod: 2}}
	providerKV := &mvccpb.KeyValue{Key: []byte(provider), Value: []byte(strings.Repeat("p", 16384)), CreateRevision: 6, ModRevision: 7, Version: 2}
	terminationKV := &mvccpb.KeyValue{Key: []byte(termination), Value: []byte(strings.Repeat("t", 16384)), CreateRevision: 8, ModRevision: 9, Version: 2}
	for _, tc := range []struct {
		name string
		cmps []clientv3.Cmp
		node int
	}{
		{"provider-initial", a.providerComparisons(nil, nil), 57},
		{"provider-before-termination", a.providerComparisons(providerKV, nil), 60},
		{"termination-reserve", a.terminationReserveComparisons(providerKV), 60},
		{"termination-exact", a.terminationComparisons(terminationKV, nil), 60},
		{"termination-provider-pin", a.terminationComparisons(terminationKV, providerKV), 61},
		{"provider-after-termination", a.providerComparisons(providerKV, terminationKV), 61},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.node, len(tc.cmps)+1)
			require.NoError(t, a.preflightWrite(tc.cmps, termination, strings.Repeat("t", 16384)))
		})
	}
}

func TestLocalRuntimeReleaseAuthorityFutureProof(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	r := localReleaseTerminationFixture(t)
	termination, provider, _, err := b.namespace.localRuntimeReleaseKeys(d.record.Reference.Task)
	require.NoError(t, err)
	prefix, err := b.taskOperationsPrefix(d.record.Reference.Task)
	require.NoError(t, err)
	_, complete, err := b.namespace.workspaceSyncLocalKeys(d.record.Reference.Task)
	require.NoError(t, err)
	a := &taskLocalRuntimeAuthority{backend: b, terminationKey: termination, providerKey: provider, prefix: prefix, complete: taskFence{key: complete, value: strings.Repeat("c", 8192), create: 2, mod: 2}}
	for i := 0; i < 50; i++ {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(b.namespace.Root()+fmt.Sprintf("authority-%02d", i)), "=", strings.Repeat("x", 512)))
	}
	pv := &mvccpb.KeyValue{Key: []byte(provider), Value: []byte("provider"), CreateRevision: 3, ModRevision: 4, Version: 2}
	require.NoError(t, a.preflightTerminationPhases(r, pv))
	r.TerminalProof = []byte("not-reserved")
	require.Error(t, a.preflightTerminationPhases(r, pv))
	r.TerminalProof = nil
	r.Current.Namespace = strings.Repeat("x", 513)
	require.Error(t, a.preflightTerminationPhases(r, pv))
}

func localReleaseTerminationFixture(t *testing.T) p.TaskRuntimeTerminationIntent {
	t.Helper()
	r := localReleaseRecordFixture(t)
	a := archiveCommandRecordFixture(t)
	s := p.RecordStamp{CreateRevision: 1, ModRevision: 1, SHA256: strings.Repeat("a", 64)}
	return p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: a.Claims.Context.Current, Partition: a.FinalSync.Task.Reference.Partition, TaskBirth: 1,
		CreationIntentID: r.Binding.CreationIntentID, EnrollmentDigest: r.Binding.EnrollmentDigest, ObserverCertificateDigest: r.Binding.EnrollmentDigest, NodeUID: r.Binding.NodeUID, NodeBootID: r.Binding.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty",
		Authority: p.TerminationAuthorityBindings{Task: s, CleanupIntent: s, TaskLink: s, Placement: s, Control: s, Owner: s, Fence: s, RuntimeIndex: s, Quiescence: s, QuiescenceReceipt: s, QuiescenceReservation: s, FinalSync: s, Issuer: s, Archive: s, LocalComplete: s, QuiescenceStage: p.TerminationQuiescenceStage{RequestID: "request", StageID: "stage", AttemptID: r.Binding.CreationIntentID, Digest: r.Binding.EnrollmentDigest}}}
}

func TestLocalRuntimeReleaseAuthorityCompletion(t *testing.T) {
	m := recoveryTestManifest(t, "bound")
	m.Seed.TaskBirth, m.Seed.ArchiveRevision = 1, 5
	seed := m.Seed
	d := strings.Repeat("d", 64)
	claim := taskCloseClaimWire{Task: seed.Task, ClaimID: seed.ClaimID, WorkerID: seed.WorkerID, CreateRevision: seed.ArchiveRevision + 1, LeaseID: seed.LeaseID}
	checkpoint := TaskCheckpointRecord{Version: 1, Reference: seed.Task, State: TaskCheckpointPending, Attempt: StageAttemptLocator{Namespace: seed.Task.Namespace, RestoreEpoch: seed.Task.RestoreEpoch, Partition: seed.Task.Partition, RequestID: "request", StageID: "destroy", AttemptID: seed.ClaimID}}
	cw, err := encodeTaskCheckpointRecord(checkpoint)
	require.NoError(t, err)
	ck := &mvccpb.KeyValue{Value: []byte(cw), Version: 1, CreateRevision: seed.TaskBirth, ModRevision: seed.TaskBirth}
	progress := taskWorkspaceSyncLocalProgress{Version: 1, Task: seed.Task, TaskBirth: seed.TaskBirth, Binding: d, Claim: claim, Phase: "sealed", CheckpointRevision: ck.ModRevision, CheckpointDigest: workspaceSyncIntentDigest(cw), IntentID: seed.ArchiveCommand, IntentBirth: claim.CreateRevision + 1, EvidenceDigest: d}
	pw, err := encodeWorkspaceSyncLocalProgress(progress)
	require.NoError(t, err)
	pk := &mvccpb.KeyValue{Value: []byte(pw), Version: 2, CreateRevision: claim.CreateRevision + 1, ModRevision: claim.CreateRevision + 2}
	complete := taskWorkspaceSyncLocalComplete{Version: 1, Domain: "sandbox.workspace-sync-local-complete.v1", Task: seed.Task, TaskBirth: seed.TaskBirth, Binding: d, Claim: claim, ProgressDigest: workspaceSyncIntentDigest(pw), EvidenceDigest: d, CheckpointRevision: ck.ModRevision, CheckpointDigest: workspaceSyncIntentDigest(cw), RootDigest: m.Binding.TargetDigest}
	w, err := encodeOperationMetadata(complete, 8192)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(w), Version: 1, CreateRevision: pk.ModRevision, ModRevision: pk.ModRevision}
	_, err = validateLocalRuntimeCompletion(m, d, pk, kv, ck)
	require.NoError(t, err)
	for _, fault := range []string{"lease", "same-value-put", "common-birth", "checkpoint", "binding", "missing", "foreign-task", "claim"} {
		t.Run(fault, func(t *testing.T) {
			bad, prog, cp, manifest, binding := *kv, *pk, *ck, m, d
			switch fault {
			case "lease":
				bad.Lease = 1
			case "same-value-put":
				bad.Version++
				bad.ModRevision++
			case "common-birth":
				prog.ModRevision++
			case "checkpoint":
				cp.ModRevision++
			case "binding":
				binding = strings.Repeat("e", 64)
			case "missing":
				bad.Value = nil
			case "foreign-task":
				manifest.Seed.Task.TaskID = "66666666-6666-4666-8666-666666666666"
			case "claim":
				copy := complete
				copy.Claim.ClaimID = "66666666-6666-4666-8666-666666666666"
				value, e := encodeOperationMetadata(copy, 8192)
				require.NoError(t, e)
				bad.Value = []byte(value)
			}
			_, err := validateLocalRuntimeCompletion(manifest, binding, &prog, &bad, &cp)
			require.Error(t, err)
		})
	}
}
