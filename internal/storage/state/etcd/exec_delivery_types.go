package etcd

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

var ErrDeliveryUnknown = errors.New("exec delivery outcome unknown")

// ExecDeliveryHandle retains original private authority and diagnostic receipt
// attribution. No historical record or copied handle can reconstruct it.
type ExecDeliveryHandle struct {
	self        *ExecDeliveryHandle
	origin      *Backend
	capability  *OperationCapability
	draft       *execEffectDraft
	destination *t.Destination
	mu          sync.Mutex
	reader      *deliveryReader
	closed      bool
	waiting     bool
	renewing    bool
	accepted    bool
	terminal    bool
	unknown     bool
	deadline    time.Time
	pending     time.Time
	receipt     p.LocalExecReceiptEvidence
}

func (h *ExecDeliveryHandle) valid() bool {
	return h != nil && h.self == h && h.origin != nil && h.capability != nil && h.draft != nil && h.capability.origin == h.origin && h.capability.execDraft == h.draft && h.draft.delivery == h
}
func (h *ExecDeliveryHandle) Reference() ExecEffectReference {
	if !h.valid() {
		return ExecEffectReference{}
	}
	return h.draft.reference
}

type deliveryOutput struct {
	kind byte
	data []byte
}
type deliveryReader struct {
	frames  chan deliveryOutput
	done    chan struct{}
	cancel  context.CancelFunc
	receipt p.LocalExecReceiptEvidence
	err     error
}

// startReader is called once after authenticated acceptance and before releasing
// the original capability mutex. Its watchdog is local cleanup only, never
// execution authority, and renewal cannot extend it.
func (h *ExecDeliveryHandle) startReader(session *t.Session, ackAt time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), ackAt.Add(time.Duration(h.draft.descriptor.Request().TimeoutSeconds)*time.Second+30*time.Second))
	parentDone := make(chan struct{})
	stopParent := context.AfterFunc(h.capability.parentCtx, func() { defer close(parentDone); cancel() })
	reader := &deliveryReader{frames: make(chan deliveryOutput, 2), done: make(chan struct{}), cancel: cancel}
	h.mu.Lock()
	h.reader = reader
	h.mu.Unlock()
	go func() {
		defer close(reader.done)
		defer func() {
			if reader.err != nil {
				h.mu.Lock()
				h.closed = true
				h.mu.Unlock()
			}
		}()
		defer close(reader.frames)
		defer cancel()
		defer session.Close()
		defer func() {
			if !stopParent() {
				<-parentDone
			}
		}()
		for {
			kind, wire, err := session.Read(ctx)
			if err != nil {
				reader.err = errors.Join(ErrDeliveryUnknown, err)
				return
			}
			switch kind {
			case t.EventStdout, t.EventStderr:
				timer := time.NewTimer(5 * time.Second)
				select {
				case reader.frames <- deliveryOutput{kind, wire}:
					timer.Stop()
				case <-ctx.Done():
					timer.Stop()
					reader.err = errors.Join(ErrDeliveryUnknown, ctx.Err())
					return
				case <-timer.C:
					reader.err = ErrDeliveryUnknown
					return
				}
			case t.EventReceipt:
				reader.receipt, reader.err = h.observeReceipt(ctx, kind, wire)
				if reader.err == nil && reader.receipt.State() != "local_terminal" {
					reader.err = ErrDeliveryUnknown
				}
				return
			default:
				reader.err = ErrDeliveryUnknown
				return
			}
		}
	}()
}

// Close releases only local stream resources and joins their reader. It cannot
// cancel remote execution, produce business End, or release original ownership.
func (h *ExecDeliveryHandle) Close() error {
	if !h.valid() {
		return ErrInvalidRecord
	}
	h.mu.Lock()
	h.closed = true
	reader := h.reader
	h.mu.Unlock()
	if reader != nil {
		reader.cancel()
		<-reader.done
	}
	return nil
}

// Wait consumes bounded output in the caller goroutine. Supplied writers must
// return; no detached worker is used to pretend arbitrary writer IO is canceled.
// After uncertain IO, another Wait queries the same command without replay.
func (h *ExecDeliveryHandle) Wait(ctx context.Context, stdout, stderr io.Writer) (p.LocalExecReceiptEvidence, error) {
	if !h.valid() || ctx == nil {
		return p.LocalExecReceiptEvidence{}, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	h.mu.Lock()
	if h.waiting {
		h.mu.Unlock()
		return p.LocalExecReceiptEvidence{}, ErrConflict
	}
	h.waiting = true
	reader := h.reader
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.waiting = false; h.mu.Unlock() }()
	if reader == nil {
		return h.query(ctx)
	}
	defer func() {
		reader.cancel()
		<-reader.done
		h.mu.Lock()
		if h.reader == reader {
			h.reader = nil
		}
		h.mu.Unlock()
	}()
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	for {
		select {
		case <-ctx.Done():
			return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, ctx.Err())
		case frame, ok := <-reader.frames:
			if !ok {
				<-reader.done
				return reader.receipt, reader.err
			}
			writer := stdout
			if frame.kind == t.EventStderr {
				writer = stderr
			}
			n, err := writer.Write(frame.data)
			if err == nil && n != len(frame.data) {
				err = io.ErrShortWrite
			}
			if err != nil {
				return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, err)
			}
		}
	}
}
func (h *ExecDeliveryHandle) query(ctx context.Context) (p.LocalExecReceiptEvidence, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.origin.verifyExecDestination(bounded, h.destination, h.draft); err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	s, err := h.destination.Open(bounded)
	if err != nil {
		return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, err)
	}
	defer s.Close()
	if err := h.origin.verifyExecDestination(bounded, h.destination, h.draft); err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	e := t.Envelope{Version: 1, Purpose: "exec_query", Context: h.draft.claims.Context, DescriptorDigest: h.draft.descriptor.Digest()}
	if err = s.Send(bounded, e); err != nil {
		return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, err)
	}
	kind, wire, err := s.Read(bounded)
	if err != nil {
		return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, err)
	}
	return h.observeReceipt(bounded, kind, wire)
}
func (h *ExecDeliveryHandle) observeReceipt(ctx context.Context, kind byte, wire []byte) (p.LocalExecReceiptEvidence, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if kind != t.EventAccepted && kind != t.EventReceipt {
		return p.LocalExecReceiptEvidence{}, ErrDeliveryUnknown
	}
	d := h.draft
	if err := h.origin.verifyExecDestination(ctx, h.destination, d); err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	verify := func(deadline time.Time) (p.LocalExecReceiptEvidence, error) {
		return h.destination.VerifyReceipt(ctx, wire, d.claims.Context, d.descriptor.Digest(), d.ticket.Digest(), d.claims.NotBefore, d.claims.NotAfter, deadline)
	}
	e, err := verify(h.deadline)
	pendingMatch := false
	if err != nil && !h.pending.IsZero() {
		e, err = verify(h.pending)
		pendingMatch = err == nil
	}
	if err != nil {
		return p.LocalExecReceiptEvidence{}, errors.Join(ErrDeliveryUnknown, err)
	}
	switch e.State() {
	case "accepted":
		if kind == t.EventAccepted && !h.terminal && !h.unknown {
			h.accepted = true
			if pendingMatch {
				h.deadline = h.pending
				h.pending = time.Time{}
			}
		}
	case "local_terminal":
		h.terminal = true
		h.accepted = false
	case "unknown":
		h.unknown = true
		h.accepted = false
	default:
		return p.LocalExecReceiptEvidence{}, ErrDeliveryUnknown
	}
	h.receipt = e
	return e, nil
}
