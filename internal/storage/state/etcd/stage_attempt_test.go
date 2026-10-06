package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// A missing locator field, self-referential digest, or separately committed
// receipt must fail these assertions on real persisted transaction values.
func TestStageAttemptBuilderAtomicLocator(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	lease := &acquisitionLease{Lease: b.client.Lease}
	b.client.Lease = lease
	calls := 0
	stage, err := b.beginStageWithBuilder(ctx, 7, "request", "runtime_dispatch", 30*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		calls++
		require.Zero(t, lease.grants, "builder must precede any lease allocation")
		require.NoError(t, locator.Validate())
		value, err := json.Marshal(locator)
		return Mutation{Writes: []Write{{Key: key, Value: value}}}, err
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	t.Cleanup(func() { _ = b.ReleaseStage(ctx, stage) })
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	response, err := raw.Txn(ctx).Then(clientv3.OpGet(key), clientv3.OpGet(stage.receiptKey)).Commit()
	require.NoError(t, err)
	business, receipt := response.Responses[0].GetResponseRange().Kvs, response.Responses[1].GetResponseRange().Kvs
	require.Len(t, business, 1)
	require.Len(t, receipt, 1)
	var locator StageAttemptLocator
	require.NoError(t, json.Unmarshal(business[0].Value, &locator))
	require.Equal(t, b.namespace.Root(), locator.Namespace)
	require.Equal(t, uint8(7), locator.Partition)
	require.Equal(t, "request", locator.RequestID)
	require.Equal(t, "runtime_dispatch", locator.StageID)
	require.Equal(t, "test-epoch", locator.RestoreEpoch)
	parsed, err := uuid.Parse(locator.AttemptID)
	require.NoError(t, err)
	require.Equal(t, parsed.String(), locator.AttemptID)
	var recorded stageReceipt
	require.NoError(t, json.Unmarshal(receipt[0].Value, &recorded))
	require.Equal(t, locator.Namespace, recorded.Namespace)
	require.Equal(t, locator.Partition, recorded.Partition)
	require.Equal(t, locator.RequestID, recorded.RequestID)
	require.Equal(t, locator.StageID, recorded.StageID)
	require.Equal(t, locator.AttemptID, recorded.AttemptID)
	require.Equal(t, locator.RestoreEpoch, recorded.RestoreEpoch)
	require.Regexp(t, "^[0-9a-f]{64}$", recorded.Digest)
	require.Equal(t, stage.Reference(), recorded.StageReference)
	require.Equal(t, OutcomeCommitted, recorded.Outcome)
	require.Equal(t, business[0].ModRevision, receipt[0].ModRevision)
	require.Zero(t, business[0].Lease)
	require.Zero(t, receipt[0].Lease)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(business[0].Value, &fields))
	require.Len(t, fields, 6)
	for _, name := range []string{"namespace", "partition", "request_id", "stage_id", "attempt_id", "restore_epoch"} {
		require.Contains(t, fields, name)
	}
}

// Accepting malformed durable locators could bind recovery to another key.
func TestStageAttemptLocatorValidate(t *testing.T) {
	valid := StageAttemptLocator{Namespace: "/sandbox/v1/authority/cell/", Partition: 255, RequestID: "request", StageID: "runtime_dispatch", AttemptID: "2d3f035e-ed63-491a-a5c6-cb5bce5f19a0", RestoreEpoch: "epoch-1"}
	require.NoError(t, valid.Validate())
	cases := map[string]func(*StageAttemptLocator){
		"empty namespace":        func(l *StageAttemptLocator) { l.Namespace = "" },
		"relative root":          func(l *StageAttemptLocator) { l.Namespace = "sandbox/v1/authority/cell/" },
		"missing trailing slash": func(l *StageAttemptLocator) { l.Namespace = "/sandbox/v1/authority/cell" },
		"missing prefix":         func(l *StageAttemptLocator) { l.Namespace = "/authority/cell/" },
		"empty component":        func(l *StageAttemptLocator) { l.Namespace = "/sandbox//authority/cell/" },
		"dot component":          func(l *StageAttemptLocator) { l.Namespace = "/sandbox/../authority/cell/" },
		"oversized root":         func(l *StageAttemptLocator) { l.Namespace = "/" + strings.Repeat("x", 500) + "/authority/cell/" },
		"oversized scope":        func(l *StageAttemptLocator) { l.Namespace = "/sandbox/" + strings.Repeat("s", 129) + "/cell/" },
		"oversized cell":         func(l *StageAttemptLocator) { l.Namespace = "/sandbox/authority/" + strings.Repeat("c", 129) + "/" },
		"empty request":          func(l *StageAttemptLocator) { l.RequestID = "" },
		"request path":           func(l *StageAttemptLocator) { l.RequestID = "request/other" },
		"oversized request":      func(l *StageAttemptLocator) { l.RequestID = strings.Repeat("r", 129) },
		"empty stage":            func(l *StageAttemptLocator) { l.StageID = "" },
		"stage path":             func(l *StageAttemptLocator) { l.StageID = ".." },
		"oversized stage":        func(l *StageAttemptLocator) { l.StageID = strings.Repeat("s", 129) },
		"empty restore":          func(l *StageAttemptLocator) { l.RestoreEpoch = "" },
		"restore path":           func(l *StageAttemptLocator) { l.RestoreEpoch = "epoch/other" },
		"oversized restore":      func(l *StageAttemptLocator) { l.RestoreEpoch = strings.Repeat("e", 129) },
		"empty attempt":          func(l *StageAttemptLocator) { l.AttemptID = "" },
		"uppercase uuid":         func(l *StageAttemptLocator) { l.AttemptID = strings.ToUpper(l.AttemptID) },
		"compact uuid":           func(l *StageAttemptLocator) { l.AttemptID = strings.ReplaceAll(l.AttemptID, "-", "") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			locator := valid
			change(&locator)
			require.ErrorIs(t, locator.Validate(), ErrInvalidMutation)
		})
	}
	valid.RequestID, valid.StageID, valid.RestoreEpoch = strings.Repeat("r", 128), strings.Repeat("s", 128), strings.Repeat("e", 128)
	require.NoError(t, valid.Validate())
}

// Nil/error/unsafe builder output or malformed inputs must allocate no lease.
func TestStageAttemptBuilderRejectsBeforeGrant(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	lease := &acquisitionLease{Lease: b.client.Lease}
	b.client.Lease = lease
	key := b.namespace.Root() + "p/07/controls/sandbox"
	sentinel := errors.New("builder rejected")
	cases := []struct {
		name  string
		build func(StageAttemptLocator) (Mutation, error)
		want  error
	}{
		{"nil builder", nil, ErrInvalidMutation},
		{"builder error", func(StageAttemptLocator) (Mutation, error) { return Mutation{}, sentinel }, sentinel},
		{"empty output", func(StageAttemptLocator) (Mutation, error) { return Mutation{}, nil }, ErrInvalidMutation},
		{"outside output", func(StageAttemptLocator) (Mutation, error) {
			return Mutation{Writes: []Write{{Key: "/other/key"}}}, nil
		}, ErrInvalidMutation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage, err := b.beginStageWithBuilder(ctx, 7, "request", "dispatch", 30*time.Second, tc.build)
			require.Nil(t, stage)
			require.ErrorIs(t, err, tc.want)
			require.Zero(t, lease.grants)
		})
	}
	calls := 0
	build := func(StageAttemptLocator) (Mutation, error) {
		calls++
		return Mutation{Writes: []Write{{Key: key}}}, nil
	}
	for _, tc := range []struct {
		name           string
		ctx            context.Context
		request, stage string
		ttl            time.Duration
	}{
		{"nil context", nil, "request", "dispatch", time.Second},
		{"invalid request", ctx, "../request", "dispatch", time.Second},
		{"long request", ctx, strings.Repeat("r", 129), "dispatch", time.Second},
		{"invalid stage", ctx, "request", "../dispatch", time.Second},
		{"long stage", ctx, "request", strings.Repeat("s", 129), time.Second},
		{"zero ttl", ctx, "request", "dispatch", 0},
		{"long ttl", ctx, "request", "dispatch", 24*time.Hour + time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, err := b.beginStageWithBuilder(tc.ctx, 7, tc.request, tc.stage, tc.ttl, build)
			require.Nil(t, stage)
			require.ErrorIs(t, err, ErrInvalidMutation)
		})
	}
	originalEpoch, originalNamespace := b.restoreEpoch, b.namespace
	b.restoreEpoch = "../epoch"
	_, err := b.beginStageWithBuilder(ctx, 7, "request", "dispatch", time.Second, build)
	require.ErrorIs(t, err, ErrInvalidMutation)
	b.restoreEpoch = originalEpoch
	b.namespace = Namespace{}
	_, err = b.beginStageWithBuilder(ctx, 7, "request", "dispatch", time.Second, build)
	require.ErrorIs(t, err, ErrInvalidMutation)
	b.namespace = originalNamespace
	require.Zero(t, calls)
	require.Zero(t, lease.grants)
	got, err := raw.Get(ctx, b.namespace.Root()+"p/07/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}

// Caller mutation and builder reuse cannot alter the frozen capability or reuse
// its attempt identity. The comparison payload is copied as well as write bytes.
func TestStageAttemptBuilderCopiesMutationAndCreatesFreshAttempts(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	_, err := raw.Put(ctx, key, "before")
	require.NoError(t, err)
	mutation := Mutation{Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.Value(key), "=", "before")}, Writes: []Write{{Key: key, Value: []byte("after")}}}
	calls := 0
	build := func(StageAttemptLocator) (Mutation, error) { calls++; return mutation, nil }
	stage, err := b.beginStageWithBuilder(ctx, 7, "request", "dispatch", 30*time.Second, build)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseStage(ctx, stage) })
	next, err := b.beginStageWithBuilder(ctx, 7, "request", "dispatch", 30*time.Second, build)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseStage(ctx, next) })
	require.Equal(t, 2, calls)
	require.NotEqual(t, stage.Reference().AttemptID, next.Reference().AttemptID)
	require.NotEqual(t, stage.guardKey, next.guardKey)
	require.NotEqual(t, stage.receiptKey, next.receiptKey)
	digest := stage.Reference().Digest
	mutation.Writes[0].Value[0] = 'X'
	mutation.Writes[0].Key = "/other/key"
	mutation.Comparisons[0].ValueBytes()[0] = 'X'
	mutation.Comparisons[0].Key[0] = 'X'
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.Equal(t, digest, stage.Reference().Digest)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "after", string(got.Kvs[0].Value))
	_, err = raw.Put(ctx, key, "before")
	require.NoError(t, err)
	outcome, err = b.CommitStage(ctx, next)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err = raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "after", string(got.Kvs[0].Value))
}

// Arbitration of the locator before a captured complete commit Txn reaches
// etcd must prevent every business write, even while its original guard lives.
func TestStageAttemptBuilderDelayedTransactionCannotCommitAfterAbort(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	stage, err := b.beginStageWithBuilder(ctx, 7, "request", "dispatch", 30*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		value, err := json.Marshal(locator)
		return Mutation{Writes: []Write{{Key: key, Value: value}}}, err
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseStage(ctx, stage) })
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(key), before: func() { close(arrived); <-release }}
	type result struct {
		outcome Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() { outcome, err := b.CommitStage(ctx, stage); done <- result{outcome, err} }()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("complete transaction did not reach delay")
	}
	outcome, err := b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	unlock()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, OutcomeAborted, got.outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("delayed transaction did not finish")
	}
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}
