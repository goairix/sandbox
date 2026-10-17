package etcd

import (
	"sync/atomic"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// A codec belongs to one original fixed-phase unary invocation. It is never
// registered globally or reused by a later call. requestBytes binds Marshal's
// actual size to the bytes already charged under the original lifecycle lock.
type coldQuotaWorkerRPCCodec struct {
	phase        coldQuotaWorkerRPCPhase
	requestBytes int
	serialized   uint32
	decoded      uint32
}

// Name selects protobuf only for this original invocation's forced codec.
func (c *coldQuotaWorkerRPCCodec) Name() string {
	return "proto"
}

// Metadata binds its receive allowance to the exact original request bytes;
// the two application protobuf envelopes share one 256 KiB Range charge.
// Existing phases retain their independent fixed response caps unchanged.
func (c *coldQuotaWorkerRPCCodec) responseLimit() int {
	if c.phase == coldQuotaRPCMetadata {
		if c.requestBytes <= 0 || c.requestBytes > coldQuotaRPCRequestLimit {
			return 0
		}
		return maxMutationBytes - c.requestBytes
	}
	return coldQuotaWorkerRPCResponseLimit(c.phase)
}

// Marshal consumes the sole serialization attempt, including an interceptor's
// reinvocation, and binds the actual flat request to its original paid size.
func (c *coldQuotaWorkerRPCCodec) Marshal(value any) ([]byte, error) {
	if !atomic.CompareAndSwapUint32(&c.serialized, 0, 1) {
		return nil, ErrConflict
	}
	size, err := coldQuotaWorkerRPCRequestBytes(c.phase, value)
	if err != nil || c.requestBytes != 0 && size != c.requestBytes {
		return nil, ErrInvalidMutation
	}
	switch request := value.(type) {
	case *pb.LeaseGrantRequest:
		return request.Marshal()
	case *pb.LeaseRevokeRequest:
		return request.Marshal()
	case *pb.TxnRequest:
		return request.Marshal()
	default:
		return nil, ErrInvalidMutation
	}
}

// Unmarshal checks the fixed phase envelope and finite object graph before
// generated decoding can allocate responses or change the caller's destination.
func (c *coldQuotaWorkerRPCCodec) Unmarshal(wire []byte, value any) error {
	limit := c.responseLimit()
	if limit == 0 || len(wire) > limit {
		return ErrCorruptRecord
	}
	kind := coldQuotaWireTxn
	switch response := value.(type) {
	case *pb.LeaseGrantResponse:
		if response == nil || c.phase != coldQuotaRPCGrant {
			return ErrCorruptRecord
		}
		kind = coldQuotaWireGrant
	case *pb.LeaseRevokeResponse:
		if response == nil || c.phase != coldQuotaRPCRevoke {
			return ErrCorruptRecord
		}
		kind = coldQuotaWireRevoke
	case *pb.TxnResponse:
		if response == nil || c.phase != coldQuotaRPCGuard && c.phase != coldQuotaRPCCommit && c.phase != coldQuotaRPCJournal && c.phase != coldQuotaRPCMetadata {
			return ErrCorruptRecord
		}
	default:
		return ErrCorruptRecord
	}
	counts := coldQuotaWorkerRPCWireCounts{metadata: c.phase == coldQuotaRPCMetadata}
	if err := coldQuotaWorkerRPCWire(wire, kind, &counts); err != nil {
		return err
	}
	if !atomic.CompareAndSwapUint32(&c.decoded, 0, 1) {
		return ErrCorruptRecord
	}

	// Invalid bytes cannot reset or allocate a destination graph. The finite
	// schema walk above rejects unknown/duplicate fields and nested Txns first.
	switch response := value.(type) {
	case *pb.LeaseGrantResponse:
		*response = pb.LeaseGrantResponse{}
		return response.Unmarshal(wire)
	case *pb.LeaseRevokeResponse:
		*response = pb.LeaseRevokeResponse{}
		return response.Unmarshal(wire)
	case *pb.TxnResponse:
		*response = pb.TxnResponse{}
		return response.Unmarshal(wire)
	default:
		return ErrCorruptRecord
	}
}

// Reject nested/recursive request graphs before calling any generated Size or
// Marshal method. At most 64 flat comparison/operation nodes and 128 KiB of
// protobuf are allowed; point reads/writes/deletes cannot ask for PrevKv graphs.
// Metadata is narrower: both nonempty branches permit only key-only Ranges.
func coldQuotaWorkerRPCRequestBytes(phase coldQuotaWorkerRPCPhase, value any) (int, error) {
	size := 0
	switch request := value.(type) {
	case *pb.LeaseGrantRequest:
		if phase != coldQuotaRPCGrant || request == nil || request.TTL != 10 || request.ID != 0 || len(request.XXX_unrecognized) != 0 {
			return 0, ErrInvalidMutation
		}
		size = request.Size()
	case *pb.LeaseRevokeRequest:
		if phase != coldQuotaRPCRevoke || request == nil || request.ID <= 0 || len(request.XXX_unrecognized) != 0 {
			return 0, ErrInvalidMutation
		}
		size = request.Size()
	case *pb.TxnRequest:
		if phase != coldQuotaRPCGuard && phase != coldQuotaRPCCommit && phase != coldQuotaRPCJournal && phase != coldQuotaRPCMetadata || request == nil || len(request.XXX_unrecognized) != 0 {
			return 0, ErrInvalidMutation
		}
		if len(request.Compare)+len(request.Success)+len(request.Failure) > 64 {
			return 0, ErrInvalidMutation
		}
		if phase == coldQuotaRPCMetadata && (len(request.Success) == 0 || len(request.Failure) == 0) {
			return 0, ErrInvalidMutation
		}
		for _, comparison := range request.Compare {
			if comparison == nil || !coldQuotaWorkerRPCKey(comparison.Key) || len(comparison.RangeEnd) != 0 || !validComparison(clientv3.Cmp(*comparison)) {
				return 0, ErrInvalidMutation
			}
		}
		for _, branch := range [][]*pb.RequestOp{request.Success, request.Failure} {
			for _, op := range branch {
				if !coldQuotaWorkerRPCPointOp(op) || phase == coldQuotaRPCMetadata && op.GetRequestRange() == nil {
					return 0, ErrInvalidMutation
				}
			}
		}
		size = request.Size()
	default:
		return 0, ErrInvalidMutation
	}
	if size > coldQuotaRPCRequestLimit {
		return 0, ErrInvalidMutation
	}
	return size, nil
}

func coldQuotaWorkerRPCKey(key []byte) bool {
	return len(key) > 0 && len(key) <= maxNamespaceKeyBytes
}

func coldQuotaWorkerRPCPointOp(op *pb.RequestOp) bool {
	if op == nil || len(op.XXX_unrecognized) != 0 {
		return false
	}
	switch request := op.Request.(type) {
	case *pb.RequestOp_RequestRange:
		if request == nil || request.RequestRange == nil {
			return false
		}
		point := request.RequestRange
		// A generated flat message's extra options/unknown fields can only
		// add bytes beyond this exact key-only native point representation.
		return coldQuotaWorkerRPCKey(point.Key) && point.Size() == (&pb.RangeRequest{Key: point.Key}).Size()
	case *pb.RequestOp_RequestPut:
		if request == nil || request.RequestPut == nil {
			return false
		}
		point := request.RequestPut
		return coldQuotaWorkerRPCKey(point.Key) && point.Lease >= 0 && len(point.Value) <= coldQuotaRPCRequestLimit && point.Size() == (&pb.PutRequest{Key: point.Key, Value: point.Value, Lease: point.Lease}).Size()
	case *pb.RequestOp_RequestDeleteRange:
		if request == nil || request.RequestDeleteRange == nil {
			return false
		}
		point := request.RequestDeleteRange
		return coldQuotaWorkerRPCKey(point.Key) && point.Size() == (&pb.DeleteRangeRequest{Key: point.Key}).Size()
	default:
		return false
	}
}
