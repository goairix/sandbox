package provisioning

import (
	"context"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
)

// ErrPlainColdDestroyConflict means the observed sandbox is already closing or
// its original ownership changed. A caller must not treat it as cleanup success.
var ErrPlainColdDestroyConflict = errors.New("plain cold destroy target changed or closing")

// ErrPlainColdDestroyProfile means no cleanup consumer supports this target's
// immutable snapshot schema. Admission remains open and no Task is prepared.
var ErrPlainColdDestroyProfile = errors.New("plain cold destroy cleanup profile unsupported")

// PlainColdDestroyState describes admission of metadata cleanup work, never the
// runtime's physical termination, workspace settlement or quota release.
type PlainColdDestroyState string

const (
	// PlainColdDestroyRejected means this call has no unresolved metadata commit.
	PlainColdDestroyRejected PlainColdDestroyState = "rejected"
	// PlainColdDestroyPending means the original close/task transaction committed.
	PlainColdDestroyPending PlainColdDestroyState = "pending"
	// PlainColdDestroyUnknown retains attribution when admission or cleanup of the
	// original Stage cannot be fully acknowledged; a new attempt is not a retry.
	PlainColdDestroyUnknown PlainColdDestroyState = "unknown"
)

// PlainColdDestroyResult contains copied diagnostics for one destroy admission.
// Task and Stage grant no operation authority. A nonzero Task may have been
// allocated before a failed preparation and does not prove that the Task exists.
type PlainColdDestroyResult struct {
	State                PlainColdDestroyState
	SandboxID, RequestID string
	Generation           int64
	Runtime              p.RuntimeReference
	Task                 state.TaskReference
	Stage                state.StageReference
}

// Destroy closes admission and creates the existing permanent cleanup Task in
// one transaction for a version2 local cleanup snapshot. Historical/detached
// targets without that descriptor are refused before preparing any Stage.
// It accepts expired active sandboxes without querying runtime health.
// The deployment's operator-wide authorization is the caller's boundary;
// this method does not claim tenant or creation-Principal ownership.
//
// RequestID is attribution only, not durable HTTP idempotency. Each invocation
// makes at most one preparation and commit, and may resolve only that exact
// Stage. Unknown results must retain these references instead of automatically
// invoking Destroy again, which could target a later generation of the same ID.
// The original service admission remains active through synchronous Stage lease
// cleanup, so Close waits for that cleanup's actual return.
func (s *PlainColdService) Destroy(ctx context.Context, sandboxID, requestID string) (result PlainColdDestroyResult, err error) {
	result = PlainColdDestroyResult{State: PlainColdDestroyRejected, SandboxID: sandboxID, RequestID: requestID}
	if s == nil || s.self != s {
		return result, ErrPlainColdProfile
	}
	parsed, parseErr := uuid.Parse(sandboxID)
	if parseErr != nil || parsed == uuid.Nil || parsed.String() != sandboxID || !validColdDestroyRequestID(requestID) {
		return result, state.ErrInvalidRecord
	}

	call, err := s.enter(ctx)
	if err != nil {
		return result, err
	}
	defer s.leave(call)
	ctx = call.context
	defer func() {
		// The later Stage-release defer runs first. An interrupted response or
		// cleanup failure cannot be reported as an unqualified accepted result.
		if err == nil {
			s.mu.Lock()
			err = ctx.Err()
			if s.closed {
				err = errors.Join(err, context.Canceled)
			}
			s.mu.Unlock()
		}
		if errors.Is(err, state.ErrOutcomeUnknown) || result.State == PlainColdDestroyPending && err != nil {
			result.State = PlainColdDestroyUnknown
			err = errors.Join(state.ErrOutcomeUnknown, err)
		}
	}()

	loaded, err := s.loadColdTarget(ctx, sandboxID)
	if err != nil {
		return result, coldDestroyTargetError(err)
	}
	result.Generation = loaded.control.Generation
	result.Runtime = p.RuntimeReference(*loaded.control.Runtime)
	if err := checkColdDestroyProfile(loaded.snapshot); err != nil {
		return result, err
	}

	// The trusted clock is a deployment precondition, not an expiry gate:
	// manual cleanup must remain legal after the sandbox's original TTL.
	if _, err := s.now(ctx); err != nil {
		return result, err
	}
	if err := s.backend.ConfirmSandboxView(ctx, loaded.view); err != nil {
		return result, coldDestroyTargetError(err)
	}

	// PrepareDestroy re-reads and fences placement/control/owner/generation/
	// runtime-index against this exact observed control revision. Its 30-second
	// dedicated Stage lease bounds one attempt, not the durable Task lifetime.
	stage, task, err := s.backend.PrepareDestroy(ctx, state.BeginDestroyInput{SandboxID: sandboxID, RequestID: requestID, ExpectedControlRevision: loaded.view.ControlRevision()}, 30*time.Second)
	result.Task = task
	if stage != nil {
		result.Stage = stage.Reference()
		defer func() {
			// Cancellation cannot skip release, nor does release imply abort. The
			// fresh bounded context owns only this original lease, within the
			// still-active call; no detached goroutine survives Close's join.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = errors.Join(err, s.backend.ReleaseStage(cleanup, stage))
		}()
	}
	if err != nil {
		return result, coldDestroyTargetError(err)
	}
	if stage == nil {
		return result, state.ErrOutcomeUnknown
	}

	outcome, commitErr := s.backend.CommitStage(ctx, stage)
	if outcome != state.OutcomeCommitted || commitErr != nil {
		// Resolve arbitrates only the original attempt. It may acknowledge an
		// already committed receipt or permanently abort the pending mutation;
		// neither case permits silently constructing a replacement attempt.
		if ctx.Err() != nil {
			return result, errors.Join(state.ErrOutcomeUnknown, commitErr, ctx.Err())
		}
		resolved, resolveErr := s.backend.ResolveStage(ctx, result.Stage)
		if resolveErr != nil {
			return result, errors.Join(state.ErrOutcomeUnknown, commitErr, resolveErr)
		}
		outcome = resolved
	}
	switch outcome {
	case state.OutcomeCommitted:
		result.State = PlainColdDestroyPending
		return result, nil
	case state.OutcomeAborted:
		return result, ErrPlainColdDestroyConflict
	default:
		return result, state.ErrOutcomeUnknown
	}
}

// checkColdDestroyProfile narrows the already authenticated snapshot to the
// implemented local cleanup consumer. The domain reader, not this shape check,
// verifies its digest and exact workspace/placement binding. No descriptor may
// be invented for detached targets or historical version1 snapshots.
func checkColdDestroyProfile(snapshot *state.SandboxSnapshotRecord) error {
	if snapshot == nil || snapshot.Version != 2 || snapshot.LocalCleanup == nil || snapshot.LocalCleanup.Provider != "local" {
		return ErrPlainColdDestroyProfile
	}
	return nil
}

// Stage request IDs are path segments with a 128-byte protocol limit. Validate
// before any metadata read instead of letting malformed HTTP attribution reach
// the Stage builder; this matches the existing storage namespace grammar.
func validColdDestroyRequestID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func coldDestroyTargetError(err error) error {
	if errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrOperationAdmissionClosed) || errors.Is(err, ErrPlainColdUnavailable) {
		return errors.Join(ErrPlainColdDestroyConflict, err)
	}
	return err
}
