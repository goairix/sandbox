package provisioning

import (
	"context"
	"time"

	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
)

func (s *PlainColdService) replayBusinessEntered(ctx context.Context, w state.WorkspaceIdentity, record state.CreationRequestRecord, incoming types.CreateSandboxRequest, output PlainColdBusinessResult) (PlainColdBusinessResult, error) {
	output.RequestID, output.IntentID, output.SandboxID = record.RequestID, record.IntentID, record.SandboxID
	output.State = PlainColdRecoveryRequired
	if record.WorkspaceHash != w.Hash() || record.RestoreEpoch != s.config.Binding.RestoreEpoch {
		return output, ErrPlainColdProfile
	}
	intent, err := s.backend.LoadCreationIntent(ctx, w, record.IntentID)
	if err != nil {
		return output, err
	}
	if intent == nil || intent.SandboxID != record.SandboxID || intent.WorkspaceHash != record.WorkspaceHash || intent.RequestHash != record.RequestHash || intent.ConfigurationDigest != record.ConfigurationDigest || intent.RestoreEpoch != record.RestoreEpoch || intent.Generation != record.Generation {
		return output, ErrPlainColdProfile
	}
	var ref state.SnapshotReference
	if intent.Phase != "published" && record.Phase != "completed" {
		control, readErr := s.backend.LoadControl(ctx, w.Partition(), record.SandboxID)
		if readErr != nil {
			return output, readErr
		}
		if control != nil {
			if control.WorkspaceHash != w.Hash() || control.IntentID != record.IntentID || control.RestoreEpoch != record.RestoreEpoch || control.Generation != record.Generation {
				return output, ErrPlainColdProfile
			}
			ref = control.Snapshot
		}
	}
	if ref == (state.SnapshotReference{}) {
		entry, readErr := s.backend.LoadRuntimePublicationEnrolled(ctx, w, record.IntentID)
		if readErr != nil {
			return output, readErr
		}
		if entry == nil || entry.Record.SandboxID != record.SandboxID || entry.Record.WorkspaceHash != record.WorkspaceHash || entry.Record.IntentID != record.IntentID || entry.Record.RestoreEpoch != record.RestoreEpoch || entry.Record.Generation != record.Generation {
			return output, ErrPlainColdProfile
		}
		ref = entry.Record.Snapshot
	}
	snapshot, err := s.backend.LoadSnapshot(ctx, w.Partition(), record.SandboxID, ref)
	if err != nil {
		return output, err
	}
	if snapshot == nil {
		return output, ErrPlainColdProfile
	}
	request, err := s.replayBusinessRequest(w, record, ref, snapshot.Payload, incoming)
	if err != nil {
		return output, err
	}
	request.Principal, request.IdempotencyKey = s.business.Principal, output.IdempotencyKey
	output.Business = request.Business
	output.PlainColdResult, err = s.createEntered(ctx, w, request)
	return output, err
}

func (s *PlainColdService) replayBusinessRequest(w state.WorkspaceIdentity, record state.CreationRequestRecord, ref state.SnapshotReference, payload []byte, incoming types.CreateSandboxRequest) (PlainColdRequest, error) {
	var capsule plainColdCapsule
	if decodeColdCapsule(payload, &capsule) != nil || capsule.Version != 2 || capsule.Deployment != s.profile || capsule.WorkspaceHash != w.Hash() || capsule.Partition != w.Partition() || record.WorkspaceHash != w.Hash() || capsule.RequestID != record.RequestID || capsule.IntentID != record.IntentID || capsule.SandboxID != record.SandboxID || capsule.SnapshotVersion != ref.Version {
		return PlainColdRequest{}, ErrPlainColdProfile
	}
	if err := s.validateColdBusiness(capsule); err != nil {
		return PlainColdRequest{}, err
	}
	business, err := sandboxsnapshot.Decode(capsule.Business)
	if err != nil {
		return PlainColdRequest{}, ErrPlainColdProfile
	}
	requested, err := sandboxsnapshot.Build(incoming, businessDefaults(business))
	if err != nil {
		return PlainColdRequest{}, err
	}
	if requested.RequestedDigest != business.RequestedDigest {
		return PlainColdRequest{}, state.ErrIdempotencyConflict
	}
	return PlainColdRequest{RequestID: capsule.RequestID, IntentID: capsule.IntentID, SandboxID: capsule.SandboxID, SnapshotVersion: capsule.SnapshotVersion, TTL: time.Duration(business.Resolved.TTL.Seconds) * time.Second, Business: business}, nil
}
