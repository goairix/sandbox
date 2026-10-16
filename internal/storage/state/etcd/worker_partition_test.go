package etcd

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestWorkerPartition(t *testing.T) {
	t.Run("competition-incarnation-and-exact-release", func(t *testing.T) {
		b, server := workerTestBackend(t)
		first, err := b.OpenWorkerSession(context.Background(), "same-worker")
		require.NoError(t, err)
		second, err := b.OpenWorkerSession(context.Background(), "same-worker")
		require.NoError(t, err)
		require.NotEqual(t, first.Reference().SessionID, second.Reference().SessionID)
		p, err := b.ClaimWorkerPartition(context.Background(), first, 17)
		require.NoError(t, err)
		again, err := b.ClaimWorkerPartition(context.Background(), first, 17)
		require.NoError(t, err)
		require.Same(t, p, again)
		_, err = b.ClaimWorkerPartition(context.Background(), second, 17)
		require.ErrorIs(t, err, ErrConflict)
		require.NoError(t, b.RenewWorkerSession(context.Background(), second), "a normal occupied partition does not poison the contender")
		copy := new(WorkerPartitionClaim)
		reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(p).Elem())
		require.ErrorIs(t, b.ReleaseWorkerPartition(context.Background(), copy), ErrInvalidRecord)
		original := p.Reference()
		diagnostic := original
		diagnostic.Partition = 99
		diagnostic.CreateRevision++
		require.Equal(t, original, p.Reference())
		require.NoError(t, b.ReleaseWorkerPartition(context.Background(), p))
		_, err = b.ClaimWorkerPartition(context.Background(), first, 17)
		require.Error(t, err, "terminal tombstone cannot become a new original claim")
		successor, err := b.ClaimWorkerPartition(context.Background(), second, 17)
		require.NoError(t, err)
		require.Greater(t, successor.Reference().CreateRevision, original.CreateRevision)
		_ = b.ReleaseWorkerPartition(context.Background(), p)
		key, _ := b.namespace.Key("p", "11", "worker")
		kv := server.point(key)
		require.NotNil(t, kv)
		require.Equal(t, second.Reference().LeaseID, kv.Lease, "an old release cannot remove the successor")
		grants, _, _ := server.counts()
		require.Equal(t, 2, grants, "partition claims share their original session Lease")
	})
	t.Run("all-fixed-partitions-one-Lease", func(t *testing.T) {
		b, server := workerTestBackend(t)
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		for i := 0; i < 256; i++ {
			p, err := b.ClaimWorkerPartition(context.Background(), s, uint8(i))
			require.NoError(t, err)
			require.Equal(t, uint8(i), p.Reference().Partition)
			key, _ := b.namespace.Key("p", fmt.Sprintf("%02x", i), "worker")
			kv := server.point(key)
			require.NotNil(t, kv)
			require.Equal(t, s.Reference().LeaseID, kv.Lease)
			require.Equal(t, int64(1), kv.Version)
		}
		grants, _, _ := server.counts()
		require.Equal(t, 1, grants)
		require.NoError(t, b.ReleaseWorkerSession(context.Background(), s))
		for i := 0; i < 256; i++ {
			key, _ := b.namespace.Key("p", fmt.Sprintf("%02x", i), "worker")
			require.Nil(t, server.point(key))
		}
	})
	t.Run("foreign-original-value-survives-release", func(t *testing.T) {
		b, server := workerTestBackend(t)
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		p, err := b.ClaimWorkerPartition(context.Background(), s, 3)
		require.NoError(t, err)
		key, _ := b.namespace.Key("p", "03", "worker")
		server.put(key, "foreign-successor", 0)
		require.Error(t, b.ReleaseWorkerPartition(context.Background(), p))
		require.Equal(t, "foreign-successor", string(server.point(key).Value))
	})
	t.Run("unknown-claim-ACK-no-retry-or-adoption", func(t *testing.T) {
		b, server := workerTestBackend(t)
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		key, _ := b.namespace.Key("p", "04", "worker")
		server.txnReply = func(request *pb.TxnRequest, reply *pb.TxnResponse) {
			for _, op := range request.Success {
				if put := op.GetRequestPut(); put != nil && string(put.Key) == key {
					reply.Header = nil
				}
			}
		}
		p, err := b.ClaimWorkerPartition(context.Background(), s, 4)
		require.Nil(t, p)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		original := server.point(key)
		require.NotNil(t, original, "the server may have committed despite the unknown ACK")
		server.txnReply = nil
		_, err = b.ClaimWorkerPartition(context.Background(), s, 4)
		require.Error(t, err)
		require.Error(t, b.RenewWorkerSession(context.Background(), s))
		require.Equal(t, original, server.point(key), "no replacement claim may hide the original unknown outcome")
	})
	t.Run("invalid-call-keeps-live-session", func(t *testing.T) {
		b, _ := workerTestBackend(t)
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		_, err = b.ClaimWorkerPartition(nil, s, 0)
		require.ErrorIs(t, err, ErrInvalidRecord)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = b.ClaimWorkerPartition(canceled, s, 0)
		require.ErrorIs(t, err, context.Canceled)
		p, err := b.ClaimWorkerPartition(context.Background(), s, 0)
		require.NoError(t, err)
		require.NoError(t, b.ReleaseWorkerPartition(context.Background(), p))
	})
}
