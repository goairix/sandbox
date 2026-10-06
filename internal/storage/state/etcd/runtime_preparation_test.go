package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

func preparationFixture(t *testing.T, mode string, clocks ...AuthorityClock) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
	t.Helper()
	base, raw, input, old := claimFixture(t)
	require.NoError(t, base.ReleaseCreationClaim(context.Background(), old))
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	o := integrationOptions(base, raw)
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}}
	o.Clock = goodAuthorityClock()
	if len(clocks) > 0 {
		o.Clock = clocks[0]
	}
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	c, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "preparer", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseCreationClaim(context.Background(), c) })
	dispatch, err := b.DeclareRuntimeDispatch(context.Background(), c, dispatchInputForDeclaration())
	require.NoError(t, err)
	wire := preparationCertificate(t, b, dispatch.Entry.Record, key, mode, RuntimeReference{ID: "id", UID: "uid", BootID: "boot"})
	return b, raw, input, c, wire, key
}
func preparationCertificate(t *testing.T, b *Backend, d RuntimeDispatchRecord, key ed25519.PrivateKey, mode string, r RuntimeReference) []byte {
	t.Helper()
	now := time.Now().UTC()
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	claims := controlprotocol.RuntimeCertificateClaims{Version: 1, Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: d.Target, RestoreEpoch: d.RestoreEpoch, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: controlprotocol.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt, Runtime: controlprotocol.RuntimeReference(r), WorkspaceMode: mode, PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: d.ExpiresAt}
	wire, err := controlprotocol.SignRuntimeCertificate(key, claims)
	require.NoError(t, err)
	return wire
}
func TestRuntimePreparationBindAndConsume(t *testing.T) {
	for _, mode := range []string{"fuse", "plain"} {
		t.Run(mode, func(t *testing.T) {
			b, raw, input, c, wire, _ := preparationFixture(t, mode)
			ctx := context.Background()
			before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			result, err := b.BindRuntime(ctx, c, wire)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome, "authenticated binding must be atomically committed")
			require.NotNil(t, result.Binding)
			require.NoError(t, result.GuardCleanupError)
			bk, ck, err := b.namespace.runtimeBindingKeys(input.Workspace.Partition(), input.IntentID)
			require.NoError(t, err)
			xk, err := b.namespace.runtimeIndexKey("uid")
			require.NoError(t, err)
			_, rk, err := b.stageKeys(result.Reference)
			require.NoError(t, err)
			values, err := b.readDomain(ctx, bk, ck, xk, rk)
			require.NoError(t, err)
			for _, kv := range values {
				require.NotNil(t, kv)
				require.Zero(t, kv.Lease)
				require.Equal(t, values[0].CreateRevision, kv.CreateRevision)
				require.Equal(t, kv.CreateRevision, kv.ModRevision)
			}
			for _, kv := range before.Kvs {
				got, e := raw.Get(ctx, string(kv.Key))
				require.NoError(t, e)
				require.Equal(t, kv, got.Kvs[0])
			}
			loaded, err := b.LoadRuntimeBinding(ctx, input.Workspace, input.IntentID)
			require.NoError(t, err)
			require.Equal(t, result.Binding, loaded)
			replay, err := b.BindRuntime(ctx, c, wire)
			require.NoError(t, err)
			require.True(t, replay.Replay)
			require.Equal(t, result.Binding, replay.Binding)
			mount, err := b.ConsumeRuntimeMount(ctx, c)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, mount.Outcome)
			require.NotNil(t, mount.Mount)
			require.Equal(t, mode, mount.Mount.Record.WorkspaceMode)
			attempt := uint8(0)
			if mode == "fuse" {
				attempt = 1
			}
			require.Equal(t, attempt, mount.Mount.Record.MountAttempt)
			require.NotEmpty(t, mount.Mount.Record.OperationID)
			again, err := b.ConsumeRuntimeMount(ctx, c)
			require.NoError(t, err)
			require.True(t, again.Replay)
			require.Equal(t, mount.Mount, again.Mount)
		})
	}
}

func TestRuntimePreparationBindingRecoveryAndIdentityConflict(t *testing.T) {
	b, _, input, c, wire, key := preparationFixture(t, "fuse")
	ctx := context.Background()
	original, err := b.BindRuntime(ctx, c, wire)
	require.NoError(t, err)
	require.NoError(t, b.ReleaseCreationClaim(ctx, c))
	next, err := b.ClaimCreation(ctx, input.Workspace, input.IntentID, "recovery", 30*time.Second)
	require.NoError(t, err)
	defer b.ReleaseCreationClaim(ctx, next)
	replay, err := b.BindRuntime(ctx, next, wire)
	require.NoError(t, err)
	require.True(t, replay.Replay)
	require.Equal(t, original.Binding, replay.Binding)
	require.NotEqual(t, next.Reference().ClaimID, replay.Binding.Record.Claim.ClaimID)
	d, err := b.LoadRuntimeDispatch(ctx, input.Workspace, input.IntentID)
	require.NoError(t, err)
	replacement := preparationCertificate(t, b, d.Record, key, "fuse", RuntimeReference{ID: "id", UID: "uid", BootID: "other-boot"})
	result, err := b.BindRuntime(ctx, next, replacement)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	require.Nil(t, result.Binding)
	mount, err := b.ConsumeRuntimeMount(ctx, next)
	require.NoError(t, err)
	require.Equal(t, next.Reference().ClaimID, mount.Mount.Record.Claim.ClaimID)
}
func TestRuntimePreparationUIDReservedAcrossWorkspaces(t *testing.T) {
	b, _, first, c, wire, key := preparationFixture(t, "fuse")
	ctx := context.Background()
	second := acquisitionInput(t, "second-runtime")
	var err error
	second.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", "team/second/")
	require.NoError(t, err)
	_, err = b.AcquireIntent(ctx, second)
	require.NoError(t, err)
	c2, err := b.ClaimCreation(ctx, second.Workspace, second.IntentID, "worker2", 30*time.Second)
	require.NoError(t, err)
	defer b.ReleaseCreationClaim(ctx, c2)
	d, err := b.DeclareRuntimeDispatch(ctx, c2, dispatchInputForDeclaration())
	require.NoError(t, err)
	wire2 := preparationCertificate(t, b, d.Entry.Record, key, "fuse", RuntimeReference{ID: "id2", UID: "uid", BootID: "other-boot"})
	type answer struct {
		r PreparationResult
		e error
	}
	done := make(chan answer, 2)
	start := make(chan struct{})
	for _, in := range []struct {
		c *CreationClaim
		w []byte
	}{{c, wire}, {c2, wire2}} {
		go func(cl *CreationClaim, w []byte) { <-start; r, e := b.BindRuntime(ctx, cl, w); done <- answer{r, e} }(in.c, in.w)
	}
	close(start)
	committed := 0
	for i := 0; i < 2; i++ {
		a := <-done
		if a.e == nil {
			require.Equal(t, OutcomeCommitted, a.r.Outcome)
			committed++
		} else {
			require.ErrorIs(t, a.e, ErrConflict)
			require.Nil(t, a.r.Binding)
		}
	}
	require.Equal(t, 1, committed)
	count := 0
	for _, input := range []AcquireIntentInput{first, second} {
		entry, e := b.LoadRuntimeBinding(ctx, input.Workspace, input.IntentID)
		require.NoError(t, e)
		if entry != nil {
			count++
		}
	}
	require.Equal(t, 1, count)
}
func TestRuntimePreparationRejectsUnauthenticatedBeforeGrant(t *testing.T) {
	for _, fault := range []string{"root", "signature", "context", "expired", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			b, _, _, c, wire, key := preparationFixture(t, "plain")
			ctx := context.Background()
			var envelope struct {
				Claims    controlprotocol.RuntimeCertificateClaims `json:"claims"`
				Signature []byte                                   `json:"signature"`
			}
			require.NoError(t, json.Unmarshal(wire, &envelope))
			switch fault {
			case "root":
				key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
			case "context":
				envelope.Claims.OperationID = "11234567-89ab-4cde-8012-3456789abcde"
			case "expired":
				envelope.Claims.NotAfter = time.Now().UTC().Add(-time.Second)
			}
			var err error
			if fault == "signature" {
				envelope.Signature[0] ^= 1
				wire, err = json.Marshal(envelope)
			} else if fault == "malformed" {
				wire = []byte(`{"claims":null}`)
			} else {
				wire, err = controlprotocol.SignRuntimeCertificate(key, envelope.Claims)
			}
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.BindRuntime(ctx, c, wire)
			require.Error(t, err)
			require.Nil(t, result.Binding)
			require.Empty(t, result.Reference.AttemptID)
			require.Zero(t, lease.grants.Load())
		})
	}
}
func TestRuntimePreparationReplayChecksServerFences(t *testing.T) {
	for _, fault := range []string{"control", "guard"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, input, c, wire, _ := preparationFixture(t, "fuse")
			ctx := context.Background()
			_, err := b.BindRuntime(ctx, c, wire)
			require.NoError(t, err)
			_, err = b.ConsumeRuntimeMount(ctx, c)
			require.NoError(t, err)
			if fault == "control" {
				control, e := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
				require.NoError(t, e)
				control.DataGateEpoch++
				key, _, e := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, "read")
				require.NoError(t, e)
				value, e := encodeDomainRecord(*control)
				require.NoError(t, e)
				_, err = raw.Put(ctx, key, value)
			} else {
				_, err = raw.Delete(ctx, c.guardKey)
				require.NoError(t, err)
				_, err = raw.Put(ctx, c.guardKey, c.value, clientv3.WithLease(clientv3.LeaseID(c.Reference().LeaseID)))
			}
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			replay, err := b.BindRuntime(ctx, c, wire)
			require.ErrorIs(t, err, ErrConflict)
			require.False(t, replay.Replay)
			replay, err = b.ConsumeRuntimeMount(ctx, c)
			require.ErrorIs(t, err, ErrConflict)
			require.False(t, replay.Replay)
			require.Zero(t, lease.grants.Load())
			require.Zero(t, lease.keeps.Load())
		})
	}
}

func TestRuntimePreparationCannotPromoteStructuralMetadataToAuthority(t *testing.T) {
	b, raw, input, c, wire, _ := preparationFixture(t, "fuse")
	ctx := context.Background()
	_, err := b.BindRuntime(ctx, c, wire)
	require.NoError(t, err)
	bundle, err := b.loadRuntimePreparation(ctx, input.Workspace, input.IntentID, "")
	require.NoError(t, err)
	var deletes, puts []clientv3.Op
	for i, kv := range bundle.BindingKVs {
		deletes = append(deletes, clientv3.OpDelete(string(kv.Key)))
		value := string(kv.Value)
		if i == 0 {
			r := bundle.Binding.Record
			r.Runtime.BootID = "forged-boot"
			value, err = encodeDomainRecord(r)
			require.NoError(t, err)
		}
		if i == 2 {
			var r RuntimeIndexRecord
			require.NoError(t, decodeDomainRecord(kv, &r))
			r.Runtime.BootID = "forged-boot"
			value, err = encodeDomainRecord(r)
			require.NoError(t, err)
		}
		puts = append(puts, clientv3.OpPut(string(kv.Key), value))
	}
	_, err = raw.Txn(ctx).Then(deletes...).Commit()
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(puts...).Commit()
	require.NoError(t, err)
	structural, err := b.LoadRuntimeBinding(ctx, input.Workspace, input.IntentID)
	require.NoError(t, err)
	require.Equal(t, "forged-boot", structural.Record.Runtime.BootID)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	result, err := b.ConsumeRuntimeMount(ctx, c)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, result.Mount)
	require.Zero(t, lease.grants.Load())
}
func TestRuntimePreparationMetadataOnlyMutationFailClosed(t *testing.T) {
	b, _, _, c := claimFixture(t)
	defer b.ReleaseCreationClaim(context.Background(), c)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	result, err := b.BindRuntime(context.Background(), c, []byte(`{}`))
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, result.Binding)
	result, err = b.ConsumeRuntimeMount(context.Background(), c)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, result.Mount)
	require.Zero(t, lease.grants.Load())
}
