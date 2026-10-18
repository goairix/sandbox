package etcd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Host positives in this file reuse only the accepted local v1 signed model.
// Detached terminal positives belong exclusively to the genuine physical
// producer/release fixture; altered host records below are negative inputs.
func TestReleasedDetachedConsumerLocalCompatibility(t *testing.T) {
	f := newReleasedProofTestFixture(t)
	actualTask := f.server.point(f.seed.keys[0])
	before := len(releasedDetachedTransportRequests(f.server))
	proof, err := f.b.readReleasedProof(context.Background(), f.seed.task.Reference, actualTask)
	require.NoError(t, err)
	require.NotNil(t, proof)
	require.NotNil(t, proof.local)
	require.Nil(t, proof.detached)
	require.Len(t, releasedDetachedTransportRequests(f.server), before+3)
	observation, err := releasedTerminalObservationFor(proof)
	require.NoError(t, err)
	require.False(t, observation.detached)
	require.Equal(t, proof.local.record.Task, observation.task)
	require.Equal(t, proof.local.record.TaskBirth, observation.taskBirth)
	require.Equal(t, proof.local.record.Runtime, observation.runtime)
	require.Equal(t, proof.local.kv, observation.kv)
	require.Equal(t, proof.local.termination, observation.termination)
	require.Len(t, observation.history, 11, "two head points and original nine history points")

	before = len(releasedDetachedTransportRequests(f.server))
	result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, f.seed.task.Reference, result.Task)
	require.Len(t, releasedDetachedTransportRequests(f.server), before+8)
}

func TestReleasedDetachedConsumerUnionRefusal(t *testing.T) {
	f := newReleasedProofTestFixture(t)
	valid, err := f.b.readReleasedProof(context.Background(), f.seed.task.Reference, f.server.point(f.seed.keys[0]))
	require.NoError(t, err)
	for name, proof := range map[string]*taskOwnerReleaseProof{
		"nil":                 nil,
		"empty":               {},
		"both":                {local: valid.local, detached: &detachedOwnerReleaseProof{}},
		"incomplete-local":    {local: &localOwnerReleaseProof{}},
		"incomplete-detached": {detached: &detachedOwnerReleaseProof{}},
	} {
		t.Run(name, func(t *testing.T) {
			observation, err := releasedTerminalObservationFor(proof)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Nil(t, observation)
		})
	}
}

func TestReleasedDetachedConsumerActualTaskBinding(t *testing.T) {
	f := newReleasedProofTestFixture(t)
	actual := f.server.point(f.seed.keys[0])
	for name, change := range map[string]func(*mvccpb.KeyValue){
		"version": func(kv *mvccpb.KeyValue) { kv.Version++ },
		"key":     func(kv *mvccpb.KeyValue) { kv.Key = []byte(string(kv.Key) + "-foreign") },
		"value":   func(kv *mvccpb.KeyValue) { kv.Value = append([]byte(" "), kv.Value...) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *actual
			change(&bad)
			proof, err := f.b.readReleasedProof(context.Background(), f.seed.task.Reference, &bad)
			require.Error(t, err, "a located cohort Task is never replaced by a different repeated Task observation")
			require.Nil(t, proof)
		})
	}
}

func TestReleasedDetachedConsumerProfileRefusal(t *testing.T) {
	f := newReleasedProofTestFixture(t)
	// Change only diagnostic bytes from an existing local model. This is an
	// invalid mixed profile, never a newly signed detached terminal positive.
	key := f.keys.Intent
	original := f.server.point(key)
	changed := bytes.Replace(original.Value, []byte(`"version":1`), []byte(`"version":2`), 1)
	require.NotEqual(t, original.Value, changed)
	f.server.put(key, string(changed), 0)
	proof, err := f.b.readReleasedProof(context.Background(), f.seed.task.Reference, f.server.point(f.seed.keys[0]))
	require.Error(t, err)
	require.Nil(t, proof)
}
