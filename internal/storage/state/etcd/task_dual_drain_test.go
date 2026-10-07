package etcd

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskDualDrainHistoricalClaimBoundary(t *testing.T) {
	b, c, d := quiescenceBuilderFixture(t)
	// Pure immutable tuple comparison only; this is not a sealed native claim.
	key, err := b.namespace.taskKey(c.reference.Task)
	require.NoError(t, err)
	value, err := encodeTaskRecord(d.task)
	require.NoError(t, err)
	c.birth = d.claims.Context.Current.ControlRevision
	c.fences[0] = taskFence{key: key, value: value, create: c.birth, mod: c.birth}
	r := TaskQuiescenceRecord{Task: d.task, Claim: c.reference, Context: d.claims.Context}
	entry := &TaskQuiescenceEntry{Record: &r, Outcome: OutcomeCommitted}
	require.NoError(t, validateTaskDualDrainHistory(c, entry))
	old := c.reference
	c.reference.CreateRevision++
	c.reference.ClaimID = "00000000-0000-4000-8000-000000000001"
	c.reference.WorkerID = "later"
	c.reference.LeaseID++
	require.NoError(t, validateTaskDualDrainHistory(c, entry), "new claimant derives only local prerequisite")
	for _, fault := range []string{"older", "equal-foreign", "task", "birth", "pending"} {
		t.Run(fault, func(t *testing.T) {
			x := TaskClaim{reference: old, fences: c.fences, birth: c.birth}
			y := r
			e := *entry
			e.Record = &y
			switch fault {
			case "older":
				x.reference.CreateRevision--
			case "equal-foreign":
				x.reference.ClaimID = "foreign"
			case "task":
				y.Task.DataGateEpoch++
			case "birth":
				y.Context.Current.ControlRevision++
			case "pending":
				e.Outcome = OutcomeUnknown
			}
			require.Error(t, validateTaskDualDrainHistory(&x, &e))
		})
	}
}

func TestTaskDualDrainRejectsNoCurrentCapability(t *testing.T) {
	b := new(Backend)
	for _, c := range []*TaskClaim{nil, {}} {
		got, err := b.PrepareAfterDualDrain(context.Background(), c, nil)
		require.Error(t, err)
		require.Nil(t, got)
	}
}
