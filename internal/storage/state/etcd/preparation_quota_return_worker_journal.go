package etcd

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// coldQuotaWorkerJournalPermit seals one trusted journal acknowledgement for
// the original preparation and enrolled owner. It is package-private and cannot
// be reconstructed from historical metadata. journalACK owns only the native
// header's scalar stamp; it proves neither physical death nor quota return.
// The original owner remains registered until the later continuation joins.
type coldQuotaWorkerJournalPermit struct {
	self      *coldQuotaWorkerJournalPermit
	backend   *Backend
	prepared  *coldQuotaReturnPrepared
	owner     *runtimeEnrolledPublicationOwner
	reference StageReference

	journalACK pb.ResponseHeader
}

// journalColdRuntimeQuotaReturnWorker submits only the original pending journal.
// The original prepared CAS selects one submitter before any paid admission;
// losing duplicates must not burn that submitter. Every refusal or unknown ACK
// burns this submitter's prepared continuation and requires original-reference
// arbitration. Neither successful submission nor its permit performs Grant,
// Stage creation, quota refund, preparation deletion, or owner completion.
func (b *Backend) journalColdRuntimeQuotaReturnWorker(o *runtimeEnrolledPublicationOwner, prepared *coldQuotaReturnPrepared, s *WorkerSession, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare, previous, aborted *mvccpb.KeyValue) (*coldQuotaWorkerJournalPermit, error) {
	if prepared == nil {
		return nil, ErrInvalidRecord
	}
	if !atomic.CompareAndSwapUint32(&prepared.journalAttempted, 0, 1) {
		return nil, ErrConflict
	}

	trusted := false
	defer func() {
		if !trusted {
			atomic.StoreUint32(&prepared.consumed, 1)
		}
	}()
	if prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.history == nil || prepared.history.release == nil || prepared.history.release.kv == nil || prepared.returnKey == "" || atomic.LoadUint32(&prepared.consumed) != 0 {
		return nil, ErrInvalidRecord
	}
	if _, err := b.coldQuotaReturnOwnerContext(o); err != nil {
		return nil, err
	}
	if p == nil || p.partition != prepared.reference.Partition || p.partition != prepared.history.reservation.Owner.Partition {
		return nil, ErrInvalidRecord
	}

	// Build from the existing protocol before charging. Binding or canonical
	// history refusal cannot spend another original worker turn's credit.
	request, err := b.coldQuotaWorkerJournalBody(prepared, previous, aborted)
	if err != nil {
		return nil, err
	}
	call, worker, err := b.beginColdQuotaWorkerCall(o, s, p, ranges, claims)
	if err != nil {
		return nil, err
	}
	// No lock spans native work. This original registration is kept until the
	// transport has actually returned, including cancellation and unknown ACKs.
	// Its enrolled owner remains registered for the next separately paid phase.
	defer call.finish()

	body := make([]clientv3.Cmp, len(request.Compare))
	for i, comparison := range request.Compare {
		body[i] = clientv3.Cmp(*comparison)
	}
	comparisons, err := b.coldQuotaWorkerJournalComparisons(body, worker)
	if err != nil {
		return nil, err
	}
	request.Compare = taskProtoComparisons(comparisons)
	failureKeys := []string{b.identityKey, b.restoreKey, string(request.Success[0].GetRequestPut().Key)}
	maxFailure := taskProtoResponse(failureKeys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", coldQuotaAttemptMaxBytes)})
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoPutResponse(1), maxFailure}); err != nil {
		return nil, err
	}

	native, err := b.coldQuotaWorkerJournalRPC(call, request)
	if err != nil {
		return nil, err
	}
	response := (*clientv3.TxnResponse)(native)
	if err := b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		points, err := b.stageEvidencePoints(response, failureKeys)
		if err != nil {
			return nil, err
		}
		if points[2] != nil && len(points[2].Value) > coldQuotaAttemptMaxBytes {
			return nil, ErrCorruptRecord
		}
		return nil, ErrConflict
	}
	if !operationPutResponses(response, 1) {
		return nil, ErrOutcomeUnknown
	}
	if err := call.postcheck(); err != nil {
		return nil, err
	}

	// Copy only the actual ACK's scalar identity/revision, never its mutable
	// response pointer or any worker capability. Later continuation must still
	// admit its own original paid call and validate the same owner/preparation.
	header := native.Header
	permit := &coldQuotaWorkerJournalPermit{
		backend:    b,
		prepared:   prepared,
		owner:      o,
		reference:  prepared.reference,
		journalACK: pb.ResponseHeader{ClusterId: header.ClusterId, MemberId: header.MemberId, Revision: header.Revision, RaftTerm: header.RaftTerm},
	}
	permit.self = permit
	trusted = true
	return permit, nil
}

// coldQuotaWorkerJournalBody preserves the original record and history-fence
// builders. Inspecting the copied return value binds the digest's original
// Attempt locator without inventing a persistent KV or authenticating a second
// history. Value decoding alone is never a physical or persistence proof.
func (b *Backend) coldQuotaWorkerJournalBody(prepared *coldQuotaReturnPrepared, previous, aborted *mvccpb.KeyValue) (*pb.TxnRequest, error) {
	_, digest, err := prepareMutation(b.namespace, prepared.mutation)
	if err != nil || digest != prepared.reference.Digest {
		return nil, ErrInvalidMutation
	}
	ref := prepared.reference
	locator := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID, RestoreEpoch: ref.RestoreEpoch}
	matched := false
	for _, write := range prepared.mutation.Writes {
		if write.Key != prepared.returnKey {
			continue
		}
		record, err := decodeColdRuntimeQuotaReturnValue(write.Value)
		if matched || write.Delete || err != nil || record.Attempt != locator {
			return nil, ErrInvalidMutation
		}
		matched = true
	}
	if !matched {
		return nil, ErrInvalidMutation
	}

	key, err := b.namespace.coldRuntimeQuotaAttemptKey(prepared.history.reservation.Owner.Partition, prepared.history.reservation.Owner.IntentID)
	if err != nil {
		return nil, err
	}
	returnKey, err := b.namespace.coldRuntimeQuotaReturnKey(prepared.history.reservation.Owner.Partition, prepared.history.reservation.Owner.IntentID)
	if err != nil || returnKey != prepared.returnKey {
		return nil, ErrInvalidRecord
	}
	wire, err := encodeColdRuntimeQuotaAttempt(b.coldQuotaAttemptRecord(prepared))
	if err != nil {
		return nil, err
	}
	comparisons, err := b.coldQuotaAttemptComparisons(prepared, key, previous, aborted)
	if err != nil {
		return nil, err
	}
	failure := []string{b.identityKey, b.restoreKey, key}
	return &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: taskProtoWrites([]Write{{Key: key, Value: []byte(wire)}}), Failure: taskProtoPoints(failure)}, nil
}

// coldQuotaWorkerJournalComparisons merges exactly the two original base4
// prefixes after each complete protobuf comparison matches this backend's fresh
// base. No other same-key comparison is removed: all history and W fences keep
// their original target, value, range, ordering and ownership. Fresh/replacement
// bodies therefore retain 50/57 comparisons and their original 1/3 branches.
func (b *Backend) coldQuotaWorkerJournalComparisons(body, worker []clientv3.Cmp) ([]clientv3.Cmp, error) {
	if b == nil || len(body) != 30 && len(body) != 37 || len(worker) != 24 {
		return nil, ErrInvalidMutation
	}
	base := taskProtoComparisons(b.baseComparisons())
	for i := range base {
		expected, err := base[i].Marshal()
		if err != nil {
			return nil, ErrInvalidMutation
		}
		for _, candidate := range []clientv3.Cmp{body[i], worker[i]} {
			comparison := pb.Compare(candidate)
			wire, err := comparison.Marshal()
			if err != nil || !bytes.Equal(wire, expected) {
				return nil, ErrInvalidMutation
			}
		}
	}
	comparisons := make([]clientv3.Cmp, 0, len(body)+len(worker)-len(base))
	comparisons = append(comparisons, body...)
	comparisons = append(comparisons, worker[len(base):]...)
	return comparisons, nil
}
