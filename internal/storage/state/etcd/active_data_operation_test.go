package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func sharedDataFixture(t *testing.T) (*Store, *ActiveSandboxRepository, *state.ActiveDataView) {
	t.Helper()
	s := testStore(t)
	r, err := NewActiveSandboxRepository(s, "shared-data")
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, r.Publish(ctx, validActiveRecord("shared-sandbox")))
	_, err = r.Activate(ctx, "shared-sandbox", 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	view, err := r.OpenDataOperation(ctx, "shared-sandbox", "shared-token", 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, view)
	return s, r, view
}

type dataCountKV struct {
	clientv3.KV
	reads, txns int
}

func (c *dataCountKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	c.reads++
	return c.KV.Get(ctx, key, opts...)
}
func (c *dataCountKV) Txn(ctx context.Context) clientv3.Txn { c.txns++; return c.KV.Txn(ctx) }

func TestSharedDataReadUsesOneSnapshot(t *testing.T) {
	s, r, view := sharedDataFixture(t)
	ctx := context.Background()
	before, err := s.client.Get(ctx, r.operationKey("shared-sandbox", "shared-token"))
	require.NoError(t, err)
	realKV := s.client.KV
	counted := &dataCountKV{KV: realKV}
	s.client.KV = counted
	got, err := r.ReadDataOperation(ctx, view.Lease, true)
	s.client.KV = realKV
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Record.Generation)
	require.Equal(t, 1, counted.txns)
	require.Zero(t, counted.reads)
	after, err := s.client.Get(ctx, r.operationKey("shared-sandbox", "shared-token"))
	require.NoError(t, err)
	require.Equal(t, before.Header.Revision, after.Header.Revision, "read cannot write any keys")
}

func TestSharedDataLeaseKeepAlivePreservesKey(t *testing.T) {
	s, r, view := sharedDataFixture(t)
	ctx := context.Background()
	before, err := s.client.Get(ctx, r.operationKey("shared-sandbox", "shared-token"))
	require.NoError(t, err)
	renewed, ttl, err := r.KeepAliveDataOperation(ctx, view.Lease)
	require.NoError(t, err)
	require.Greater(t, ttl, time.Duration(0))
	require.Equal(t, view.Lease.LeaseID, renewed.Lease.LeaseID)
	after, err := s.client.Get(ctx, r.operationKey("shared-sandbox", "shared-token"))
	require.NoError(t, err)
	require.Equal(t, before.Kvs[0].ModRevision, after.Kvs[0].ModRevision)
	require.Equal(t, before.Kvs[0].Lease, after.Kvs[0].Lease)
	require.Equal(t, before.Kvs[0].Value, after.Kvs[0].Value)
}

func TestSharedDataKeepAliveRejectsMissingLease(t *testing.T) {
	s, r, view := sharedDataFixture(t)
	_, err := s.client.Revoke(context.Background(), clientv3.LeaseID(view.Lease.LeaseID))
	require.NoError(t, err)
	_, _, err = r.KeepAliveDataOperation(context.Background(), view.Lease)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	live, err := r.LiveOperations(context.Background(), "shared-sandbox")
	require.NoError(t, err)
	require.Zero(t, live)
}

type unknownDataLease struct {
	clientv3.Lease
	grants, revokes int
}

func (l *unknownDataLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	_, err := l.Lease.KeepAliveOnce(ctx, id)
	if err != nil {
		return nil, err
	}
	return nil, errors.New("injected lost keepalive reply")
}
func (l *unknownDataLease) Grant(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
	l.grants++
	return l.Lease.Grant(ctx, ttl)
}
func (l *unknownDataLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	l.revokes++
	return l.Lease.Revoke(ctx, id)
}
func TestSharedDataKeepAliveUnknownReply(t *testing.T) {
	s, r, view := sharedDataFixture(t)
	original := s.client.Lease
	fault := &unknownDataLease{Lease: original}
	s.client.Lease = fault
	_, ttl, err := r.KeepAliveDataOperation(context.Background(), view.Lease)
	s.client.Lease = original
	require.ErrorContains(t, err, "lost keepalive reply")
	require.Zero(t, ttl)
	require.Zero(t, fault.grants)
	require.Zero(t, fault.revokes)
	live, err := r.LiveOperations(context.Background(), "shared-sandbox")
	require.NoError(t, err)
	require.EqualValues(t, 1, live)
}

func TestSharedDataExpiredAdvisoryTimestamp(t *testing.T) {
	s, r, view := sharedDataFixture(t)
	op := view.Lease.Operation
	op.ExpiresAt = time.Now().Add(-time.Hour)
	raw, err := json.Marshal(op)
	require.NoError(t, err)
	_, err = s.client.Put(context.Background(), r.operationKey(op.SandboxID, op.Token), string(raw), clientv3.WithLease(clientv3.LeaseID(view.Lease.LeaseID)))
	require.NoError(t, err)
	_, err = r.ReadDataOperation(context.Background(), view.Lease, true)
	require.NoError(t, err)
	require.NoError(t, r.EndOperation(context.Background(), view.Lease.Operation))
}

func TestSharedDataReadRejectsCorruptIdentity(t *testing.T) {
	for _, field := range []string{"token", "kind", "generation", "lease"} {
		t.Run(field, func(t *testing.T) {
			_, r, view := sharedDataFixture(t)
			lease := view.Lease
			switch field {
			case "token":
				lease.Operation.Token = "other"
			case "kind":
				lease.Operation.Kind = state.ActiveOperationMutation
			case "generation":
				lease.Operation.Generation++
			case "lease":
				lease.LeaseID++
			}
			_, err := r.ReadDataOperation(context.Background(), lease, true)
			require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
		})
	}
}

func TestSharedDataDrainKeepsLeaseButClosesAdmission(t *testing.T) {
	_, r, view := sharedDataFixture(t)
	_, _, _, err := r.BeginDestroy(context.Background(), "shared-sandbox")
	require.NoError(t, err)
	_, err = r.ReadDataOperation(context.Background(), view.Lease, true)
	require.ErrorIs(t, err, state.ErrActiveSandboxAdmissionClosed)
	got, ttl, err := r.KeepAliveDataOperation(context.Background(), view.Lease)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxDestroying, got.Record.Phase)
	require.Positive(t, ttl)
}
