package etcd

import (
	"context"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestLocalRuntimeTerminationAdmission(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		result, err := b.TerminateTaskRuntimeLocal(context.Background(), nil, nil)
		require.Error(t, err)
		require.NotEqual(t, OutcomeCommitted, result.Outcome)
		_, err = b.LoadTaskRuntimeTermination(context.Background(), TaskReference{})
		require.Error(t, err)
	}
}

func TestLocalRuntimeTerminationOriginalQuery(t *testing.T) {
	r := localReleaseTerminationFixture(t)
	before, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: before, Version: 1, CreateRevision: 100, ModRevision: 100}
	query, err := localRuntimeTerminationQuery(r, kv)
	require.NoError(t, err)
	require.Equal(t, workspaceSyncIntentDigest(string(before)), query.IntentDigest)
	r.Phase, r.TerminalProof = "terminated", []byte("opaque-proof-not-authenticated-by-codec")
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	kv.Value, kv.ModRevision, kv.Version = wire, 101, 2
	again, err := localRuntimeTerminationQuery(r, kv)
	require.NoError(t, err)
	require.Equal(t, query, again, "historical query must identify the reserved original bytes")
	changed := r
	changed.Current.CommandID = "66666666-6666-4666-8666-666666666666"
	_, err = localRuntimeTerminationQuery(changed, kv)
	require.Error(t, err, "copied different Current must not acquire original query identity")
}
