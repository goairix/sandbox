package fuseprotocol

import (
	"encoding/hex"
	"fmt"
	"time"
)

const (
	// MounterReadinessPort is the resident read-only HTTP TCP port on the Pod
	// network. It grants no management capability and carries no credentials.
	MounterReadinessPort = 9808
	// MounterReadinessPath accepts only a GET boolean readiness observation.
	// Positive readiness is not execution, flush or ownership-release proof.
	MounterReadinessPath = "/readyz"
	// MounterReadinessVersion selects only the opt-in Unix health-ready leaf.
	// Original MounterStatus and socket envelopes retain Version1.
	MounterReadinessVersion = 2
	// MounterReadinessMaximumAge is three missed one-second resident refreshes.
	// It bounds source observations, not a consumer's independently renewed TTL.
	MounterReadinessMaximumAge = 3 * time.Second
)

// MounterReadiness is a finite, copied resident observation. Its mount identity
// comes from the original supervisor; it is never execution, flush or release
// authority. SampleSequence advances only on successful source publication and
// remains monotonic across observer replacement within that supervisor.
type MounterReadiness struct {
	Version        int    `json:"version"`
	RuntimeUID     string `json:"runtime_uid"`
	PoolKey        string `json:"pool_key"`
	Generation     int64  `json:"generation"`
	MountAttempt   uint8  `json:"mount_attempt"`
	MountID        uint64 `json:"mount_id"`
	SampleSequence uint64 `json:"sample_sequence"`

	// UTC timestamps are diagnostic only: JSON cannot carry Go's monotonic
	// clock component and must not become a cross-process freshness clock.
	SampledAt       time.Time `json:"sampled_at"`
	CacheObservedAt time.Time `json:"cache_observed_at"`
	// Each pull consumer captures a local monotonic anchor before calling its
	// source, uses anchor+ValidForNanoseconds as its deadline, and refuses an
	// already expired return. Repeated sequences may only shorten that deadline.
	ValidForNanoseconds int64 `json:"valid_for_nanoseconds"`
}

// DecodeMounterReadiness accepts only the complete bounded Version2 leaf. It
// validates shape and ranges, not source identity or elapsed transport time;
// callers must authenticate their source and retain their original pull anchor.
func DecodeMounterReadiness(raw []byte) (MounterReadiness, error) {
	var value MounterReadiness
	if err := DecodeExact(raw, &value); err != nil {
		return MounterReadiness{}, err
	}
	pool, err := hex.DecodeString(value.PoolKey)
	if value.Version != MounterReadinessVersion || !ValidIdentity(value.RuntimeUID) || err != nil || len(pool) != 32 || hex.EncodeToString(pool) != value.PoolKey || value.Generation <= 0 || value.MountAttempt != 1 || value.MountID == 0 || value.SampleSequence == 0 {
		return MounterReadiness{}, fmt.Errorf("invalid resident readiness identity")
	}
	if value.SampledAt.IsZero() || value.SampledAt.Location() != time.UTC || value.CacheObservedAt.IsZero() || value.CacheObservedAt.Location() != time.UTC || value.ValidForNanoseconds <= 0 || value.ValidForNanoseconds > int64(MounterReadinessMaximumAge) {
		return MounterReadiness{}, fmt.Errorf("invalid resident readiness lifetime")
	}
	return value, nil
}
