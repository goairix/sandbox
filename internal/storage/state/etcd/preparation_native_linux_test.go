//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func coldPreparationNativeBackends(t *testing.T) ([2]*Backend, *clientv3.Client) {
	t.Helper()
	if os.Getenv("SANDBOX_COLD_CREATE_NATIVE") != "1" {
		t.Skip("requires owned cold-create TLS etcd runner")
	}
	dir := os.Getenv("COLD_CREATE_TLS_DIRECTORY")
	require.NotEmpty(t, dir)
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(ca))
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	ctx := context.Background()
	status, err := raw.Status(ctx, raw.Endpoints()[0])
	require.NoError(t, err)
	n, err := NewNamespace("/sandbox/cold-create-native", "state", uuid.NewString())
	require.NoError(t, err)
	identity := Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: status.Header.ClusterId, StorageID: "test-storage", RuntimeID: "test-runtime", RestoreEpoch: "restore"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	ik, _ := n.Key("meta", "identity")
	rk, _ := n.Key("meta", "restore_epoch")
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(ik, string(wire)), clientv3.OpPut(rk, identity.RestoreEpoch)).Commit()
	require.NoError(t, err)
	profile := &PreparationInventoryOptions{}
	profile.QuotaSlots[7], profile.QuotaSlots[8] = 40, 40
	options := Options{Endpoints: raw.Endpoints(), Namespace: n, Identity: identity, TLS: tlsConfig, RequestTimeout: time.Second, PreparationInventory: profile}
	var backends [2]*Backend
	for i := range backends {
		backends[i], err = New(ctx, options)
		require.NoError(t, err)
		b := backends[i]
		t.Cleanup(func() { require.NoError(t, b.Close()) })
	}
	// Callers cannot shrink the constructor's captured inventory after New.
	profile.QuotaSlots[7], profile.QuotaSlots[8] = 1, 1
	return backends, raw
}

func TestColdPreparationReservationNative(t *testing.T) {
	backends, raw := coldPreparationNativeBackends(t)
	ctx := context.Background()
	b := backends[0]
	_, err := b.ReadPreparationInventory(ctx)
	require.ErrorIs(t, err, ErrPreparationNotReady)
	traces := [2]*coldTraceKV{}
	for i, backend := range backends {
		traces[i] = &coldTraceKV{KV: backend.client.KV}
		backend.client.KV = traces[i]
	}
	for step := 0; step < 20; step++ {
		progress, e := backends[step%2].InitializePreparationInventory(ctx)
		require.NoError(t, e)
		if progress.Ready {
			break
		}
	}
	progress, err := b.ReadPreparationInventory(ctx)
	require.NoError(t, err)
	require.True(t, progress.Ready)
	require.Equal(t, uint64(112), progress.Total)
	require.Equal(t, int32(1), traces[0].nodes[10].Load()+traces[1].nodes[10].Load(), "initial cursor literal native transaction")
	require.Equal(t, int32(14), traces[0].nodes[44].Load()+traces[1].nodes[44].Load(), "eight-slot batch literal native transactions")
	require.Equal(t, int32(1), traces[0].nodes[13].Load()+traces[1].nodes[13].Load(), "ready barrier literal native transaction")
	var firstInput AcquireIntentInput
	var first ColdPreparationRecord
	for i := 0; i < 33; i++ {
		worker := backends[i%2]
		input := acquisitionInput(t, fmt.Sprintf("cold-%d", i))
		input.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("cold/%d/", i))
		require.NoError(t, err)
		acquired, e := worker.AcquireIntent(ctx, input)
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, acquired.Outcome)
		claim, e := worker.ClaimCreation(ctx, input.Workspace, input.IntentID, "cold-worker", 30*time.Second)
		require.NoError(t, e)
		result, e := worker.ReserveColdPreparation(ctx, claim, uint8(7+i%2))
		if i == 32 {
			require.ErrorIs(t, e, ErrPreparationCapacity, "preparation budget is cell-wide across quota/workspace partitions")
			require.Nil(t, result.Record)
		} else {
			require.NoError(t, e)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			require.NotNil(t, result.Record)
			if i == 0 {
				firstInput, first = input, *result.Record
			}
			again, e := worker.ReserveColdPreparation(ctx, claim, uint8(7+i%2))
			require.NoError(t, e)
			require.True(t, again.Replay)
			require.Equal(t, result.Record, again.Record)
		}
		require.NoError(t, worker.ReleaseCreationClaim(ctx, claim))
	}
	loaded, err := backends[1].LoadColdPreparation(ctx, firstInput.Workspace, firstInput.IntentID)
	require.NoError(t, err)
	require.Equal(t, &first, loaded, "claim release never refunds durable quota/preparation")
	freeKey, _ := b.namespace.Key("preparation", "free")
	free, err := raw.Get(ctx, freeKey+"/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, free.Kvs)
	for _, ref := range []ColdSlotReference{first.Quota, first.Preparation} {
		key, free, e := b.namespace.coldSlotKeys(ref)
		require.NoError(t, e)
		values, e := b.readDomain(ctx, key, free)
		require.NoError(t, e)
		require.NotNil(t, values[0])
		require.Zero(t, values[0].Lease)
		require.Nil(t, values[1])
	}
}

type coldTraceKV struct {
	clientv3.KV
	before       func()
	drop         atomic.Bool
	reservations atomic.Int32
	maximum      atomic.Int32
	nodes        [65]atomic.Int32
}

type coldTraceTxn struct {
	clientv3.Txn
	owner            *coldTraceKV
	compares         int
	success, failure []clientv3.Op
}

func (k *coldTraceKV) Txn(ctx context.Context) clientv3.Txn {
	return &coldTraceTxn{Txn: k.KV.Txn(ctx), owner: k}
}
func (x *coldTraceTxn) If(cmps ...clientv3.Cmp) clientv3.Txn {
	x.compares = len(cmps)
	x.Txn = x.Txn.If(cmps...)
	return x
}
func (x *coldTraceTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	x.success = ops
	x.Txn = x.Txn.Then(ops...)
	return x
}
func (x *coldTraceTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	x.failure = ops
	x.Txn = x.Txn.Else(ops...)
	return x
}
func (x *coldTraceTxn) Commit() (*clientv3.TxnResponse, error) {
	nodes := int32(x.compares + len(x.success) + len(x.failure))
	if nodes >= 0 && nodes < int32(len(x.owner.nodes)) {
		x.owner.nodes[nodes].Add(1)
	}
	for current := x.owner.maximum.Load(); nodes > current; current = x.owner.maximum.Load() {
		if x.owner.maximum.CompareAndSwap(current, nodes) {
			break
		}
	}
	reserve := x.compares == 45 && len(x.success) == 6
	if reserve && x.owner.reservations.Add(1) == 1 && x.owner.before != nil {
		x.owner.before()
	}
	r, err := x.Txn.Commit()
	if reserve && err == nil && r.Succeeded && x.owner.drop.CompareAndSwap(true, false) {
		return nil, ErrOutcomeUnknown
	}
	return r, err
}

func coldNativeClaim(t *testing.T, b *Backend, suffix string) (AcquireIntentInput, *CreationClaim) {
	t.Helper()
	input := acquisitionInput(t, suffix)
	var err error
	input.Workspace, err = NewWorkspaceIdentity("s3", "test-storage", "bucket", "cold-claim/"+suffix+"/")
	require.NoError(t, err)
	acquired, err := b.AcquireIntent(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	claim, err := b.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "cold-worker", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseCreationClaim(context.Background(), claim) })
	return input, claim
}

func coldNativeInitialize(t *testing.T, b *Backend) {
	t.Helper()
	for i := 0; i < 20; i++ {
		r, err := b.InitializePreparationInventory(context.Background())
		require.NoError(t, err)
		if r.Ready {
			return
		}
	}
	t.Fatal("bounded operator initialization never reached ready")
}

func TestColdPreparationReservationFaultNative(t *testing.T) {
	t.Run("cross-cell-ready-rejected", func(t *testing.T) {
		first, raw := coldPreparationNativeBackends(t)
		second, _ := coldPreparationNativeBackends(t)
		coldNativeInitialize(t, first[0])
		origin, _ := first[0].namespace.Key("preparation", "inventory", "ready")
		target, _ := second[0].namespace.Key("preparation", "inventory", "ready")
		value, err := raw.Get(context.Background(), origin)
		require.NoError(t, err)
		require.Len(t, value.Kvs, 1)
		_, err = raw.Put(context.Background(), target, string(value.Kvs[0].Value))
		require.NoError(t, err)
		_, err = second[0].ReadPreparationInventory(context.Background())
		require.ErrorIs(t, err, ErrCorruptRecord, "a valid ready record from another cell is not local inventory")
	})
	t.Run("concurrent-candidate", func(t *testing.T) {
		backends, _ := coldPreparationNativeBackends(t)
		coldNativeInitialize(t, backends[0])
		arrived, release := make(chan struct{}, 2), make(chan struct{})
		traces := [2]*coldTraceKV{}
		claims := [2]*CreationClaim{}
		for i, b := range backends {
			_, claims[i] = coldNativeClaim(t, b, fmt.Sprintf("parallel-%d", i))
			traces[i] = &coldTraceKV{KV: b.client.KV, before: func() { arrived <- struct{}{}; <-release }}
			b.client.KV = traces[i]
		}
		type answer struct {
			result ColdPreparationResult
			err    error
		}
		done := make(chan answer, 2)
		for i := range backends {
			go func(i int) {
				r, e := backends[i].ReserveColdPreparation(context.Background(), claims[i], 7)
				done <- answer{r, e}
			}(i)
		}
		for i := 0; i < 2; i++ {
			<-arrived
		}
		close(release)
		first, second := <-done, <-done
		require.NoError(t, first.err)
		require.NoError(t, second.err)
		require.NotEqual(t, first.result.Record.Quota, second.result.Record.Quota)
		require.NotEqual(t, first.result.Record.Preparation, second.result.Record.Preparation)
		require.Equal(t, int32(3), traces[0].reservations.Load()+traces[1].reservations.Load(), "same original candidate forces one real CAS conflict")
		for _, trace := range traces {
			require.Equal(t, int32(54), trace.maximum.Load())
		}
	})
	t.Run("lost-ack-and-ready-fault", func(t *testing.T) {
		backends, raw := coldPreparationNativeBackends(t)
		b := backends[0]
		input, claim := coldNativeClaim(t, b, "lost-ack")
		_, err := b.ReserveColdPreparation(context.Background(), claim, 7)
		require.ErrorIs(t, err, ErrPreparationNotReady)
		coldNativeInitialize(t, b)
		trace := &coldTraceKV{KV: b.client.KV}
		trace.drop.Store(true)
		b.client.KV = trace
		unknown, err := b.ReserveColdPreparation(context.Background(), claim, 7)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, unknown.Outcome)
		require.NoError(t, b.ReleaseCreationClaim(context.Background(), claim))
		recovered, err := backends[1].LoadColdPreparation(context.Background(), input.Workspace, input.IntentID)
		require.NoError(t, err)
		require.NotNil(t, recovered)
		next, err := backends[1].ClaimCreation(context.Background(), input.Workspace, input.IntentID, "recovery", 30*time.Second)
		require.NoError(t, err)
		defer backends[1].ReleaseCreationClaim(context.Background(), next)
		replay, err := backends[1].ReserveColdPreparation(context.Background(), next, 7)
		require.NoError(t, err)
		require.True(t, replay.Replay)
		require.Equal(t, recovered, replay.Record)
		require.Equal(t, int32(1), trace.reservations.Load())
		ready, _ := b.namespace.Key("preparation", "inventory", "ready")
		configuredDigest := b.preparationDigest
		b.preparationDigest = "different-deployment-profile"
		_, err = b.ReadPreparationInventory(context.Background())
		require.ErrorIs(t, err, ErrCorruptRecord, "ready must match this deployment configuration")
		b.preparationDigest = configuredDigest
		_, err = raw.Delete(context.Background(), ready)
		require.NoError(t, err)
		_, err = b.InitializePreparationInventory(context.Background())
		require.ErrorIs(t, err, ErrCorruptRecord, "operator never resets a previously ready inventory")
		_, other := coldNativeClaim(t, b, "not-ready")
		_, err = b.ReserveColdPreparation(context.Background(), other, 8)
		require.ErrorIs(t, err, ErrPreparationNotReady)
	})
}
