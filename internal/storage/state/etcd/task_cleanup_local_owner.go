package etcd

import (
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Only the authentic history loader publishes this completed mode. Both
// mutable records retain their exact original native fences across new-Run
// admission; the copied values are expectations, never release authority.
type cleanupCompletedTermination struct {
	termination taskFence
	provider    taskFence
}

// This pure matcher checks attribution after the original history loader has
// authenticated the observer signature, enrollment and historical stamps.
// A diagnostic phase alone, or codec-valid proof bytes, cannot authorize work.
func matchCleanupCompletedTermination(state *cleanupSlotState, intent, provider *mvccpb.KeyValue) error {
	if state == nil || state.record.Validate() != nil || state.record.Phase != "occupied" || state.record.Run == nil || !state.record.Run.Joined || state.record.Archive == nil || state.record.Release != nil {
		return ErrInvalidRecord
	}
	r, err := decodeLocalRuntimeTermination(intent)
	if err != nil || r.Phase != "cleanup-complete" || localRuntimeTerminationTask(r) != state.record.Task || r.TaskBirth != state.record.TaskBirth {
		return errors.Join(ErrCorruptRecord, err)
	}
	cleanup, err := decodeTaskRuntimeProviderCleanup(provider)
	if err != nil || !localRuntimeProviderMatches(cleanup, r, cleanup.Binding.ContractDigest) || !localRuntimeProviderComplete(cleanup) || workspaceSyncIntentDigest(string(provider.Value)) != r.CleanupDigest {
		return errors.Join(ErrCorruptRecord, err)
	}
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	if err != nil || string(intent.Key) != keys.Intent || string(provider.Key) != keys.Task+"/runtime-provider-cleanup" {
		return errors.Join(ErrCorruptRecord, err)
	}

	purpose := ""
	switch state.record.Confirmed {
	case "local-complete":
		purpose = "termination"
	case "terminated":
		purpose = "owner-release"
	default:
		return ErrOutcomeUnknown
	}
	want := cleanupExecutionPending{Purpose: purpose, ClaimID: r.Current.ClaimID, LeaseID: r.Current.LeaseID}
	if state.record.Pending == nil || *state.record.Pending != want {
		return ErrConflict
	}
	return nil
}

// Completed records are mutable: Version>1 and Mod>Create are legitimate.
// Their original codecs validate the envelope, while exact native fences
// reject deletion/recreation or any intervening change, including same bytes.
func matchCleanupCompletedFences(expected *cleanupCompletedTermination, intent, provider *mvccpb.KeyValue) error {
	if expected == nil {
		return ErrInvalidRecord
	}
	if _, err := decodeLocalRuntimeTermination(intent); err != nil {
		return err
	}
	if _, err := decodeTaskRuntimeProviderCleanup(provider); err != nil {
		return err
	}
	if ownTaskFence(intent) != expected.termination || ownTaskFence(provider) != expected.provider {
		return ErrConflict
	}
	return nil
}

// A completed recovery never calls Terminate, observer Query or provider
// mutation. Its original job owns this read and renewal; both actually return
// before Release. Release then reloads genuine authority/history/local state
// and uses its unchanged full 60-comparison, four-write transaction.
func (b *Backend) cleanupReleaseCompletedOwner(j *cleanupLocalJob) (resultErr error) {
	if j == nil || j.claim == nil || j.terminationComplete == nil || j.terminationReservation != nil {
		return ErrInvalidRecord
	}
	stopRenew := b.renewCleanupClaim(j, j.claim)
	defer func() {
		if stopRenew != nil {
			resultErr = errors.Join(resultErr, stopRenew())
		}
	}()

	r, intent, provider, err := b.readLocalRuntimeHistory(j.ctx, j.task)
	if err != nil || r.Phase != "cleanup-complete" || localRuntimeTerminationTask(r) != j.task || r.TaskBirth != j.claim.birth {
		return errors.Join(ErrCorruptRecord, err)
	}
	if err = matchCleanupCompletedFences(j.terminationComplete, intent, provider); err != nil {
		return err
	}
	// The new Run has already replaced Pending. Only the authenticated old
	// two-record expectation is rechecked here, never the old Pending against
	// the new Claim. Renewal must join before intentional owner-key deletion.
	renewErr := stopRenew()
	stopRenew = nil
	if renewErr != nil {
		return renewErr
	}
	return b.cleanupReleaseOwner(j)
}
