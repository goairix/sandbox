package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Removing contextual linkage validation must make every affected reader fail.
func TestDomainReadMissingAndContextLinkage(t *testing.T) {
	for _, kind := range []string{"missing", "owner dangling", "owner generation", "control dangling", "placement id", "request hash", "snapshot digest", "control workspace", "snapshot id", "old restore"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, kind)
			if kind == "missing" {
				owner, err := b.LoadOwner(ctx, input.Workspace)
				require.NoError(t, err)
				require.Nil(t, owner)
				request, err := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
				require.NoError(t, err)
				require.Nil(t, request)
				control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
				require.NoError(t, err)
				require.Nil(t, control)
				placement, err := b.LoadPlacement(ctx, input.SandboxID)
				require.NoError(t, err)
				require.Nil(t, placement)
				snapshot, err := b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, SnapshotReference{Version: input.SnapshotVersion, Digest: strings.Repeat("a", 64)})
				require.NoError(t, err)
				require.Nil(t, snapshot)
				return
			}
			_, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			ownerKey, fenceKey, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			controlKey, snapshotKey, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
			require.NoError(t, err)
			placementKey, err := b.namespace.placementKey(input.SandboxID)
			require.NoError(t, err)
			hash, err := requestKeyHash(input.Principal, input.IdempotencyKey)
			require.NoError(t, err)
			requestKey, err := b.namespace.requestKey(hash)
			require.NoError(t, err)
			mutate := func(key, field string, value any) {
				got, err := raw.Get(ctx, key)
				require.NoError(t, err)
				var record map[string]any
				require.NoError(t, json.Unmarshal(got.Kvs[0].Value, &record))
				record[field] = value
				encoded, err := json.Marshal(record)
				require.NoError(t, err)
				_, err = raw.Put(ctx, key, string(encoded))
				require.NoError(t, err)
			}
			switch kind {
			case "owner dangling":
				_, err = raw.Delete(ctx, fenceKey)
				require.NoError(t, err)
				_, err = b.LoadOwner(ctx, input.Workspace)
			case "owner generation":
				mutate(ownerKey, "generation", 2)
				_, err = b.LoadOwner(ctx, input.Workspace)
			case "control dangling":
				_, err = raw.Delete(ctx, placementKey)
				require.NoError(t, err)
				_, err = b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			case "placement id":
				mutate(placementKey, "sandbox_id", "different-id")
				_, err = b.LoadPlacement(ctx, input.SandboxID)
			case "request hash":
				mutate(requestKey, "request_hash", strings.Repeat("a", 64))
				_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
			case "snapshot digest":
				control, e := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
				require.NoError(t, e)
				ref := control.Snapshot
				ref.Digest = strings.Repeat("a", 64)
				_, err = b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, ref)
			case "control workspace":
				mutate(controlKey, "workspace_hash", strings.Repeat("a", 64))
				_, err = b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			case "snapshot id":
				control, e := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
				require.NoError(t, e)
				mutate(snapshotKey, "sandbox_id", "different-id")
				_, err = b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
			case "old restore":
				mutate(requestKey, "restore_epoch", "old-epoch")
				_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
			}
			if kind == "old restore" {
				require.ErrorIs(t, err, ErrIdentityMismatch)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
		})
	}
}

func TestDomainReadWrongResponseKeyAndEnvelope(t *testing.T) {
	for _, kind := range []string{"wrong key", "extra kv", "nil range", "nil kv", "count", "more", "response count", "wrong cluster", "nil header", "nil response"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := integrationBackend(t)
			input := acquisitionInput(t, kind)
			ctx := context.Background()
			_, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) > 0 && ops[0].IsGet() }, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				switch kind {
				case "wrong key":
					response.Responses[0].GetResponseRange().Kvs[0].Key = []byte("/wrong/namespace")
				case "extra kv":
					r := response.Responses[0].GetResponseRange()
					r.Kvs = append(r.Kvs, r.Kvs[0])
				case "nil range":
					response.Responses[0] = nil
				case "nil kv":
					response.Responses[0].GetResponseRange().Kvs[0] = nil
				case "count":
					response.Responses[0].GetResponseRange().Count++
				case "more":
					response.Responses[0].GetResponseRange().More = true
				case "response count":
					response.Responses = nil
				case "wrong cluster":
					response.Header.ClusterId++
				case "nil header":
					response.Header = nil
				case "nil response":
					return nil, nil
				}
				return response, nil
			}}
			_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
			want := ErrCorruptRecord
			if kind == "wrong cluster" || kind == "nil header" || kind == "nil response" {
				want = ErrIdentityMismatch
			}
			require.ErrorIs(t, err, want)
			require.NotErrorIs(t, err, ErrCorruptReceipt)
		})
	}
}

func TestDomainReadLeasedPermanentRecords(t *testing.T) {
	for _, kind := range []string{"owner", "request", "control", "snapshot", "placement"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := acquisitionInput(t, kind)
			_, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			require.NoError(t, err)
			ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			controlKey, snapshotKey, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
			require.NoError(t, err)
			requestHash, err := requestKeyHash(input.Principal, input.IdempotencyKey)
			require.NoError(t, err)
			requestKey, err := b.namespace.requestKey(requestHash)
			require.NoError(t, err)
			placementKey, err := b.namespace.placementKey(input.SandboxID)
			require.NoError(t, err)
			keys := map[string]string{"owner": ownerKey, "request": requestKey, "control": controlKey, "snapshot": snapshotKey, "placement": placementKey}
			got, err := raw.Get(ctx, keys[kind])
			require.NoError(t, err)
			lease, err := raw.Grant(ctx, 30)
			require.NoError(t, err)
			_, err = raw.Put(ctx, keys[kind], string(got.Kvs[0].Value), clientv3.WithLease(lease.ID))
			require.NoError(t, err)
			switch kind {
			case "owner":
				_, err = b.LoadOwner(ctx, input.Workspace)
			case "request":
				_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
			case "control":
				_, err = b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			case "snapshot":
				_, err = b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
			case "placement":
				_, err = b.LoadPlacement(ctx, input.SandboxID)
			}
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
}

func TestAcquireIntentSnapshotPrivateBeforeCallerMutation(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "private")
	original := string(input.Payload)
	var once sync.Once
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) > 0 && ops[0].IsGet() }, before: func() {
		once.Do(func() {
			for i := range input.Payload {
				input.Payload[i] = ' '
			}
		})
	}}
	result, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	snapshot, err := b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
	require.NoError(t, err)
	require.JSONEq(t, original, string(snapshot.Payload))
}

func TestAcquireIntentConcurrentGlobalSandboxReservation(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	a := acquisitionInput(t, "global-a")
	other := acquisitionInput(t, "global-b")
	for i := 0; other.Workspace.Partition() == a.Workspace.Partition(); i++ {
		var err error
		other.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("other/p%d/", i))
		require.NoError(t, err)
	}
	other.SandboxID = a.SandboxID
	start := make(chan struct{})
	done := make(chan AcquireIntentResult, 2)
	for _, input := range []AcquireIntentInput{a, other} {
		go func(input AcquireIntentInput) {
			<-start
			result, err := b.AcquireIntent(ctx, input)
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected error: %v", err)
			}
			done <- result
		}(input)
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		if (<-done).Disposition == AcquireCreated {
			winners++
		}
	}
	require.Equal(t, 1, winners)
}

func TestAcquireIntentBoundsBeforeGrant(t *testing.T) {
	for _, kind := range []string{"nil context", "zero now", "expiry overflow", "ttl overflow", "wire overflow", "id length"} {
		t.Run(kind, func(t *testing.T) {
			b, raw := integrationBackend(t)
			input := acquisitionInput(t, kind)
			lease := &acquisitionLease{Lease: b.client.Lease}
			b.client.Lease = lease
			ctx := context.Background()
			switch kind {
			case "nil context":
				ctx = nil
			case "zero now":
				input.Now = time.Time{}
			case "expiry overflow":
				input.Now = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
			case "ttl overflow":
				input.TTL = 365*24*time.Hour + 1
			case "wire overflow":
				input.Payload = json.RawMessage(`"` + strings.Repeat("a", 65400) + `"`)
			case "id length":
				input.RequestID = strings.Repeat("a", 129)
			}
			result, err := b.AcquireIntent(ctx, input)
			require.ErrorIs(t, err, ErrInvalidRecord)
			require.Empty(t, result.Reference)
			require.Zero(t, lease.grants)
			got, err := raw.Get(context.Background(), b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Len(t, got.Kvs, 2)
		})
	}
}

// Grant counting keeps the actual etcd lease service and observes allocation.
type acquisitionLease struct {
	clientv3.Lease
	grants     int
	failRevoke bool
}

func (l *acquisitionLease) Grant(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
	l.grants++
	return l.Lease.Grant(ctx, ttl)
}
func (l *acquisitionLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	if l.failRevoke {
		return nil, context.DeadlineExceeded
	}
	return l.Lease.Revoke(ctx, id)
}

func TestAcquireIntentOccupiedAndCompletedReplayMakeNoAttempt(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "occupied")
	first, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	lease := &acquisitionLease{Lease: b.client.Lease}
	b.client.Lease = lease
	other := acquisitionInput(t, "other")
	other.TTL = time.Millisecond
	result, err := b.AcquireIntent(ctx, other)
	require.NoError(t, err)
	require.Equal(t, AcquireOccupied, result.Disposition)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Empty(t, result.Reference)
	require.Equal(t, first.Owner, result.Owner)
	request, err := b.LoadRequest(ctx, other.Principal, other.IdempotencyKey)
	require.NoError(t, err)
	require.Nil(t, request)
	completed := first.Request
	completed.Phase = "completed"
	key, err := b.namespace.requestKey(completed.RequestHash)
	require.NoError(t, err)
	value, err := encodeDomainRecord(completed)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	before, err := raw.Get(ctx, key)
	require.NoError(t, err)
	replay := acquisitionInput(t, "occupied")
	result, err = b.AcquireIntent(ctx, replay)
	require.NoError(t, err)
	require.Equal(t, AcquireReplay, result.Disposition)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, completed, result.Request)
	require.Empty(t, result.Reference)
	require.Zero(t, lease.grants)
	after, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].ModRevision, after.Kvs[0].ModRevision)
}

func TestAcquireIntentCleanupFailurePreservesCommittedEvidence(t *testing.T) {
	b, raw := integrationBackend(t)
	lease := &acquisitionLease{Lease: b.client.Lease, failRevoke: true}
	b.client.Lease = lease
	input := acquisitionInput(t, "cleanup")
	ctx := context.Background()
	result, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, AcquireCreated, result.Disposition)
	require.ErrorIs(t, result.GuardCleanupError, ErrOutcomeUnknown)
	outcome, err := b.ResolveStage(ctx, result.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	request, err := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
	require.NoError(t, err)
	require.Equal(t, "pending", request.Phase)
	guard, _, err := b.stageKeys(result.Reference)
	require.NoError(t, err)
	got, err := raw.Get(ctx, guard)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	_, err = raw.Revoke(ctx, clientv3.LeaseID(got.Kvs[0].Lease))
	require.NoError(t, err)
}

func TestAcquireIntentExistingRecordsCannotBeOverwritten(t *testing.T) {
	for _, kind := range []string{"control", "snapshot", "intent", "placement", "request", "owner"} {
		for _, corruption := range []string{"leased", "malformed"} {
			t.Run(kind+"/"+corruption, func(t *testing.T) {
				b, raw := integrationBackend(t)
				ctx := context.Background()
				input := acquisitionInput(t, kind)
				_, err := b.AcquireIntent(ctx, input)
				require.NoError(t, err)
				ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
				require.NoError(t, err)
				controlKey, snapshotKey, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
				require.NoError(t, err)
				intentKey, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
				require.NoError(t, err)
				requestHash, err := requestKeyHash(input.Principal, input.IdempotencyKey)
				require.NoError(t, err)
				requestKey, err := b.namespace.requestKey(requestHash)
				require.NoError(t, err)
				placementKey, err := b.namespace.placementKey(input.SandboxID)
				require.NoError(t, err)
				keys := map[string]string{"control": controlKey, "snapshot": snapshotKey, "intent": intentKey, "placement": placementKey, "request": requestKey, "owner": ownerKey}
				key := keys[kind]
				got, err := raw.Get(ctx, key)
				require.NoError(t, err)
				value := string(got.Kvs[0].Value)
				opts := []clientv3.OpOption{}
				if corruption == "malformed" {
					value = "{"
				} else {
					lease, err := raw.Grant(ctx, 30)
					require.NoError(t, err)
					opts = append(opts, clientv3.WithLease(lease.ID))
				}
				_, err = raw.Put(ctx, key, value, opts...)
				require.NoError(t, err)
				before, err := raw.Get(ctx, key)
				require.NoError(t, err)
				lease := &acquisitionLease{Lease: b.client.Lease}
				b.client.Lease = lease
				_, err = b.AcquireIntent(ctx, input)
				require.ErrorIs(t, err, ErrCorruptRecord)
				require.Zero(t, lease.grants)
				after, err := raw.Get(ctx, key)
				require.NoError(t, err)
				require.Equal(t, before.Kvs[0].ModRevision, after.Kvs[0].ModRevision)
			})
		}
	}
}

func TestAcquireIntentValidIntentCollisionFailsBeforeGrant(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "intent-collision")
	hash, err := requestKeyHash("other", "key")
	require.NoError(t, err)
	intent := CreationIntentRecord{Version: 1, IntentID: input.IntentID, SandboxID: "old-sandbox", WorkspaceHash: input.Workspace.Hash(), RequestHash: hash, ConfigurationDigest: strings.Repeat("a", 64), RestoreEpoch: b.restoreEpoch, Generation: 1, Phase: "pending"}
	key, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
	require.NoError(t, err)
	value, err := encodeDomainRecord(intent)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	lease := &acquisitionLease{Lease: b.client.Lease}
	b.client.Lease = lease
	result, err := b.AcquireIntent(ctx, input)
	require.ErrorIs(t, err, ErrConflict)
	require.Empty(t, result.Reference)
	require.Zero(t, lease.grants)
	got, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, value, string(got.Kvs[0].Value))
}

func TestDomainReadFailedIdentityChecksExactMetadataKeys(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "metadata-key")
	_, err := raw.Put(ctx, b.restoreKey, "different-epoch")
	require.NoError(t, err)
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) > 0 && ops[0].IsGet() }, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.False(t, response.Succeeded)
		response.Responses[0].GetResponseRange().Kvs[0].Key = []byte("/wrong/meta/identity")
		response.Responses[1].GetResponseRange().Kvs[0].Value = []byte(b.restoreEpoch)
		return response, nil
	}}
	_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
	require.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestDomainReadNilMetadataKVFailsClosed(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	input := acquisitionInput(t, "nil-metadata")
	_, err := raw.Put(ctx, b.restoreKey, "different-epoch")
	require.NoError(t, err)
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) > 0 && ops[0].IsGet() }, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		response.Responses[0].GetResponseRange().Kvs[0] = nil
		return response, nil
	}}
	require.NotPanics(t, func() {
		_, err = b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
		require.ErrorIs(t, err, ErrIdentityMismatch)
	})
}
