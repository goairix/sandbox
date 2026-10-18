package etcd

import (
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This model can pass only the strict seal codec/discriminator. Its terminal
// bytes are deliberately unsigned and it has no detached signed source or
// settlement. It must never produce an authenticated history or quota result.
func coldQuotaDetachedSealRefusalModel(t *testing.T, f *coldQuotaHistoryFixture) *mvccpb.KeyValue {
	t.Helper()
	r := f.termination
	r.Version, r.MountAttempt = 2, 0
	r.Authority.Archive, r.Authority.LocalComplete = p.RecordStamp{}, p.RecordStamp{}
	r.DetachedEffects = &p.DetachedRuntimeEffectBindings{
		Version: 1, Profile: "detached_enrolled_plain",
		ContractDigest: f.b.enrolledPublication.ContractDigest,
		Settlement:     p.RecordStamp{CreateRevision: 1, ModRevision: 1, SHA256: strings.Repeat("d", 64)},
	}
	r.TerminalProof = []byte("unsigned-v2-routing-model")
	termination := f.server.point(f.keys.Intent)
	local, err := decodeLocalOwnerRelease(f.server.point(f.releaseKey))
	require.NoError(t, err)
	record, err := newDetachedOwnerReleaseRecord(r, termination.CreateRevision, termination.ModRevision, local.Claim)
	require.NoError(t, err)
	wire, err := encodeDetachedOwnerRelease(record)
	require.NoError(t, err)
	model := workerTestKV(f.server.point(f.releaseKey))
	model.Value = []byte(wire)
	_, err = decodeDetachedOwnerRelease(model)
	require.NoError(t, err, "only the strict seal codec is a positive precondition")
	profile, err := decodeTaskOwnerReleaseProfile(f.task, model)
	require.NoError(t, err)
	require.Equal(t, taskOwnerReleaseDetached, profile)
	return model
}

// The existing fixture remains entirely v1. Replace only the bounded native
// reply copy for the release point, leaving its stored signed source intact.
// Matching the full point key prevents another paid group from being injected.
func coldQuotaDetachedSealReply(t *testing.T, f *coldQuotaHistoryFixture, model *mvccpb.KeyValue) {
	t.Helper()
	f.server.mu.Lock()
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if len(request.Success) != 3 || string(request.Success[2].GetRequestRange().GetKey()) != f.releaseKey {
			return
		}
		response.Responses[2].GetResponseRange().Kvs = []*mvccpb.KeyValue{workerTestKV(model)}
	}
	f.server.mu.Unlock()
	t.Cleanup(func() {
		f.server.mu.Lock()
		f.server.txnReply = nil
		f.server.mu.Unlock()
	})
}

func coldQuotaDetachedHistoryPrefix(t *testing.T, f *coldQuotaHistoryFixture) *coldQuotaWorkerHistory {
	t.Helper()
	groups := coldQuotaHistoryGroups(t, f)
	state := coldQuotaHistoryCursor(t, f)
	for phase := 0; phase < 4; phase++ {
		f.fund()
		history, err := coldQuotaHistoryStep(f, state)
		require.NoError(t, err)
		require.Nil(t, history)
		requests := coldQuotaHistoryRequests(f)
		require.Len(t, requests, phase+1)
		require.Equal(t, taskProtoPoints(append([]string{f.b.identityKey, f.b.restoreKey}, groups[phase]...)), requests[phase].Success)
		require.Equal(t, requests[phase].Success, requests[phase].Failure)
		require.Len(t, requests[phase].Compare, 20)
	}
	require.Equal(t, coldQuotaWorkerHistoryWantRelease, state.phase)
	return state
}

// A valid v2 seal selects the three-point head, but the existing v1 terminal
// must fail there. Reading actual Task does not convert it into detached proof.
// Full thirteen-point authentication and the unsupported-writer handoff need
// Root's real native producer; no host terminal signature is minted for them.
func TestColdQuotaWorkerHistoryDetachedSealRoutesActualTaskHead(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	model := coldQuotaDetachedSealRefusalModel(t, f)
	storedSeal := workerTestKV(f.server.point(f.releaseKey))
	state := coldQuotaDetachedHistoryPrefix(t, f)
	coldQuotaDetachedSealReply(t, f, model)
	grants, keeps, revokes := f.server.counts()

	f.fund()
	history, err := coldQuotaHistoryStep(f, state)
	require.NoError(t, err, "strict v2 seal must reach the original paid head phase")
	require.Nil(t, history)
	require.Nil(t, state.history.release, "a v2 seal cannot populate the v1 proof field")
	require.Equal(t, coldQuotaWorkerHistoryWantHead, state.phase)
	require.Len(t, coldQuotaHistoryRequests(f), 5)

	termination, provider, _, err := f.b.namespace.localRuntimeReleaseKeys(f.task)
	require.NoError(t, err)
	taskKey, err := f.b.namespace.taskKey(f.task)
	require.NoError(t, err)
	want := taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey, termination, provider, taskKey})
	for attempt := 0; attempt < 2; attempt++ {
		f.fund()
		f.b.archiveLife.mu.Lock()
		claims := f.claims.bucket
		f.b.archiveLife.mu.Unlock()
		history, err = coldQuotaHistoryStep(f, state)
		require.ErrorIs(t, err, ErrCorruptRecord, "the real retained v1 terminal is not the selected v2 variant")
		require.Nil(t, history)
		require.Nil(t, state.history.release)
		require.Equal(t, coldQuotaWorkerHistoryWantHead, state.phase)
		requests := coldQuotaHistoryRequests(f)
		require.Len(t, requests, 6+attempt)
		request := requests[len(requests)-1]
		require.Equal(t, want, request.Success)
		require.Equal(t, want, request.Failure)
		require.Equal(t, 30, len(request.Compare)+len(request.Success)+len(request.Failure))
		f.b.archiveLife.mu.Lock()
		afterClaims, rangeTokens := f.claims.bucket, f.ranges.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, claims, afterClaims)
		require.Zero(t, rangeTokens)
	}
	require.Equal(t, storedSeal, f.server.point(f.releaseKey), "injected codec metadata cannot rewrite the real source")
	afterGrants, afterKeeps, afterRevokes := f.server.counts()
	require.Equal(t, []int{grants, keeps, revokes}, []int{afterGrants, afterKeeps, afterRevokes})
}

// These are rejection models only. A malformed or foreign variant must not
// advance to Head, consume Claim, return a nil-local prepared input, or cache
// an unsupported-profile result without authenticating its signed history.
func TestColdQuotaWorkerHistoryDetachedSealRefusals(t *testing.T) {
	for _, fault := range []string{"foreign-task", "foreign-profile", "local-with-detached-tuple", "noncanonical", "leased", "mutable"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaHistoryFixture(t)
			model := coldQuotaDetachedSealRefusalModel(t, f)
			switch fault {
			case "foreign-task":
				record, err := decodeDetachedOwnerRelease(model)
				require.NoError(t, err)
				record.Task.TaskID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
				record.Claim.Task = record.Task
				wire, err := encodeDetachedOwnerRelease(record)
				require.NoError(t, err)
				model.Value = []byte(wire)
			case "foreign-profile":
				model.Value = []byte(strings.Replace(string(model.Value), "detached_enrolled_plain", "foreign_enrolled_plain", 1))
			case "local-with-detached-tuple":
				local := f.server.point(f.releaseKey)
				model.Value = append(append([]byte{}, local.Value[:len(local.Value)-1]...), []byte(`,"detached_effects":{}}`)...)
			case "noncanonical":
				model.Value = append([]byte(" "), model.Value...)
			case "leased":
				model.Lease = f.session.reference.LeaseID
			case "mutable":
				model.Version++
				model.ModRevision++
			}
			state := coldQuotaDetachedHistoryPrefix(t, f)
			coldQuotaDetachedSealReply(t, f, model)
			f.fund()
			history, err := coldQuotaHistoryStep(f, state)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Nil(t, history)
			require.Nil(t, state.history.release)
			require.Equal(t, coldQuotaWorkerHistoryWantRelease, state.phase)
			require.Len(t, coldQuotaHistoryRequests(f), 5)
		})
	}
}
