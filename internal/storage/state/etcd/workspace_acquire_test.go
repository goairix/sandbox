package etcd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func acquisitionInput(t *testing.T, suffix string) AcquireIntentInput {
	t.Helper()
	w, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", "team/project/")
	require.NoError(t, err)
	return AcquireIntentInput{Workspace: w, Principal: "principal", IdempotencyKey: "key-" + suffix, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"language":"python","text":"<>&","id":9007199254740993}`), Now: time.Now().UTC(), TTL: 48 * time.Hour}
}

func TestAcquireIntentWritesPermanentAtomicGraph(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "atomic")
	result, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, AcquireCreated, result.Disposition)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NoError(t, result.GuardCleanupError)
	owner, err := b.LoadOwner(ctx, input.Workspace)
	require.NoError(t, err)
	require.NotNil(t, owner)
	require.Equal(t, int64(1), owner.Generation)
	require.Nil(t, owner.Runtime)
	request, err := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
	require.NoError(t, err)
	require.Equal(t, "pending", request.Phase)
	require.Equal(t, input.IntentID, request.IntentID)
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	require.Equal(t, PhasePublishing, control.Phase)
	require.Equal(t, input.Now.Add(input.TTL), control.ExpiresAt)
	snapshot, err := b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
	require.NoError(t, err)
	require.JSONEq(t, string(input.Payload), string(snapshot.Payload))
	require.Contains(t, string(snapshot.Payload), "9007199254740993")
	placement, err := b.LoadPlacement(ctx, input.SandboxID)
	require.NoError(t, err)
	require.Equal(t, input.Workspace.Partition(), placement.Partition)
	ok, fk, err := b.namespace.workspaceKeys(input.Workspace)
	require.NoError(t, err)
	ck, sk, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
	require.NoError(t, err)
	ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
	require.NoError(t, err)
	rk, err := b.namespace.requestKey(request.RequestHash)
	require.NoError(t, err)
	pk, err := b.namespace.placementKey(input.SandboxID)
	require.NoError(t, err)
	_, receipt, err := b.stageKeys(result.Reference)
	require.NoError(t, err)
	reads := []clientv3.Op{}
	for _, key := range []string{ok, fk, ck, sk, ik, rk, pk, receipt} {
		reads = append(reads, clientv3.OpGet(key))
	}
	response, err := raw.Txn(ctx).Then(reads...).Commit()
	require.NoError(t, err)
	var revision int64
	for _, response := range response.Responses {
		values := response.GetResponseRange().Kvs
		require.Len(t, values, 1)
		require.Zero(t, values[0].Lease)
		if revision == 0 {
			revision = values[0].ModRevision
		}
		require.Equal(t, revision, values[0].ModRevision)
	}
	guard, _, err := b.stageKeys(result.Reference)
	require.NoError(t, err)
	gone, err := raw.Get(ctx, guard)
	require.NoError(t, err)
	require.Empty(t, gone.Kvs)
	outcome, err := b.ResolveStage(ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
}

func TestAcquireIntentConcurrentWorkspaceHasOneWinner(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	inputs := make([]AcquireIntentInput, 16)
	for i := range inputs {
		inputs[i] = acquisitionInput(t, strconv.Itoa(i))
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for _, input := range inputs {
		wg.Add(1)
		go func(input AcquireIntentInput) {
			defer wg.Done()
			result, err := b.AcquireIntent(ctx, input)
			if err == nil && result.Disposition == AcquireCreated && result.Outcome == OutcomeCommitted {
				winners.Add(1)
			} else if err != nil && !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected: %v", err)
			}
		}(input)
	}
	wg.Wait()
	require.Equal(t, int32(1), winners.Load())
	owner, err := b.LoadOwner(ctx, inputs[0].Workspace)
	require.NoError(t, err)
	require.NotNil(t, owner)
	require.Equal(t, int64(1), owner.Generation)
	records, err := raw.Get(ctx, b.namespace.Root()+fmt.Sprintf("p/%02x/controls/", inputs[0].Workspace.Partition()), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, records.Kvs, 1)
}

func TestAcquireIntentReplayPreservesOriginalIdentityAndTTL(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "replay")
	first, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	requestKey, err := b.namespace.requestKey(first.Request.RequestHash)
	require.NoError(t, err)
	before, err := raw.Get(ctx, requestKey)
	require.NoError(t, err)
	replayInput := acquisitionInput(t, "replay")
	replayInput.Now = input.Now.Add(time.Hour)
	result, err := b.AcquireIntent(ctx, replayInput)
	require.NoError(t, err)
	require.Equal(t, AcquireReplay, result.Disposition)
	require.Equal(t, first.Request, result.Request)
	after, err := raw.Get(ctx, requestKey)
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].ModRevision, after.Kvs[0].ModRevision)
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	require.Equal(t, input.Now.Add(input.TTL), control.ExpiresAt)
	changed := replayInput
	changed.TTL++
	_, err = b.AcquireIntent(ctx, changed)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	changed = replayInput
	changed.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", "another/project/")
	require.NoError(t, err)
	_, err = b.AcquireIntent(ctx, changed)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
}

func TestAcquireIntentGlobalSandboxIDUniqueAcrossPartitions(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	a := acquisitionInput(t, "a")
	other := acquisitionInput(t, "b")
	var err error
	for i := 0; other.Workspace.Partition() == a.Workspace.Partition(); i++ {
		other.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("other/p%d/", i))
		require.NoError(t, err)
	}
	other.SandboxID = a.SandboxID
	result, err := b.AcquireIntent(ctx, a)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	_, err = b.AcquireIntent(ctx, other)
	require.ErrorIs(t, err, ErrConflict)
	owner, err := b.LoadOwner(ctx, other.Workspace)
	require.NoError(t, err)
	require.Nil(t, owner)
}

func TestAcquireIntentGenerationAdvanceAndOverflow(t *testing.T) {
	for _, generation := range []int64{7, math.MaxInt64} {
		t.Run(strconv.FormatInt(generation, 10), func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, "generation")
			_, key, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: input.Workspace.Hash(), Generation: generation, RestoreEpoch: b.restoreEpoch}
			value, err := encodeDomainRecord(fence)
			require.NoError(t, err)
			_, err = raw.Put(ctx, key, value)
			require.NoError(t, err)
			result, err := b.AcquireIntent(ctx, input)
			if generation == math.MaxInt64 {
				require.ErrorIs(t, err, ErrGenerationOverflow)
				owner, err := b.LoadOwner(ctx, input.Workspace)
				require.NoError(t, err)
				require.Nil(t, owner)
				got, err := raw.Get(ctx, key)
				require.NoError(t, err)
				require.Equal(t, value, string(got.Kvs[0].Value))
			} else {
				require.NoError(t, err)
				require.Equal(t, generation+1, result.Request.Generation)
			}
		})
	}
}

func TestAcquireIntentLostReplyResolvesStageWithoutCompletingRequest(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "lost")
	ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
	require.NoError(t, err)
	b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(ownerKey), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	result, err := b.AcquireIntent(ctx, input)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	outcome, err := b.ResolveStage(ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	request, err := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
	require.NoError(t, err)
	require.Equal(t, "pending", request.Phase)
	owner, err := raw.Get(ctx, ownerKey)
	require.NoError(t, err)
	require.Len(t, owner.Kvs, 1)
}

func TestAcquireIntentInvalidInputMakesNoDomainWrites(t *testing.T) {
	cases := map[string]func(*AcquireIntentInput){"zero identity": func(i *AcquireIntentInput) { i.Workspace = WorkspaceIdentity{} }, "invalid id": func(i *AcquireIntentInput) { i.IntentID = "../x" }, "zero ttl": func(i *AcquireIntentInput) { i.TTL = 0 }, "oversized snapshot": func(i *AcquireIntentInput) { i.Payload = make([]byte, maxRecordBytes+1) }, "invalid utf8": func(i *AcquireIntentInput) { i.Payload = json.RawMessage{'"', 0xff, '"'} }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, "invalid")
			mutate(&input)
			_, err := b.AcquireIntent(ctx, input)
			require.ErrorIs(t, err, ErrInvalidRecord)
			entries, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Len(t, entries.Kvs, 2)
		})
	}
}

func TestAcquireIntentStorageBindingAndRestoreRejectBeforeCommit(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "scope")
	var err error
	input.Workspace, err = NewWorkspaceIdentity("s3", "wrong-storage", "bucket", "team/project/")
	require.NoError(t, err)
	_, err = b.AcquireIntent(ctx, input)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	input = acquisitionInput(t, "epoch")
	_, err = raw.Put(ctx, b.restoreKey, "another-epoch")
	require.NoError(t, err)
	_, err = b.AcquireIntent(ctx, input)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	entries, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, entries.Kvs, 2)
}

func TestAcquireIntentCorruptOrLeasedFenceFailsClosed(t *testing.T) {
	for _, kind := range []string{"malformed", "leased", "wrong workspace", "old restore"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, "fence")
			_, key, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: input.Workspace.Hash(), Generation: 1, RestoreEpoch: b.restoreEpoch}
			if kind == "wrong workspace" {
				fence.WorkspaceHash = hex.EncodeToString(make([]byte, 32))
			}
			if kind == "old restore" {
				fence.RestoreEpoch = "old-epoch"
			}
			value, err := encodeDomainRecord(fence)
			require.NoError(t, err)
			if kind == "malformed" {
				value = "{"
			}
			var opts []clientv3.OpOption
			if kind == "leased" {
				lease, err := raw.Grant(ctx, 30)
				require.NoError(t, err)
				opts = []clientv3.OpOption{clientv3.WithLease(lease.ID)}
			}
			_, err = raw.Put(ctx, key, value, opts...)
			require.NoError(t, err)
			_, err = b.AcquireIntent(ctx, input)
			if kind == "old restore" {
				require.ErrorIs(t, err, ErrIdentityMismatch)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
			ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			got, err := raw.Get(ctx, ownerKey)
			require.NoError(t, err)
			require.Empty(t, got.Kvs)
		})
	}
}

func TestAcquireIntentDelayedTransactionCannotCreateAfterAbort(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "delayed")
	ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
	require.NoError(t, err)
	refs := make(chan StageReference, 1)
	released := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(released) }) }
	t.Cleanup(unlock)
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		if !putsKey(ownerKey)(ops) {
			return false
		}
		for _, op := range ops {
			if op.IsPut() && string(op.KeyBytes()) != ownerKey {
				var receipt stageReceipt
				if json.Unmarshal(op.ValueBytes(), &receipt) == nil && receipt.Outcome == OutcomeCommitted && receipt.StageID == "acquire" {
					refs <- receipt.StageReference
					break
				}
			}
		}
		return true
	}, before: func() { <-released }}
	type response struct {
		result AcquireIntentResult
		err    error
	}
	done := make(chan response, 1)
	go func() { result, err := b.AcquireIntent(ctx, input); done <- response{result, err} }()
	var ref StageReference
	select {
	case ref = <-refs:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire transaction not intercepted")
	}
	outcome, err := b.ResolveStage(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	unlock()
	select {
	case response := <-done:
		require.NoError(t, response.err)
		require.Equal(t, OutcomeAborted, response.result.Outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("delayed acquire did not finish")
	}
	all, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, all.Kvs, 3, "only identity, restore and abort receipt may remain")
}

func TestAcquireIntentBusinessTTLIndependentOfStageLease(t *testing.T) {
	for _, ttl := range []time.Duration{time.Millisecond, 48 * time.Hour, 365 * 24 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, ttl.String())
			input.TTL = ttl
			var guardTTL int64
			var guarded struct {
				LeaseID int64 `json:"lease_id"`
			}
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				if len(ops) != 1 || !ops[0].IsPut() || !strings.Contains(string(ops[0].KeyBytes()), "/attempts/") {
					return false
				}
				require.NoError(t, json.Unmarshal(ops[0].ValueBytes(), &guarded))
				return true
			}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				lease, err := raw.TimeToLive(ctx, clientv3.LeaseID(guarded.LeaseID))
				require.NoError(t, err)
				guardTTL = lease.GrantedTTL
				return response, err
			}}
			result, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			require.Equal(t, int64(30), guardTTL)
			control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			require.NoError(t, err)
			require.Equal(t, input.Now.Add(ttl), control.ExpiresAt)
		})
	}
}

func TestAcquireIntentReplayRejectsStoredWorkspaceMismatch(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "request-workspace-mismatch")
	first, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	otherWorkspace, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", "other/project/")
	require.NoError(t, err)
	require.NotEqual(t, input.Workspace.Hash(), otherWorkspace.Hash())
	tampered := first.Request
	tampered.WorkspaceHash = otherWorkspace.Hash()
	value, err := encodeDomainRecord(tampered)
	require.NoError(t, err, "tampered JSON remains schema-valid")
	key, err := b.namespace.requestKey(tampered.RequestHash)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	before, err := raw.Get(ctx, key)
	require.NoError(t, err)
	lease := &acquisitionLease{Lease: b.client.Lease}
	b.client.Lease = lease
	result, err := b.AcquireIntent(ctx, input)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Empty(t, result.Disposition)
	require.Empty(t, result.Reference)
	require.Zero(t, lease.grants)
	after, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].ModRevision, after.Kvs[0].ModRevision)
	require.Equal(t, value, string(after.Kvs[0].Value))
}
