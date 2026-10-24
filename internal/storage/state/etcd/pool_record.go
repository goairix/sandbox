package etcd

import (
	"encoding/json"

	"github.com/goairix/sandbox/internal/storage/state"
)

// decodePoolRecord rejects impossible persistent state before an exact lookup
// exposes it to business recovery. New deployments do not accept legacy
// records with inferred cleanup phases or missing runtime identity.
func decodePoolRecord(raw []byte, id string) (state.FUSEPoolRecord, error) {
	var record state.FUSEPoolRecord
	if json.Unmarshal(raw, &record) != nil || record.PreparationID != id ||
		!validPoolID(record.PoolKey) || !validPoolID(record.MaintainerToken) ||
		record.Revision == 0 || record.UpdatedAt.IsZero() || record.PrepareUntil.IsZero() ||
		(record.RuntimeID == "") != (record.RuntimeUID == "") ||
		(record.ReservationToken == "") != record.ReservedUntil.IsZero() {
		return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
	}
	if record.RuntimeID != "" && (!validPoolID(record.RuntimeID) || !validPoolID(record.RuntimeUID)) {
		return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
	}
	if record.ReservationToken != "" && !validPoolID(record.ReservationToken) {
		return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
	}
	if record.State != state.FUSEPoolCleanup && (record.CleanupToken != "" ||
		!record.CleanupUntil.IsZero() || record.CleanupPhase != "" || record.TerminationEvidence != nil) {
		return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
	}
	switch record.State {
	case state.FUSEPoolPreparing:
		// A preparing record may be unbound or runtime-bound. Cold
		// preparation can also already own its reservation token.
	case state.FUSEPoolPrepared:
		if record.RuntimeID == "" || record.ReservationToken != "" {
			return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
		}
	case state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed:
		if record.RuntimeID == "" || record.ReservationToken == "" {
			return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
		}
	case state.FUSEPoolCleanup:
		if !validPoolID(record.CleanupToken) || record.CleanupUntil.IsZero() {
			return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
		}
		switch record.CleanupPhase {
		case state.FUSEPoolCleanupTerminating:
			if record.TerminationEvidence != nil {
				return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
			}
		case state.FUSEPoolCleanupTerminated:
			if record.RuntimeUID == "" || record.TerminationEvidence == nil ||
				record.TerminationEvidence.RuntimeUID != record.RuntimeUID ||
				len(record.TerminationEvidence.NodeName) > 1024 ||
				(!record.TerminationEvidence.ProcessExited &&
					(!record.TerminationEvidence.InfrastructureFenced || !validPoolID(record.TerminationEvidence.NodeName))) {
				return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
			}
		default:
			return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
		}
	default:
		return state.FUSEPoolRecord{}, state.ErrFUSEPoolCorrupt
	}
	return record, nil
}
