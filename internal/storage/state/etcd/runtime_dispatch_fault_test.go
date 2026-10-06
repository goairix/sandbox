package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestRuntimeDispatchDeclareLostCommitReply(t *testing.T) {
	b, raw, acquire, c := claimFixture(t)
	ctx := context.Background()
	dk, _, e := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
	require.NoError(t, e)
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(dk), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		require.NoError(t, e)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	result, err := declareDispatch(t, b, c, dispatchInputForDeclaration())
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Nil(t, result.Entry)
	require.Empty(t, result.Disposition)
	require.NotEmpty(t, result.Reference.AttemptID)
	fresh := freshDispatchBackend(t, b, raw)
	outcome, err := fresh.ResolveStage(ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	loaded, err := fresh.LoadRuntimeDispatch(ctx, acquire.Workspace, acquire.IntentID)
	require.NoError(t, err)
	require.Equal(t, result.Reference, loaded.Reference)
	again, err := declareDispatch(t, b, c, dispatchInputForDeclaration())
	require.NoError(t, err)
	require.Equal(t, DispatchReplay, again.Disposition)
	require.Equal(t, loaded, again.Entry)
	require.NoError(t, b.ReleaseCreationClaim(ctx, c))
}

func TestRuntimeDispatchDeclareFences(t *testing.T) {
	for _, fault := range []string{"lease lost", "claim recreated", "guard recreated", "control changed", "restore changed", "local lost after begin", "canceled after begin"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, acquire, c := claimFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dk, ik, e := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
			require.NoError(t, e)
			lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				switch fault {
				case "lease lost":
					_, e = raw.Revoke(context.Background(), clientv3.LeaseID(c.Reference().LeaseID))
				case "claim recreated", "guard recreated":
					key := c.claimKey
					if fault == "guard recreated" {
						key = c.guardKey
					}
					_, e = raw.Delete(context.Background(), key)
					require.NoError(t, e)
					_, e = raw.Put(context.Background(), key, c.value, clientv3.WithLease(clientv3.LeaseID(c.Reference().LeaseID)))
				case "control changed":
					control, err := b.LoadControl(ctx, acquire.Workspace.Partition(), acquire.SandboxID)
					require.NoError(t, err)
					control.DataGateEpoch++
					wire, err := encodeDomainRecord(*control)
					require.NoError(t, err)
					key, _, err := b.namespace.sandboxKeys(acquire.Workspace.Partition(), acquire.SandboxID, "read")
					require.NoError(t, err)
					_, e = raw.Put(ctx, key, wire)
				case "restore changed":
					_, e = raw.Put(ctx, b.restoreKey, "new-restore")
				}
				require.NoError(t, e)
				return r, e
			}}
			b.client.Lease = lease
			if fault == "local lost after begin" || fault == "canceled after begin" {
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
					return len(ops) == 1 && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					if fault == "local lost after begin" {
						c.mu.Lock()
						c.lost = true
						c.mu.Unlock()
					} else {
						cancel()
					}
					return r, e
				}}
			}
			declarer, ok := any(b).(runtimeDeclarer)
			require.True(t, ok)
			result, err := declarer.DeclareRuntimeDispatch(ctx, c, dispatchInputForDeclaration())
			require.Error(t, err)
			require.Nil(t, result.Entry)
			require.Empty(t, result.Disposition)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			if fault != "restore changed" {
				require.NotEmpty(t, result.Reference.AttemptID)
			}
			for _, key := range []string{dk, ik} {
				got, e := raw.Get(context.Background(), key)
				require.NoError(t, e)
				require.Empty(t, got.Kvs)
			}
			if result.Reference.AttemptID != "" && fault != "restore changed" {
				outcome, err := b.ResolveStage(context.Background(), result.Reference)
				require.NoError(t, err)
				require.Equal(t, OutcomeAborted, outcome)
			}
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}
func TestRuntimeDispatchDeclareCopiesPayloadBeforeRPC(t *testing.T) {
	b, _, _, c := claimFixture(t)
	input := dispatchInputForDeclaration()
	original := string(input.Payload)
	b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
		for i := range input.Payload {
			input.Payload[i] = 'x'
		}
		return r, e
	}}
	result, err := declareDispatch(t, b, c, input)
	require.NoError(t, err)
	require.Equal(t, original, string(result.Entry.Payload))
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestRuntimeDispatchDeclareCleanupFailurePreservesOutcome(t *testing.T) {
	b, raw, acquire, c := claimFixture(t)
	var stageLease clientv3.LeaseID
	cleanupErr := errors.New("cleanup reply lost")
	b.client.Lease = &creationFaultLease{Lease: b.client.Lease, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
		require.NotEqual(t, clientv3.LeaseID(c.Reference().LeaseID), id)
		require.NoError(t, ctx.Err())
		stageLease = id
		return nil, cleanupErr
	}}
	result, err := declareDispatch(t, b, c, dispatchInputForDeclaration())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, DispatchDeclared, result.Disposition)
	require.ErrorIs(t, result.GuardCleanupError, cleanupErr)
	loaded, err := b.LoadRuntimeDispatch(context.Background(), acquire.Workspace, acquire.IntentID)
	require.NoError(t, err)
	require.Equal(t, result.Entry, loaded)
	_, err = raw.Revoke(context.Background(), stageLease)
	require.NoError(t, err)
	_, err = raw.Revoke(context.Background(), clientv3.LeaseID(c.Reference().LeaseID))
	require.NoError(t, err)
}

func TestRuntimeDispatchDeclareDelayedTransactionLosesToAbort(t *testing.T) {
	b, raw, acquire, c := claimFixture(t)
	ctx := context.Background()
	dk, ik, err := b.namespace.runtimeDispatchKeys(acquire.Workspace.Partition(), acquire.IntentID)
	require.NoError(t, err)
	declarer, ok := any(b).(runtimeDeclarer)
	require.True(t, ok)
	arrived := make(chan StageReference, 1)
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	// Capture the exact digest-bearing reference from the real stage guard.
	var ref StageReference
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		if len(ops) == 1 && strings.Contains(string(ops[0].KeyBytes()), "/attempts/") {
			require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &ref))
		}
		return putsKey(dk)(ops)
	}, before: func() { arrived <- ref; <-release }}
	type answer struct {
		r RuntimeDispatchResult
		e error
	}
	done := make(chan answer, 1)
	go func() {
		r, e := declarer.DeclareRuntimeDispatch(ctx, c, dispatchInputForDeclaration())
		done <- answer{r, e}
	}()
	var reference StageReference
	select {
	case reference = <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch transaction did not reach interceptor")
	}
	outcome, err := b.ResolveStage(ctx, reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	unlock()
	var result answer
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed dispatch transaction did not return")
	}
	require.NoError(t, result.e)
	require.Equal(t, OutcomeAborted, result.r.Outcome)
	require.Equal(t, reference, result.r.Reference)
	require.Nil(t, result.r.Entry)
	require.Empty(t, result.r.Disposition)
	for _, key := range []string{dk, ik} {
		got, e := raw.Get(ctx, key)
		require.NoError(t, e)
		require.Empty(t, got.Kvs)
	}
	require.NoError(t, b.ReleaseCreationClaim(ctx, c))
}

func TestRuntimeDispatchDeclareLostGuardReplyKeepsExactReference(t *testing.T) {
	b, raw, acquire, c := claimFixture(t)
	var original StageReference
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		match := len(ops) == 1 && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
		if match {
			require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &original))
		}
		return match
	}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		require.NoError(t, e)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	result, err := declareDispatch(t, b, c, dispatchInputForDeclaration())
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Nil(t, result.Entry)
	require.NotEmpty(t, original.Digest)
	require.Equal(t, original, result.Reference)
	fresh := freshDispatchBackend(t, b, raw)
	outcome, err := fresh.ResolveStage(context.Background(), result.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	loaded, err := fresh.LoadRuntimeDispatch(context.Background(), acquire.Workspace, acquire.IntentID)
	require.NoError(t, err)
	require.Nil(t, loaded)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
