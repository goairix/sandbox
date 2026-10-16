package provisioning

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

type PlainColdBusinessOptions struct {
	Principal string
	Defaults  sandboxsnapshot.Defaults
}

type PlainColdBusinessInput struct {
	IdempotencyKey string
	Request        types.CreateSandboxRequest
}

type PlainColdBusinessResult struct {
	PlainColdResult
	IdempotencyKey string
	Business       *sandboxsnapshot.Snapshot
	Reused         bool
}

func ownColdBusiness(backend *state.Backend, input *PlainColdBusinessOptions, cfg launcherconfig.Config, cpu, memory, disk, tmp string) (*PlainColdBusinessOptions, error) {
	if input == nil {
		return nil, nil
	}
	if _, err := backend.DetachedIdentity(input.Principal, "configuration"); err != nil || input.Defaults.WorkspaceMountMode != "" {
		return nil, ErrPlainColdProfile
	}
	request := types.CreateSandboxRequest{Mode: "ephemeral", Network: &types.NetworkConfig{Enabled: cfg.NetworkAllowed}}
	business, err := sandboxsnapshot.Build(request, input.Defaults)
	if err != nil || business.Resolved.TTL.Kind != "finite" || business.Resolved.TTL.Seconds > 365*24*60*60 {
		return nil, ErrPlainColdProfile
	}
	wire, err := sandboxsnapshot.Encode(business)
	if err != nil {
		return nil, ErrPlainColdProfile
	}
	probe := &PlainColdService{config: cfg, spec: runtime.SandboxSpec{CPU: cpu, Memory: memory, Disk: disk, TmpDisk: tmp}}
	if err := probe.validateColdBusiness(plainColdCapsule{Version: 2, TTL: time.Duration(business.Resolved.TTL.Seconds) * time.Second, Business: wire}); err != nil {
		return nil, err
	}
	return &PlainColdBusinessOptions{Principal: strings.Clone(input.Principal), Defaults: businessDefaults(business)}, nil
}

func businessDefaults(business *sandboxsnapshot.Snapshot) sandboxsnapshot.Defaults {
	return sandboxsnapshot.Defaults{TimeoutSeconds: int(business.Resolved.TTL.Seconds), Resources: business.Resolved.Resources, WorkspaceMountMode: business.Resolved.WorkspaceMountMode}
}

// CreateBusiness owns one original service admission from request lookup to
// completion. A returned configuration is metadata, never execution authority.
func (s *PlainColdService) CreateBusiness(ctx context.Context, input PlainColdBusinessInput) (output PlainColdBusinessResult, err error) {
	output.IdempotencyKey = input.IdempotencyKey
	if output.IdempotencyKey == "" {
		output.IdempotencyKey = uuid.NewString()
	}
	output.State = PlainColdPending
	call, err := s.enter(ctx)
	if err != nil {
		return output, err
	}
	defer s.leave(call)
	defer func() {
		if err == nil {
			s.mu.Lock()
			if s.closed {
				err = context.Canceled
			} else {
				err = call.context.Err()
			}
			s.mu.Unlock()
		}
	}()
	if s.business == nil {
		return output, ErrPlainColdProfile
	}
	ctx = call.context
	w, err := s.backend.DetachedIdentity(s.business.Principal, output.IdempotencyKey)
	if err != nil {
		return output, err
	}
	// Capture the normalized request before the first RPC. Replay below still
	// resolves against the original snapshot rather than these current defaults.
	business, err := sandboxsnapshot.Build(input.Request, s.business.Defaults)
	if err != nil {
		return output, err
	}
	input.Request = business.Requested
	record, err := s.backend.LoadRequest(ctx, s.business.Principal, output.IdempotencyKey)
	if err != nil {
		return output, err
	}
	if record != nil {
		return s.replayBusinessEntered(ctx, w, *record, input.Request, output)
	}
	request := PlainColdRequest{Principal: s.business.Principal, IdempotencyKey: output.IdempotencyKey, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), TTL: time.Duration(business.Resolved.TTL.Seconds) * time.Second, Business: business}
	output.Business = business
	result, acquired, acquireErr := s.acquireEntered(ctx, w, request)
	output.PlainColdResult = result
	contention := errors.Is(acquireErr, state.ErrIdempotencyConflict)
	if errors.Is(acquireErr, state.ErrConflict) && acquired.Reference != (state.StageReference{}) && acquired.Reference.StageID == "acquire" {
		// The original comparison loss still reports Unknown. Resolve that
		// exact stage once; only a durable aborted receipt permits recovery.
		outcome, resolveErr := s.backend.ResolveStage(ctx, acquired.Reference)
		if resolveErr != nil {
			return output, resolveErr
		}
		switch outcome {
		case state.OutcomeAborted:
			contention = true
		case state.OutcomeCommitted:
			acquired.Outcome, acquireErr = outcome, nil
		default:
			return output, state.ErrOutcomeUnknown
		}
	}
	if contention {
		// A different generated capsule won this key before our acquisition.
		// Only one bounded, fully authenticated original-request read follows.
		original, readErr := s.backend.LoadRequest(ctx, s.business.Principal, output.IdempotencyKey)
		if readErr != nil {
			return output, readErr
		}
		if original != nil {
			return s.replayBusinessEntered(ctx, w, *original, input.Request, output)
		}
		output.State = PlainColdRetryable
		return output, state.ErrConflict
	}
	if acquireErr != nil {
		return output, acquireErr
	}
	if acquired.Disposition == state.AcquireOccupied {
		output.State = PlainColdRetryable
		return output, state.ErrConflict
	}
	if acquired.Outcome != state.OutcomeCommitted {
		return output, state.ErrOutcomeUnknown
	}
	output.PlainColdResult, err = s.resume(ctx, w, result)
	return output, err
}
