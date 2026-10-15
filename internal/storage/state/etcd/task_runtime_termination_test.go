package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type localReleaseTestIssuer struct{}

func (*localReleaseTestIssuer) Certificate(context.Context) ([]byte, error) {
	panic("construction must not invoke issuer")
}
func (*localReleaseTestIssuer) SignRuntimeTermination(context.Context, string, p.TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	panic("construction must not invoke issuer")
}

func TestLocalRuntimeReleaseProfile(t *testing.T) {
	require.NoError(t, validateLocalRuntimeRelease(Options{}))
	for _, o := range []Options{
		{TaskRuntimeTerminationIssuer: (*localReleaseTestIssuer)(nil)},
		{LocalRuntimeRelease: &LocalRuntimeReleaseOptions{}},
		{LocalRuntimeRelease: &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}},
	} {
		require.ErrorIs(t, validateLocalRuntimeRelease(o), ErrInvalidConfiguration)
	}
	for _, limit := range []uint32{0, 33} {
		require.Error(t, (&LocalRuntimeReleaseOptions{MaxActive: limit, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}).validate())
	}
	for _, timeout := range []time.Duration{0, -1, 30*time.Second + 1} {
		require.Error(t, (&LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: timeout, Provider: &k.TaskRuntimeCleanupClient{}}).validate())
	}
	require.NoError(t, (&LocalRuntimeReleaseOptions{MaxActive: 32, Timeout: 30 * time.Second, Provider: &k.TaskRuntimeCleanupClient{}}).validate())
}

func localReleaseRecordFixture(t *testing.T) taskRuntimeProviderCleanupRecord {
	t.Helper()
	seed := recoveryTestSeed(t)
	d := strings.Repeat("a", 64)
	r := taskRuntimeProviderCleanupRecord{Version: 1, Task: seed.Task, TaskBirth: seed.TaskBirth, Binding: k.TaskRuntimeCleanupBinding{
		Authority:  p.TrustBinding{Namespace: seed.Task.Namespace, RestoreEpoch: seed.Task.RestoreEpoch, AuthorityID: "authority", Target: "target"},
		Runtime:    p.RuntimeReference{ID: "pod", UID: "11111111-1111-4111-8111-111111111111", BootID: "22222222-2222-4222-8222-222222222222"},
		Generation: 1, DataGateEpoch: 1, TaskID: seed.Task.TaskID, LogicalID: seed.Task.SandboxID, WorkspaceHash: d, CreationIntentID: "33333333-3333-4333-8333-333333333333",
		NodeUID: "44444444-4444-4444-8444-444444444444", NodeBootID: "55555555-5555-4555-8555-555555555555", EnrollmentDigest: d, ContractDigest: d,
	}, Descriptor: []byte(`{"version":1}`)}
	for i, step := range taskRuntimeCleanupSteps {
		r.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "unstarted"}
	}
	return r
}

func TestLocalRuntimeReleaseRecords(t *testing.T) {
	r := localReleaseRecordFixture(t)
	wire, err := encodeTaskRuntimeProviderCleanup(r)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(wire), Version: 1, CreateRevision: 10, ModRevision: 10}
	got, err := decodeTaskRuntimeProviderCleanup(kv)
	require.NoError(t, err)
	require.Equal(t, r, got)
	for _, bad := range []string{" " + wire, strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(wire, `"version":1`, `"version":1,"unknown":0`, 1), strings.Replace(wire, `"slots":[`, `"slots":null,"extra":[`, 1), strings.Replace(wire, `"state":"unstarted"`, `"state":"complete"`, 1), strings.Repeat("x", 16385)} {
		copy := *kv
		copy.Value = []byte(bad)
		_, err := decodeTaskRuntimeProviderCleanup(&copy)
		require.Error(t, err)
	}
	for _, mutate := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 1 }, func(k *mvccpb.KeyValue) { k.ModRevision++ }, func(k *mvccpb.KeyValue) { k.Version = 0 }} {
		copy := *kv
		mutate(&copy)
		_, err := decodeTaskRuntimeProviderCleanup(&copy)
		require.Error(t, err)
	}
	r.Slots[1].Step = r.Slots[0].Step
	_, err = encodeTaskRuntimeProviderCleanup(r)
	require.Error(t, err)
}

func TestLocalRuntimeReleaseOpaqueBinding(t *testing.T) {
	r := localReleaseRecordFixture(t)
	r.Binding.Authority.AuthorityID = "authority / 生产"
	r.Binding.Authority.Target = strings.Repeat("t", 128)
	r.Task.RestoreEpoch, r.Binding.Authority.RestoreEpoch = "restore-non-uuid", "restore-non-uuid"
	_, err := encodeTaskRuntimeProviderCleanup(r)
	require.NoError(t, err, "opaque trust identities are compared exactly, never joined into keys")
	for _, bad := range []string{"", strings.Repeat("x", 129), "line\nfeed", "nul\x00byte", string([]byte{0xff})} {
		copy := r
		copy.Binding.Authority.AuthorityID = bad
		_, err := encodeTaskRuntimeProviderCleanup(copy)
		require.Error(t, err)
		copy = r
		copy.Binding.Authority.Target = bad
		_, err = encodeTaskRuntimeProviderCleanup(copy)
		require.Error(t, err)
	}
}

func TestLocalRuntimeReleaseOwners(t *testing.T) {
	b := &Backend{archiveLife: &taskArchiveBackend{}, localRuntimeRelease: &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}}
	seed := recoveryTestSeed(t)
	c := &TaskClaim{origin: b, parentCtx: context.Background(), reference: TaskClaimReference{Task: seed.Task, ClaimID: seed.ClaimID, CreateRevision: 2, LeaseID: 3}, deadline: time.Now().Add(time.Minute)}
	c.self = c
	o, err := b.beginLocalRuntimeRelease(context.Background(), c)
	require.NoError(t, err)
	_, err = b.beginLocalRuntimeRelease(context.Background(), c)
	require.Error(t, err)
	forged := &TaskClaim{origin: b, self: c, parentCtx: c.parentCtx, reference: c.reference}
	_, err = b.beginLocalRuntimeRelease(context.Background(), forged)
	require.Error(t, err)
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-o.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel original owner")
	}
	select {
	case <-closed:
		t.Fatal("cancellation was mistaken for actual join")
	case <-time.After(20 * time.Millisecond):
	}
	o.finish()
	require.NoError(t, <-closed)
	_, err = b.beginLocalRuntimeRelease(context.Background(), c)
	require.Error(t, err)
}

func TestLocalRuntimeReleaseBounds(t *testing.T) {
	r := localReleaseRecordFixture(t)
	r.Descriptor = []byte(`{"payload":"` + strings.Repeat("x", 16384) + `"}`)
	_, err := encodeTaskRuntimeProviderCleanup(r)
	require.Error(t, err, "raw descriptor cap precedes serialization")
	r.Descriptor = []byte(`{"payload":"` + strings.Repeat("x", 13000) + `"}`)
	_, err = encodeTaskRuntimeProviderCleanup(r)
	require.Error(t, err, "encoded record cap includes base64 expansion and all seven slots")
	a := archiveCommandRecordFixture(t)
	s := p.RecordStamp{CreateRevision: 1, ModRevision: 1, SHA256: strings.Repeat("a", 64)}
	original := p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: a.Claims.Context.Current, Partition: a.FinalSync.Task.Reference.Partition, TaskBirth: 1,
		CreationIntentID: r.Binding.CreationIntentID, EnrollmentDigest: r.Binding.EnrollmentDigest, ObserverCertificateDigest: r.Binding.EnrollmentDigest, NodeUID: r.Binding.NodeUID, NodeBootID: r.Binding.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty",
		Authority: p.TerminationAuthorityBindings{Task: s, CleanupIntent: s, TaskLink: s, Placement: s, Control: s, Owner: s, Fence: s, RuntimeIndex: s, Quiescence: s, QuiescenceReceipt: s, QuiescenceReservation: s, FinalSync: s, Issuer: s, Archive: s, LocalComplete: s, QuiescenceStage: p.TerminationQuiescenceStage{RequestID: "request", StageID: "stage", AttemptID: r.Binding.CreationIntentID, Digest: r.Binding.EnrollmentDigest}}}
	wire, err := p.EncodeTaskRuntimeTerminationIntent(original)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: wire, CreateRevision: 100, ModRevision: 100, Version: 1}
	_, err = decodeLocalRuntimeTermination(kv)
	require.NoError(t, err)
	terminal := original
	terminal.Phase, terminal.TerminalProof = "terminated", make([]byte, 4096)
	_, err = p.EncodeTaskRuntimeTerminationIntent(terminal)
	require.NoError(t, err)
	require.True(t, sameLocalRuntimeTerminationOrigin(original, terminal))
	terminal.Current.ClaimID = "66666666-6666-4666-8666-666666666666"
	require.False(t, sameLocalRuntimeTerminationOrigin(original, terminal), "fresh claimant must preserve original Current")
	terminal = original
	terminal.TerminalProof = []byte("proof")
	_, err = p.EncodeTaskRuntimeTerminationIntent(terminal)
	require.Error(t, err)
}
