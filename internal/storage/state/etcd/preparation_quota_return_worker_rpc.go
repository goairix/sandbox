package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
)

type coldQuotaWorkerRPCPhase uint8

const (
	coldQuotaRPCGrant coldQuotaWorkerRPCPhase = iota + 1
	coldQuotaRPCGuard
	coldQuotaRPCCommit
	coldQuotaRPCRevoke
	coldQuotaRPCJournal
	// Metadata is a sixth independent single-Txn cohort; original phase
	// values and their fixed transport limits remain unchanged.
	coldQuotaRPCMetadata
	// Resolve is a seventh independent one-Txn cohort. Its fixed 16 KiB
	// reply does not inherit metadata's variable history-read allowance.
	// Phase seven uses the final bit of coldQuotaWorkerRPCState.used;
	// adding another phase requires expanding that mask before admission.
	coldQuotaRPCResolve
)

const (
	// These are application protobuf envelope bytes, excluding HTTP/2, TLS,
	// status and trailers, and bytes rejected by gRPC's receive limit. The four
	// response caps total 37888; their requests must fit the remaining 224256
	// bytes of the original 256 KiB Range charge. A positive int64 Revoke ID
	// needs at most one tag plus nine varint bytes, reserved before mutations.
	coldQuotaRPCRequestLimit      = 128 << 10
	coldQuotaRPCStageRequestLimit = 262144 - 37888
	coldQuotaRPCRevokeReserve     = 10
)

// The existing lifecycle mutex owns this state. Journal, metadata and Resolve
// each own an independent paid cohort; a call may instead perform the fixed
// Grant/guard/Commit/Revoke cohort.
// A failed or locally refused phase is never reset or refunded. busy spans the
// actual native return, not merely admission or caller-context cancellation.
// This is transport accounting only: the private caller retains all business
// fences, nonce/owner binding, paid admission and ownership of call.finish.
type coldQuotaWorkerRPCState struct {
	self         *workerCall
	cohort       coldQuotaWorkerRPCPhase
	used         uint8
	busy         bool
	requestBytes int
	leaseID      int64
	grantReady   bool
	guardReady   bool
}

func coldQuotaWorkerRPCResponseLimit(phase coldQuotaWorkerRPCPhase) int {
	switch phase {
	case coldQuotaRPCGrant:
		return 4096
	case coldQuotaRPCGuard, coldQuotaRPCCommit, coldQuotaRPCJournal, coldQuotaRPCResolve:
		return 16384
	case coldQuotaRPCRevoke:
		return 1024
	default:
		return 0
	}
}

// Admission must use the original registered call; a diagnostic session or a
// copied call cannot supply it. Shape failures burn an otherwise admissible
// phase, but foreign cleanup IDs and out-of-order calls cannot consume the
// original actor's cleanup reservation. No lifecycle lock spans native work.
func (b *Backend) admitColdQuotaWorkerRPC(call *workerCall, phase coldQuotaWorkerRPCPhase, size int, shapeErr error, leaseID int64) error {
	if !b.validWorkerBackend() || call == nil || call.session == nil {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	s := call.session
	if !b.validWorkerSession(s) || s.operation != call || call.ctx == nil {
		return ErrInvalidRecord
	}
	state := &call.coldQuotaRPC
	if state.self != nil && state.self != call || state.busy || state.used&(1<<phase) != 0 {
		return ErrConflict
	}
	if phase == coldQuotaRPCJournal || phase == coldQuotaRPCMetadata || phase == coldQuotaRPCResolve {
		if state.cohort != 0 {
			return ErrConflict
		}
	} else {
		if state.cohort == coldQuotaRPCJournal || state.cohort == coldQuotaRPCMetadata || state.cohort == coldQuotaRPCResolve {
			return ErrConflict
		}
		switch phase {
		case coldQuotaRPCGrant:
			if state.cohort != 0 {
				return ErrConflict
			}
		case coldQuotaRPCGuard:
			if !state.grantReady || state.used&(1<<coldQuotaRPCRevoke) != 0 {
				return ErrConflict
			}
		case coldQuotaRPCCommit:
			if !state.guardReady || state.used&(1<<coldQuotaRPCRevoke) != 0 {
				return ErrConflict
			}
		case coldQuotaRPCRevoke:
			if leaseID <= 0 || state.leaseID != leaseID {
				return ErrInvalidRecord
			}
		default:
			return ErrInvalidRecord
		}
	}

	// Cleanup keeps the same registered call but outlives its cancellation.
	// Close and session release already join call.done before closing the client.
	if phase != coldQuotaRPCRevoke {
		if b.archiveLife.closed {
			return context.Canceled
		}
		if err := call.ctx.Err(); err != nil {
			return err
		}
		if err := s.live(); err != nil {
			return err
		}
	}
	state.self = call
	if state.cohort == 0 {
		state.cohort = coldQuotaRPCGrant
		if phase == coldQuotaRPCJournal || phase == coldQuotaRPCMetadata || phase == coldQuotaRPCResolve {
			state.cohort = phase
		}
	}
	state.used |= 1 << phase
	if shapeErr != nil {
		return shapeErr
	}
	if phase != coldQuotaRPCJournal && phase != coldQuotaRPCMetadata && phase != coldQuotaRPCResolve {
		reserve := coldQuotaRPCRevokeReserve
		if phase == coldQuotaRPCRevoke {
			reserve = 0
		}
		if size > coldQuotaRPCStageRequestLimit-state.requestBytes-reserve {
			return ErrInvalidMutation
		}
	}
	state.requestBytes += size
	state.busy = true
	return nil
}

// Completion is called only after the original native invocation returns.
// A trustworthy Grant identity remains available for bounded cleanup even if
// TTL, Error, cancellation or a postcheck rejects use of the granted lease.
func (b *Backend) completeColdQuotaWorkerRPC(call *workerCall, phase coldQuotaWorkerRPCPhase, leaseID int64, ready bool) {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	state := &call.coldQuotaRPC
	if phase == coldQuotaRPCGrant {
		state.leaseID = leaseID
		state.grantReady = ready
	}
	if phase == coldQuotaRPCGuard {
		state.guardReady = ready
	}
	state.busy = false
}

func (b *Backend) coldQuotaWorkerRPCContext(call *workerCall, phase coldQuotaWorkerRPCPhase) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if phase == coldQuotaRPCRevoke {
		// The original call remains the cleanup owner. Its requestTimeout bounds
		// this once-only Revoke, independent of canceled publication/session ctx.
		ctx, cancel = b.requestContext(context.Background())
	} else {
		ctx, cancel = call.requestContext()
	}
	if ctx.Err() == nil {
		b.archiveLife.mu.Lock()
		call.started = true
		b.archiveLife.mu.Unlock()
	}
	return ctx, cancel
}

func coldQuotaWorkerRPCOptions(codec *coldQuotaWorkerRPCCodec) []grpc.CallOption {
	// grpc-go v1.82.1's unary sendMsg reports Write failure as nil. Its send
	// success path therefore commits the attempt when the positive five-byte
	// gRPC message header exceeds this zero retry buffer, before RecvMsg can
	// transparently replay cached payload. Pre-send stream selection can retry
	// without payload; etcd interceptor reinvocation hits the one-shot Marshal.
	return []grpc.CallOption{
		grpc.ForceCodec(codec),
		grpc.MaxCallSendMsgSize(coldQuotaRPCRequestLimit),
		grpc.MaxCallRecvMsgSize(codec.responseLimit()),
		grpc.MaxRetryRPCBufferSize(0),
	}
}

func (b *Backend) coldQuotaWorkerRPCHeader(header *pb.ResponseHeader) bool {
	return header != nil && header.ClusterId == b.clusterID && header.Revision > 0
}

// coldQuotaWorkerGrantRPC grants at most once with the original ten-second
// stage request. It does not create a Stage, grant quota authority or retry an
// unknown outcome. The caller holds the original owner/call until this returns.
func (b *Backend) coldQuotaWorkerGrantRPC(call *workerCall) (*pb.LeaseGrantResponse, error) {
	request := &pb.LeaseGrantRequest{TTL: 10}
	size, shapeErr := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCGrant, request)
	if err := b.admitColdQuotaWorkerRPC(call, coldQuotaRPCGrant, size, shapeErr, 0); err != nil {
		return nil, err
	}
	leaseID, ready := int64(0), false
	defer func() { b.completeColdQuotaWorkerRPC(call, coldQuotaRPCGrant, leaseID, ready) }()
	ctx, cancel := b.coldQuotaWorkerRPCContext(call, coldQuotaRPCGrant)
	defer cancel()
	codec := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCGrant, requestBytes: size}
	response, err := pb.NewLeaseClient(b.client.ActiveConnection()).LeaseGrant(ctx, request, coldQuotaWorkerRPCOptions(codec)...)
	if response != nil && b.coldQuotaWorkerRPCHeader(response.Header) && response.ID > 0 {
		leaseID = response.ID
	}
	if err != nil || ctx.Err() != nil {
		return response, errors.Join(ErrOutcomeUnknown, err, ctx.Err())
	}
	if leaseID == 0 || response.TTL <= 0 || response.Error != "" {
		return response, ErrOutcomeUnknown
	}
	if err := call.postcheck(); err != nil {
		return response, errors.Join(ErrOutcomeUnknown, err)
	}
	ready = true
	return response, nil
}

// coldQuotaWorkerGuardRPC sends the private caller's actual frozen guard body.
// The request and its complete object graph remain caller-owned and immutable
// until the native return; the transport neither changes fences nor adds ops.
func (b *Backend) coldQuotaWorkerGuardRPC(call *workerCall, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	return b.coldQuotaWorkerTxnRPC(call, coldQuotaRPCGuard, request)
}

// coldQuotaWorkerCommitRPC sends the original frozen Commit only after the
// original guard's successful ACK. Business ACK validation remains the caller's.
func (b *Backend) coldQuotaWorkerCommitRPC(call *workerCall, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	return b.coldQuotaWorkerTxnRPC(call, coldQuotaRPCCommit, request)
}

// coldQuotaWorkerJournalRPC owns a separate one-Txn paid cohort. Its exact
// success and failure bodies are preserved, including the journal's three point
// reads. This helper cannot authorize a subsequent Grant on the same call.
func (b *Backend) coldQuotaWorkerJournalRPC(call *workerCall, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	return b.coldQuotaWorkerTxnRPC(call, coldQuotaRPCJournal, request)
}

// coldQuotaWorkerResolveRPC owns one independent paid arbitration transaction.
// It uses the original registered call, ActiveConnection, single serialization
// and actual native-return join; no other phase can share this call's cohort.
// The caller keeps the request graph unchanged until the native return.
func (b *Backend) coldQuotaWorkerResolveRPC(call *workerCall, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	return b.coldQuotaWorkerTxnRPC(call, coldQuotaRPCResolve, request)
}

func (b *Backend) coldQuotaWorkerTxnRPC(call *workerCall, phase coldQuotaWorkerRPCPhase, request *pb.TxnRequest) (*pb.TxnResponse, error) {
	size, shapeErr := coldQuotaWorkerRPCRequestBytes(phase, request)
	if err := b.admitColdQuotaWorkerRPC(call, phase, size, shapeErr, 0); err != nil {
		return nil, err
	}
	ready := false
	defer func() { b.completeColdQuotaWorkerRPC(call, phase, 0, ready) }()
	ctx, cancel := b.coldQuotaWorkerRPCContext(call, phase)
	defer cancel()
	codec := &coldQuotaWorkerRPCCodec{phase: phase, requestBytes: size}
	response, err := pb.NewKVClient(b.client.ActiveConnection()).Txn(ctx, request, coldQuotaWorkerRPCOptions(codec)...)
	if err != nil || ctx.Err() != nil {
		return response, errors.Join(ErrOutcomeUnknown, err, ctx.Err())
	}
	if response == nil || !b.coldQuotaWorkerRPCHeader(response.Header) {
		return response, ErrOutcomeUnknown
	}
	if err := call.postcheck(); err != nil {
		return response, errors.Join(ErrOutcomeUnknown, err)
	}
	ready = response.Succeeded
	return response, nil
}

// coldQuotaWorkerRevokeRPC consumes the reserved cleanup once, only for the
// original call's identified Grant. Cancellation/Close cannot replace that ID
// or finish its owner early; the original caller joins this actual return.
func (b *Backend) coldQuotaWorkerRevokeRPC(call *workerCall, request *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	size, shapeErr := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCRevoke, request)
	if shapeErr != nil {
		return nil, shapeErr
	}
	// Copy the scalar before admitting cleanup, so the dispatched identity is
	// exactly the original ID checked under the lifecycle mutex.
	owned := &pb.LeaseRevokeRequest{ID: request.ID}
	if err := b.admitColdQuotaWorkerRPC(call, coldQuotaRPCRevoke, size, nil, owned.ID); err != nil {
		return nil, err
	}
	defer b.completeColdQuotaWorkerRPC(call, coldQuotaRPCRevoke, 0, false)
	ctx, cancel := b.coldQuotaWorkerRPCContext(call, coldQuotaRPCRevoke)
	defer cancel()
	codec := &coldQuotaWorkerRPCCodec{phase: coldQuotaRPCRevoke, requestBytes: size}
	response, err := pb.NewLeaseClient(b.client.ActiveConnection()).LeaseRevoke(ctx, owned, coldQuotaWorkerRPCOptions(codec)...)
	if err != nil || ctx.Err() != nil {
		return response, errors.Join(ErrOutcomeUnknown, err, ctx.Err())
	}
	if response == nil || !b.coldQuotaWorkerRPCHeader(response.Header) {
		return response, ErrOutcomeUnknown
	}
	return response, nil
}
