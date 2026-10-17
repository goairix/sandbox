package provisioning

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
)

func TestPlainColdDestroyAdmissionAndAttribution(t *testing.T) {
	id, requestID := uuid.NewString(), "destroy-original"
	var absent *PlainColdService
	result, err := absent.Destroy(context.Background(), id, requestID)
	require.ErrorIs(t, err, ErrPlainColdProfile)
	require.Equal(t, PlainColdDestroyRejected, result.State)
	require.Equal(t, id, result.SandboxID)
	require.Equal(t, requestID, result.RequestID)
	require.Empty(t, result.Stage)
	require.Empty(t, result.Task)

	s, _, _ := coldBusinessFixture(t)
	s.closed = true
	result, err = s.Destroy(context.Background(), id, requestID)
	require.ErrorIs(t, err, state.ErrConflict)
	require.Equal(t, PlainColdDestroyRejected, result.State)
	require.Equal(t, requestID, result.RequestID)
	require.Empty(t, s.active)
	s.closed = false

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Destroy(ctx, id, requestID)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, s.active)

	call, err := s.enter(context.Background())
	require.NoError(t, err)
	defer s.leave(call)
	_, err = s.Destroy(context.Background(), id, requestID)
	require.ErrorIs(t, err, state.ErrConflict, "DELETE shares the original service MaxActive")
}

func TestPlainColdDestroyInvalidIdentityBeforeBackend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		id        string
		requestID string
	}{
		{"sandbox", "../sandbox", "destroy"},
		{"empty-request", uuid.NewString(), ""},
		{"request-path", uuid.NewString(), "../request"},
		{"request-length", uuid.NewString(), strings.Repeat("r", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := coldBusinessFixture(t)
			result, err := s.Destroy(context.Background(), tc.id, tc.requestID)
			require.Error(t, err)
			require.Equal(t, PlainColdDestroyRejected, result.State)
			require.Empty(t, result.Stage)
			require.Empty(t, result.Task)
			require.Empty(t, s.active, "invalid identity must not reach the nil Backend")
		})
	}
}

// The domain reader already validates the full immutable digest and placement.
// This additional gate selects only schemas with an implemented cleanup
// consumer; legacy/detached data must not create permanently unconsumable Tasks.
func TestPlainColdDestroyCleanupProfile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot *state.SandboxSnapshotRecord
		allowed  bool
	}{
		{"absent", nil, false},
		{"legacy-or-detached", &state.SandboxSnapshotRecord{Version: 1}, false},
		{"legacy-with-descriptor", &state.SandboxSnapshotRecord{Version: 1, LocalCleanup: &state.LocalCleanupDescriptor{Provider: "local"}}, false},
		{"no-descriptor", &state.SandboxSnapshotRecord{Version: 2}, false},
		{"remote", &state.SandboxSnapshotRecord{Version: 2, LocalCleanup: &state.LocalCleanupDescriptor{Provider: "s3"}}, false},
		{"unknown-version", &state.SandboxSnapshotRecord{Version: 3, LocalCleanup: &state.LocalCleanupDescriptor{Provider: "local"}}, false},
		{"validated-local", &state.SandboxSnapshotRecord{Version: 2, LocalCleanup: &state.LocalCleanupDescriptor{Provider: "local"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkColdDestroyProfile(tc.snapshot)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrPlainColdDestroyProfile)
			}
		})
	}
}

// This uses the original concrete Backend RPC path. The endpoint deliberately
// blocks only the first metadata read and never fabricates an active runtime,
// committed destroy, signed receipt or physical cleanup success.
func TestPlainColdDestroyLookupAdmissionAndClose(t *testing.T) {
	ns, err := state.NewNamespace("/sandbox", "destroy-close", "cell")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "destroy-close", Cell: "cell", ClusterID: 1, StorageID: "storage", RuntimeID: "launcher", RestoreEpoch: "restore"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	endpoint := &businessLookupCancellationServer{identity: wire, root: ns.Root(), entered: make(chan struct{}), joined: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc := grpc.NewServer()
	pb.RegisterKVServer(rpc, endpoint)
	pb.RegisterMaintenanceServer(rpc, endpoint)
	served := make(chan error, 1)
	go func() { served <- rpc.Serve(listener) }()
	t.Cleanup(func() {
		rpc.Stop()
		require.NoError(t, <-served)
	})
	backend, err := state.New(context.Background(), state.Options{Endpoints: []string{"http://" + listener.Addr().String()}, Namespace: ns, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })

	api := kube.EnrolledBridgeOptions{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second}
	driver, err := kube.NewEnrolledCreateClient(kube.EnrolledCreateOptions{API: api, Image: "registry.example/sandbox:v1@sha256:" + strings.Repeat("a", 64), NodeName: "node", NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()})
	require.NoError(t, err)
	bridge, err := kube.NewEnrolledBridgeClient(api)
	require.NoError(t, err)
	s := &PlainColdService{backend: backend, driver: driver, bridge: bridge, clock: &cleanupProducerClock{now: time.Now().UTC()}, limit: 1, active: make(map[*plainColdCall]struct{}), closeDone: make(chan struct{})}
	s.self = s
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	type answer struct {
		result PlainColdDestroyResult
		err    error
	}
	done := make(chan answer, 1)
	id, requestID := uuid.NewString(), "destroy-original"
	go func() {
		result, callErr := s.Destroy(context.Background(), id, requestID)
		done <- answer{result, callErr}
	}()
	select {
	case <-endpoint.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("DELETE did not reach original Backend metadata read")
	}
	s.mu.Lock()
	active := len(s.active)
	s.mu.Unlock()
	require.Equal(t, 1, active)
	_, err = s.Destroy(context.Background(), id, requestID)
	require.ErrorIs(t, err, state.ErrConflict)
	require.NoError(t, s.Close())
	result := <-done
	require.Error(t, result.err)
	require.NotEqual(t, PlainColdDestroyPending, result.result.State)
	require.Equal(t, requestID, result.result.RequestID)
	require.Empty(t, result.result.Task)
	require.Empty(t, result.result.Stage)
	require.Empty(t, s.active)
	select {
	case <-endpoint.joined:
	case <-time.After(time.Second):
		t.Fatal("original cancelled Backend handler did not join")
	}
}
