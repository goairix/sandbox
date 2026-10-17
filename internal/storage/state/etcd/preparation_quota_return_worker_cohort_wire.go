package etcd

import (
	"encoding/json"
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// preflightColdQuotaWorkerCohort checks the complete paid request envelope
// before Grant. Maximal positive lease/revision widths dominate the later
// original ACK values. The fixed transport separately enforces 37888 aggregate
// receive bytes and reserves the ten-byte original Revoke before every send.
func (b *Backend) preflightColdQuotaWorkerCohort(prepared *coldQuotaReturnPrepared, worker []clientv3.Cmp) error {
	guard, commit, err := b.coldQuotaWorkerCohortRequests(prepared, worker, math.MaxInt64, math.MaxInt64)
	if err != nil {
		return err
	}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{guard, commit}, nil); err != nil {
		return err
	}
	grantBytes, err := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCGrant, &pb.LeaseGrantRequest{TTL: 10})
	if err != nil {
		return err
	}
	guardBytes, err := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCGuard, guard)
	if err != nil {
		return err
	}
	commitBytes, err := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCCommit, commit)
	if err != nil {
		return err
	}
	if grantBytes+guardBytes+commitBytes+coldQuotaRPCRevokeReserve > coldQuotaRPCStageRequestLimit {
		return ErrInvalidMutation
	}
	return nil
}

// coldQuotaWorkerCohortRequests composes only the original Stage protocol. The
// prepared mutation is borrowed unchanged; no Stage, UUID or history snapshot is
// reconstructed. Guard and Commit retain their original failure branches and
// the exact full guard identity, with W24 increasing their nodes to 29 and 62.
func (b *Backend) coldQuotaWorkerCohortRequests(prepared *coldQuotaReturnPrepared, worker []clientv3.Cmp, leaseID, guardRevision int64) (*pb.TxnRequest, *pb.TxnRequest, error) {
	guardKey, receiptKey, err := b.stageKeys(prepared.reference)
	if err != nil {
		return nil, nil, err
	}
	guardValue, err := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{prepared.reference, leaseID})
	if err != nil {
		return nil, nil, err
	}
	committed, err := encodeReceipt(prepared.reference, OutcomeCommitted)
	if err != nil {
		return nil, nil, err
	}
	guardBody := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	guardComparisons, err := b.coldQuotaWorkerCohortComparisons(guardBody, worker)
	if err != nil {
		return nil, nil, err
	}
	commitBody := append(b.baseComparisons(),
		clientv3.Compare(clientv3.Value(guardKey), "=", string(guardValue)),
		clientv3.Compare(clientv3.LeaseValue(guardKey), "=", leaseID),
		clientv3.Compare(clientv3.CreateRevision(guardKey), "=", guardRevision),
		clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0),
	)
	commitBody = append(commitBody, prepared.mutation.Comparisons...)
	commitComparisons, err := b.coldQuotaWorkerCohortComparisons(commitBody, worker)
	if err != nil {
		return nil, nil, err
	}
	guard := &pb.TxnRequest{
		Compare: taskProtoComparisons(guardComparisons),
		Success: taskProtoWrites([]Write{{Key: guardKey, Value: guardValue}}),
		Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey}),
	}
	guard.Success[0].GetRequestPut().Lease = leaseID
	operations := taskProtoWrites(prepared.mutation.Writes)
	operations = append(operations, taskProtoWrites([]Write{{Key: receiptKey, Value: []byte(committed)}})...)
	commit := &pb.TxnRequest{
		Compare: taskProtoComparisons(commitComparisons),
		Success: operations,
		Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey, receiptKey, guardKey}),
	}
	return guard, commit, nil
}

// coldQuotaWorkerCohortComparisons removes exactly the duplicated original
// base4, after both prefixes match this backend's fresh complete protobufs.
// Matching by key would discard required Value/Lease/Create/Mod comparisons;
// even identically corrupted body/worker prefixes must fail this check.
func (b *Backend) coldQuotaWorkerCohortComparisons(body, worker []clientv3.Cmp) ([]clientv3.Cmp, error) {
	if b == nil || len(body) != 6 && len(body) != 34 || len(worker) != 24 {
		return nil, ErrInvalidMutation
	}
	return b.coldQuotaWorkerBaseComparisons(body, worker)
}
