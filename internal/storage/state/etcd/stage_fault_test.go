package etcd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// faultKV wraps the real client, retaining real server transactions. It can
// delay dispatch before the server receives a request, or drop a real response.
type faultKV struct {
	clientv3.KV
	match  func([]clientv3.Op) bool
	before func()
	after  func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)
}

func (kv *faultKV) Txn(ctx context.Context) clientv3.Txn {
	return &faultTxn{Txn: kv.KV.Txn(ctx), fault: kv}
}

type faultTxn struct {
	clientv3.Txn
	fault *faultKV
	ops   []clientv3.Op
}

func (txn *faultTxn) If(comparisons ...clientv3.Cmp) clientv3.Txn {
	txn.Txn = txn.Txn.If(comparisons...)
	return txn
}
func (txn *faultTxn) Then(operations ...clientv3.Op) clientv3.Txn {
	txn.ops = operations
	txn.Txn = txn.Txn.Then(operations...)
	return txn
}
func (txn *faultTxn) Else(operations ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(operations...)
	return txn
}
func (txn *faultTxn) Commit() (*clientv3.TxnResponse, error) {
	if !txn.fault.match(txn.ops) {
		return txn.Txn.Commit()
	}
	if txn.fault.before != nil {
		txn.fault.before()
	}
	response, err := txn.Txn.Commit()
	if txn.fault.after != nil {
		return txn.fault.after(response, err)
	}
	return response, err
}

func putsKey(key string) func([]clientv3.Op) bool {
	return func(operations []clientv3.Op) bool {
		for _, operation := range operations {
			if operation.IsPut() && string(operation.KeyBytes()) == key {
				return true
			}
		}
		return false
	}
}

func TestStageRequestDelayedBeforeServerCannotCommitAfterAbort(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
	require.NoError(t, err)
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
		t.Fatal("transaction was not intercepted before dispatch")
	}
	outcome, err := b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	unlock()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, OutcomeAborted, result.outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("delayed commit did not finish")
	}
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}

func TestStageLostCommitResponseResolvesDurableResult(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
	require.NoError(t, err)
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(key), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	outcome, err := b.CommitStage(ctx, stage)
	require.Equal(t, OutcomeUnknown, outcome)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	revision := got.Kvs[0].ModRevision
	require.NoError(t, b.ReleaseStage(ctx, stage))
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	got, err = raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, revision, got.Kvs[0].ModRevision)
}

func TestStageLostGuardResponseNeverReturnsCapability(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	b.client.KV = &faultKV{KV: b.client.KV, match: func(operations []clientv3.Op) bool {
		return len(operations) == 1 && strings.Contains(string(operations[0].KeyBytes()), "/attempts/")
	}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/sandbox", Value: []byte("created")}}}, time.Second)
	require.Nil(t, stage)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	guards, err := raw.Get(ctx, b.namespace.Root()+"p/07/attempts/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, guards.Kvs, "confirmed cleanup revoked only the original lease")
}

func TestStageReceiptSurvivesNaturalLeaseExpiry(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/sandbox", Value: []byte("created")}}}, time.Second)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		guard, err := raw.Get(ctx, stage.guardKey)
		return err == nil && len(guard.Kvs) == 0
	}, 8*time.Second, 50*time.Millisecond)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
}

func TestStageUncommittedExpiredGuardCannotBeRecreated(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		guard, err := raw.Get(ctx, stage.guardKey)
		return err == nil && len(guard.Kvs) == 0
	}, 8*time.Second, 50*time.Millisecond)
	outcome, err := b.CommitStage(ctx, stage)
	require.Equal(t, OutcomeUnknown, outcome)
	require.ErrorIs(t, err, ErrGuardExpired)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}

func TestStageCommitAbortRaceHasOneOutcome(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	for i := 0; i < 16; i++ {
		key := b.namespace.Root() + fmt.Sprintf("p/07/controls/sandbox-%d", i)
		stage, err := b.BeginStage(ctx, 7, fmt.Sprintf("request-%d", i), "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
		require.NoError(t, err)
		start, done := make(chan struct{}), make(chan Outcome, 2)
		go func() {
			<-start
			outcome, err := b.CommitStage(ctx, stage)
			if err != nil {
				t.Errorf("commit: %v", err)
			}
			done <- outcome
		}()
		go func() {
			<-start
			outcome, err := b.ResolveStage(ctx, stage.Reference())
			if err != nil {
				t.Errorf("resolve: %v", err)
			}
			done <- outcome
		}()
		close(start)
		first, second := <-done, <-done
		require.Equal(t, first, second)
		require.Contains(t, []Outcome{OutcomeCommitted, OutcomeAborted}, first)
		got, err := raw.Get(ctx, key)
		require.NoError(t, err)
		if first == OutcomeCommitted {
			require.Len(t, got.Kvs, 1)
		} else {
			require.Empty(t, got.Kvs)
		}
		require.NoError(t, b.ReleaseStage(ctx, stage))
	}
}

func TestStageNOSPACEResolverKeepsUnknown(t *testing.T) {
	ownedFixtureContainers(t)
	b, raw := integrationBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := b.namespace.Root() + "p/07/controls/sandbox"
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: key, Value: []byte("created")}}}, 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := pb.NewMaintenanceClient(raw.ActiveConnection()).Alarm(ctx, &pb.AlarmRequest{Action: pb.AlarmRequest_DEACTIVATE, Alarm: pb.AlarmType_NOSPACE})
		if err != nil {
			t.Errorf("disarm fixture alarm: %v", err)
		}
	})
	_, err = pb.NewMaintenanceClient(raw.ActiveConnection()).Alarm(ctx, &pb.AlarmRequest{Action: pb.AlarmRequest_ACTIVATE, Alarm: pb.AlarmType_NOSPACE})
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.Equal(t, OutcomeUnknown, outcome)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.Equal(t, OutcomeUnknown, outcome)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	_, err = pb.NewMaintenanceClient(raw.ActiveConnection()).Alarm(ctx, &pb.AlarmRequest{Action: pb.AlarmRequest_DEACTIVATE, Alarm: pb.AlarmType_NOSPACE})
	require.NoError(t, err)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Contains(t, []Outcome{OutcomeCommitted, OutcomeAborted}, outcome)
}

func TestStageSurvivesFixtureLeaderFailure(t *testing.T) {
	containers := ownedFixtureContainers(t)
	b, raw := integrationBackend(t)
	ctx := context.Background()
	endpoints := raw.Endpoints()
	leaderIndex := -1
	var leaderID uint64
	for i, endpoint := range endpoints {
		status, err := raw.Status(ctx, endpoint)
		require.NoError(t, err)
		if status.Header.MemberId == status.Leader {
			leaderIndex = i
			leaderID = status.Header.MemberId
		}
	}
	require.NotEqual(t, -1, leaderIndex)
	id := containers[leaderIndex]
	t.Cleanup(func() {
		if output, err := runFixtureDocker("unpause", id); err != nil {
			t.Errorf("resume fixture member: %v: %s", err, output)
			return
		}
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			status, err := raw.Status(ctx, endpoints[leaderIndex])
			return err == nil && status.Leader != 0
		}, 10*time.Second, 100*time.Millisecond, "fixture member must recover before the next test")
	})
	stage, err := b.BeginStage(ctx, 7, "request", "acquire", Mutation{Writes: []Write{{Key: b.namespace.Root() + "p/07/controls/sandbox", Value: []byte("created")}}}, 30*time.Second)
	require.NoError(t, err)
	// Pausing freezes the actual leader while retaining its random host port.
	// Docker stop/start reallocates that port and would break later test Opens.
	output, err := runFixtureDocker("pause", id)
	require.NoError(t, err, string(output))
	var survivors []string
	for i, endpoint := range endpoints {
		if i != leaderIndex {
			survivors = append(survivors, endpoint)
		}
	}
	raw.SetEndpoints(survivors...)
	b.client.SetEndpoints(survivors...)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		status, err := raw.Status(ctx, survivors[0])
		return err == nil && status.Leader != 0 && status.Leader != leaderID
	}, 10*time.Second, 100*time.Millisecond)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	outcome, err = b.ResolveStage(ctx, stage.Reference())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
}
