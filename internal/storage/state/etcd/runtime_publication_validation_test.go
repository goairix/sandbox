package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestRuntimePublicationRejectsProofBeforeGrant(t *testing.T) {
	for _, fault := range []string{"unsigned", "unknown root", "signature", "expired", "old claim", "gate", "mount operation", "operation", "input", "snapshot", "runtime", "boot", "mode", "target", "restore", "oversize", "null", "duplicate", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			b, _, _, c, wire := publicationFixture(t, "fuse")
			var envelope struct {
				Certificate json.RawMessage                    `json:"certificate"`
				Claims      controlprotocol.ReadyReceiptClaims `json:"claims"`
				Signature   []byte                             `json:"signature"`
			}
			require.NoError(t, json.Unmarshal(wire, &envelope))
			var cert struct {
				Claims    controlprotocol.RuntimeCertificateClaims `json:"claims"`
				Signature []byte                                   `json:"signature"`
			}
			require.NoError(t, json.Unmarshal(envelope.Certificate, &cert))
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
			switch fault {
			case "unknown root":
				key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
			case "old claim":
				envelope.Claims.Claim.ClaimID = "33333333-3333-4333-8333-333333333333"
			case "gate":
				envelope.Claims.DataGateEpoch++
			case "mount operation":
				envelope.Claims.MountOperationID = "33333333-3333-4333-8333-333333333333"
			case "operation":
				cert.Claims.OperationID = "33333333-3333-4333-8333-333333333333"
			case "input":
				cert.Claims.PayloadDigest = strings.Repeat("a", 64)
			case "snapshot":
				cert.Claims.Snapshot.Version = "another"
			case "runtime":
				cert.Claims.Runtime.UID = "another"
			case "boot":
				cert.Claims.Runtime.BootID = "another"
			case "mode":
				cert.Claims.WorkspaceMode = "plain"
				envelope.Claims.MountAttempt = 0
				envelope.Claims.MountOperationID = ""
			case "target":
				cert.Claims.Target = "another"
			case "restore":
				cert.Claims.RestoreEpoch = "another"
			case "expired":
				envelope.Claims.ObservedAt = time.Now().UTC().Add(-10 * time.Second)
				envelope.Claims.ValidUntil = envelope.Claims.ObservedAt.Add(5 * time.Second)
			}
			certificate, err := controlprotocol.SignRuntimeCertificate(key, cert.Claims)
			require.NoError(t, err)
			envelope.Claims.CertificateDigest, err = snapshotDigest(certificate)
			require.NoError(t, err)
			wire, err = controlprotocol.SignReadyReceipt(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), certificate, envelope.Claims)
			require.NoError(t, err)
			switch fault {
			case "unsigned":
				wire = []byte(`{"completed":true}`)
			case "signature":
				require.NoError(t, json.Unmarshal(wire, &envelope))
				envelope.Signature[0] ^= 1
				wire, err = json.Marshal(envelope)
				require.NoError(t, err)
			case "oversize":
				wire = append(wire, bytes.Repeat([]byte{' '}, 8193-len(wire))...)
			case "null":
				wire = []byte(`null`)
			case "duplicate":
				wire = append([]byte(`{"signature":"bad",`), wire[1:]...)
			case "trailing":
				wire = append(wire, []byte(`{}`)...)
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.PublishRuntime(context.Background(), c, wire)
			require.Error(t, err)
			require.Nil(t, result.Entry)
			require.Empty(t, result.Reference.Digest)
			require.Zero(t, lease.grants.Load())
		})
	}
}
func TestRuntimePublicationRequiresPreparationBeforeGrant(t *testing.T) {
	for _, fault := range []string{"no binding", "no mount", "missing index", "wrong index", "recreated index"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, _, c, cert, _ := preparationFixture(t, "plain")
			ctx := context.Background()
			wire := []byte(`{}`)
			if fault != "no binding" {
				_, err := b.BindRuntime(ctx, c, cert)
				require.NoError(t, err)
			}
			if fault != "no binding" && fault != "no mount" {
				_, err := b.ConsumeRuntimeMount(ctx, c)
				require.NoError(t, err)
				wire = publicationProof(t, b, c, cert, nil)
				key, err := b.namespace.runtimeIndexKey("uid")
				require.NoError(t, err)
				got, err := raw.Get(ctx, key)
				require.NoError(t, err)
				_, err = raw.Delete(ctx, key)
				require.NoError(t, err)
				if fault != "missing index" {
					value := string(got.Kvs[0].Value)
					if fault == "wrong index" {
						value = strings.Replace(value, `"boot_id":"boot"`, `"boot_id":"wrong"`, 1)
					}
					_, err = raw.Put(ctx, key, value)
					require.NoError(t, err)
				}
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.PublishRuntime(ctx, c, wire)
			require.Error(t, err)
			require.Nil(t, result.Entry)
			require.Zero(t, lease.grants.Load())
		})
	}
}
func TestRuntimePublicationReplayFreshProofAndHistoricalRead(t *testing.T) {
	var expired atomic.Bool
	clock := testAuthorityClock(func(context.Context) (ClockObservation, error) {
		now := time.Now().UTC()
		if expired.Load() {
			now = now.Add(72 * time.Hour)
		}
		return ClockObservation{UTC: now}, nil
	})
	b, _, in, c, wire := publicationFixture(t, "plain", clock)
	result, err := b.PublishRuntime(context.Background(), c, wire)
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	bundle, err := b.loadRuntimePreparation(context.Background(), in.Workspace, in.IntentID, "")
	require.NoError(t, err)
	other := publicationProof(t, b, c, bundle.Binding.Certificate, nil)
	replay, err := b.PublishRuntime(context.Background(), c, other)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	require.Nil(t, replay.Entry)
	expired.Store(true)
	replay, err = b.PublishRuntime(context.Background(), c, wire)
	require.Error(t, err)
	require.Nil(t, replay.Entry)
	historical, err := b.LoadRuntimePublication(context.Background(), in.Workspace, in.IntentID)
	require.NoError(t, err)
	require.Equal(t, result.Entry, historical)
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
}

// Capture the full real request at the transport boundary. Removing original
// comparisons or index fences must fail even if the happy path still commits.
type publicationCaptureKV struct {
	clientv3.KV
	capture func([]clientv3.Cmp, []clientv3.Op)
}

func (k *publicationCaptureKV) Txn(ctx context.Context) clientv3.Txn {
	return &publicationCaptureTxn{Txn: k.KV.Txn(ctx), capture: k.capture}
}

type publicationCaptureTxn struct {
	clientv3.Txn
	capture     func([]clientv3.Cmp, []clientv3.Op)
	comparisons []clientv3.Cmp
	ops         []clientv3.Op
}

func (t *publicationCaptureTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	t.comparisons = c
	t.Txn = t.Txn.If(c...)
	return t
}
func (t *publicationCaptureTxn) Then(o ...clientv3.Op) clientv3.Txn {
	t.ops = o
	t.Txn = t.Txn.Then(o...)
	return t
}
func (t *publicationCaptureTxn) Else(o ...clientv3.Op) clientv3.Txn {
	t.Txn = t.Txn.Else(o...)
	return t
}
func (t *publicationCaptureTxn) Commit() (*clientv3.TxnResponse, error) {
	t.capture(t.comparisons, t.ops)
	return t.Txn.Commit()
}
func TestRuntimePublicationExactBudgetAndOriginalFences(t *testing.T) {
	b, _, _, c, wire := publicationFixture(t, "fuse")
	keys := publicationKeys(t, b, c)
	original, err := b.creationClaimComparisons(c)
	require.NoError(t, err)
	require.Len(t, original, 24)
	bundle, err := b.loadRuntimePreparation(context.Background(), c.workspace, c.reference.IntentID, "")
	require.NoError(t, err)
	var observed bool
	b.client.KV = &publicationCaptureKV{KV: b.client.KV, capture: func(cmps []clientv3.Cmp, ops []clientv3.Op) {
		if !putsKey(keys[4])(ops) {
			return
		}
		observed = true
		business := cmps[len(b.baseComparisons())+4:]
		require.Len(t, business, 44)
		require.Equal(t, original, business[:24])
		require.Len(t, ops, 7)
		mutation := Mutation{Comparisons: business}
		for _, op := range ops[:6] {
			require.True(t, op.IsPut())
			require.Empty(t, op.RangeBytes())
			require.NotEqual(t, bundle.IndexKey, string(op.KeyBytes()))
			mutation.Writes = append(mutation.Writes, Write{Key: string(op.KeyBytes()), Value: op.ValueBytes()})
		}
		require.Equal(t, 64, len(business)+len(mutation.Writes)+14)
		_, _, err := prepareMutation(b.namespace, mutation)
		require.NoError(t, err)
		mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(keys[4]), "=", 0))
		_, _, err = prepareMutation(b.namespace, mutation)
		require.ErrorIs(t, err, ErrInvalidMutation)
	}}
	result, err := b.PublishRuntime(context.Background(), c, wire)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.True(t, observed)
}
func TestRuntimePublicationInvalidInputAndBudgetBeforeGrant(t *testing.T) {
	for _, fault := range []string{"nil ctx", "cancel ctx", "nil claim", "foreign claim", "lost claim", "budget", "unsupported original"} {
		t.Run(fault, func(t *testing.T) {
			b, _, _, c, wire := publicationFixture(t, "plain")
			ctx := context.Background()
			switch fault {
			case "nil ctx":
				ctx = nil
			case "cancel ctx":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			case "nil claim":
				c = nil
			case "foreign claim":
				c.origin = &Backend{}
			case "lost claim":
				c.mu.Lock()
				c.lost = true
				c.mu.Unlock()
			case "budget":
				c.comparisons = append(c.comparisons, clientv3.Compare(clientv3.CreateRevision(c.claimKey), "=", 1))
			case "unsupported original":
				key, _, err := b.namespace.workspaceKeys(c.workspace)
				require.NoError(t, err)
				for i, cmp := range c.comparisons {
					if string(cmp.Key) == key && cmp.Target == clientv3.Compare(clientv3.Value(key), "=", "").Target {
						var owner WorkspaceOwnerRecord
						require.NoError(t, decodePublicationOriginal(c.comparisons, key, &owner))
						value, err := encodeDomainRecord(owner)
						require.NoError(t, err)
						c.comparisons[i] = clientv3.Compare(clientv3.Value(key), "=", `{"future_field":1,`+value[1:])
						break
					}
				}
			}
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.PublishRuntime(ctx, c, wire)
			require.Error(t, err)
			require.Nil(t, result.Entry)
			require.Zero(t, lease.grants.Load())
		})
	}
}
