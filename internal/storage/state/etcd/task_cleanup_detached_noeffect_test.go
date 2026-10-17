package etcd

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A detached settlement attempt has no Stage: its original native writer is
// retained by Claim/lease/attempt/digest. Accepting missing coordinates would
// let cold diagnosis erase the identity needed to arbitrate a lost ACK.
func TestCleanupDetachedNoEffectPendingRetainsOriginalAttempt(t *testing.T) {
	original := cleanupExecutionPending{
		Purpose: "detached-noeffect", ClaimID: uuid.NewString(), LeaseID: 501,
		AttemptID: uuid.NewString(), Digest: strings.Repeat("a", 64),
	}
	require.True(t, original.valid(), "the no-Stage detached writer must retain its complete original attempt")

	for name, mutate := range map[string]func(*cleanupExecutionPending){
		"missing claim":       func(p *cleanupExecutionPending) { p.ClaimID = "" },
		"missing lease":       func(p *cleanupExecutionPending) { p.LeaseID = 0 },
		"negative lease":      func(p *cleanupExecutionPending) { p.LeaseID = -1 },
		"missing attempt":     func(p *cleanupExecutionPending) { p.AttemptID = "" },
		"missing digest":      func(p *cleanupExecutionPending) { p.Digest = "" },
		"noncanonical digest": func(p *cleanupExecutionPending) { p.Digest = strings.Repeat("A", 64) },
		"fake stage":          func(p *cleanupExecutionPending) { p.StageID = "detached-noeffect" },
		"fake request":        func(p *cleanupExecutionPending) { p.RequestID = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			changed := original
			mutate(&changed)
			require.False(t, changed.valid())
		})
	}

	// This assignment and comparison intentionally compile only while Pending
	// remains a comparable value; recovery compares exact original coordinates.
	copy := original
	require.True(t, copy == original)
}

func TestCleanupDetachedNoEffectPendingPreservesLocalVocabulary(t *testing.T) {
	for _, purpose := range []string{"claim", "close", "quiescence", "final-sync", "archive", "finalize", "termination", "owner-release"} {
		pending := cleanupExecutionPending{Purpose: purpose, ClaimID: uuid.NewString()}
		require.True(t, pending.valid(), purpose)
		if purpose != "claim" {
			pending.ClaimID = ""
			require.True(t, pending.valid(), "older optional Claim coordinates keep their original schema")
		}
	}
	require.False(t, (cleanupExecutionPending{Purpose: "unknown"}).valid())
}
