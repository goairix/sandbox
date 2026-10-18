package etcd

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

// This model measures transport only. The original host members supply the
// production W20 request shape; maximum codec records replace only its sizing
// inputs afterwards. No widened frame enters admission, a decoder of signed
// history, or a native call, and no model member is registered as authority.
type coldQuotaDetachedHistoryWireModel struct {
	backend     *Backend
	headKeys    []string
	historyKeys []string
	historyCaps []int
	fences      map[string]taskFence
}

func coldQuotaDetachedHistoryMaximumWireModel(t *testing.T, f *coldQuotaAdmissionFixture) coldQuotaDetachedHistoryWireModel {
	t.Helper()
	b, intent, actualTask := detachedOwnerHistoryModel(t)
	require.Len(t, b.namespace.Root(), maxNamespaceRootBytes)
	require.Len(t, b.restoreEpoch, maxNamespaceComponentBytes)
	b.identityValue += strings.Repeat(" ", maxIdentityJSONBytes-len(b.identityValue))
	_, err := decodeIdentity(b.identityValue, b.namespace)
	require.NoError(t, err)
	require.Len(t, b.identityValue, maxIdentityJSONBytes)

	// These maximum ordinary locator fields are accepted by the original
	// locator and termination codecs. StageID128 is a conservative sizing
	// allowance: real Q uses its fixed stage name, so these simultaneous
	// dimensions do not claim a reachable signed quiescence positive.
	intent.Authority.QuiescenceStage.RequestID = strings.Repeat("r", maxNamespaceComponentBytes)
	intent.Authority.QuiescenceStage.StageID = strings.Repeat("q", maxNamespaceComponentBytes)
	locator := StageAttemptLocator{
		Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch,
		Partition: intent.Partition,
		RequestID: intent.Authority.QuiescenceStage.RequestID,
		StageID:   intent.Authority.QuiescenceStage.StageID,
		AttemptID: intent.Authority.QuiescenceStage.AttemptID,
	}
	require.NoError(t, locator.Validate())
	_, receiptKey, err := b.stageKeys(locator.reference(strings.Repeat("f", 64)))
	require.NoError(t, err)
	_, err = p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err)
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	require.Equal(t, receiptKey, keys.QuiescenceReceipt)
	terminationKey, providerKey, _, err := b.namespace.localRuntimeReleaseKeys(localRuntimeTerminationTask(intent))
	require.NoError(t, err)
	require.Equal(t, keys.Intent, terminationKey)
	historyKeys, caps, err := b.taskOwnerReleaseHistoryKeys(intent, actualTask)
	require.NoError(t, err)
	require.Equal(t, detachedRuntimeHistoryCaps(), caps)
	require.Len(t, historyKeys, 13)
	require.Equal(t, keys.QuiescenceReceipt, historyKeys[2])
	for _, key := range historyKeys {
		require.True(t, validNamespaceKey(b.namespace, key))
	}

	// Use canonical worker records at their legal string/scalar maxima,
	// rather than treating every small fixed worker record as 2048 opaque
	// bytes. Their generated codecs check the simultaneous dimensions.
	var profile workerProfileRecord
	require.NoError(t, json.Unmarshal([]byte(f.b.workerControl.profile.value), &profile))
	profile.Namespace, profile.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	profile.IdentityDigest = workerIdentityDigest(b.identityValue)
	profileWire, err := encodeOperationMetadata(profile, workerRecordBytes)
	require.NoError(t, err)
	profileKey, err := b.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)

	session := f.session.record(math.MaxInt64)
	session.Namespace, session.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	session.IdentityDigest = profile.IdentityDigest
	session.WorkerID = strings.Repeat("w", maxNamespaceComponentBytes)
	sessionWire, err := encodeWorkerSessionRecord(session)
	require.NoError(t, err)
	sessionKey, err := b.namespace.workerSessionKey(session.SessionID)
	require.NoError(t, err)
	partitionWire, err := encodeWorkerPartitionRecord(workerPartitionRecord{workerSessionRecord: session, Partition: math.MaxUint8})
	require.NoError(t, err)
	partitionKey, err := b.namespace.workerPartitionKey(math.MaxUint8)
	require.NoError(t, err)
	limits, ok := workerBudgetSettings(WorkerBudgetRange)
	require.True(t, ok)
	rangeRecord := workerBudgetRecord{workerSessionRecord: session, Kind: limits.name, Slot: limits.count - 1}
	rangeWire, err := encodeOperationMetadata(rangeRecord, workerRecordBytes)
	require.NoError(t, err)
	rangeKey, err := b.namespace.workerBudgetKey(WorkerBudgetRange, rangeRecord.Slot)
	require.NoError(t, err)

	maximumFence := func(key, value string, lease int64) taskFence {
		return taskFence{key: key, value: value, create: math.MaxInt64, mod: math.MaxInt64, lease: lease}
	}
	return coldQuotaDetachedHistoryWireModel{
		backend:     b,
		headKeys:    []string{terminationKey, providerKey, keys.Task},
		historyKeys: historyKeys,
		historyCaps: caps,
		fences: map[string]taskFence{
			f.b.workerControl.profile.key: maximumFence(profileKey, profileWire, 0),
			f.session.guard.key:           maximumFence(sessionKey, sessionWire, math.MaxInt64),
			f.part.fence.key:              maximumFence(partitionKey, partitionWire, math.MaxInt64),
			f.ranges.fence.key:            maximumFence(rangeKey, rangeWire, math.MaxInt64),
		},
	}
}

// Reuse the production builder before substituting dimensional copies. Every
// original comparison must map to base4 or one of the four complete fences;
// an added, removed or unaccounted comparison fails instead of being omitted.
func coldQuotaDetachedHistoryWireProjection(t *testing.T, f *coldQuotaAdmissionFixture, model coldQuotaDetachedHistoryWireModel, keys []string, caps []int) (*pb.TxnRequest, *pb.TxnResponse) {
	t.Helper()
	b := model.backend
	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	request, err := f.b.coldQuotaWorkerMetadataRequest(f.session, f.part, f.ranges, all)
	require.NoError(t, err)
	require.Len(t, request.Compare, 20)
	seen := make(map[string]map[pb.Compare_CompareTarget]bool)
	for _, comparison := range request.Compare {
		oldKey := string(comparison.Key)
		if seen[oldKey] == nil {
			seen[oldKey] = make(map[pb.Compare_CompareTarget]bool)
		}
		require.False(t, seen[oldKey][comparison.Target], "duplicate original W20 key/target")
		seen[oldKey][comparison.Target] = true
		var fence taskFence
		switch oldKey {
		case f.b.identityKey:
			require.Contains(t, []pb.Compare_CompareTarget{pb.Compare_VALUE, pb.Compare_LEASE}, comparison.Target)
			fence = taskFence{key: b.identityKey, value: b.identityValue}
		case f.b.restoreKey:
			require.Contains(t, []pb.Compare_CompareTarget{pb.Compare_VALUE, pb.Compare_LEASE}, comparison.Target)
			fence = taskFence{key: b.restoreKey, value: b.restoreEpoch}
		default:
			var ok bool
			fence, ok = model.fences[oldKey]
			require.True(t, ok, "every original W20 fence needs an explicit maximum record")
		}
		comparison.Key = []byte(fence.key)
		switch comparison.Target {
		case pb.Compare_VALUE:
			comparison.TargetUnion = &pb.Compare_Value{Value: []byte(fence.value)}
		case pb.Compare_CREATE:
			comparison.TargetUnion = &pb.Compare_CreateRevision{CreateRevision: fence.create}
		case pb.Compare_MOD:
			comparison.TargetUnion = &pb.Compare_ModRevision{ModRevision: fence.mod}
		case pb.Compare_LEASE:
			comparison.TargetUnion = &pb.Compare_Lease{Lease: fence.lease}
		default:
			t.Fatalf("unexpected W20 target %v", comparison.Target)
		}
	}
	require.Len(t, seen, 6)
	require.Len(t, seen[f.b.identityKey], 2)
	require.Len(t, seen[f.b.restoreKey], 2)
	for originalKey := range model.fences {
		require.Len(t, seen[originalKey], 4, "every original worker fence retains Value/Create/Mod/Lease")
	}
	values := []string{b.identityValue, b.restoreEpoch}
	for _, cap := range caps {
		values = append(values, strings.Repeat("x", cap))
	}
	return request, taskProtoResponse(all, values)
}

func TestColdQuotaWorkerDetachedHistoryMaximumWire(t *testing.T) {
	f := newColdQuotaAdmissionFixture(t)
	model := coldQuotaDetachedHistoryMaximumWireModel(t, f)
	before := coldQuotaMetadataState(f)
	requests := len(coldQuotaMetadataRequests(f))
	for _, group := range []struct {
		name  string
		keys  []string
		caps  []int
		nodes int
	}{
		{"head", model.headKeys, []int{16384, 16384, 4096}, 30},
		{"history", model.historyKeys, model.historyCaps, 50},
	} {
		t.Run(group.name, func(t *testing.T) {
			request, reply := coldQuotaDetachedHistoryWireProjection(t, f, model, group.keys, group.caps)
			nodes := len(request.Compare) + len(request.Success) + len(request.Failure)
			t.Logf("actual W20 sizing only: points=%d nodes=%d request=%d worst_reply=%d aggregate=%d allowance=%d", len(group.keys), nodes, request.Size(), reply.Size(), request.Size()+reply.Size(), maxMutationBytes)
			require.Equal(t, group.nodes, nodes)
			require.LessOrEqual(t, nodes, maxStageOperations)
			require.LessOrEqual(t, request.Size(), 128<<10)
			require.LessOrEqual(t, reply.Size(), 256<<10)
			require.NoError(t, taskCheckWire(model.backend.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{reply}))
			// Keep this hard assertion even if the complete thirteen-point
			// group cannot fit: the actual failure requires a new contract.
			require.NoError(t, model.backend.checkColdQuotaWorkerMetadataWire(request, reply))
			require.LessOrEqual(t, request.Size()+reply.Size(), maxMutationBytes)
		})
	}
	require.Equal(t, before, coldQuotaMetadataState(f))
	require.Len(t, coldQuotaMetadataRequests(f), requests, "sizing cannot spend credits or dispatch a frame")
}

func TestColdQuotaWorkerDetachedHistoryOverCapBeforeAdmission(t *testing.T) {
	f := newColdQuotaAdmissionFixture(t)
	keys := coldQuotaMetadataPoints(t, f, []int{1})
	before := coldQuotaMetadataState(f)
	requests := len(coldQuotaMetadataRequests(f))
	grants, keeps, revokes := f.server.counts()
	points, err := f.b.readColdQuotaWorkerPoints(f.owner, f.session, f.part, f.ranges, keys, []int{maxRecordBytes + 1})
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, points)
	require.Equal(t, before, coldQuotaMetadataState(f))
	require.Len(t, coldQuotaMetadataRequests(f), requests)
	afterGrants, afterKeeps, afterRevokes := f.server.counts()
	require.Equal(t, grants, afterGrants)
	require.Equal(t, keeps, afterKeeps)
	require.Equal(t, revokes, afterRevokes)

	locator := StageAttemptLocator{
		Namespace: f.b.namespace.Root(), RestoreEpoch: f.b.restoreEpoch,
		Partition: f.part.partition,
		RequestID: strings.Repeat("r", maxNamespaceComponentBytes),
		StageID:   strings.Repeat("s", maxNamespaceComponentBytes),
		AttemptID: "ffffffff-ffff-4fff-bfff-ffffffffffff",
	}
	require.NoError(t, locator.Validate())
	locator.RequestID += "r"
	require.Error(t, locator.Validate())
	require.Len(t, coldQuotaMetadataRequests(f), requests)
}
