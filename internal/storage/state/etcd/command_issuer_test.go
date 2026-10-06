package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const registryCertificateID = "11234567-89ab-4cde-8012-3456789abcde"

type registryProvider struct {
	wire        []byte
	calls       atomic.Int64
	certificate func(context.Context) ([]byte, error)
}

func (p *registryProvider) Certificate(ctx context.Context) ([]byte, error) {
	p.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded provider")
	}
	if p.certificate != nil {
		return p.certificate(ctx)
	}
	return p.wire, nil
}
func (*registryProvider) SignStart(context.Context, string, controlprotocol.ExecStartTicketClaims) ([]byte, error) {
	panic("registry must never sign start tickets")
}

// Count both streaming and one-shot keepalive calls on the real lease client.
type registryLease struct{ *creationFaultLease }

func (l *registryLease) KeepAlive(ctx context.Context, id clientv3.LeaseID) (<-chan *clientv3.LeaseKeepAliveResponse, error) {
	l.keeps.Add(1)
	return l.Lease.KeepAlive(ctx, id)
}

type registryFixture struct {
	b         *Backend
	raw       *clientv3.Client
	provider  *registryProvider
	root      ed25519.PrivateKey
	claims    controlprotocol.CommandIssuerCertificateClaims
	now       time.Time
	key       string
	canonical []byte
}

func newRegistryFixture(t *testing.T) *registryFixture {
	t.Helper()
	base, raw := integrationBackend(t)
	identity, err := decodeIdentity(base.identityValue, base.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = base.restoreEpoch
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	delegate := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	claims := controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: registryCertificateID, Role: "command_issuer", Namespace: base.namespace.Root(), AuthorityID: identity.RuntimeID, Target: "kubernetes", RestoreEpoch: base.restoreEpoch, PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)}
	wire, err := controlprotocol.SignCommandIssuerCertificate(root, claims)
	require.NoError(t, err)
	provider := &registryProvider{wire: wire}
	var clockCalls atomic.Int64
	clock := testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		clockCalls.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			return ClockObservation{}, errors.New("unbounded clock")
		}
		return ClockObservation{UTC: now}, nil
	})
	b, err := New(context.Background(), Options{Endpoints: raw.Endpoints(), Namespace: base.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, ExecIssuer: provider, Clock: clock, PublicationTrust: &RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Zero(t, provider.calls.Load(), "New must not call Certificate")
	require.Zero(t, clockCalls.Load(), "New must not observe clock")
	return &registryFixture{b: b, raw: raw, provider: provider, root: root, claims: claims, now: now, key: base.namespace.Root() + "command-issuers/" + registryCertificateID, canonical: wire}
}
func (f *registryFixture) sign(t *testing.T, change func(*controlprotocol.CommandIssuerCertificateClaims)) []byte {
	t.Helper()
	c := f.claims
	change(&c)
	wire, err := controlprotocol.SignCommandIssuerCertificate(f.root, c)
	require.NoError(t, err)
	return wire
}
func registryRaw(t *testing.T, f *registryFixture) *clientv3.GetResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := f.raw.Get(ctx, f.key)
	require.NoError(t, err)
	return r
}

// Observe actual server branches, counting only operations executed by etcd.
type registryCountKV struct {
	clientv3.KV
	txns, ranges, puts atomic.Int64
	t                  *testing.T
	b                  *Backend
	key                string
}
type registryCountTxn struct {
	clientv3.Txn
	owner           *registryCountKV
	then, otherwise []clientv3.Op
}

func (kv *registryCountKV) Txn(ctx context.Context) clientv3.Txn {
	kv.txns.Add(1)
	_, ok := ctx.Deadline()
	require.True(kv.t, ok)
	return &registryCountTxn{Txn: kv.KV.Txn(ctx), owner: kv}
}
func (tx *registryCountTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	require.GreaterOrEqual(tx.owner.t, len(c), 4)
	require.Equal(tx.owner.t, tx.owner.b.baseComparisons(), c[:4])
	if len(c) > 4 {
		require.Equal(tx.owner.t, []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(tx.owner.key), "=", 0)}, c[4:])
	}
	tx.Txn = tx.Txn.If(c...)
	return tx
}
func (tx *registryCountTxn) Then(o ...clientv3.Op) clientv3.Txn {
	tx.then = o
	tx.Txn = tx.Txn.Then(o...)
	return tx
}
func (tx *registryCountTxn) Else(o ...clientv3.Op) clientv3.Txn {
	tx.otherwise = o
	tx.Txn = tx.Txn.Else(o...)
	return tx
}
func (tx *registryCountTxn) Commit() (*clientv3.TxnResponse, error) {
	r, e := tx.Txn.Commit()
	if e != nil || r == nil {
		return r, e
	}
	ops := tx.then
	if !r.Succeeded {
		ops = tx.otherwise
	}
	for _, op := range ops {
		if op.IsGet() {
			tx.owner.ranges.Add(1)
			require.Empty(tx.owner.t, op.RangeBytes(), "registry reads must be points")
			require.False(tx.owner.t, op.IsSerializable())
		} else if op.IsPut() {
			tx.owner.puts.Add(1)
		} else {
			tx.owner.t.Error("unexpected registry operation")
		}
	}
	return r, e
}
func (kv *registryCountKV) counts() []int64 {
	return []int64{kv.txns.Load(), kv.ranges.Load(), kv.puts.Load()}
}

func TestExecIssuerRegistry(t *testing.T) {
	f := newRegistryFixture(t)
	b := f.b
	spy := &registryCountKV{KV: b.client.KV, t: t, b: b, key: f.key}
	b.client.KV = spy
	lease := &registryLease{creationFaultLease: &creationFaultLease{Lease: b.client.Lease}}
	b.client.Lease = lease
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, f.canonical, "", "  "))
	f.provider.wire = pretty.Bytes()
	entry, err := b.RegisterExecIssuer(context.Background())
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, []int64{2, 3, 1}, spy.counts())
	require.Equal(t, f.canonical, []byte(entry.Record.Certificate))
	digest := sha256.Sum256(f.canonical)
	require.Equal(t, hex.EncodeToString(digest[:]), entry.Record.CertificateDigest)
	require.Equal(t, b.namespace.Root(), entry.Record.Namespace)
	require.Equal(t, b.restoreEpoch, entry.Record.RestoreEpoch)
	stored := registryRaw(t, f)
	require.Len(t, stored.Kvs, 1)
	kv := stored.Kvs[0]
	require.Positive(t, entry.Revision)
	require.Equal(t, kv.CreateRevision, entry.Revision)
	require.Equal(t, kv.CreateRevision, kv.ModRevision)
	require.Zero(t, kv.Lease)
	require.Equal(t, f.key, string(kv.Key))
	t.Logf("registry cost fresh=2Txn/3point/1Put; certificate=%dB record=%dB firstRevision=%d", len(f.canonical), len(kv.Value), entry.Revision)
	replay, err := b.RegisterExecIssuer(context.Background())
	require.NoError(t, err)
	require.Equal(t, entry, replay)
	require.Equal(t, []int64{4, 9, 1}, spy.counts())
	require.Equal(t, kv, registryRaw(t, f).Kvs[0])
	// The historical reader remains usable with no authority dependencies and after expiry.
	b.execIssuer = nil
	b.execVerifier = nil
	b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		t.Fatal("historical Load consulted clock")
		return ClockObservation{}, nil
	})
	entry.Record.Certificate[0] = '!'
	replay.Record.Certificate[0] = '!'
	f.provider.wire[0] = '!'
	loaded, err := b.LoadExecIssuer(context.Background(), registryCertificateID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, f.canonical, []byte(loaded.Record.Certificate))
	require.Equal(t, []int64{5, 12, 1}, spy.counts())
	loaded.Record.Certificate[0] = '!'
	loaded, err = b.LoadExecIssuer(context.Background(), registryCertificateID)
	require.NoError(t, err)
	require.Equal(t, f.canonical, []byte(loaded.Record.Certificate))
	absent, err := b.LoadExecIssuer(context.Background(), "21234567-89ab-4cde-8012-3456789abcde")
	require.NoError(t, err)
	require.Nil(t, absent)
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
	require.Zero(t, lease.revokes.Load())
	t.Log("registry cost replay=2Txn/6point/0Put; Load=1Txn/3point; Grant/KeepAlive/Revoke=0")
}

func TestExecIssuerRegistryRejects(t *testing.T) {
	for _, defect := range []string{"role", "root", "namespace", "authority", "target", "epoch", "expired", "future", "nil ctx", "canceled ctx", "missing provider", "missing verifier", "missing clock", "provider error", "provider timeout", "oversize", "clock error", "clock uncertainty", "copy before clock"} {
		t.Run(defect, func(t *testing.T) {
			f := newRegistryFixture(t)
			b := f.b
			ctx := context.Background()
			switch defect {
			case "role":
				f.provider.wire = bytes.Replace(f.canonical, []byte("command_issuer"), []byte("runtime_identity"), 1)
			case "root":
				other := ed25519.NewKeyFromSeed([]byte("23456789012345678901234567890123"))
				wire, err := controlprotocol.SignCommandIssuerCertificate(other, f.claims)
				require.NoError(t, err)
				f.provider.wire = wire
			case "namespace":
				f.provider.wire = f.sign(t, func(c *controlprotocol.CommandIssuerCertificateClaims) { c.Namespace = "/other/scope/cell/" })
			case "authority":
				f.provider.wire = f.sign(t, func(c *controlprotocol.CommandIssuerCertificateClaims) { c.AuthorityID = "other" })
			case "target":
				f.provider.wire = f.sign(t, func(c *controlprotocol.CommandIssuerCertificateClaims) { c.Target = "other" })
			case "epoch":
				f.provider.wire = f.sign(t, func(c *controlprotocol.CommandIssuerCertificateClaims) { c.RestoreEpoch = "other" })
			case "expired":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					return ClockObservation{UTC: f.now.Add(2 * time.Minute)}, nil
				})
			case "future":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					return ClockObservation{UTC: f.now.Add(-2 * time.Minute)}, nil
				})
			case "nil ctx":
				ctx = nil
			case "canceled ctx":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing provider":
				b.execIssuer = nil
			case "missing verifier":
				b.execVerifier = nil
			case "missing clock":
				b.authorityClock = nil
			case "provider error":
				f.provider.certificate = func(context.Context) ([]byte, error) { return f.canonical, errors.New("provider unavailable") }
			case "provider timeout":
				b.requestTimeout = 10 * time.Millisecond
				f.provider.certificate = func(ctx context.Context) ([]byte, error) { <-ctx.Done(); return f.canonical, nil }
			case "oversize":
				f.provider.wire = []byte(strings.Repeat(" ", 4097))
			case "clock error":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					return ClockObservation{}, errors.New("clock unavailable")
				})
			case "clock uncertainty":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					return ClockObservation{UTC: f.now, Uncertainty: 2 * time.Second}, nil
				})
			case "copy before clock":
				b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
					f.provider.wire[0] = '!'
					return ClockObservation{UTC: f.now}, nil
				})
			}
			lease := &registryLease{creationFaultLease: &creationFaultLease{Lease: b.client.Lease}}
			b.client.Lease = lease
			spy := &registryCountKV{KV: b.client.KV, t: t, b: b, key: f.key}
			b.client.KV = spy
			entry, err := b.RegisterExecIssuer(ctx)
			if defect == "copy before clock" {
				require.NoError(t, err)
				require.NotNil(t, entry)
			} else {
				require.Error(t, err)
				require.Nil(t, entry)
				require.Empty(t, registryRaw(t, f).Kvs)
				require.Equal(t, []int64{0, 0, 0}, spy.counts(), "preflight rejection must perform no authority RPC or write")
			}
			if strings.HasPrefix(defect, "missing ") {
				require.ErrorIs(t, err, ErrInvalidConfiguration)
			}
			require.Zero(t, lease.grants.Load())
			require.Zero(t, lease.keeps.Load())
			require.Zero(t, lease.revokes.Load())
		})
	}
	for _, defect := range []string{"lease", "mutation", "epoch", "identity", "restore", "namespace", "partial"} {
		t.Run("record "+defect, func(t *testing.T) {
			f := newRegistryFixture(t)
			b := f.b
			entry, err := b.RegisterExecIssuer(context.Background())
			require.NoError(t, err)
			require.NotNil(t, entry)
			value := registryRaw(t, f).Kvs[0].Value
			key := f.key
			opts := []clientv3.OpOption{}
			want := ErrCorruptRecord
			switch defect {
			case "lease":
				l, e := f.raw.Grant(context.Background(), 30)
				require.NoError(t, e)
				opts = append(opts, clientv3.WithLease(l.ID))
				t.Cleanup(func() { _, _ = f.raw.Revoke(context.Background(), l.ID) })
			case "epoch":
				entry.Record.RestoreEpoch = "other"
				v, e := encodeCommandIssuerRecord(entry.Record)
				require.NoError(t, e)
				value = []byte(v)
				want = ErrIdentityMismatch
			case "identity":
				key = b.identityKey
				value = []byte("{}")
				want = ErrIdentityMismatch
			case "restore":
				key = b.restoreKey
				value = []byte("other")
				want = ErrIdentityMismatch
			case "namespace":
				entry.Record.Namespace = "/other/scope/cell/"
				v, e := encodeCommandIssuerRecord(entry.Record)
				require.NoError(t, e)
				value = []byte(v)
			case "partial":
				value = []byte(`{"version":1}`)
			}
			if defect != "mutation" && defect != "identity" && defect != "restore" {
				_, e := f.raw.Delete(context.Background(), key)
				require.NoError(t, e)
			}
			_, err = f.raw.Put(context.Background(), key, string(value), opts...)
			require.NoError(t, err)
			before := registryRaw(t, f)
			loaded, err := b.LoadExecIssuer(context.Background(), registryCertificateID)
			require.Nil(t, loaded)
			require.ErrorIs(t, err, want)
			replay, err := b.RegisterExecIssuer(context.Background())
			require.Nil(t, replay)
			require.ErrorIs(t, err, want)
			require.Equal(t, before.Kvs, registryRaw(t, f).Kvs, "rejection must not rewrite corrupt or foreign records")
		})
	}
	t.Run("historical expiry", func(t *testing.T) {
		f := newRegistryFixture(t)
		entry, err := f.b.RegisterExecIssuer(context.Background())
		require.NoError(t, err)
		require.NotNil(t, entry)
		f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
			return ClockObservation{UTC: f.now.Add(time.Hour)}, nil
		})
		_, err = f.b.RegisterExecIssuer(context.Background())
		require.Error(t, err)
		loaded, err := f.b.LoadExecIssuer(context.Background(), registryCertificateID)
		require.NoError(t, err)
		require.Equal(t, entry, loaded)
	})
	t.Run("invalid load input", func(t *testing.T) {
		f := newRegistryFixture(t)
		for _, id := range []string{"", "../other", "00000000-0000-0000-0000-000000000000", strings.ToUpper(registryCertificateID)} {
			e, err := f.b.LoadExecIssuer(context.Background(), id)
			require.Nil(t, e)
			require.ErrorIs(t, err, ErrInvalidRecord)
		}
		e, err := f.b.LoadExecIssuer(nil, registryCertificateID)
		require.Nil(t, e)
		require.ErrorIs(t, err, ErrInvalidRecord)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e, err = f.b.LoadExecIssuer(ctx, registryCertificateID)
		require.Nil(t, e)
		require.ErrorIs(t, err, context.Canceled)
	})
}
