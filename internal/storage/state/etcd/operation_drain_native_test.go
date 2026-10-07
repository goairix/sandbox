package etcd

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

func newQuiescenceOperationFixture(t *testing.T, count int) (*taskCloseFixture, []BeginOperationResult) {
	t.Helper()
	// Construct the original published identity with the live target's UUID
	// birth contract; do not rewrite any already-frozen destroy/claim evidence.
	ownedFixtureContainers(t)
	base, raw, input, creation, certificate, publicationRoot := preparationFixture(t, "plain")
	var original struct {
		Claims p.RuntimeCertificateClaims `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(certificate, &original))
	original.Claims.Runtime.BootID = uuid.NewString()
	certificate, e := p.SignRuntimeCertificate(publicationRoot, original.Claims)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	bound, e := base.BindRuntime(ctx, creation, certificate)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mounted, e := base.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	published, e := base.PublishRuntime(ctx, creation, publicationProof(t, base, creation, certificate, nil))
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	controlKey, _, e := base.namespace.sandboxKeys(creation.workspace.Partition(), input.SandboxID, "read")
	require.NoError(t, e)
	var operations []BeginOperationResult
	for i := 0; i < count; i++ {
		op, e := base.BeginOperation(ctx, BeginOperationInput{SandboxID: input.SandboxID, RequestID: fmt.Sprintf("drain-%d", i), Kind: OperationData})
		require.NoError(t, e)
		require.Equal(t, OperationCommitted, op.Outcome)
		require.NotNil(t, op.Capability)
		operations = append(operations, op)
		original := op.Reference.LeaseID
		t.Cleanup(func() {
			bounded, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			_, _ = raw.Revoke(bounded, clientv3.LeaseID(original))
		})
	}
	got, e := raw.Get(ctx, controlKey)
	require.NoError(t, e)
	require.Len(t, got.Kvs, 1)
	in := BeginDestroyInput{SandboxID: input.SandboxID, RequestID: "destroy-request", ExpectedControlRevision: got.Kvs[0].ModRevision}
	if true {
		r, e := raw.Get(ctx, controlKey)
		require.NoError(t, e)
		var c SandboxControlRecord
		require.NoError(t, decodeDomainRecord(r.Kvs[0], &c))
		c.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		v, e := encodeDomainRecord(c)
		require.NoError(t, e)
		put, e := raw.Put(ctx, controlKey, v)
		require.NoError(t, e)
		in.ExpectedControlRevision = put.Header.Revision
	}
	s, ref := taskDestroyPrepare(t, base, ctx, in)
	out, e := base.CommitStage(ctx, s)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	wire, e := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: base.namespace.Root(), AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, RestoreEpoch: base.restoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, e)
	p := &taskCloseProvider{wire: wire, key: key}
	id, e := decodeIdentity(base.identityValue, base.namespace)
	require.NoError(t, e)
	id.RestoreEpoch = base.restoreEpoch
	clockCalls := 0
	b, e := New(ctx, Options{Endpoints: raw.Endpoints(), Namespace: base.namespace, Identity: id, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, TaskIssuer: p, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) { clockCalls++; return ClockObservation{UTC: now}, nil }), PublicationTrust: &RuntimePublicationTrust{AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Zero(t, clockCalls)
	require.Zero(t, p.certCalls)
	require.Zero(t, p.signCalls)
	p.identity, e = b.taskVerifier.VerifyCommandIssuerCertificate(wire, now)
	require.NoError(t, e)
	c, e := b.ClaimTask(ctx, ref, "close-worker", 30*time.Second)
	require.NoError(t, e)
	t.Cleanup(func() { taskReleaseClaim(t, b, c) })
	return &taskCloseFixture{b, raw, c, p, now, ctx}, operations
}

func TestTaskQuiesceNativePages(t *testing.T) {
	t.Run("pagination", func(t *testing.T) {
		base, ops := newQuiescenceOperationFixture(t, 20)
		f := finishQuiescenceFixture(t, base)
		r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
		require.NoError(t, e)
		require.NotNil(t, r.Prepared)
		entry, e := f.b.LoadTaskQuiescence(f.ctx, f.c.reference.Task)
		require.NoError(t, e)
		originalKV := f.b.client.KV
		observed := &quiescenceCaptureKV{KV: originalKV, t: t, b: f.b, c: f.c}
		f.b.client.KV = observed
		defer func() { f.b.client.KV = originalKV }()
		rev, empty, e := f.b.observeTaskOperationsEmpty(f.ctx, f.c, entry)
		require.Equal(t, 1, observed.emptyReads)
		f.b.client.KV = originalKV
		require.NoError(t, e)
		require.False(t, empty)
		require.Positive(t, rev)
		first, e := f.b.ObserveTaskOperationsPage(f.ctx, f.c, nil)
		require.NoError(t, e)
		require.Len(t, first.Entries, 16)
		require.EqualValues(t, 20, first.Count)
		require.True(t, first.More)
		require.NotNil(t, first.Next)
		task, e := taskCloseOriginalTask(f.c)
		require.NoError(t, e)
		for _, op := range first.Entries {
			require.Less(t, op.Record.ControlRevision, task.ControlRevision)
			require.False(t, op.Record.ExpiresAt.Equal(task.ExpiresAt))
		}
		// Only the test's actual original operation owner removes two tokens between
		// pages. The observer never deletes, revokes or constructs an End authority.
		cursor := first.Next
		last := cursor.lastKey
		removed := 0
		for _, op := range ops {
			key, _, _, _, e := f.b.namespace.operationKeys(op.Reference)
			require.NoError(t, e)
			if key > last && removed < 2 {
				_, e = f.raw.Revoke(f.ctx, clientv3.LeaseID(op.Reference.LeaseID))
				require.NoError(t, e)
				removed++
			}
		}
		require.Equal(t, 2, removed)
		clear(first.Entries[0].Key)
		clear(first.Entries[0].Value)
		second, e := f.b.ObserveTaskOperationsPage(f.ctx, f.c, cursor)
		require.NoError(t, e)
		require.Len(t, second.Entries, 2)
		require.False(t, second.More)
		require.Nil(t, second.Next)
		require.GreaterOrEqual(t, second.Revision, first.Revision)
		copied := *cursor
		bad, e := f.b.ObserveTaskOperationsPage(f.ctx, f.c, &copied)
		require.ErrorIs(t, e, ErrInvalidRecord)
		require.Empty(t, bad)
		for _, op := range ops {
			_, _ = f.raw.Revoke(f.ctx, clientv3.LeaseID(op.Reference.LeaseID))
		}
		rev, empty, e = f.b.observeTaskOperationsEmpty(f.ctx, f.c, entry)
		require.NoError(t, e)
		require.True(t, empty)
		require.GreaterOrEqual(t, rev, second.Revision)
		old := f.c
		require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, old))
		fresh, e := f.b.ClaimTask(f.ctx, old.reference.Task, "later-observer", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, f.b, fresh)
		rev, empty, e = f.b.observeTaskOperationsEmpty(f.ctx, fresh, entry)
		require.NoError(t, e)
		require.True(t, empty)
		require.Positive(t, rev)
		rejected, e := f.b.PrepareTaskQuiescence(f.ctx, fresh, f.target.destination)
		require.ErrorIs(t, e, ErrConflict)
		require.Nil(t, rejected.Prepared)
		bad, e = f.b.ObserveTaskOperationsPage(f.ctx, fresh, cursor)
		require.ErrorIs(t, e, ErrInvalidRecord)
		require.Empty(t, bad)
	})
	for _, fault := range []string{"range-count", "cluster", "nested", "order", "lease", "future-control", "same-control-expiry", "after-birth", "restore", "claim-loss", "fence"} {
		t.Run(fault, func(t *testing.T) {
			base, _ := newQuiescenceOperationFixture(t, 2)
			f := finishQuiescenceFixture(t, base)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			prefix, e := f.b.taskOperationsPrefix(f.c.reference.Task)
			require.NoError(t, e)
			task, e := taskCloseOriginalTask(f.c)
			require.NoError(t, e)
			fired := false
			f.b.client.KV = &faultKV{KV: original, match: func(o []clientv3.Op) bool { return len(o) == 1 && o[0].IsGet() && string(o[0].KeyBytes()) == prefix }, before: func() {
				if fault == "restore" {
					_, e := f.raw.Put(f.ctx, f.b.restoreKey, "changed")
					require.NoError(t, e)
					deferRestore(t, f)
				}
				if fault == "claim-loss" {
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
				}
				if fault == "fence" {
					_, e := f.raw.Put(f.ctx, f.c.fences[5].key, f.c.fences[5].value)
					require.NoError(t, e)
				}
			}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				fired = true
				rangeResult := r.Responses[0].GetResponseRange()
				switch fault {
				case "range-count":
					rangeResult.Count++
				case "cluster":
					r.Header.ClusterId++
				case "nested":
					rangeResult.Header = r.Header
					rangeResult.Header.Revision = 0
				case "order":
					rangeResult.Kvs[0], rangeResult.Kvs[1] = rangeResult.Kvs[1], rangeResult.Kvs[0]
				case "lease":
					rangeResult.Kvs[0].Lease++
				case "after-birth":
					rangeResult.Kvs[0].CreateRevision = f.c.birth
					rangeResult.Kvs[0].ModRevision = f.c.birth
				case "future-control", "same-control-expiry":
					var op OperationRecord
					kv := rangeResult.Kvs[0]
					require.NoError(t, json.Unmarshal(kv.Value, &op))
					op.ControlRevision = task.ControlRevision
					if fault == "future-control" {
						op.ControlRevision++
					}
					wire, e := encodeOperationRecord(op)
					require.NoError(t, e)
					kv.Value = []byte(wire)
				}
				return r, nil
			}}
			page, e := f.b.ObserveTaskOperationsPage(f.ctx, f.c, nil)
			require.True(t, fired)
			require.Error(t, e)
			require.Empty(t, page)
		})
	}
}
