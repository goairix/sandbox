package etcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type runtimeDeclarer interface {
	DeclareRuntimeDispatch(context.Context, *CreationClaim, RuntimeDispatchInput) (RuntimeDispatchResult, error)
}

func TestRuntimeDispatchDeclareAtomic(t *testing.T) {
	b, raw, input, claim := claimFixture(t)
	ctx := context.Background()
	before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	declarer, ok := any(b).(runtimeDeclarer)
	require.True(t, ok, "backend must declare dispatch under its original creation claim")
	result, err := declarer.DeclareRuntimeDispatch(ctx, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "kubernetes", Payload: json.RawMessage(` { "n": 1e+02, "html":"<&中>" } `)})
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, DispatchDeclared, result.Disposition)
	require.NoError(t, result.GuardCleanupError)
	require.Equal(t, int64(30), lease.requestedTTL.Load())
	require.Equal(t, int64(1), lease.grants.Load())
	require.Equal(t, int64(1), lease.revokes.Load())
	require.Zero(t, lease.keeps.Load())
	require.NotNil(t, result.Entry)
	require.Equal(t, `{"n":1e+02,"html":"<&中>"}`, string(result.Entry.Payload))
	require.Equal(t, claim.Reference().ClaimID, result.Entry.Record.Claim.ClaimID)
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	require.Equal(t, control.ExpiresAt, result.Entry.Record.ExpiresAt)
	require.Equal(t, result.Reference, result.Entry.Reference)
	for _, kv := range before.Kvs {
		got, e := raw.Get(ctx, string(kv.Key))
		require.NoError(t, e)
		require.Equal(t, kv, got.Kvs[0])
	}
	dk, ik, err := b.namespace.runtimeDispatchKeys(input.Workspace.Partition(), input.IntentID)
	require.NoError(t, err)
	_, rk, err := b.stageKeys(result.Reference)
	require.NoError(t, err)
	var revision int64
	for _, key := range []string{dk, ik, rk} {
		got, e := raw.Get(ctx, key)
		require.NoError(t, e)
		require.Len(t, got.Kvs, 1)
		kv := got.Kvs[0]
		require.Zero(t, kv.Lease)
		require.Equal(t, kv.CreateRevision, kv.ModRevision)
		if revision == 0 {
			revision = kv.CreateRevision
		}
		require.Equal(t, revision, kv.CreateRevision)
	}
	loaded, err := b.LoadRuntimeDispatch(ctx, input.Workspace, input.IntentID)
	require.NoError(t, err)
	require.Equal(t, result.Entry, loaded)
	require.NoError(t, b.ReleaseCreationClaim(ctx, claim))
}

func declareDispatch(t *testing.T, b *Backend, c *CreationClaim, input RuntimeDispatchInput) (RuntimeDispatchResult, error) {
	t.Helper()
	declarer, ok := any(b).(runtimeDeclarer)
	require.True(t, ok, "backend must declare dispatch under its original creation claim")
	return declarer.DeclareRuntimeDispatch(context.Background(), c, input)
}
func dispatchInputForDeclaration() RuntimeDispatchInput {
	return RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "kubernetes", Payload: json.RawMessage(`{"image":"example"}`)}
}
func freshDispatchBackend(t *testing.T, b *Backend, raw *clientv3.Client) *Backend {
	t.Helper()
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = b.restoreEpoch
	fresh, err := New(context.Background(), Options{Endpoints: raw.Endpoints(), Namespace: b.namespace, Identity: identity, AllowInsecureLoopback: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	return fresh
}
func TestRuntimeDispatchDeclareReplayAndRecovery(t *testing.T) {
	b, raw, acquire, c := claimFixture(t)
	ctx := context.Background()
	input := dispatchInputForDeclaration()
	original, err := declareDispatch(t, b, c, input)
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	replay, err := declareDispatch(t, b, c, input)
	require.NoError(t, err)
	require.Equal(t, DispatchReplay, replay.Disposition)
	require.Equal(t, original.Entry, replay.Entry)
	for _, change := range []func(*RuntimeDispatchInput){func(i *RuntimeDispatchInput) { i.Kind = RuntimeDispatchPrepare }, func(i *RuntimeDispatchInput) { i.Target = "docker" }, func(i *RuntimeDispatchInput) { i.Payload = json.RawMessage(`{}`) }} {
		altered := input
		change(&altered)
		result, err := declareDispatch(t, b, c, altered)
		require.ErrorIs(t, err, ErrIdempotencyConflict)
		require.Nil(t, result.Entry)
	}
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
	require.Zero(t, lease.revokes.Load())
	require.NoError(t, b.ReleaseCreationClaim(ctx, c))
	fresh := freshDispatchBackend(t, b, raw)
	loaded, err := fresh.LoadRuntimeDispatch(ctx, acquire.Workspace, acquire.IntentID)
	require.NoError(t, err)
	require.Equal(t, original.Entry, loaded)
	next, err := fresh.ClaimCreation(ctx, acquire.Workspace, acquire.IntentID, "recovery", 30*time.Second)
	require.NoError(t, err)
	nextLease := &creationFaultLease{Lease: fresh.client.Lease}
	fresh.client.Lease = nextLease
	replay, err = declareDispatch(t, fresh, next, input)
	require.NoError(t, err)
	require.Equal(t, DispatchReplay, replay.Disposition)
	require.Equal(t, original.Entry, replay.Entry)
	require.NotEqual(t, next.Reference().ClaimID, replay.Entry.Record.Claim.ClaimID)
	require.Zero(t, nextLease.grants.Load())
	require.NoError(t, fresh.ReleaseCreationClaim(ctx, next))
}
func TestRuntimeDispatchDeclareConcurrentOneOperation(t *testing.T) {
	b, _, acquire, c := claimFixture(t)
	input := dispatchInputForDeclaration()
	declarer, ok := any(b).(runtimeDeclarer)
	require.True(t, ok)
	type answer struct {
		result RuntimeDispatchResult
		err    error
	}
	done := make(chan answer, 16)
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			<-start
			r, e := declarer.DeclareRuntimeDispatch(context.Background(), c, input)
			done <- answer{r, e}
		}()
	}
	close(start)
	declared := 0
	var original *RuntimeDispatchEntry
	for i := 0; i < 16; i++ {
		a := <-done
		if a.err != nil {
			require.ErrorIs(t, a.err, ErrConflict)
			require.Nil(t, a.result.Entry)
			require.NotEmpty(t, a.result.Reference.AttemptID)
			continue
		}
		require.Equal(t, OutcomeCommitted, a.result.Outcome)
		if a.result.Disposition == DispatchDeclared {
			declared++
			original = a.result.Entry
		}
	}
	require.Equal(t, 1, declared)
	loaded, err := b.LoadRuntimeDispatch(context.Background(), acquire.Workspace, acquire.IntentID)
	require.NoError(t, err)
	require.Equal(t, original, loaded)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	for i := 0; i < 16; i++ {
		r, e := declareDispatch(t, b, c, input)
		require.NoError(t, e)
		require.Equal(t, DispatchReplay, r.Disposition)
		require.Equal(t, original, r.Entry)
	}
	require.Zero(t, lease.grants.Load())
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}

func TestRuntimeDispatchDeclareCapacitySample(t *testing.T) {
	samples := []struct {
		name    string
		payload json.RawMessage
	}{
		{"fixture input", json.RawMessage(`{"image":"example"}`)},
		{"synthetic 4KiB", json.RawMessage(`"` + strings.Repeat("x", 4094) + `"`)},
		{"synthetic 8KiB", json.RawMessage(`"` + strings.Repeat("x", 8190) + `"`)},
	}
	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			b, raw, acquire, c := claimFixture(t)
			input := dispatchInputForDeclaration()
			input.Payload = sample.payload
			r, err := declareDispatch(t, b, c, input)
			require.NoError(t, err)
			dk, ik, err := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
			require.NoError(t, err)
			_, rk, err := b.stageKeys(r.Reference)
			require.NoError(t, err)
			sizes := make([]int, 0, 3)
			total := 0
			for _, key := range []string{dk, ik, rk} {
				got, e := raw.Get(context.Background(), key)
				require.NoError(t, e)
				require.Len(t, got.Kvs, 1)
				sizes = append(sizes, len(got.Kvs[0].Value))
				total += len(got.Kvs[0].Value)
			}
			t.Logf("%s payload=%d bytes declaration=%d input=%d receipt=%d total=%d; 100000 retained: payload-only=%d bytes, encoded-values=%d bytes", sample.name, len(sample.payload), sizes[0], sizes[1], sizes[2], total, len(sample.payload)*100000, total*100000)
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}
