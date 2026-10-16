package etcd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestWorkerSessionAdmission(t *testing.T) {
	b, server := workerTestBackend(t)
	for _, id := range []string{"", ".", "..", "bad/id", strings.Repeat("x", 129)} {
		_, err := b.OpenWorkerSession(context.Background(), id)
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	_, err := b.OpenWorkerSession(nil, "worker")
	require.ErrorIs(t, err, ErrInvalidRecord)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = b.OpenWorkerSession(canceled, "worker")
	require.ErrorIs(t, err, context.Canceled)
	grants, _, _ := server.counts()
	require.Zero(t, grants)
	var sessions []*WorkerSession
	for i := 0; i < 32; i++ {
		s, err := b.OpenWorkerSession(context.Background(), "same-worker")
		require.NoError(t, err)
		sessions = append(sessions, s)
	}
	_, err = b.OpenWorkerSession(context.Background(), "overflow")
	require.Error(t, err)
	grants, _, _ = server.counts()
	require.Equal(t, 32, grants, "capacity must be reserved before Grant")
	require.NoError(t, b.ReleaseWorkerSession(context.Background(), sessions[0]))
	replacement, err := b.OpenWorkerSession(context.Background(), "same-worker")
	require.NoError(t, err)
	require.NotEqual(t, sessions[0].Reference().SessionID, replacement.Reference().SessionID)
	require.NotEqual(t, sessions[0].Reference().LeaseID, replacement.Reference().LeaseID)
}

func TestWorkerSessionOwnership(t *testing.T) {
	b, server := workerTestBackend(t)
	s, err := b.OpenWorkerSession(context.Background(), "worker")
	require.NoError(t, err)
	original := s.Reference()
	require.Equal(t, b.namespace.Root(), original.Namespace)
	require.Equal(t, b.restoreEpoch, original.RestoreEpoch)
	require.Equal(t, "partition-worker-v1", original.ProfileID)
	require.Len(t, original.IdentityDigest, 64)
	require.Greater(t, original.LeaseID, int64(0))
	require.Greater(t, original.CreateRevision, int64(0))
	diagnostic := original
	diagnostic.WorkerID, diagnostic.SessionID, diagnostic.LeaseID = "changed", "changed", 9
	require.Equal(t, original, s.Reference())
	copy := new(WorkerSession)
	reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(s).Elem())
	for _, bad := range []*WorkerSession{nil, {}, copy} {
		require.ErrorIs(t, b.RenewWorkerSession(context.Background(), bad), ErrInvalidRecord)
		require.ErrorIs(t, b.ReleaseWorkerSession(context.Background(), bad), ErrInvalidRecord)
	}
	foreign := *b
	require.ErrorIs(t, foreign.RenewWorkerSession(context.Background(), s), ErrInvalidRecord)
	_, err = foreign.OpenWorkerSession(context.Background(), "worker")
	require.ErrorIs(t, err, ErrInvalidRecord)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, b.RenewWorkerSession(canceled, s), context.Canceled)
	require.ErrorIs(t, b.RenewWorkerSession(nil, s), ErrInvalidRecord)
	require.NoError(t, b.RenewWorkerSession(context.Background(), s), "pure validation failures must not poison a live session")
	require.Equal(t, original, s.Reference(), "renewal must retain the original Lease, guard and incarnation")
	grants, keeps, _ := server.counts()
	require.Equal(t, 1, grants)
	require.Equal(t, 1, keeps)
	key, _ := b.namespace.Key("workers", "sessions", original.SessionID)
	guard := server.point(key)
	require.Equal(t, original.LeaseID, guard.Lease)
	require.Equal(t, original.CreateRevision, guard.CreateRevision)
	require.Equal(t, guard.CreateRevision, guard.ModRevision)
	require.NoError(t, b.ReleaseWorkerSession(context.Background(), s))
	require.Error(t, b.RenewWorkerSession(context.Background(), s))
	require.Nil(t, server.point(key))
}

func TestWorkerSessionNativeResponses(t *testing.T) {
	for name, change := range map[string]func(*pb.LeaseGrantResponse){
		"header":      func(r *pb.LeaseGrantResponse) { r.Header = nil },
		"cluster":     func(r *pb.LeaseGrantResponse) { r.Header.ClusterId++ },
		"revision":    func(r *pb.LeaseGrantResponse) { r.Header.Revision = 0 },
		"lease":       func(r *pb.LeaseGrantResponse) { r.ID = 0 },
		"ttl-zero":    func(r *pb.LeaseGrantResponse) { r.TTL = 0 },
		"ttl-short":   func(r *pb.LeaseGrantResponse) { r.TTL = 14 },
		"ttl-excess":  func(r *pb.LeaseGrantResponse) { r.TTL = 61 },
		"error-field": func(r *pb.LeaseGrantResponse) { r.Error = "invalid grant" },
	} {
		t.Run("grant/"+name, func(t *testing.T) {
			b, server := workerTestBackend(t)
			server.grantReply = change
			s, err := b.OpenWorkerSession(context.Background(), "worker")
			require.Nil(t, s)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			grants, keeps, _ := server.counts()
			require.Equal(t, 1, grants)
			require.Zero(t, keeps)
		})
	}
	t.Run("native-raised-TTL", func(t *testing.T) {
		server, options := workerTestEndpoint(t)
		options.WorkerControl.SessionTTL = 3 * time.Second
		server.grantReply = func(r *pb.LeaseGrantResponse) { r.TTL = 5; server.leases[r.ID] = 5 }
		b, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		require.NoError(t, b.RenewWorkerSession(context.Background(), s), "actual granted TTL, not requested TTL, bounds renewal")
	})
	for name, change := range map[string]func(*pb.LeaseKeepAliveResponse){
		"cluster":    func(r *pb.LeaseKeepAliveResponse) { r.Header.ClusterId++ },
		"lease":      func(r *pb.LeaseKeepAliveResponse) { r.ID++ },
		"ttl-zero":   func(r *pb.LeaseKeepAliveResponse) { r.TTL = 0 },
		"ttl-excess": func(r *pb.LeaseKeepAliveResponse) { r.TTL = 16 },
	} {
		t.Run("renew/"+name, func(t *testing.T) {
			b, server := workerTestBackend(t)
			s, err := b.OpenWorkerSession(context.Background(), "worker")
			require.NoError(t, err)
			server.keepReply = change
			require.Error(t, b.RenewWorkerSession(context.Background(), s))
			server.keepReply = nil
			require.Error(t, b.RenewWorkerSession(context.Background(), s), "lost original object must never revive")
			_, keeps, _ := server.counts()
			require.Equal(t, 1, keeps)
		})
	}
	t.Run("unknown-original-ACK", func(t *testing.T) {
		b, server := workerTestBackend(t)
		s, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		original := b.client.Lease
		b.client.Lease = &workerTestLease{Lease: original, keep: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
			r, err := original.KeepAliveOnce(ctx, id)
			return r, errors.Join(err, errors.New("delivered ACK lost"))
		}}
		require.ErrorIs(t, b.RenewWorkerSession(context.Background(), s), ErrOutcomeUnknown)
		b.client.Lease = original
		require.Error(t, b.RenewWorkerSession(context.Background(), s))
		grants, keeps, _ := server.counts()
		require.Equal(t, 1, grants)
		require.Equal(t, 1, keeps)
	})
	for _, which := range []string{"guard", "profile", "identity", "restore"} {
		t.Run("lost/"+which, func(t *testing.T) {
			b, server := workerTestBackend(t)
			s, err := b.OpenWorkerSession(context.Background(), "worker")
			require.NoError(t, err)
			key := b.identityKey
			switch which {
			case "guard":
				key, _ = b.namespace.Key("workers", "sessions", s.Reference().SessionID)
			case "profile":
				key, _ = b.namespace.Key("meta", "worker_control_budget")
			case "restore":
				key = b.restoreKey
			}
			old := server.point(key)
			value := string(old.Value)
			if which == "identity" || which == "restore" {
				value += "changed"
			}
			server.put(key, value, old.Lease)
			require.Error(t, b.RenewWorkerSession(context.Background(), s))
			server.put(key, string(old.Value), old.Lease)
			require.Error(t, b.RenewWorkerSession(context.Background(), s))
			_, keeps, _ := server.counts()
			require.Zero(t, keeps, "changed original fences must prevent KeepAlive")
		})
	}
}
