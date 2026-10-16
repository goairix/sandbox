package provisioning

import (
	"context"
	"errors"
	"io"
	"reflect"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

type PlainExecState string

const (
	PlainExecRejected      PlainExecState = "rejected"
	PlainExecUnknown       PlainExecState = "unknown"
	PlainExecLocalTerminal PlainExecState = "local_terminal"
)

// PlainExecResult retains exact original attribution even when execution is
// unknown. Only LocalTerminal permits reading actual root wait/drain scalars
// from Receipt; the signed local assertion never authorizes business End.
type PlainExecResult struct {
	State     PlainExecState
	Operation state.OperationReference
	Effect    state.ExecEffectReference
	Receipt   p.LocalExecReceiptEvidence
}

// Exec creates one new operation per invocation. RequestID is attribution, not
// a deduplication or recovery key: callers must not automatically retry unknown
// results, even with the same RequestID. Retain the original references instead.
// Request is fully resolved and copied; no environment or execution defaults
// are inherited. Writers run synchronously in original Wait and must return.
// Close joins their owner and cannot cancel arbitrary blocking writer code.
func (s *PlainColdService) Exec(ctx context.Context, sandboxID, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (result PlainExecResult, err error) {
	result.State = PlainExecRejected
	if s == nil || s.self != s {
		return result, ErrPlainColdProfile
	}
	descriptor, err := p.NewExecutionDescriptor(request)
	if err != nil {
		return result, err
	}
	request = descriptor.Request()
	if request.UID != s.config.UID || request.GID != s.config.GID || request.TTY || (request.RequiresNetwork && !s.config.NetworkAllowed) {
		return result, ErrPlainColdProfile
	}
	call, err := s.enterFor(ctx, time.Duration(request.TimeoutSeconds)*time.Second+30*time.Second)
	if err != nil {
		return result, err
	}
	defer s.leave(call)
	ctx = call.context
	loaded, err := s.loadColdTarget(ctx, sandboxID)
	if err != nil {
		return result, err
	}
	now, err := s.now(ctx)
	if err != nil {
		return result, err
	}
	if err := checkPlainColdLookupTTL(now, loaded.control.ExpiresAt); err != nil {
		return result, err
	}
	if err := s.backend.ConfirmSandboxView(ctx, loaded.view); err != nil {
		return result, err
	}
	admitted, err := s.backend.BeginOperation(ctx, state.BeginOperationInput{SandboxID: sandboxID, RequestID: requestID, Kind: state.OperationData})
	result.Operation = admitted.Reference
	if admitted.Reference.OperationID != "" {
		result.State = PlainExecUnknown
	}
	if err != nil {
		return result, errors.Join(err, admitted.GuardCleanupError)
	}
	capability := admitted.Capability
	if capability == nil || admitted.Outcome != state.OperationCommitted {
		return result, state.ErrDeliveryUnknown
	}
	result.State = PlainExecUnknown
	// Cleanup owns only the original Lease. It cannot assert remote drain. This
	// defer runs after the later original handle.Close and renewal worker join.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := s.backend.CancelOperationCapability(cleanup, capability)
		cancel()
		err = errors.Join(err, cleanupErr)
		s.mu.Lock()
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		if s.closed {
			err = errors.Join(err, context.Canceled)
		}
		if err != nil {
			result.State = PlainExecUnknown
		}
		s.mu.Unlock()
	}()
	if !reflect.DeepEqual(capability.Control(), *loaded.control) {
		return result, state.ErrConflict
	}
	c := loaded.control
	identity := p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: p.RuntimeReference(*c.Runtime)}
	enrollment := loaded.history.Publication.Enrollment.Request
	destination, credential, err := s.destination(s.target(loaded.dispatch.Pod, c.Runtime.UID), state.ColdBootstrapMaterial{RuntimeCertificate: enrollment.RuntimeCertificate, IssuerCertificate: enrollment.IssuerCertificate}, identity)
	if err != nil {
		return result, err
	}
	defer credential.Close()
	prepared, err := s.backend.PrepareExecEffect(ctx, capability, request)
	result.Effect = prepared.Reference
	if err != nil || prepared.GuardCleanupError != nil {
		return result, errors.Join(err, prepared.GuardCleanupError)
	}
	if prepared.Outcome != state.OutcomeCommitted || prepared.Prepared == nil {
		return result, state.ErrDeliveryUnknown
	}
	handle, err := s.backend.DeliverExecEffect(ctx, prepared.Prepared, destination)
	if handle != nil {
		defer func() { err = errors.Join(err, handle.Close()) }()
	}
	if err != nil {
		return result, err
	}
	if handle == nil {
		return result, state.ErrDeliveryUnknown
	}
	result.Receipt, err = s.waitExec(call, handle, stdout, stderr)
	if err != nil {
		return result, err
	}
	if result.Receipt.State() != "local_terminal" || !result.Receipt.DrainConfirmed() || result.Receipt.Context().CommandID != result.Effect.CommandID || result.Receipt.Context().OperationID != result.Operation.OperationID || result.Receipt.DescriptorDigest() != descriptor.Digest() {
		return result, state.ErrDeliveryUnknown
	}
	result.State = PlainExecLocalTerminal
	return result, nil
}

func (s *PlainColdService) waitExec(call *plainColdCall, handle *state.ExecDeliveryHandle, stdout, stderr io.Writer) (p.LocalExecReceiptEvidence, error) {
	stop := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				joined <- nil
				return
			case <-call.context.Done():
				joined <- call.context.Err()
				return
			case <-ticker.C:
				// A tick selected beside stop must not schedule another renewal.
				select {
				case <-stop:
					joined <- nil
					return
				default:
				}
				err := s.backend.RenewExecDelivery(call.context, handle)
				if err != nil {
					if errors.Is(err, state.ErrExecDeliveryTerminal) {
						joined <- nil
					} else {
						call.cancel()
						joined <- err
					}
					return
				}
			}
		}
	}()
	receipt, waitErr := handle.Wait(call.context, stdout, stderr)
	close(stop)
	// Do not manufacture cancellation of a bounded renewal after terminal.
	// Its actual error remains an error even if Wait concurrently saw terminal.
	renewErr := <-joined
	return receipt, errors.Join(waitErr, renewErr)
}
