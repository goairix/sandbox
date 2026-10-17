package etcd

import (
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This fixture is canonical reservation metadata, not a signed terminal proof
// or genuine Claim/Kernel. The native observer recovery is the positive effect
// boundary; these tests cover the exact pure gate preceding that effect path.
func cleanupTerminationMetadata(t *testing.T) (TaskReference, int64, taskFence, *mvccpb.KeyValue) {
	t.Helper()
	a := archiveCommandRecordFixture(t)
	r := localReleaseRecordFixture(t)
	s := p.RecordStamp{CreateRevision: 1, ModRevision: 1, SHA256: strings.Repeat("a", 64)}
	intent := p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: a.Claims.Context.Current, Partition: a.FinalSync.Task.Reference.Partition, TaskBirth: 1,
		CreationIntentID: r.Binding.CreationIntentID, EnrollmentDigest: r.Binding.EnrollmentDigest, ObserverCertificateDigest: r.Binding.EnrollmentDigest, NodeUID: r.Binding.NodeUID, NodeBootID: r.Binding.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty",
		Authority: p.TerminationAuthorityBindings{Task: s, CleanupIntent: s, TaskLink: s, Placement: s, Control: s, Owner: s, Fence: s, RuntimeIndex: s, Quiescence: s, QuiescenceReceipt: s, QuiescenceReservation: s, FinalSync: s, Issuer: s, Archive: s, LocalComplete: s, QuiescenceStage: p.TerminationQuiescenceStage{RequestID: "request", StageID: "stage", AttemptID: r.Binding.CreationIntentID, Digest: r.Binding.EnrollmentDigest}}}
	wire, err := p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err)
	keys, err := p.TaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Key: []byte(keys.Intent), Value: wire, Version: 1, CreateRevision: 100, ModRevision: 100}
	return localRuntimeTerminationTask(intent), intent.TaskBirth, ownTaskFence(kv), kv
}

func TestCleanupLocalTerminationReservation(t *testing.T) {
	task, birth, fence, kv := cleanupTerminationMetadata(t)
	require.NoError(t, matchCleanupTerminationReservation(task, birth, &fence, kv))
	t.Run("missing-expected", func(t *testing.T) {
		require.Error(t, matchCleanupTerminationReservation(task, birth, nil, kv))
	})
	t.Run("missing-actual", func(t *testing.T) {
		require.Error(t, matchCleanupTerminationReservation(task, birth, &fence, nil))
	})
	t.Run("wrong-task", func(t *testing.T) {
		other := task
		other.TaskID = uuid.NewString()
		require.Error(t, matchCleanupTerminationReservation(other, birth, &fence, kv))
	})
	t.Run("wrong-birth", func(t *testing.T) {
		task, birth, fence, kv := cleanupTerminationMetadata(t)
		require.Error(t, matchCleanupTerminationReservation(task, birth+1, &fence, kv))
	})

	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"lease":        func(kv *mvccpb.KeyValue) { kv.Lease = 1 },
		"version-zero": func(kv *mvccpb.KeyValue) { kv.Version = 0 },
		"version-two":  func(kv *mvccpb.KeyValue) { kv.Version = 2 },
		"absent-birth": func(kv *mvccpb.KeyValue) { kv.CreateRevision = 0 },
		"aba": func(kv *mvccpb.KeyValue) {
			kv.CreateRevision++
			kv.ModRevision++
		},
		"modified": func(kv *mvccpb.KeyValue) {
			kv.ModRevision++
			kv.Version++
		},
		"wrong-key":     func(kv *mvccpb.KeyValue) { kv.Key = append(kv.Key, 'x') },
		"noncanonical":  func(kv *mvccpb.KeyValue) { kv.Value = append([]byte(" "), kv.Value...) },
		"missing-value": func(kv *mvccpb.KeyValue) { kv.Value = nil },
	} {
		t.Run(name, func(t *testing.T) {
			task, birth, fence, kv := cleanupTerminationMetadata(t)
			mutate(kv)
			require.Error(t, matchCleanupTerminationReservation(task, birth, &fence, kv))
			if name != "aba" {
				// Equal copied fences cannot legalize a malformed schema/native
				// envelope or wrong key. ABA is rejected against the original pin.
				matching := ownTaskFence(kv)
				require.Error(t, matchCleanupTerminationReservation(task, birth, &matching, kv))
			}
		})
	}

	for name, mutate := range map[string]func(*p.TaskRuntimeTerminationIntent){
		"current-command": func(r *p.TaskRuntimeTerminationIntent) { r.Current.CommandID = uuid.NewString() },
		"current-claim":   func(r *p.TaskRuntimeTerminationIntent) { r.Current.ClaimID = uuid.NewString() },
		"current-worker":  func(r *p.TaskRuntimeTerminationIntent) { r.Current.WorkerID = "another-worker" },
		"enrollment":      func(r *p.TaskRuntimeTerminationIntent) { r.EnrollmentDigest = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			task, birth, fence, kv := cleanupTerminationMetadata(t)
			intent, err := p.DecodeTaskRuntimeTerminationIntent(kv.Value)
			require.NoError(t, err)
			mutate(&intent)
			kv.Value, err = p.EncodeTaskRuntimeTerminationIntent(intent)
			require.NoError(t, err)
			require.Error(t, matchCleanupTerminationReservation(task, birth, &fence, kv))
		})
	}
}
