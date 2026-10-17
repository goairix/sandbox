//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestNodeObserverTLS3NativeFixture checks only the shared TLS/native store
// fixture. It does not create a supervisor, an authority carrier or physical
// termination evidence. The explicit selector keeps ordinary host tests inert.
func TestNodeObserverTLS3NativeFixture(t *testing.T) {
	if os.Getenv("SANDBOX_NODE_OBSERVER_TLS3_NATIVE") != "1" {
		t.Skip("requires the explicit owned TLS3 fixture container")
	}
	admin, observerTLS, clusterID := observerNativeEtcdMembers(t, t.TempDir(), 3)
	endpoints := admin.Endpoints()
	require.Len(t, endpoints, 3)
	require.NotZero(t, clusterID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	membership, err := admin.MemberList(ctx)
	require.NoError(t, err)
	require.Len(t, membership.Members, 3)
	memberIDs := make(map[uint64]bool, 3)
	for _, member := range membership.Members {
		require.NotZero(t, member.ID)
		require.False(t, memberIDs[member.ID], "member IDs must identify three actual servers")
		memberIDs[member.ID] = true
		require.Len(t, member.ClientURLs, 1)
		require.Contains(t, endpoints, member.ClientURLs[0])
		require.Len(t, member.PeerURLs, 1)
		require.Contains(t, []string{"https://127.0.0.1:2380", "https://127.0.0.1:2480", "https://127.0.0.1:2580"}, member.PeerURLs[0])
	}

	// Pin each writer and observer reader to exactly one actual endpoint.
	// Dial reuses the original admin TLS credentials with a single-endpoint
	// resolver, so a write cannot silently use a different healthy member.
	writers := make([]clientv3.KV, 0, len(endpoints))
	readers := make([]*clientv3.Client, 0, len(endpoints))
	statusIDs := make(map[uint64]bool, 3)
	for _, endpoint := range endpoints {
		status, err := admin.Status(ctx, endpoint)
		require.NoError(t, err)
		require.Equal(t, clusterID, status.Header.ClusterId)
		require.True(t, memberIDs[status.Header.MemberId])
		require.False(t, statusIDs[status.Header.MemberId], "each endpoint must reach a distinct member")
		statusIDs[status.Header.MemberId] = true
		t.Logf("TLS3 endpoint=%s cluster=%d member=%d leader=%d", endpoint, status.Header.ClusterId, status.Header.MemberId, status.Leader)

		conn, err := admin.Dial(endpoint)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })
		writers = append(writers, clientv3.NewKVFromKVClient(etcdserverpb.NewKVClient(conn), admin))

		reader, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, TLS: observerTLS.Clone(), DialTimeout: 2 * time.Second})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		readers = append(readers, reader)
	}

	// Default Get is linearizable. All three readers must observe each
	// acknowledged endpoint write at or after its actual native revision.
	key := "/sandbox/observer/v1/tls3-native-fixture"
	for writerIndex, writer := range writers {
		sequence := writerIndex + 1
		value := fmt.Sprintf("acknowledged-%d", sequence)
		written, err := writer.Put(ctx, key, value)
		require.NoError(t, err)
		t.Logf("TLS3 writer endpoint=%s sequence=%d revision=%d", endpoints[writerIndex], sequence, written.Header.Revision)
		for i, reader := range readers {
			observed, err := reader.Get(ctx, key)
			require.NoError(t, err)
			require.Len(t, observed.Kvs, 1)
			require.Equal(t, value, string(observed.Kvs[0].Value))
			require.Equal(t, written.Header.Revision, observed.Kvs[0].ModRevision)
			require.GreaterOrEqual(t, observed.Header.Revision, written.Header.Revision)
			require.Equal(t, clusterID, observed.Header.ClusterId)
			t.Logf("TLS3 linearizable endpoint=%s sequence=%d writeRevision=%d readRevision=%d", endpoints[i], sequence, written.Header.Revision, observed.Header.Revision)
		}
	}

	// A successful read must not disguise root credentials: the same concrete
	// observer connections must reject writes and reads outside their namespace.
	for _, reader := range readers {
		_, err := reader.Put(ctx, key, "forbidden")
		require.ErrorIs(t, err, rpctypes.ErrPermissionDenied)
		_, err = reader.Get(ctx, "/sandbox/outside-observer")
		require.ErrorIs(t, err, rpctypes.ErrPermissionDenied)
	}
	unchanged, err := admin.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, unchanged.Kvs, 1)
	require.Equal(t, "acknowledged-3", string(unchanged.Kvs[0].Value))
}
