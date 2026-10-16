package etcd

import (
	"context"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestColdRuntimeQuotaReturnAdmission(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		result, err := b.SettleColdRuntimeQuota(context.Background(), WorkspaceIdentity{}, "intent", TaskReference{})
		require.Error(t, err)
		require.Equal(t, OutcomeUnknown, result.Outcome)
		require.Nil(t, result.Entry)
	}
}

// These values exercise codecs/relationships only; they are not death evidence.
func quotaReturnRecordFixture(t *testing.T) coldRuntimeQuotaReturnRecord {
	t.Helper()
	r := localReleaseTerminationFixture(t)
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", []byte("codec-only"), strings.Repeat("e", 64)
	task := localRuntimeTerminationTask(r)
	claim := taskCloseClaimWire{Task: task, ClaimID: task.TaskID, WorkerID: "worker", CreateRevision: 99, LeaseID: 2}
	release, err := newLocalOwnerReleaseRecord(r, 100, 103, claim)
	require.NoError(t, err)
	a := StageAttemptLocator{Namespace: task.Namespace, RestoreEpoch: task.RestoreEpoch, Partition: task.Partition, RequestID: "request", StageID: "cold_runtime_quota_return", AttemptID: task.TaskID}
	pub, prep := a, a
	pub.StageID, prep.StageID = enrolledPublicationStage, "cold_preparation_settle"
	stamp := func(rev int64) p.RecordStamp {
		return p.RecordStamp{CreateRevision: rev, ModRevision: rev, SHA256: strings.Repeat("a", 64)}
	}
	return coldRuntimeQuotaReturnRecord{Version: 1, Domain: "sandbox.cold-runtime-quota-return.v1", Namespace: task.Namespace, RestoreEpoch: task.RestoreEpoch, ProfileDigest: strings.Repeat("b", 64), Owner: ColdSlotOwner{IntentID: r.CreationIntentID, SandboxID: task.SandboxID, WorkspaceHash: r.Current.WorkspaceHash, RequestID: "request", Generation: r.Current.Generation, Partition: task.Partition}, Quota: ColdSlotReference{Kind: "quota", Partition: 7, ID: 0, Epoch: 1}, Preparation: ColdSlotReference{Kind: "preparation", ID: 0, Epoch: 1}, Runtime: release.Runtime, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, DataGateEpoch: r.Current.DataGateEpoch, Publication: pub.reference(strings.Repeat("c", 64)), PublicationStamp: stamp(20), Reservation: p.RecordStamp{CreateRevision: 10, ModRevision: 30, SHA256: strings.Repeat("d", 64)}, PreparationWork: p.RecordStamp{CreateRevision: 10, ModRevision: 30, SHA256: strings.Repeat("e", 64)}, PreparationSettlement: prep.reference(strings.Repeat("f", 64)), Release: release, ReleaseStamp: stamp(104), Attempt: a}
}

func TestColdRuntimeQuotaReturnOriginRelationships(t *testing.T) {
	r := quotaReturnRecordFixture(t)
	termination := localReleaseTerminationFixture(t)
	termination.Phase = "cleanup-complete"
	proof := &localOwnerReleaseProof{record: r.Release, termination: termination}
	reservation := ColdPreparationRecord{Owner: r.Owner, RestoreEpoch: r.RestoreEpoch}
	publication := RuntimeEnrolledPublicationRecord{RuntimePublicationRecord: RuntimePublicationRecord{IntentID: r.Owner.IntentID, SandboxID: r.Owner.SandboxID, WorkspaceHash: r.Owner.WorkspaceHash, Generation: r.Owner.Generation, RestoreEpoch: r.RestoreEpoch, Attempt: StageAttemptLocator{Namespace: r.Namespace, RestoreEpoch: r.RestoreEpoch, Partition: r.Owner.Partition, RequestID: r.Owner.RequestID}, Runtime: r.Runtime, DataGateEpoch: r.DataGateEpoch}, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, EnrollmentDigest: termination.EnrollmentDigest, ObserverCertificateDigest: termination.ObserverCertificateDigest}
	require.True(t, coldQuotaReturnOriginMatches(reservation, publication, proof))
	for _, change := range []func(*p.TaskRuntimeTerminationIntent){
		func(t *p.TaskRuntimeTerminationIntent) { t.CreationIntentID = "other" },
		func(t *p.TaskRuntimeTerminationIntent) { t.Current.Generation++ },
		func(t *p.TaskRuntimeTerminationIntent) { t.Current.DataGateEpoch++ },
		func(t *p.TaskRuntimeTerminationIntent) { t.Current.Runtime.UID = t.NodeUID },
		func(t *p.TaskRuntimeTerminationIntent) { t.Current.Runtime.BootID = t.NodeBootID },
		func(t *p.TaskRuntimeTerminationIntent) { t.NodeUID = t.Current.Runtime.UID },
		func(t *p.TaskRuntimeTerminationIntent) { t.NodeBootID = t.Current.Runtime.BootID },
		func(t *p.TaskRuntimeTerminationIntent) { t.EnrollmentDigest = strings.Repeat("0", 64) },
		func(t *p.TaskRuntimeTerminationIntent) { t.ObserverCertificateDigest = strings.Repeat("0", 64) },
		func(t *p.TaskRuntimeTerminationIntent) { t.Phase = "terminated" },
	} {
		copy := *proof
		change(&copy.termination)
		require.False(t, coldQuotaReturnOriginMatches(reservation, publication, &copy))
	}
}

func TestColdRuntimeQuotaReturnBudget(t *testing.T) {
	b, _ := recyclingLocalDraftFixture(t)
	s := &Stage{guardKey: b.namespace.Root() + "guard", guardValue: strings.Repeat("g", 4096), guardRevision: 3, leaseID: 1, receiptKey: b.namespace.Root() + "receipt", committedValue: strings.Repeat("r", 4096)}
	// Eight bounded evidence triples and two absent points are the complete
	// approved domain envelope; all three success records use their full cap.
	for i := 0; i < 8; i++ {
		key := b.namespace.Root() + strings.Repeat("k", i+1)
		kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(strings.Repeat("v", 16384)), ModRevision: 2}
		s.mutation.Comparisons = append(s.mutation.Comparisons, coldComparisons(kv)...)
	}
	for _, name := range []string{"absent-free", "absent-return"} {
		s.mutation.Comparisons = append(s.mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(b.namespace.Root()+name), "=", 0))
	}
	for _, name := range []string{"slot", "free", "return"} {
		s.mutation.Writes = append(s.mutation.Writes, Write{Key: b.namespace.Root() + name, Value: []byte(strings.Repeat("w", 16384))})
	}
	request, responses, err := b.coldQuotaReturnProjection(s)
	require.NoError(t, err)
	require.Len(t, request.Compare, 34)
	require.Len(t, request.Success, 4)
	require.Len(t, request.Failure, 4)
	require.Equal(t, 42, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.LessOrEqual(t, request.Size(), 262144)
	for _, response := range responses {
		require.LessOrEqual(t, response.Size(), 262144)
	}
	t.Logf("quota return nodes=42 compares=34 success=4 failure=4 request=%d success_response=%d failure_response=%d", request.Size(), responses[0].Size(), responses[1].Size())
	s.mutation.Comparisons = s.mutation.Comparisons[1:]
	_, _, err = b.coldQuotaReturnProjection(s)
	require.Error(t, err, "dropping a proof comparison is rejected")
}

func TestColdRuntimeQuotaReturnCanonicalRecord(t *testing.T) {
	r := quotaReturnRecordFixture(t)
	wire, err := encodeColdRuntimeQuotaReturn(r)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(wire), Version: 1, CreateRevision: 105, ModRevision: 105}
	got, err := decodeColdRuntimeQuotaReturn(kv)
	require.NoError(t, err)
	require.Equal(t, r, got)
	for _, bad := range []string{" " + wire, strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(wire, `"version":1`, `"version":1,"unknown":0`, 1)} {
		copy := *kv
		copy.Value = []byte(bad)
		_, err := decodeColdRuntimeQuotaReturn(&copy)
		require.Error(t, err)
	}
	for _, change := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 1 }, func(k *mvccpb.KeyValue) { k.Version++ }, func(k *mvccpb.KeyValue) { k.ModRevision++ }, func(k *mvccpb.KeyValue) { k.CreateRevision, k.ModRevision = 104, 104 }} {
		copy := *kv
		change(&copy)
		_, err := decodeColdRuntimeQuotaReturn(&copy)
		require.Error(t, err)
	}
}

func TestColdRuntimeQuotaReturnOriginalBinding(t *testing.T) {
	r := quotaReturnRecordFixture(t)
	for _, change := range []func(*coldRuntimeQuotaReturnRecord){
		func(r *coldRuntimeQuotaReturnRecord) { r.Owner.IntentID = "" },
		func(r *coldRuntimeQuotaReturnRecord) { r.Owner.SandboxID = "other" },
		func(r *coldRuntimeQuotaReturnRecord) { r.Owner.WorkspaceHash = strings.Repeat("0", 64) },
		func(r *coldRuntimeQuotaReturnRecord) { r.Owner.Generation++ },
		func(r *coldRuntimeQuotaReturnRecord) { r.Owner.Partition++ },
		func(r *coldRuntimeQuotaReturnRecord) { r.Runtime.UID = r.NodeUID },
		func(r *coldRuntimeQuotaReturnRecord) { r.Runtime.BootID = r.NodeBootID },
		func(r *coldRuntimeQuotaReturnRecord) { r.NodeUID = r.Runtime.UID },
		func(r *coldRuntimeQuotaReturnRecord) { r.NodeBootID = r.Runtime.BootID },
		func(r *coldRuntimeQuotaReturnRecord) { r.RestoreEpoch = "other" },
		func(r *coldRuntimeQuotaReturnRecord) { r.Attempt.StageID = "other" },
		func(r *coldRuntimeQuotaReturnRecord) { r.PreparationSettlement.StageID = "other" },
		func(r *coldRuntimeQuotaReturnRecord) { r.Quota.Epoch = 0 },
	} {
		copy := r
		change(&copy)
		_, err := encodeColdRuntimeQuotaReturn(copy)
		require.Error(t, err, "foreign original binding cannot become a return record")
	}
}
