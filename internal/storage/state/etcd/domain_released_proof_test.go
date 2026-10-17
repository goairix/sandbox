package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type releasedProofTestFixture struct {
	*releasedReadTestFixture
	termination p.TaskRuntimeTerminationIntent
	keys        p.TaskRuntimeTerminationKeys
	history     []string
	provider    string
	release     string
	observerKey ed25519.PrivateKey
	observer    p.NodeObserverIdentity
}

// This signed metadata model proves the production cryptographic join and
// observation fences, never physical cleanup. The six history artifacts that
// the original cold loader authenticates only by signed immutable stamps use
// explicit model JSON; no unrelated historical-schema hardening is implied.
func newReleasedProofTestFixture(t *testing.T) *releasedProofTestFixture {
	t.Helper()
	f := newReleasedReadTestFixture(t)
	f.seedCurrent(t, PhaseCleanupPending)
	task := f.seed.task
	digest := strings.Repeat("d", 64)
	r := localReleaseTerminationFixture(t)
	r.Current.Namespace, r.Current.RestoreEpoch = f.b.namespace.Root(), f.b.restoreEpoch
	r.Current.AuthorityID, r.Current.Target = f.authority, "released-read"
	r.Current.TaskID, r.Current.SandboxID = task.Reference.TaskID, task.Reference.SandboxID
	r.Current.WorkspaceHash, r.Current.Generation = task.WorkspaceHash, task.Generation
	r.Current.DataGateEpoch, r.Current.ControlRevision = task.DataGateEpoch, task.ControlRevision
	r.Current.TaskDigest = workspaceSyncIntentDigest(f.seed.wires[0])
	r.Current.Runtime = p.RuntimeReference(task.Runtime)
	r.Current.ClaimCreateRevision = f.server.point(f.keys[2]).ModRevision + 1
	r.Partition, r.TaskBirth, r.CreationIntentID = task.Reference.Partition, f.seed.birth, task.CreationIntentID
	f.server.put(f.b.namespace.Root()+"model-claim-order", "claim", 0)
	claim := taskCloseClaimWire{Task: task.Reference, ClaimID: r.Current.ClaimID, WorkerID: r.Current.WorkerID, CreateRevision: r.Current.ClaimCreateRevision, LeaseID: r.Current.LeaseID}

	now := time.Now().UTC().Truncate(time.Second)
	observerPublic, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	certificate, err := p.SignNodeObserverCertificate(f.root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Role: "node_observer", Namespace: f.b.namespace.Root(), AuthorityID: f.authority, Target: "released-read", RestoreEpoch: f.b.restoreEpoch, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, PublicKey: observerPublic, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)})
	require.NoError(t, err)
	observer, err := f.b.enrolledVerifier.VerifyNodeObserverCertificate(certificate, r.NodeUID, r.NodeBootID, now)
	require.NoError(t, err)
	r.ObserverCertificateDigest = observer.Digest()
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	require.NoError(t, err)
	index := RuntimeIndexRecord{Version: 1, IntentID: task.CreationIntentID, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, Runtime: task.Runtime}
	indexWire, err := encodeDomainRecord(index)
	require.NoError(t, err)
	f.server.put(keys.RuntimeIndex, indexWire, 0)

	complete := taskWorkspaceSyncLocalComplete{Version: 1, Domain: "sandbox.workspace-sync-local-complete.v1", Task: task.Reference, TaskBirth: f.seed.birth, Binding: digest, Claim: claim, ProgressDigest: digest, EvidenceDigest: digest, CheckpointRevision: f.seed.birth, CheckpointDigest: digest, RootDigest: digest}
	completeWire, err := encodeOperationMetadata(complete, 8192)
	require.NoError(t, err)
	f.server.put(keys.LocalComplete, completeWire, 0)
	for _, key := range []string{keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Archive} {
		f.server.put(key, `{"model":"signed-stamp-only"}`, 0)
	}
	enrollmentKey, _, err := f.b.namespace.runtimeEnrollmentKeys(task.Reference.Partition, task.CreationIntentID)
	require.NoError(t, err)
	enrollment := RuntimeEnrollmentRecord{Version: 2, IntentID: task.CreationIntentID, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, Runtime: task.Runtime, ObserverCertificate: certificate, Receipt: json.RawMessage(`{"model":"receipt"}`)}
	enrollmentWire, err := encodeEnrolledRecord(enrollment)
	require.NoError(t, err)
	f.server.put(enrollmentKey, enrollmentWire, 0)

	// All stamps come from actual stored points, including create/mod revisions
	// and the full canonical-byte digest; none is inferred from TaskBirth.
	for _, binding := range []struct {
		key   string
		stamp *p.RecordStamp
	}{{keys.Task, &r.Authority.Task}, {keys.CleanupIntent, &r.Authority.CleanupIntent}, {keys.TaskLink, &r.Authority.TaskLink}, {keys.Placement, &r.Authority.Placement}, {keys.Control, &r.Authority.Control}, {keys.Owner, &r.Authority.Owner}, {keys.Fence, &r.Authority.Fence}, {keys.RuntimeIndex, &r.Authority.RuntimeIndex}, {keys.LocalComplete, &r.Authority.LocalComplete}, {keys.Quiescence, &r.Authority.Quiescence}, {keys.QuiescenceReceipt, &r.Authority.QuiescenceReceipt}, {keys.QuiescenceReservation, &r.Authority.QuiescenceReservation}, {keys.FinalSync, &r.Authority.FinalSync}, {keys.Issuer, &r.Authority.Issuer}, {keys.Archive, &r.Authority.Archive}} {
		*binding.stamp = localRuntimeStamp(f.server.point(binding.key))
	}
	reserved, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(keys.Intent, string(reserved), 0)
	intent := f.server.point(keys.Intent)
	proof, err := p.SignNodeTerminationProof(observerKey, observer, p.NodeTerminationProofClaims{Version: 1, Purpose: "task_runtime_terminated", Current: r.Current, Partition: r.Partition, IntentCreateRevision: intent.CreateRevision, IntentDigest: workspaceSyncIntentDigest(string(reserved)), EnrollmentDigest: r.EnrollmentDigest, ObserverCertificateDigest: observer.Digest(), TicketDigest: digest, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, PodUID: task.Runtime.UID, PodResourceVersion: "model-rv", ProviderDeletingAt: now.Add(-time.Second), PID1Dead: true, CgroupEmpty: true, ObservedAt: now})
	require.NoError(t, err)
	provider := taskRuntimeProviderCleanupRecord{Version: 1, Task: task.Reference, TaskBirth: f.seed.birth, Binding: localRuntimeProviderBinding(r, f.b.enrolledPublication.ContractDigest), Descriptor: []byte(`{"model":"cleanup"}`)}
	for i, step := range taskRuntimeCleanupSteps {
		provider.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "skipped"}
		if i == 0 || i == 1 || i == 6 {
			provider.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "complete", Kind: "Pod", Name: "model-pod", UID: task.Runtime.UID, ResourceVersion: "model-rv", EffectID: r.Current.CommandID, Claim: &claim}
		}
	}
	providerWire, err := encodeTaskRuntimeProviderCleanup(provider)
	require.NoError(t, err)
	_, providerKey, releaseKey, err := f.b.namespace.localRuntimeReleaseKeys(task.Reference)
	require.NoError(t, err)
	f.server.put(providerKey, providerWire, 0)
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", proof, workspaceSyncIntentDigest(providerWire)
	terminal, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(keys.Intent, string(terminal), 0)
	intent = f.server.point(keys.Intent)
	require.NoError(t, f.b.verifyLocalRuntimeProof(r, intent, certificate))
	release, err := newLocalOwnerReleaseRecord(r, intent.CreateRevision, intent.ModRevision, claim)
	require.NoError(t, err)
	releaseWire, err := encodeLocalOwnerRelease(release)
	require.NoError(t, err)
	_, err = f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys.Owner), clientv3.OpDelete(keys.Control), clientv3.OpDelete(keys.RuntimeIndex), clientv3.OpPut(releaseKey, releaseWire)).Commit()
	require.NoError(t, err)
	return &releasedProofTestFixture{releasedReadTestFixture: f, termination: r, keys: keys, history: []string{keys.Task, keys.LocalComplete, keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Archive, enrollmentKey}, provider: providerKey, release: releaseKey, observerKey: observerKey, observer: observer}
}

func TestSandboxReleasedReadSignedModel(t *testing.T) {
	f := newReleasedProofTestFixture(t)
	before := len(backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server}))
	result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, result)
	requests := backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server})[before:]
	require.Len(t, requests, 8, "the complete proof has a fixed finite RPC path")
	for _, request := range requests {
		require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
		require.LessOrEqual(t, request.Size(), 256*1024)
		for _, branch := range [][]*pb.RequestOp{request.Success, request.Failure} {
			for _, op := range branch {
				point := op.GetRequestRange()
				require.NotNil(t, point, "Released observation performs no writes or leases")
				require.Empty(t, point.RangeEnd)
				require.Zero(t, point.Revision)
				require.False(t, point.Serializable)
			}
		}
	}
	final := requests[len(requests)-1]
	mods := make(map[string]int64)
	for _, cmp := range final.Compare {
		if cmp.Target == pb.Compare_MOD {
			mods[string(cmp.Key)] = cmp.GetModRevision()
		}
	}
	retained := append(append([]string{}, f.history...), f.seed.keys[1], f.seed.keys[2], f.seed.keys[3], f.provider, f.release, f.keys.Intent, f.keys.Placement, f.keys.Fence)
	for _, key := range retained {
		require.Equal(t, f.server.point(key).ModRevision, mods[key], "final request must retain original MVCC pin for %s", key)
	}
}

func TestSandboxReleasedReadEnvelopeEveryRPC(t *testing.T) {
	for rpc := 1; rpc <= 8; rpc++ {
		for _, fault := range []string{"cluster", "revision", "top-unknown", "nested-cluster", "nested-member", "nested-unknown", "count", "recursive"} {
			t.Run(fmt.Sprintf("rpc-%d-%s", rpc, fault), func(t *testing.T) {
				f := newReleasedProofTestFixture(t)
				calls := 0
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func([]clientv3.Op) bool { calls++; return calls == rpc }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					switch fault {
					case "cluster":
						r.Header.ClusterId++
					case "revision":
						r.Header.Revision = 0
					case "top-unknown":
						r.XXX_unrecognized = []byte{8, 1}
					case "nested-cluster":
						r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{ClusterId: r.Header.ClusterId + 1, Revision: r.Header.Revision}
					case "nested-member":
						r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{ClusterId: r.Header.ClusterId, MemberId: r.Header.MemberId + 1, Revision: r.Header.Revision}
					case "nested-unknown":
						r.Responses[0].GetResponseRange().XXX_unrecognized = []byte{8, 1}
					case "count":
						r.Responses[0].GetResponseRange().Count++
					case "recursive":
						r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(r)}}
					}
					return r, nil
				}}
				result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
				require.Nil(t, result)
				require.Error(t, err)
				require.Equal(t, rpc, calls, "malformed envelope cannot trigger a retry or further history read")
			})
		}
	}
}

func TestSandboxReleasedReadLateCloseAndDeadline(t *testing.T) {
	for _, action := range []string{"Close", "caller-deadline"} {
		t.Run(action, func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			calls := 0
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func([]clientv3.Op) bool { calls++; return calls == 7 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				entered <- struct{}{}
				<-release
				return r, err
			}}
			ctx := context.Background()
			if action == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				t.Cleanup(cancel)
			}
			done, closed := make(chan error, 1), make(chan error, 1)
			started := time.Now()
			go func() {
				_, err := f.b.ReadSandboxReleased(ctx, f.seed.task.Reference.SandboxID)
				done <- err
			}()
			workerTestReceive(t, entered)
			f.b.archiveLife.mu.Lock()
			count := len(f.b.snapshotReads.calls)
			var owned context.Context
			for call := range f.b.snapshotReads.calls {
				owned = call.context
			}
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, 1, count)
			deadline, bounded := owned.Deadline()
			require.True(t, bounded)
			require.LessOrEqual(t, deadline.Sub(started), 8*f.b.requestTimeout+time.Second+100*time.Millisecond)
			if action == "Close" {
				go func() { closed <- f.b.Close() }()
			}
			workerTestReceive(t, owned.Done())
			select {
			case err := <-done:
				t.Fatalf("terminal reader returned while its original history client was blocked: %v", err)
			default:
			}
			if action == "Close" {
				select {
				case err := <-closed:
					t.Fatalf("Close returned while its original history client was blocked: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			unblock()
			require.Error(t, workerTestReceive(t, done))
			if action == "Close" {
				require.NoError(t, workerTestReceive(t, closed))
			}
		})
	}
}

func TestSandboxReleasedReadEveryHistoricalPin(t *testing.T) {
	for i := 0; i < 15; i++ {
		for _, change := range []string{"rewrite", "ABA"} {
			t.Run(fmt.Sprintf("point-%02d-%s", i, change), func(t *testing.T) {
				f := newReleasedProofTestFixture(t)
				points := append(append([]string{}, f.history...), f.seed.keys[1], f.seed.keys[2], f.seed.keys[3], f.provider, f.release, f.keys.Intent)
				key := points[i]
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 || len(ops) == 7 }, before: func() {
					old := f.server.point(key)
					if change == "ABA" {
						_, err := f.b.client.KV.(*faultKV).KV.Txn(context.Background()).Then(clientv3.OpDelete(key)).Commit()
						require.NoError(t, err)
					}
					f.server.put(key, string(old.Value), 0)
				}}
				result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
				require.Nil(t, result)
				require.ErrorIs(t, err, ErrConflict, "final confirmation must pin every actually authenticated MVCC point")
			})
		}
	}
}

// Re-signing a changed reserved origin makes these adversarial fixtures test
// the cohort/retained-state join independently of proof signature validation.
// The release record is then recomputed from the actual changed termination.
func (f *releasedProofTestFixture) replaceTermination(t *testing.T, r p.TaskRuntimeTerminationIntent, sign bool) {
	t.Helper()
	if sign {
		original := r
		original.Phase, original.TerminalProof, original.CleanupDigest = "reserved", nil, ""
		wire, err := p.EncodeTaskRuntimeTerminationIntent(original)
		require.NoError(t, err)
		var envelope struct {
			Claims    p.NodeTerminationProofClaims `json:"claims"`
			Signature []byte                       `json:"signature"`
		}
		require.NoError(t, json.Unmarshal(f.termination.TerminalProof, &envelope))
		envelope.Claims.Current = r.Current
		envelope.Claims.IntentDigest = workspaceSyncIntentDigest(string(wire))
		proof, err := p.SignNodeTerminationProof(f.observerKey, f.observer, envelope.Claims)
		require.NoError(t, err)
		r.TerminalProof = proof
	}
	old, err := decodeLocalOwnerRelease(f.server.point(f.release))
	require.NoError(t, err)
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(f.keys.Intent, string(wire), 0)
	kv := f.server.point(f.keys.Intent)
	release, err := newLocalOwnerReleaseRecord(r, kv.CreateRevision, kv.ModRevision, old.Claim)
	require.NoError(t, err)
	releaseWire, err := encodeLocalOwnerRelease(release)
	require.NoError(t, err)
	_, err = f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(f.release)).Commit()
	require.NoError(t, err)
	f.server.put(f.release, releaseWire, 0)
}

func TestSandboxReleasedReadSignedAuthorityJoins(t *testing.T) {
	for _, fault := range []string{"bad-signature", "task-birth", "task-stamp", "intent-stamp", "link-stamp", "placement-stamp", "fence-stamp", "creation-intent", "generation", "workspace"} {
		t.Run(fault, func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			r := f.termination
			sign := true
			switch fault {
			case "bad-signature":
				var envelope struct {
					Claims    p.NodeTerminationProofClaims `json:"claims"`
					Signature []byte                       `json:"signature"`
				}
				require.NoError(t, json.Unmarshal(r.TerminalProof, &envelope))
				envelope.Signature[0] ^= 1
				wire, err := json.Marshal(envelope)
				require.NoError(t, err)
				r.TerminalProof, sign = wire, false
			case "task-birth":
				r.TaskBirth++
			case "task-stamp":
				r.Authority.Task.CreateRevision++
				r.Authority.Task.ModRevision++
			case "intent-stamp":
				r.Authority.CleanupIntent.ModRevision++
			case "link-stamp":
				r.Authority.TaskLink.SHA256 = strings.Repeat("f", 64)
			case "placement-stamp":
				r.Authority.Placement.ModRevision++
			case "fence-stamp":
				r.Authority.Fence.SHA256 = strings.Repeat("f", 64)
			case "creation-intent":
				r.CreationIntentID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			case "generation":
				r.Current.Generation++
			case "workspace":
				r.Current.WorkspaceHash = "aa" + strings.Repeat("f", 62)
			}
			f.replaceTermination(t, r, sign)
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.Nil(t, result)
			require.Error(t, err, "validly signed different authority must not authenticate this sandbox release")
		})
	}
}

func TestSandboxReleasedReadMissingProofPoints(t *testing.T) {
	for i := 0; i < 15; i++ {
		t.Run(fmt.Sprintf("missing-point-%02d", i), func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			points := append(append([]string{}, f.history...), f.seed.keys[1], f.seed.keys[2], f.seed.keys[3], f.provider, f.release, f.keys.Intent)
			_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(points[i])).Commit()
			require.NoError(t, err)
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.Nil(t, result)
			require.Error(t, err, "partial terminal metadata must remain unavailable")
		})
	}
}

func TestSandboxReleasedReadFinalCurrentFences(t *testing.T) {
	for _, fault := range []string{"owner-reappears", "control-reappears", "index-reappears", "fence-advances", "placement-rewrite", "placement-ABA", "restore-changes", "identity-changes"} {
		t.Run(fault, func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			want := ErrCorruptRecord
			f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 || len(ops) == 7 }, before: func() {
				switch fault {
				case "owner-reappears":
					f.server.put(f.keys.Owner, "{}", 0)
				case "control-reappears":
					f.server.put(f.keys.Control, "{}", 0)
				case "index-reappears":
					f.server.put(f.keys.RuntimeIndex, "{}", 0)
				case "fence-advances":
					fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: f.seed.task.WorkspaceHash, RestoreEpoch: f.b.restoreEpoch, Generation: f.seed.task.Generation + 1}
					wire, err := encodeDomainRecord(fence)
					require.NoError(t, err)
					f.server.put(f.keys.Fence, wire, 0)
					want = ErrConflict
				case "placement-rewrite", "placement-ABA":
					old := f.server.point(f.keys.Placement)
					if fault == "placement-ABA" {
						_, err := f.b.client.KV.(*faultKV).KV.Txn(context.Background()).Then(clientv3.OpDelete(f.keys.Placement)).Commit()
						require.NoError(t, err)
					}
					f.server.put(f.keys.Placement, string(old.Value), 0)
					want = ErrConflict
				case "restore-changes":
					f.server.put(f.b.restoreKey, "new-epoch", 0)
					want = ErrIdentityMismatch
				case "identity-changes":
					f.server.put(f.b.identityKey, "{}", 0)
					want = ErrIdentityMismatch
				}
			}}
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.Nil(t, result)
			require.ErrorIs(t, err, want)
		})
	}
}

// A successor owns its own ID. Its complete current quartet is born together
// with the retained fence's next modification; looking up the old ID must not
// misclassify ordinary reuse as corruption or follow a concurrent third owner.
func (f *releasedProofTestFixture) seedSuccessor(t *testing.T, phase SandboxPhase, generation int64, successorIDs ...string) [4]string {
	t.Helper()
	task := f.seed.task
	sandbox, intent := "successor-sandbox", "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if len(successorIDs) != 0 {
		sandbox = successorIDs[0]
	}
	runtime := &RuntimeReference{ID: "successor-runtime", UID: "abababab-abab-4bab-8bab-abababababab", BootID: "bcbcbcbc-bcbc-4cbc-8cbc-bcbcbcbcbcbc"}
	if phase == PhasePublishing {
		runtime = nil
	}
	owner := WorkspaceOwnerRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, SandboxID: sandbox, IntentID: intent, RestoreEpoch: f.b.restoreEpoch, Generation: generation, Runtime: runtime}
	fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: f.b.restoreEpoch, Generation: generation}
	placement := SandboxPlacementRecord{Version: 1, SandboxID: sandbox, WorkspaceHash: task.WorkspaceHash, IntentID: intent, RestoreEpoch: f.b.restoreEpoch, Partition: task.Reference.Partition, Generation: generation}
	control := SandboxControlRecord{Version: 1, SandboxID: sandbox, WorkspaceHash: task.WorkspaceHash, IntentID: intent, RestoreEpoch: f.b.restoreEpoch, Generation: generation, DataGateEpoch: task.DataGateEpoch, Phase: phase, Snapshot: task.Snapshot, Runtime: runtime, ExpiresAt: task.ExpiresAt}
	keys, err := f.b.readViewKeys(task.WorkspaceHash, sandbox)
	require.NoError(t, err)
	var ops []clientv3.Op
	for i, record := range []interface{ Validate() error }{owner, fence, placement, control} {
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		ops = append(ops, clientv3.OpPut(keys[i], wire))
	}
	_, err = f.b.client.KV.Txn(context.Background()).Then(ops...).Commit()
	require.NoError(t, err)
	return keys
}

func TestSandboxReleasedReadSuccessor(t *testing.T) {
	for _, phase := range []SandboxPhase{PhasePublishing, PhaseActive, PhaseDestroying} {
		t.Run(string(phase), func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			keys := f.seedSuccessor(t, phase, f.seed.task.Generation+1)
			before := len(backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server}))
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.NoError(t, err)
			require.NotNil(t, result, "authenticated old release remains terminal after legitimate workspace reuse")
			requests := backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server})[before:]
			require.Len(t, requests, 8)
			final := requests[7]
			require.Len(t, final.Success, 7)
			mods := map[string]int64{}
			for _, cmp := range final.Compare {
				if cmp.Target == pb.Compare_MOD {
					mods[string(cmp.Key)] = cmp.GetModRevision()
				}
			}
			require.Equal(t, f.server.point(keys[0]).ModRevision, mods[keys[0]])
			require.Equal(t, f.server.point(keys[1]).ModRevision, mods[keys[1]])
			view, err := f.b.ReadSandboxView(context.Background(), "successor-sandbox")
			require.NoError(t, err)
			require.NotNil(t, view, "old-ID observation leaves the successor untouched")
		})
	}
	t.Run("later-generation-without-current-owner", func(t *testing.T) {
		f := newReleasedProofTestFixture(t)
		fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: f.seed.task.WorkspaceHash, RestoreEpoch: f.b.restoreEpoch, Generation: f.seed.task.Generation + 3}
		wire, err := encodeDomainRecord(fence)
		require.NoError(t, err)
		f.server.put(f.keys.Fence, wire, 0)
		result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
		require.NoError(t, err)
		require.NotNil(t, result, "old-ID terminal observation never asserts that a successor was safely released or workspace is free")
	})
	t.Run("successor-current-records-may-advance-modification", func(t *testing.T) {
		f := newReleasedProofTestFixture(t)
		keys := f.seedSuccessor(t, PhaseActive, f.seed.task.Generation+1)
		birth := f.server.point(keys[1]).ModRevision
		for _, key := range []string{keys[0], keys[3]} {
			f.server.put(key, string(f.server.point(key).Value), 0)
			require.Equal(t, birth, f.server.point(key).CreateRevision)
			require.Greater(t, f.server.point(key).ModRevision, birth)
		}
		before := len(backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server}))
		result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
		require.NoError(t, err)
		require.NotNil(t, result, "successor owner and control remain mutable after their common creation")
		requests := backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server})[before:]
		require.Len(t, requests, 8)
		require.Len(t, requests[7].Success, 7)
	})
}

func TestSandboxReleasedReadSuccessorCorruption(t *testing.T) {
	for _, fault := range []string{"owner-only", "new-control-only", "new-control-mismatch", "new-placement-missing", "new-owner-ABA", "same-old-id", "fence-recreated", "old-index-revived", "final-owner-switch"} {
		t.Run(fault, func(t *testing.T) {
			f := newReleasedProofTestFixture(t)
			keys := f.seedSuccessor(t, PhaseActive, f.seed.task.Generation+1)
			switch fault {
			case "owner-only":
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys[2]), clientv3.OpDelete(keys[3])).Commit()
				require.NoError(t, err)
			case "new-control-only":
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys[0])).Commit()
				require.NoError(t, err)
				// An absent owner grants no successor diagnosis. Reappearance of
				// this old ID's control must still defeat the terminal result.
				f.server.put(f.keys.Control, string(f.server.point(keys[3]).Value), 0)
			case "new-control-mismatch":
				var control SandboxControlRecord
				require.NoError(t, decodeDomainRecord(f.server.point(keys[3]), &control))
				control.IntentID = "different-intent"
				wire, err := encodeDomainRecord(control)
				require.NoError(t, err)
				f.server.put(keys[3], wire, 0)
			case "new-placement-missing":
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys[2])).Commit()
				require.NoError(t, err)
			case "new-owner-ABA":
				wire := string(f.server.point(keys[0]).Value)
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys[0])).Commit()
				require.NoError(t, err)
				f.server.put(keys[0], wire, 0)
			case "same-old-id":
				var owner WorkspaceOwnerRecord
				require.NoError(t, decodeDomainRecord(f.server.point(keys[0]), &owner))
				owner.SandboxID = f.seed.task.Reference.SandboxID
				wire, err := encodeDomainRecord(owner)
				require.NoError(t, err)
				f.server.put(keys[0], wire, 0)
			case "fence-recreated":
				wire := string(f.server.point(keys[1]).Value)
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(keys[1])).Commit()
				require.NoError(t, err)
				f.server.put(keys[1], wire, 0)
			case "old-index-revived":
				f.server.put(f.keys.RuntimeIndex, "{}", 0)
			case "final-owner-switch":
				base := f.b.client.KV
				f.b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool { return len(ops) == 5 || len(ops) == 7 }, before: func() {
					_, err := base.Txn(context.Background()).Then(clientv3.OpDelete(keys[0])).Commit()
					require.NoError(t, err)
					f.seedSuccessor(t, PhasePublishing, f.seed.task.Generation+2, "third-sandbox")
				}}
			}
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.Nil(t, result)
			require.Error(t, err, "incomplete or changed successor state cannot authenticate the old ID observation")
		})
	}
}
