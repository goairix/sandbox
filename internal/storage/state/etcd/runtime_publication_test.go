package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func publicationProof(t *testing.T, b *Backend, c *CreationClaim, cert []byte, change func(*controlprotocol.ReadyReceiptClaims)) []byte {
	t.Helper()
	bundle, err := b.loadRuntimePreparation(context.Background(), c.workspace, c.reference.IntentID, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	r := controlprotocol.ReadyReceiptClaims{Version: 1, CertificateDigest: bundle.Binding.Record.CertificateDigest, Claim: controlprotocol.ClaimReference{ClaimID: c.reference.ClaimID, CreateRevision: c.reference.CreateRevision, LeaseID: c.reference.LeaseID}, DataGateEpoch: c.control.DataGateEpoch, GateState: "open", MountAttempt: bundle.Mount.Record.MountAttempt, ObservedAt: now, ValidUntil: now.Add(5 * time.Second)}
	if r.MountAttempt == 1 {
		r.MountOperationID = bundle.Mount.Record.OperationID
	}
	if change != nil {
		change(&r)
	}
	wire, err := controlprotocol.SignReadyReceipt(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), cert, r)
	require.NoError(t, err)
	return wire
}
func publicationFixture(t *testing.T, mode string, clocks ...AuthorityClock) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte) {
	t.Helper()
	b, raw, in, c, cert, _ := preparationFixture(t, mode, clocks...)
	bind, err := b.BindRuntime(context.Background(), c, cert)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bind.Outcome)
	mount, err := b.ConsumeRuntimeMount(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mount.Outcome)
	return b, raw, in, c, publicationProof(t, b, c, cert, nil)
}
func publicationKeys(t *testing.T, b *Backend, c *CreationClaim) []string {
	t.Helper()
	owner, _, err := b.namespace.workspaceKeys(c.workspace)
	require.NoError(t, err)
	control, _, err := b.namespace.sandboxKeys(c.reference.Partition, c.reference.SandboxID, c.control.Snapshot.Version)
	require.NoError(t, err)
	request, err := b.namespace.requestKey(c.request.RequestHash)
	require.NoError(t, err)
	intent, err := b.namespace.intentKey(c.reference.Partition, c.reference.IntentID)
	require.NoError(t, err)
	journal, proof, err := b.namespace.runtimePublicationKeys(c.reference.Partition, c.reference.IntentID)
	require.NoError(t, err)
	return []string{owner, control, request, intent, journal, proof}
}
func TestRuntimePublicationAtomicAndReplay(t *testing.T) {
	for _, mode := range []string{"plain", "fuse"} {
		t.Run(mode, func(t *testing.T) {
			b, raw, in, c, proof := publicationFixture(t, mode)
			ctx := context.Background()
			before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.PublishRuntime(ctx, c, proof)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			require.Equal(t, PublishDeclared, result.Disposition)
			require.NotNil(t, result.Entry)
			require.NoError(t, result.GuardCleanupError)
			keys := publicationKeys(t, b, c)
			_, receipt, err := b.stageKeys(result.Reference)
			require.NoError(t, err)
			values, err := b.readDomain(ctx, append(keys, receipt)...)
			require.NoError(t, err)
			for _, kv := range values {
				require.NotNil(t, kv)
				require.Zero(t, kv.Lease)
				require.Equal(t, values[0].ModRevision, kv.ModRevision)
			}
			var owner WorkspaceOwnerRecord
			require.NoError(t, decodeDomainRecord(values[0], &owner))
			var control SandboxControlRecord
			require.NoError(t, decodeDomainRecord(values[1], &control))
			var request CreationRequestRecord
			require.NoError(t, decodeDomainRecord(values[2], &request))
			var intent CreationIntentRecord
			require.NoError(t, decodeDomainRecord(values[3], &intent))
			require.Equal(t, &RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, owner.Runtime)
			require.Equal(t, owner.Runtime, control.Runtime)
			require.Equal(t, PhaseActive, control.Phase)
			require.Equal(t, "completed", request.Phase)
			require.Equal(t, "published", intent.Phase)
			require.Equal(t, c.control.DataGateEpoch, control.DataGateEpoch)
			require.Equal(t, c.control.Snapshot, control.Snapshot)
			require.Equal(t, c.control.ExpiresAt, control.ExpiresAt)
			attempt := uint8(0)
			if mode == "fuse" {
				attempt = 1
			}
			require.Equal(t, attempt, owner.MountAttempt)
			require.Equal(t, attempt, control.MountAttempt)
			for _, kv := range before.Kvs {
				changed := false
				for _, k := range keys[:4] {
					if string(kv.Key) == k {
						changed = true
					}
				}
				if !changed {
					got, e := raw.Get(ctx, string(kv.Key))
					require.NoError(t, e)
					require.Equal(t, kv, got.Kvs[0])
				}
			}
			loaded, err := b.LoadRuntimePublication(ctx, in.Workspace, in.IntentID)
			require.NoError(t, err)
			require.Equal(t, result.Entry, loaded)
			ttlBefore, err := raw.TimeToLive(ctx, clientv3.LeaseID(c.reference.LeaseID))
			require.NoError(t, err)
			replay, err := b.PublishRuntime(ctx, c, proof)
			require.NoError(t, err)
			require.Equal(t, PublishReplay, replay.Disposition)
			require.Equal(t, result.Entry, replay.Entry)
			require.Equal(t, int64(1), lease.grants.Load())
			require.Zero(t, lease.keeps.Load())
			ttlAfter, err := raw.TimeToLive(ctx, clientv3.LeaseID(c.reference.LeaseID))
			require.NoError(t, err)
			require.LessOrEqual(t, ttlAfter.TTL, ttlBefore.TTL)
			require.Error(t, b.checkPreparationReplay(ctx, c), "publication invalidates original publishing fences")
		})
	}
}
