package etcd

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Closed key roles prevent the smaller worker/B cap from silently sizing a
// Task, intent, execution or release value. This is accounting only: the native
// builder still supplies every exact key and its original ownership fences.
func (b *Backend) cleanupBackfillAdvanceRole(key string) (string, int) {
	if !validNamespaceKey(b.namespace, key) {
		return "", 0
	}
	switch key {
	case b.identityKey:
		return "identity", maxIdentityJSONBytes
	case b.restoreKey:
		return "restore", maxNamespaceComponentBytes
	case b.workerControl.profile.key:
		return "worker", workerRecordBytes
	}
	parts := strings.Split(strings.TrimPrefix(key, b.namespace.Root()), "/")
	if len(parts) == 3 && parts[0] == "workers" && parts[1] == "sessions" && validPreparationUUID(parts[2]) {
		return "worker", workerRecordBytes
	}
	if len(parts) == 4 && parts[0] == "workers" && parts[1] == "budget" && parts[2] == "claim" {
		slot, err := strconv.ParseUint(parts[3], 16, 16)
		limits, ok := workerBudgetSettings(WorkerBudgetClaim)
		if err == nil && ok && slot < uint64(limits.count) && parts[3] == fmt.Sprintf("%02x", slot) {
			return "worker", workerRecordBytes
		}
		return "", 0
	}
	if len(parts) < 3 || parts[0] != "p" {
		return "", 0
	}
	partition, err := strconv.ParseUint(parts[1], 16, 8)
	if err != nil || parts[1] != fmt.Sprintf("%02x", partition) {
		return "", 0
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "worker":
			return "worker", workerRecordBytes
		case "cleanup-pending-backfill":
			return "backfill", cleanupBackfillBytes
		}
	}
	if len(parts) == 4 && validPreparationUUID(parts[3]) {
		switch parts[2] {
		case "tasks", "cleanup-intents":
			return "birth", 4096
		case "cleanup-pending":
			return "pending", cleanupPendingBytes
		}
	}
	if len(parts) == 5 && parts[2] == "tasks" && validPreparationUUID(parts[3]) {
		switch parts[4] {
		case "execution":
			return "execution", cleanupExecutionBytes
		case "owner-release":
			return "release", 8192
		}
	}
	if len(parts) == 7 && parts[2] == "stages" && len(parts[3]) <= 128 && len(parts[4]) <= 128 && validPreparationUUID(parts[5]) && parts[6] == "receipt" {
		return "receipt", 2048
	}
	return "", 0
}

// Measure the concrete dispatch first, then a separate maximal protobuf copy.
// The largest terminal branch is W20+A12+B4+E4+R4+P4+two writes+two failure
// reads = 52 nodes. At 1024-byte keys and role-specific values its conservative
// request allowance is 107136 bytes; failure/success replies cap at 14592/4096.
// Generic Stage limits and physical release authority are unchanged.
func (b *Backend) preflightCleanupBackfillAdvance(m Mutation) error {
	return b.preflightCleanupPendingMutation(m, false)
}

// Cold deletion omits B and has exactly 41 or 44 comparisons (E absent/present).
// Its 47-node conservative request cap is 47*1152+36992+4096 = 95232 bytes.
func (b *Backend) preflightCleanupPendingDelete(m Mutation) error {
	return b.preflightCleanupPendingMutation(m, true)
}

// deleteOnly selects fixed wire dimensions and CRUD shape, never permission.
// Only the concrete caller's signed-proof validator can authorize a deletion.
func (b *Backend) preflightCleanupPendingMutation(m Mutation, deleteOnly bool) error {
	if !b.validWorkerBackend() {
		return ErrInvalidMutation
	}
	nodes, requestBytes, name := 52, 107136, "backfill-advance"
	if deleteOnly {
		nodes, requestBytes, name = 47, 95232, "pending-delete"
		if len(m.Comparisons) != 41 && len(m.Comparisons) != 44 || len(m.Writes) != 1 {
			return ErrInvalidMutation
		}
	} else if len(m.Comparisons) < 24 || len(m.Writes) < 1 || len(m.Writes) > 2 {
		return ErrInvalidMutation
	}
	if len(m.Comparisons)+len(m.Writes)+2 > nodes {
		return ErrInvalidMutation
	}
	key := strings.Repeat("k", maxKeyBytes)
	success := taskProtoPutResponse(len(m.Writes))
	for i, write := range m.Writes {
		if write.Delete {
			success.Responses[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: math.MaxInt64}}}
		}
	}
	failure := taskProtoResponse([]string{key, key}, []string{cleanupWirePadding(maxIdentityJSONBytes), cleanupWirePadding(maxNamespaceComponentBytes)})
	actual := b.cleanupProjection(name, m.Comparisons, taskProtoWrites(m.Writes), success)
	actual.failure = failure
	if err := validateCleanupWire(actual); err != nil {
		return err
	}

	maximumCmps := make([]clientv3.Cmp, len(m.Comparisons))
	for i, cmp := range m.Comparisons {
		role, limit := b.cleanupBackfillAdvanceRole(string(cmp.Key))
		if limit == 0 || deleteOnly && role == "backfill" || !validComparison(cmp) || cmp.Result != pb.Compare_EQUAL || len(cmp.RangeEnd) != 0 {
			return ErrInvalidMutation
		}
		maximumCmps[i] = copyComparison(cmp)
		maximumCmps[i].Key = []byte(key)
		switch v := maximumCmps[i].TargetUnion.(type) {
		case *pb.Compare_Value:
			if len(v.Value) > limit {
				return ErrInvalidMutation
			}
			v.Value = []byte(cleanupWirePadding(limit))
		case *pb.Compare_CreateRevision:
			if v.CreateRevision > 0 {
				v.CreateRevision = math.MaxInt64
			}
		case *pb.Compare_ModRevision:
			v.ModRevision = math.MaxInt64
		case *pb.Compare_Lease:
			if v.Lease > 0 {
				v.Lease = math.MaxInt64
			}
		default:
			return ErrInvalidMutation
		}
	}

	maximumWrites := make([]Write, len(m.Writes))
	for i, write := range m.Writes {
		role, limit := b.cleanupBackfillAdvanceRole(write.Key)
		last := i == len(m.Writes)-1
		if deleteOnly {
			if role != "pending" || !write.Delete {
				return ErrInvalidMutation
			}
		} else if last && (role != "backfill" || write.Delete) || !last && role != "pending" {
			return ErrInvalidMutation
		}
		if len(write.Value) > limit || write.Delete && len(write.Value) != 0 {
			return ErrInvalidMutation
		}
		maximumWrites[i] = Write{Key: key, Delete: write.Delete}
		if !write.Delete {
			maximumWrites[i].Value = []byte(cleanupWirePadding(limit))
		}
	}
	maximum := cleanupWireProjection{
		name:    name + "-maximum",
		request: &pb.TxnRequest{Compare: taskProtoComparisons(maximumCmps), Success: taskProtoWrites(maximumWrites), Failure: taskProtoPoints([]string{key, key})},
		success: success,
		failure: failure,
	}
	if maximum.request.Size() > requestBytes || success.Size() > 4096 || failure.Size() > 14592 {
		return ErrInvalidMutation
	}
	return validateCleanupWire(maximum)
}
