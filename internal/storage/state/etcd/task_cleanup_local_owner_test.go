package etcd

import (
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Canonical codec metadata is not an authenticated observer proof. Only the
// native owner-held case exercises the real history verifier and Release.
func cleanupCompletedMetadata(t *testing.T) (*cleanupSlotState, *mvccpb.KeyValue, *mvccpb.KeyValue) {
	t.Helper()
	intent := localReleaseTerminationFixture(t)
	task := localRuntimeTerminationTask(intent)
	provider := localReleaseRecordFixture(t)
	// The generic Task fixture uses opaque "uid"/"boot" values. Completed
	// provider metadata requires canonical UUIDs, shared by context and slots.
	intent.Current.Runtime.UID = provider.Binding.Runtime.UID
	intent.Current.Runtime.BootID = provider.Binding.Runtime.BootID
	provider.Task, provider.TaskBirth = task, intent.TaskBirth
	provider.Binding = localRuntimeProviderBinding(intent, provider.Binding.ContractDigest)
	claim := taskCloseClaimWire{Task: task, ClaimID: intent.Current.ClaimID, WorkerID: intent.Current.WorkerID, CreateRevision: intent.Current.ClaimCreateRevision, LeaseID: intent.Current.LeaseID}
	for i := range provider.Slots {
		if i >= 2 && i <= 5 {
			provider.Slots[i].State = "skipped"
		} else {
			provider.Slots[i] = taskRuntimeCleanupSlot{Step: taskRuntimeCleanupSteps[i], State: "complete", Kind: "Pod", Name: "pod", UID: intent.Current.Runtime.UID, ResourceVersion: "1", EffectID: uuid.NewString(), Claim: &claim}
		}
	}
	providerWire, err := encodeTaskRuntimeProviderCleanup(provider)
	require.NoError(t, err)
	intent.Phase, intent.TerminalProof = "cleanup-complete", []byte("codec-only-not-authenticated")
	intent.CleanupDigest = workspaceSyncIntentDigest(providerWire)
	wire, err := p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err)
	keys, err := p.TaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	birth := intent.Current.ClaimCreateRevision + 10
	termination := &mvccpb.KeyValue{Key: []byte(keys.Intent), Value: wire, CreateRevision: birth, ModRevision: birth + 3, Version: 3}
	providerKV := &mvccpb.KeyValue{Key: []byte(strings.TrimSuffix(keys.Intent, "runtime-termination") + "runtime-provider-cleanup"), Value: []byte(providerWire), CreateRevision: birth - 1, ModRevision: birth + 2, Version: 7}
	f := cleanupBoundMetadata(t)
	r := &f.state.record
	r.Task, r.TaskBirth, r.Confirmed = task, intent.TaskBirth, "local-complete"
	r.InitialAttempt.Namespace, r.InitialAttempt.RestoreEpoch, r.InitialAttempt.Partition = task.Namespace, task.RestoreEpoch, task.Partition
	r.Incarnation = cleanupSlotTestIncarnation(*r)
	r.Pending = &cleanupExecutionPending{Purpose: "termination", ClaimID: intent.Current.ClaimID, LeaseID: intent.Current.LeaseID}
	require.NoError(t, r.Validate())
	return f.state, termination, providerKV
}

func TestCleanupLocalCompletedTermination(t *testing.T) {
	for _, phase := range []string{"local-complete", "terminated"} {
		t.Run(phase, func(t *testing.T) {
			s, intent, provider := cleanupCompletedMetadata(t)
			s.record.Confirmed = phase
			if phase == "terminated" {
				s.record.Pending.Purpose = "owner-release"
			}
			require.NoError(t, matchCleanupCompletedTermination(s, intent, provider))
			expected := &cleanupCompletedTermination{termination: ownTaskFence(intent), provider: ownTaskFence(provider)}
			require.NoError(t, matchCleanupCompletedFences(expected, intent, provider))
		})
	}
	for name, change := range map[string]func(*cleanupSlotState){
		"unjoined": func(s *cleanupSlotState) { s.record.Run.Joined = false },
		"wrong-task": func(s *cleanupSlotState) {
			s.record.Task.TaskID = uuid.NewString()
			s.record.Incarnation = cleanupSlotTestIncarnation(s.record)
		},
		"wrong-birth": func(s *cleanupSlotState) {
			s.record.TaskBirth++
			s.record.Incarnation = cleanupSlotTestIncarnation(s.record)
		},
		"wrong-phase":     func(s *cleanupSlotState) { s.record.Confirmed = "archive-bound" },
		"missing-pending": func(s *cleanupSlotState) { s.record.Pending = nil },
		"wrong-purpose":   func(s *cleanupSlotState) { s.record.Pending.Purpose = "owner-release" },
		"missing-claim":   func(s *cleanupSlotState) { s.record.Pending.ClaimID = "" },
		"wrong-claim":     func(s *cleanupSlotState) { s.record.Pending.ClaimID = uuid.NewString() },
		"missing-lease":   func(s *cleanupSlotState) { s.record.Pending.LeaseID = 0 },
		"wrong-lease":     func(s *cleanupSlotState) { s.record.Pending.LeaseID++ },
		"stage-not-empty": func(s *cleanupSlotState) {
			s.record.Pending.StageID, s.record.Pending.RequestID = "stage", "request"
			s.record.Pending.AttemptID, s.record.Pending.Digest = uuid.NewString(), strings.Repeat("a", 64)
		},
		"missing-archive": func(s *cleanupSlotState) { s.record.Archive = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, provider := cleanupCompletedMetadata(t)
			change(s)
			require.Error(t, matchCleanupCompletedTermination(s, intent, provider))
		})
	}

	for _, which := range []string{"termination", "provider"} {
		for _, mutation := range []string{"missing", "lease", "version", "key", "noncanonical", "aba", "modified"} {
			t.Run(which+"-"+mutation, func(t *testing.T) {
				s, intent, provider := cleanupCompletedMetadata(t)
				expected := &cleanupCompletedTermination{termination: ownTaskFence(intent), provider: ownTaskFence(provider)}
				point := intent
				if which == "provider" {
					point = provider
				}
				switch mutation {
				case "missing":
					if which == "provider" {
						provider = nil
					} else {
						intent = nil
					}
				case "lease":
					point.Lease = 1
				case "version":
					point.Version = 1
				case "key":
					point.Key = append(point.Key, 'x')
				case "noncanonical":
					point.Value = append([]byte(" "), point.Value...)
				case "aba":
					point.CreateRevision++
					point.ModRevision++
				case "modified":
					point.ModRevision++
					point.Version++
				}
				require.Error(t, matchCleanupCompletedFences(expected, intent, provider))
				if mutation != "aba" && mutation != "modified" {
					require.Error(t, matchCleanupCompletedTermination(s, intent, provider))
				}
			})
		}
	}
	for _, mutation := range []string{"reserved", "terminated", "digest", "incomplete-provider", "provider-binding"} {
		t.Run(mutation, func(t *testing.T) {
			s, intentKV, providerKV := cleanupCompletedMetadata(t)
			intent, err := p.DecodeTaskRuntimeTerminationIntent(intentKV.Value)
			require.NoError(t, err)
			switch mutation {
			case "reserved":
				intent.Phase, intent.TerminalProof, intent.CleanupDigest = "reserved", nil, ""
			case "terminated":
				intent.Phase, intent.CleanupDigest = "terminated", ""
			case "digest":
				intent.CleanupDigest = strings.Repeat("b", 64)
			default:
				provider, err := decodeTaskRuntimeProviderCleanup(providerKV)
				require.NoError(t, err)
				if mutation == "incomplete-provider" {
					provider.Slots[0] = taskRuntimeCleanupSlot{Step: taskRuntimeCleanupSteps[0], State: "unstarted"}
				} else {
					provider.Binding.NodeUID = uuid.NewString()
				}
				wire, err := encodeTaskRuntimeProviderCleanup(provider)
				require.NoError(t, err)
				providerKV.Value = []byte(wire)
				intent.CleanupDigest = workspaceSyncIntentDigest(wire)
			}
			intentKV.Value, err = p.EncodeTaskRuntimeTerminationIntent(intent)
			require.NoError(t, err)
			require.Error(t, matchCleanupCompletedTermination(s, intentKV, providerKV))
		})
	}
	t.Run("missing-state-or-fences", func(t *testing.T) {
		_, intent, provider := cleanupCompletedMetadata(t)
		require.Error(t, matchCleanupCompletedTermination(nil, intent, provider))
		require.Error(t, matchCleanupCompletedFences(nil, intent, provider))
	})
}
