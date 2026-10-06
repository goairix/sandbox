package etcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestCreationClaimPublishedGraphIsOccupied(t *testing.T) {
	for _, phase := range []SandboxPhase{PhaseActive, PhaseWorkspaceExclusive, PhaseDestroying, PhaseCleanupPending} {
		t.Run(string(phase), func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ctx := context.Background()
			owner, err := b.LoadOwner(ctx, input.Workspace)
			require.NoError(t, err)
			control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			require.NoError(t, err)
			intent, err := b.LoadCreationIntent(ctx, input.Workspace, input.IntentID)
			require.NoError(t, err)
			request, err := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
			require.NoError(t, err)
			runtime := &RuntimeReference{ID: "container", UID: "uid", BootID: "boot"}
			owner.Runtime = runtime
			owner.MountAttempt = 1
			control.Runtime = runtime
			control.MountAttempt = 1
			control.Phase = phase
			intent.Phase = "published"
			request.Phase = "completed"
			ok, _, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			ck, _, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
			require.NoError(t, err)
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			rk, err := b.namespace.requestKey(request.RequestHash)
			require.NoError(t, err)
			records := []interface{ Validate() error }{*owner, *control, *intent, *request}
			keys := []string{ok, ck, ik, rk}
			for i, r := range records {
				value, e := encodeDomainRecord(r)
				require.NoError(t, e)
				_, e = raw.Put(ctx, keys[i], value)
				require.NoError(t, e)
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			require.ErrorIs(t, err, ErrConflict)
			require.Zero(t, lease.grants.Load())

		})
	}
}

func TestCreationClaimInvalidInputBeforeGrant(t *testing.T) {
	cases := []string{"nil context", "cancelled", "zero workspace", "wrong storage", "bad intent", "bad worker", "long worker", "unicode worker", "zero ttl", "negative ttl", "long ttl"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			b, _, input := pendingCreationFixture(t)
			ctx := context.Background()
			worker := "worker"
			ttl := 30 * time.Second
			switch kind {
			case "nil context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero workspace":
				input.Workspace = WorkspaceIdentity{}
			case "wrong storage":
				var e error
				input.Workspace, e = NewWorkspaceIdentity("s3", "wrong", "bucket", "team/project/")
				require.NoError(t, e)
			case "bad intent":
				input.IntentID = "../intent"
			case "bad worker":
				worker = "a/b"
			case "long worker":
				worker = strings.Repeat("a", 129)
			case "unicode worker":
				worker = "工人"
			case "zero ttl":
				ttl = 0
			case "negative ttl":
				ttl = -time.Second
			case "long ttl":
				ttl = 24*time.Hour + 1
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, worker, ttl)
			require.Nil(t, c)
			require.Error(t, err)
			require.Zero(t, lease.grants.Load())
		})
	}
}
func TestCreationClaimPermanentGraphRejectsCorruptionBeforeGrant(t *testing.T) {
	for _, kind := range []string{"missing owner", "missing fence", "missing control", "missing request", "missing placement", "intent workspace", "intent id", "intent request", "intent generation", "request digest", "request sandbox", "request workspace", "owner intent", "owner bound", "fence generation", "control generation", "placement intent", "old epoch", "leased domain", "corrupt domain"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ctx := context.Background()
			intent, err := b.LoadCreationIntent(ctx, input.Workspace, input.IntentID)
			require.NoError(t, err)
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			ok, fk, err := b.namespace.workspaceKeys(input.Workspace)
			require.NoError(t, err)
			ck, _, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
			require.NoError(t, err)
			rk, err := b.namespace.requestKey(intent.RequestHash)
			require.NoError(t, err)
			pk, err := b.namespace.placementKey(input.SandboxID)
			require.NoError(t, err)
			key := ik
			field := ""
			var value any
			switch kind {
			case "missing owner":
				key = ok
			case "missing fence":
				key = fk
			case "missing control":
				key = ck
			case "missing request":
				key = rk
			case "missing placement":
				key = pk
			case "intent workspace":
				field = "workspace_hash"
				value = strings.Repeat("0", 64)
			case "intent id":
				field = "intent_id"
				value = "other"
			case "intent request":
				field = "request_hash"
				value = strings.Repeat("0", 64)
			case "intent generation":
				field = "generation"
				value = 2
			case "request digest":
				key = rk
				field = "configuration_digest"
				value = strings.Repeat("0", 64)
			case "request sandbox":
				key = rk
				field = "sandbox_id"
				value = "other"
			case "request workspace":
				key = rk
				field = "workspace_hash"
				value = strings.Repeat("0", 64)
			case "owner intent":
				key = ok
				field = "intent_id"
				value = "other"
			case "owner bound":
				key = ok
				field = "runtime"
				value = RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}
			case "fence generation":
				key = fk
				field = "generation"
				value = 2
			case "control generation":
				key = ck
				field = "generation"
				value = 2
			case "placement intent":
				key = pk
				field = "intent_id"
				value = "other"
			case "old epoch":
				field = "restore_epoch"
				value = "old"
			}
			got, err := raw.Get(ctx, key)
			require.NoError(t, err)
			require.Len(t, got.Kvs, 1)
			if strings.HasPrefix(kind, "missing") {
				_, err = raw.Delete(ctx, key)
			} else if kind == "corrupt domain" {
				_, err = raw.Put(ctx, key, "{")
			} else if kind == "leased domain" {
				lease, e := raw.Grant(ctx, 30)
				require.NoError(t, e)
				_, err = raw.Put(ctx, key, string(got.Kvs[0].Value), clientv3.WithLease(lease.ID))
				t.Cleanup(func() { _, e := raw.Revoke(ctx, lease.ID); require.NoError(t, e) })
			} else {
				var record map[string]any
				require.NoError(t, json.Unmarshal(got.Kvs[0].Value, &record))
				record[field] = value
				encoded, e := json.Marshal(record)
				require.NoError(t, e)
				_, err = raw.Put(ctx, key, string(encoded))
			}
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			if kind == "old epoch" {
				require.ErrorIs(t, err, ErrIdentityMismatch)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestCreationClaimExistingClaimMustBeExactAndLeased(t *testing.T) {
	for _, kind := range []string{"valid", "unleased", "corrupt", "unknown field", "invalid utf8", "wrong lease", "wrong sandbox", "wrong epoch", "wrong gate", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input, c := claimFixture(t)
			ctx := context.Background()
			value := c.value
			options := []clientv3.OpOption{clientv3.WithLease(clientv3.LeaseID(c.Reference().LeaseID))}
			var record map[string]any
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.UseNumber()
			require.NoError(t, decoder.Decode(&record))
			switch kind {
			case "unleased":
				options = nil
			case "corrupt":
				value = "{"
			case "unknown field":
				record["extra"] = true
			case "invalid utf8":
				value = string([]byte{'"', 255, '"'})
			case "wrong lease":
				record["lease_id"] = 1
			case "wrong sandbox":
				record["sandbox_id"] = "other"
			case "wrong epoch":
				record["restore_epoch"] = "old"
			case "wrong gate":
				record["data_gate_epoch"] = 2
			case "oversized":
				value = strings.Repeat(" ", 4097)
			}
			if kind == "unknown field" || strings.HasPrefix(kind, "wrong") {
				encoded, e := json.Marshal(record)
				require.NoError(t, e)
				value = string(encoded)
			}
			_, err := raw.Put(ctx, c.claimKey, value, options...)
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			next, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "next", 30*time.Second)
			require.Nil(t, next)
			if kind == "valid" {
				require.ErrorIs(t, err, ErrConflict)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
			require.Zero(t, lease.grants.Load())
			require.NoError(t, b.ReleaseCreationClaim(ctx, c))
		})
	}
}

func TestCreationClaimMalformedReadEnvelopeBeforeGrant(t *testing.T) {
	for _, kind := range []string{"wrong key", "nil response", "nil kv", "extra kv", "wrong count", "bad revision"} {
		t.Run(kind, func(t *testing.T) {
			b, _, input := pendingCreationFixture(t)
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 1 && string(ops[0].KeyBytes()) == ik }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				response := r.Responses[0].GetResponseRange()
				switch kind {
				case "wrong key":
					response.Kvs[0].Key = []byte(ik + "other")
				case "nil response":
					r.Responses[0] = nil
				case "nil kv":
					response.Kvs[0] = nil
				case "extra kv":
					response.Kvs = append(response.Kvs, response.Kvs[0])
					response.Count = 2
				case "wrong count":
					response.Count = 2
				case "bad revision":
					response.Kvs[0].CreateRevision = 0
				}
				return r, e
			}}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestCreationClaimRestoreChangesAfterGrantRejectCapability(t *testing.T) {
	b, raw, input := pendingCreationFixture(t)
	lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
		require.NoError(t, e)
		_, e = raw.Put(context.Background(), b.restoreKey, "new-epoch")
		require.NoError(t, e)
		return r, nil
	}}
	b.client.Lease = lease
	c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	require.Equal(t, int64(1), lease.revokes.Load())
	ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
	require.NoError(t, err)
	got, err := raw.Get(context.Background(), ik)
	require.NoError(t, err)
	require.Len(t, got.Kvs, 1)
	got, err = raw.Get(context.Background(), ik+"/claim")
	require.NoError(t, err)
	require.Empty(t, got.Kvs)
}
func TestCreationClaimMetadataFailureEnvelopeIsStrict(t *testing.T) {
	for _, kind := range []string{"wrong key", "nil op", "nil kv", "extra kv", "wrong count", "wrong cardinality"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ik, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			b.client.Lease = &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
				require.NoError(t, e)
				_, e = raw.Put(context.Background(), b.restoreKey, "new-epoch")
				require.NoError(t, e)
				return r, nil
			}}
			b.client.KV = &faultKV{KV: b.client.KV, match: putsKey(ik + "/claim"), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.False(t, r.Succeeded)
				response := r.Responses[0].GetResponseRange()
				switch kind {
				case "wrong key":
					response.Kvs[0].Key = []byte(b.identityKey + "other")
				case "nil op":
					r.Responses[0] = nil
				case "nil kv":
					response.Kvs[0] = nil
				case "extra kv":
					response.Kvs = append(response.Kvs, response.Kvs[0])
					response.Count = 2
				case "wrong count":
					response.Count = 2
				case "wrong cardinality":
					r.Responses = r.Responses[:1]
				}
				return r, e
			}}
			c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			require.ErrorIs(t, err, ErrIdentityMismatch)
		})
	}
}
func TestCreationClaimBackendCapabilityCannotBeReconstructed(t *testing.T) {
	b, _, _, c := claimFixture(t)
	other := *b
	_, err := other.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.ErrorIs(t, other.RenewCreationClaim(context.Background(), c), ErrInvalidRecord)
	require.ErrorIs(t, other.ReleaseCreationClaim(context.Background(), c), ErrInvalidRecord)
	for _, bad := range []*CreationClaim{nil, {reference: c.Reference()}} {
		_, err = b.creationClaimComparisons(bad)
		require.ErrorIs(t, err, ErrInvalidRecord)
		require.ErrorIs(t, b.RenewCreationClaim(context.Background(), bad), ErrInvalidRecord)
		require.ErrorIs(t, b.ReleaseCreationClaim(context.Background(), bad), ErrInvalidRecord)
	}
	require.NoError(t, b.RenewCreationClaim(context.Background(), c))
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}
func TestCreationClaimRenewMalformedSuccessResponseStopsCapability(t *testing.T) {
	b, _, _, c := claimFixture(t)
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 0 }, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		require.NoError(t, e)
		r.Responses = []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Key: []byte(c.claimKey)}}}}}}
		return r, e
	}}
	require.ErrorIs(t, b.RenewCreationClaim(context.Background(), c), ErrOutcomeUnknown)
	_, err := b.creationClaimComparisons(c)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
}

func TestCreationClaimPendingCleanupGraphClassification(t *testing.T) {
	for _, kind := range []string{"destroying", "cleanup_pending", "mismatched cleanup runtime", "contradictory request phase", "contradictory active phase"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			ctx := context.Background()
			control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			require.NoError(t, err)
			control.Phase = PhaseDestroying
			if kind == "cleanup_pending" || kind == "mismatched cleanup runtime" {
				control.Phase = PhaseCleanupPending
			}
			if kind == "mismatched cleanup runtime" {
				control.Runtime = &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
			}
			if kind == "contradictory active phase" {
				control.Phase = PhaseActive
				control.Runtime = &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
				owner, e := b.LoadOwner(ctx, input.Workspace)
				require.NoError(t, e)
				owner.Runtime = control.Runtime
				key, _, e := b.namespace.workspaceKeys(input.Workspace)
				require.NoError(t, e)
				value, e := encodeDomainRecord(*owner)
				require.NoError(t, e)
				_, e = raw.Put(ctx, key, value)
				require.NoError(t, e)
			}
			key, _, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
			require.NoError(t, err)
			value, err := encodeDomainRecord(*control)
			require.NoError(t, err)
			_, err = raw.Put(ctx, key, value)
			require.NoError(t, err)
			if kind == "contradictory request phase" {
				request, e := b.LoadRequest(ctx, input.Principal, input.IdempotencyKey)
				require.NoError(t, e)
				request.Phase = "completed"
				key, e := b.namespace.requestKey(request.RequestHash)
				require.NoError(t, e)
				value, e := encodeDomainRecord(*request)
				require.NoError(t, e)
				_, e = raw.Put(ctx, key, value)
				require.NoError(t, e)
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
			require.Nil(t, c)
			if kind == "destroying" || kind == "cleanup_pending" {
				require.ErrorIs(t, err, ErrConflict)
			} else {
				require.ErrorIs(t, err, ErrCorruptRecord)
			}
			require.Zero(t, lease.grants.Load())
		})
	}
}

func TestCreationClaimPermanentCASRejectsChangeAfterGrant(t *testing.T) {
	b, raw, input := pendingCreationFixture(t)
	ctx := context.Background()
	ownerKey, _, err := b.namespace.workspaceKeys(input.Workspace)
	require.NoError(t, err)
	before, err := raw.Get(ctx, ownerKey)
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	lease := &creationFaultLease{Lease: b.client.Lease, grant: func(r *clientv3.LeaseGrantResponse, e error) (*clientv3.LeaseGrantResponse, error) {
		require.NoError(t, e)
		_, e = raw.Put(ctx, ownerKey, string(before.Kvs[0].Value))
		require.NoError(t, e)
		return r, e
	}}
	b.client.Lease = lease
	c, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "worker", 30*time.Second)
	require.Nil(t, c)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, int64(1), lease.revokes.Load())
	got, err := raw.Get(ctx, ownerKey)
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].Value, got.Kvs[0].Value)
	require.Greater(t, got.Kvs[0].ModRevision, before.Kvs[0].ModRevision)
}
func TestCreationClaimTTLSecondsRoundUp(t *testing.T) {
	for _, test := range []struct {
		ttl     time.Duration
		seconds int64
	}{{time.Nanosecond, 1}, {1500 * time.Millisecond, 2}, {24 * time.Hour, 86400}} {
		t.Run(test.ttl.String(), func(t *testing.T) {
			b, raw, input := pendingCreationFixture(t)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "worker", test.ttl)
			require.NoError(t, err)
			require.Equal(t, test.seconds, lease.requestedTTL.Load())
			ttl, err := raw.TimeToLive(context.Background(), clientv3.LeaseID(c.Reference().LeaseID))
			require.NoError(t, err)
			require.GreaterOrEqual(t, ttl.GrantedTTL, test.seconds)
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), c))
		})
	}
}
