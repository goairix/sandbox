package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
)

func detachedCleanupOptionsForTest(t *testing.T) (*workerTestServer, Options) {
	t.Helper()
	server, options := workerTestEndpoint(t)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	options.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: time.Now().UTC()}, nil
	})
	contract, err := kubecontract.EnrolledPlainContractDigest(detachedCleanupPlainImageForTest)
	require.NoError(t, err)
	options.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 4}
	options.DetachedCleanup = &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}
	return server, options
}

func TestDetachedCleanupOptionsRejectBeforeFirstRPC(t *testing.T) {
	model, options := detachedCleanupOptionsForTest(t)
	var calls atomic.Int64
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		calls.Add(1)
		return handler(ctx, request)
	}))
	pb.RegisterMaintenanceServer(server, model)
	pb.RegisterKVServer(server, model)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		<-done
	})
	options.Endpoints = []string{"http://" + listener.Addr().String()}

	for _, test := range []struct {
		name   string
		image  string
		change func(*Options)
	}{
		{name: "empty"},
		{name: "tag only", image: "registry.example/sandbox:1.0.0"},
		{name: "mutable tag", image: "registry.example/sandbox:latest"},
		{name: "over 4096 bytes", image: strings.Repeat("x", 4097)},
		{name: "foreign contract", image: detachedCleanupPlainImageForTest, change: func(o *Options) {
			o.EnrolledPublication.ContractDigest = strings.Repeat("f", 64)
		}},
		{name: "missing enrolled profile", image: detachedCleanupPlainImageForTest, change: func(o *Options) {
			o.EnrolledPublication = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			owned := options
			profile := *options.EnrolledPublication
			owned.EnrolledPublication = &profile
			owned.DetachedCleanup = &DetachedCleanupOptions{PinnedPlainImage: test.image}
			if test.change != nil {
				test.change(&owned)
			}
			b, err := New(context.Background(), owned)
			if b != nil {
				t.Cleanup(func() { require.NoError(t, b.Close()) })
			}
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.Nil(t, b)
			require.Zero(t, calls.Load(), "invalid source configuration must fail before endpoint Status or any other RPC")
		})
	}

	// Failure priority must also hold when no server can answer a first RPC.
	options.Endpoints = []string{"http://127.0.0.1:1"}
	options.RequestTimeout = 10 * time.Millisecond
	options.DetachedCleanup = &DetachedCleanupOptions{}
	b, err := New(context.Background(), options)
	require.Nil(t, b)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}

func TestDetachedCleanupOptionsKeepDefaultDisabled(t *testing.T) {
	_, options := workerTestEndpoint(t)
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Nil(t, b.detachedCleanup)

	f := newDetachedCleanupSourceFixture(t, "detached", nil)
	_, err = f.b.detachedCleanupSource(f.task, f.snapshot, f.intent, &f.publication)
	require.ErrorIs(t, err, ErrInvalidConfiguration, "real signed plain history alone does not enable detached classification")
}

func TestDetachedCleanupOptionsOwnBothCallerProfiles(t *testing.T) {
	_, options := detachedCleanupOptionsForTest(t)
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	contract := options.EnrolledPublication.ContractDigest
	options.DetachedCleanup.PinnedPlainImage = "changed"
	options.EnrolledPublication.ContractDigest = strings.Repeat("f", 64)
	require.Equal(t, contract, b.enrolledPublication.ContractDigest)
	require.NotNil(t, b.detachedCleanup)
	require.Equal(t, contract, b.detachedCleanup.contractDigest)
	require.Equal(t, detachedCleanupPlainImageForTest, b.detachedCleanup.pinnedPlainImage)
	require.Nil(t, b.localCleanup, "source opt-in must not construct an execution consumer")
}
