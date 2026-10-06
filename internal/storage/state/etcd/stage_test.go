package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestMutationRejectsUnsafeWrites(t *testing.T) {
	n, err := NewNamespace("/sandbox/v1", "authority", "cell-01")
	require.NoError(t, err)
	key, err := n.Key("p", "07", "controls", "sandbox")
	require.NoError(t, err)
	many := make([]Write, 64)
	for i := range many {
		many[i] = Write{Key: key + fmt.Sprint(i), Value: []byte("x")}
	}
	cases := map[string]Mutation{
		"no writes":          {},
		"cross authority":    {Writes: []Write{{Key: "/other/control", Value: []byte("x")}}},
		"path escape":        {Writes: []Write{{Key: n.Root() + "p/07/../meta/identity", Value: []byte("x")}}},
		"identity":           {Writes: []Write{{Key: n.Root() + "meta/identity", Value: []byte("x")}}},
		"attempt guard":      {Writes: []Write{{Key: n.Root() + "p/07/attempts/a/guard", Value: []byte("x")}}},
		"receipt":            {Writes: []Write{{Key: n.Root() + "p/07/stages/r/s/a/receipt", Value: []byte("x")}}},
		"duplicate write":    {Writes: []Write{{Key: key}, {Key: key, Delete: true}}},
		"delete with value":  {Writes: []Write{{Key: key, Value: []byte("x"), Delete: true}}},
		"oversized value":    {Writes: []Write{{Key: key, Value: make([]byte, 64*1024+1)}}},
		"operation budget":   {Writes: many},
		"outside comparison": {Writes: []Write{{Key: key}}, Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Version("/other"), "=", 0)}},
		"outside range":      {Writes: []Write{{Key: key}}, Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Version(key), "=", 0).WithRange("\x00")}},
	}
	for name, mutation := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := prepareMutation(n, mutation)
			require.ErrorIs(t, err, ErrInvalidMutation)
		})
	}
	_, _, err = prepareMutation(n, Mutation{Writes: []Write{{Key: key, Value: []byte("ok")}}})
	require.NoError(t, err)
}

func TestMutationComparisonRangeEndLengthBudget(t *testing.T) {
	n, err := NewNamespace("/sandbox/v1", "authority", "cell-01")
	require.NoError(t, err)
	key := n.Root() + "p/07/controls/sandbox"
	for _, size := range []int{maxKeyBytes, maxKeyBytes + 1} {
		rangeEnd := n.Root() + strings.Repeat("z", size-len(n.Root()))
		_, _, err := prepareMutation(n, Mutation{Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Version(key), "=", 0).WithRange(rangeEnd)}, Writes: []Write{{Key: key}}})
		if size == maxKeyBytes {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrInvalidMutation)
		}
	}
}

func TestReceiptEncoderRejectsOversizedReference(t *testing.T) {
	_, err := encodeReceipt(StageReference{RestoreEpoch: strings.Repeat("x", maxRecordBytes)}, OutcomeCommitted)
	require.ErrorIs(t, err, ErrInvalidMutation)
}

func TestStageCommitIsAtomicAndReceiptSurvivesRevoke(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	keys := []string{key, b.namespace.Root() + "p/07/snapshots/sandbox/v1", b.namespace.Root() + "p/07/workspaces/workspace/owner", b.namespace.Root() + "p/07/runtime-index/runtime"}
	writes := make([]Write, len(keys))
	reads := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		writes[i] = Write{Key: key, Value: []byte("owner")}
		reads[i] = clientv3.OpGet(key)
	}
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{
		Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)},
		Writes:      writes,
	}, 30*time.Second)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	response, err := raw.Txn(ctx).Then(append(reads, clientv3.OpGet(stage.receiptKey))...).Commit()
	require.NoError(t, err)
	value := response.Responses[0].GetResponseRange().Kvs[0]
	receipt := response.Responses[len(keys)].GetResponseRange().Kvs[0]
	for _, result := range response.Responses[:len(keys)] {
		kvs := result.GetResponseRange().Kvs
		require.Len(t, kvs, 1)
		require.Equal(t, receipt.ModRevision, kvs[0].ModRevision, "all business writes must share the receipt revision")
		require.Zero(t, kvs[0].Lease)
	}
	require.Equal(t, value.ModRevision, receipt.ModRevision)
	require.Zero(t, value.Lease)
	require.Zero(t, receipt.Lease)
	require.NoError(t, b.ReleaseStage(ctx, stage))
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	outcome, err = b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, value.ModRevision, got.Kvs[0].ModRevision, "retry must not rewrite business state")
}

func TestStageConcurrentOwnerCAS(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/workspaces/workspace/owner"
	var committed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		stage, err := b.BeginStage(ctx, 7, fmt.Sprintf("request-%d", i), "acquire", Mutation{
			Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)},
			Writes:      []Write{{Key: key, Value: []byte(fmt.Sprintf("owner-%d", i))}},
		}, 30*time.Second)
		require.NoError(t, err)
		wg.Add(1)
		go func(stage *Stage) {
			defer wg.Done()
			outcome, err := b.CommitStage(ctx, stage)
			if err == nil && outcome == OutcomeCommitted {
				committed.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected commit result: %s %v", outcome, err)
			}
			if err := b.ReleaseStage(ctx, stage); err != nil {
				t.Errorf("release: %v", err)
			}
		}(stage)
	}
	wg.Wait()
	require.Equal(t, int32(1), committed.Load())
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
}

func TestStageAbortBlocksDelayedCommitAndNewAttemptCanRetry(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	mutation := Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", mutation, 30*time.Second)
	require.NoError(t, err)
	outcome, err := b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	outcome, err = b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
	next, err := b.BeginStage(ctx, 7, "request", "acquire", mutation, 30*time.Second)
	require.NoError(t, err)
	require.NotEqual(t, stage.Reference().AttemptID, next.Reference().AttemptID)
	outcome, err = b.CommitStage(ctx, next)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	old, err := b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, old)
	require.NoError(t, b.ReleaseStage(ctx, stage))
	require.NoError(t, b.ReleaseStage(ctx, next))
}

func TestStageExpiredGuardAndChangedRestoreCannotCommit(t *testing.T) {
	for _, kind := range []string{"revoke", "restore"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			key := b.namespace.Root() + "p/07/controls/sandbox"
			stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
			require.NoError(t, err)
			if kind == "revoke" {
				require.NoError(t, b.ReleaseStage(ctx, stage))
			} else {
				_, err := raw.Put(ctx, b.restoreKey, "different-restore")
				require.NoError(t, err)
			}
			outcome, err := b.CommitStage(ctx, stage)
			require.Equal(t, OutcomeUnknown, outcome)
			if kind == "revoke" {
				require.ErrorIs(t, err, ErrGuardExpired)
				outcome, err = b.ResolveStage(ctx, stage.Reference())
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, outcome)
			} else {
				require.ErrorIs(t, err, ErrIdentityMismatch)
				_, err = b.ResolveStage(ctx, stage.Reference())
				require.ErrorIs(t, err, ErrIdentityMismatch)
			}
			got, err := raw.Get(ctx, key)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}

func TestStageCopiesMutationBeforeCallerChangesIt(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	mutation := Mutation{
		Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", "original")},
		Writes:      []Write{{Key: key, Value: []byte("after")}},
	}
	_, err := raw.Put(ctx, key, "original")
	require.NoError(t, err)
	stage, err := b.BeginStage(ctx, 7, "request", "update", mutation, 30*time.Second)
	require.NoError(t, err)
	mutation.Writes[0].Value[0] = 'X'
	mutation.Comparisons[0].ValueBytes()[0] = 'X'
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "after", string(got.Kvs[0].Value))
}

func TestMutationTotalByteBudget(t *testing.T) {
	n, err := NewNamespace("/sandbox/v1", "authority", "cell-01")
	require.NoError(t, err)
	var mutation Mutation
	for i := 0; i < 5; i++ {
		mutation.Writes = append(mutation.Writes, Write{Key: n.Root() + fmt.Sprintf("p/07/snapshots/s%d/v1", i), Value: []byte(strings.Repeat("x", 64*1024))})
	}
	_, _, err = prepareMutation(n, mutation)
	require.ErrorIs(t, err, ErrInvalidMutation)
}

func TestStageReferenceResolvesAfterCreatorCloses(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	options := integrationOptions(b, raw)
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/sandbox", Value: []byte("created")}}}, 30*time.Second)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	reference := stage.Reference()
	require.NoError(t, b.ReleaseStage(ctx, stage))
	require.NoError(t, b.Close())
	recovered, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { _ = recovered.Close() })
	outcome, err = recovered.ResolveStage(ctx, reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
}

func TestStageCorruptReceiptFailsClosedWithoutRewriting(t *testing.T) {
	for _, kind := range []string{"json", "digest", "lease"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			key := b.namespace.Root() + "p/07/controls/sandbox"
			stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
			require.NoError(t, err)
			value := stage.committedValue
			var options []clientv3.OpOption
			switch kind {
			case "json":
				value = "broken JSON"
			case "digest":
				ref := stage.Reference()
				ref.Digest = strings.Repeat("0", 64)
				value, err = encodeReceipt(ref, OutcomeCommitted)
				require.NoError(t, err)
			case "lease":
				options = append(options, clientv3.WithLease(stage.leaseID))
			}
			put, err := raw.Put(ctx, stage.receiptKey, value, options...)
			require.NoError(t, err)
			outcome, err := b.CommitStage(ctx, stage)
			require.Equal(t, OutcomeUnknown, outcome)
			require.ErrorIs(t, err, ErrCorruptReceipt)
			outcome, err = b.ResolveStage(ctx, stage.Reference())
			require.Equal(t, OutcomeUnknown, outcome)
			require.ErrorIs(t, err, ErrCorruptReceipt)
			got, err := raw.Get(ctx, key)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
			receipt, err := raw.Get(ctx, stage.receiptKey)
			require.NoError(t, err)
			require.Equal(t, put.Header.Revision, receipt.Kvs[0].ModRevision)
		})
	}
}

func TestStageCASConflictIsUnknownUntilArbitration(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	_, err := raw.Put(ctx, key, "existing")
	require.NoError(t, err)
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{
		Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)},
		Writes:      []Write{{Key: key, Value: []byte("replacement")}},
	}, 30*time.Second)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.Equal(t, OutcomeUnknown, outcome)
	require.ErrorIs(t, err, ErrConflict)
	marker, err := raw.Get(ctx, stage.receiptKey)
	require.NoError(t, err)
	require.Empty(t, marker.Kvs, "known CAS rejection must not guess a durable aborted result")
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "existing", string(got.Kvs[0].Value))
}

func TestMutationCombinedOperationBudget(t *testing.T) {
	n, err := NewNamespace("/sandbox/v1", "authority", "cell-01")
	require.NoError(t, err)
	var mutation Mutation
	for i := 0; i < maxStageOperations-stageProtocolOperations; i++ {
		mutation.Writes = append(mutation.Writes, Write{Key: n.Root() + fmt.Sprintf("p/07/controls/s%d", i), Value: []byte("x")})
	}
	_, _, err = prepareMutation(n, mutation)
	require.NoError(t, err)
	mutation.Writes = append(mutation.Writes, Write{Key: n.Root() + "p/07/controls/extra", Value: []byte("x")})
	_, _, err = prepareMutation(n, mutation)
	require.ErrorIs(t, err, ErrInvalidMutation)
}
