package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var errCleanupProducerBoundary = errors.New("test: acquisition committed; stop before runtime preparation")

// The first ready check and acquisition clock observation remain real. The
// second ready check stops Create after its durable acquisition, before it can
// obtain a creation claim or contact Kubernetes. It never fabricates success.
type cleanupProducerClock struct {
	mu    sync.Mutex
	now   time.Time
	armed bool
	calls int
}

func (c *cleanupProducerClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	if err := ctx.Err(); err != nil {
		return p.ClockObservation{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.armed {
		c.calls++
		if c.calls > 2 {
			return p.ClockObservation{}, errCleanupProducerBoundary
		}
	}
	return p.ClockObservation{UTC: c.now}, nil
}

func (c *cleanupProducerClock) arm() {
	c.mu.Lock()
	c.calls, c.armed = 0, true
	c.mu.Unlock()
}

// This fixture uses the same explicit three-member endpoint contract as the
// state package. Only this unique namespace is provisioned and later deleted;
// application construction never initializes operator metadata or inventory.
func cleanupProducerFixture(t *testing.T) (*PlainColdService, *state.Backend, *clientv3.Client, state.Namespace, *cleanupProducerClock) {
	t.Helper()
	configured := os.Getenv("TEST_ETCD_ENDPOINTS")
	if configured == "" {
		t.Skip("set TEST_ETCD_ENDPOINTS to a real three-member etcd cluster")
	}
	endpoints := strings.Split(configured, ",")
	require.Len(t, endpoints, 3)
	for i := range endpoints {
		endpoints[i] = strings.TrimSpace(endpoints[i])
	}
	raw, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	members, err := raw.MemberList(ctx)
	require.NoError(t, err)
	require.Len(t, members.Members, 3)
	memberIDs := make(map[uint64]bool)
	for _, member := range members.Members {
		memberIDs[member.ID] = true
	}
	observed := make(map[uint64]bool)
	for _, endpoint := range endpoints {
		status, err := raw.Status(ctx, endpoint)
		require.NoError(t, err)
		require.Equal(t, members.Header.ClusterId, status.Header.ClusterId)
		require.True(t, memberIDs[status.Header.MemberId])
		observed[status.Header.MemberId] = true
	}
	require.Len(t, observed, 3, "three addresses must reach three distinct members")
	cell := uuid.NewString()
	ns, err := state.NewNamespace("/codex-test", "cleanup-producer", cell)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := raw.Delete(cleanup, ns.Root(), clientv3.WithPrefix())
		require.NoError(t, err)
	})
	identity := state.Identity{SchemaVersion: 1, Prefix: "/codex-test", AuthorityID: "cleanup-producer", Cell: cell, ClusterID: members.Header.ClusterId, StorageID: "test-storage", RuntimeID: "test-runtime", RestoreEpoch: "restore"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	identityKey, err := ns.Key("meta", "identity")
	require.NoError(t, err)
	restoreKey, err := ns.Key("meta", "restore_epoch")
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(identityKey, string(wire)), clientv3.OpPut(restoreKey, identity.RestoreEpoch)).Commit()
	require.NoError(t, err)

	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	clock := &cleanupProducerClock{now: time.Now().UTC()}
	contract := strings.Repeat("b", 64)
	inventory := &state.PreparationInventoryOptions{}
	inventory.QuotaSlots[7] = 1
	backend, err := state.New(ctx, state.Options{
		Endpoints: endpoints, Namespace: ns, Identity: identity, AllowInsecureLoopback: true,
		RequestTimeout: 5 * time.Second, Clock: clock,
		PublicationTrust:     &state.RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}},
		EnrolledPublication:  &state.EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 1},
		PreparationInventory: inventory,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	ready := false
	for range 8 {
		progress, err := backend.InitializePreparationInventory(ctx)
		require.NoError(t, err)
		if progress.Ready {
			ready = true
			break
		}
	}
	require.True(t, ready, "bounded operator setup must complete the original inventory")
	certificate, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{
		Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer",
		Namespace: ns.Root(), AuthorityID: identity.RuntimeID, Target: "kubernetes", RestoreEpoch: identity.RestoreEpoch,
		PublicKey: issuer.Public().(ed25519.PublicKey), NotBefore: clock.now.Add(-time.Minute), NotAfter: clock.now.Add(time.Hour),
	})
	require.NoError(t, err)
	s, _, _ := coldBusinessFixture(t)
	s.backend, s.clock, s.issuerWire, s.quota = backend, clock, certificate, 7
	s.config.Binding = launcherconfig.Binding{Namespace: ns.Root(), AuthorityID: identity.RuntimeID, Target: "kubernetes", RestoreEpoch: identity.RestoreEpoch}
	s.config.ContractDigest = contract
	require.NoError(t, s.ready(ctx), "production trust and inventory checks must pass before arming the boundary")
	t.Cleanup(func() {
		s.wg.Wait()
		require.Empty(t, s.active, "Create must leave its actual service admission")
	})
	return s, backend, raw, ns, clock
}

func TestPlainColdCleanupSnapshotProducer(t *testing.T) {
	for _, profile := range []string{"local", "remote", "detached business", "legacy retry"} {
		t.Run(profile, func(t *testing.T) {
			s, backend, raw, ns, clock := cleanupProducerFixture(t)
			ctx := context.Background()
			_, _, request := coldBusinessFixture(t)
			provider := "local"
			if profile == "remote" {
				provider = "s3"
			}
			w, err := state.NewWorkspaceIdentity(provider, "test-storage", "bucket", "workspace/")
			require.NoError(t, err)
			var before *clientv3.GetResponse
			if profile == "legacy retry" {
				payload, err := coldRequestCapsule(s.profile, w, request)
				require.NoError(t, err)
				accepted, err := backend.AcquireIntent(ctx, state.AcquireIntentInput{Workspace: w, Principal: request.Principal, IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, IntentID: request.IntentID, SandboxID: request.SandboxID, SnapshotVersion: request.SnapshotVersion, Payload: payload, Now: clock.now, TTL: request.TTL})
				require.NoError(t, err)
				require.Equal(t, state.OutcomeCommitted, accepted.Outcome)
				response, err := raw.Get(ctx, ns.Root(), clientv3.WithPrefix())
				require.NoError(t, err)
				before = response
			}

			clock.arm()
			var result PlainColdResult
			if profile == "detached business" {
				s.business = &PlainColdBusinessOptions{Principal: request.Principal, Defaults: sandboxsnapshot.Defaults{TimeoutSeconds: 60, Resources: types.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}}}
				business, callErr := s.CreateBusiness(ctx, PlainColdBusinessInput{IdempotencyKey: request.IdempotencyKey, Request: types.CreateSandboxRequest{Mode: "ephemeral"}})
				result, err = business.PlainColdResult, callErr
				w, callErr = backend.DetachedIdentity(request.Principal, request.IdempotencyKey)
				require.NoError(t, callErr)
			} else {
				result, err = s.Create(ctx, w, request)
			}
			require.ErrorIs(t, err, errCleanupProducerBoundary, "actual Create must reach the post-acquisition boundary")
			control, err := backend.LoadControl(ctx, w.Partition(), result.SandboxID)
			require.NoError(t, err)
			require.NotNil(t, control)
			snapshot, err := backend.LoadSnapshot(ctx, w.Partition(), result.SandboxID, control.Snapshot)
			require.NoError(t, err)
			require.NotNil(t, snapshot)
			var capsule plainColdCapsule
			require.NoError(t, decodeColdCapsule(snapshot.Payload, &capsule))
			if profile == "local" {
				require.Equal(t, uint32(2), snapshot.Version)
				require.NotNil(t, snapshot.LocalCleanup)
				require.Equal(t, "local", snapshot.LocalCleanup.Provider)
				require.Equal(t, "bucket", snapshot.LocalCleanup.Bucket)
				require.Equal(t, "workspace/", snapshot.LocalCleanup.Prefix)
				require.Equal(t, w.Hash(), snapshot.LocalCleanup.WorkspaceHash)
				require.Equal(t, w.Partition(), snapshot.LocalCleanup.Partition)
				require.Equal(t, []string{}, snapshot.LocalCleanup.Exclude)
				require.True(t, snapshot.LocalCleanup.DeleteExtraneous)
			} else {
				require.Equal(t, uint32(1), snapshot.Version)
				require.Nil(t, snapshot.LocalCleanup, "unsupported and historical profiles must not acquire invented cleanup metadata")
			}
			if profile == "detached business" {
				require.Equal(t, 2, capsule.Version)
				require.NotEmpty(t, capsule.Business)
			} else {
				require.Equal(t, 1, capsule.Version)
				require.Empty(t, capsule.Business)
				want, err := coldRequestCapsule(s.profile, w, request)
				require.NoError(t, err)
				require.Equal(t, []byte(want), []byte(snapshot.Payload), "snapshot envelope upgrade cannot change accepted capsule bytes")
			}
			if profile == "legacy retry" {
				after, err := raw.Get(ctx, ns.Root(), clientv3.WithPrefix())
				require.NoError(t, err)
				require.Equal(t, before.Kvs, after.Kvs, "automatic default capture must replay without migration writes")
			}
		})
	}
}
