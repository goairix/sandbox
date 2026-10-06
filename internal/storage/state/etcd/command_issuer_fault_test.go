package etcd

import (
	"context"
	"crypto/ed25519"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func registryReads(key string) func([]clientv3.Op) bool {
	return func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if op.IsGet() && string(op.KeyBytes()) == key {
				return true
			}
		}
		return false
	}
}

func TestExecIssuerRegistryRejectsResponses(t *testing.T) {
	defects := []string{"nil response", "nil header", "cluster", "revision", "cardinality", "nil op", "wrong kind", "nested cluster", "nested revision", "count", "more", "nil kv", "wrong key", "zero create", "backward mod", "future mod"}
	for _, mode := range []string{"load", "then", "else"} {
		for _, defect := range defects {
			if mode == "then" && (defect == "count" || defect == "more" || defect == "nil kv" || defect == "wrong key" || defect == "zero create" || defect == "backward mod" || defect == "future mod") {
				continue
			}
			t.Run(mode+"/"+defect, func(t *testing.T) {
				f := newRegistryFixture(t)
				b := f.b
				if mode != "then" {
					entry, err := b.RegisterExecIssuer(context.Background())
					require.NoError(t, err)
					require.NotNil(t, entry)
				}
				match := registryReads(f.key)
				if mode != "load" {
					match = putsKey(f.key)
				}
				b.client.KV = &faultKV{KV: b.client.KV, match: match, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					switch defect {
					case "nil response":
						return nil, nil
					case "nil header":
						r.Header = nil
					case "cluster":
						r.Header.ClusterId++
					case "revision":
						r.Header.Revision = 0
					case "cardinality":
						r.Responses = append(r.Responses, nil)
					case "nil op":
						r.Responses[0] = nil
					case "wrong kind":
						r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{}}}
					case "nested cluster", "nested revision":
						h := &pb.ResponseHeader{ClusterId: r.Header.ClusterId, Revision: r.Header.Revision}
						if defect == "nested cluster" {
							h.ClusterId++
						} else {
							h.Revision++
						}
						if mode == "then" {
							r.Responses[0].GetResponsePut().Header = h
						} else {
							r.Responses[2].GetResponseRange().Header = h
						}
					default:
						point := r.Responses[2].GetResponseRange()
						switch defect {
						case "count":
							point.Count++
						case "more":
							point.More = true
						case "nil kv":
							point.Kvs[0] = nil
						case "wrong key":
							point.Kvs[0].Key = []byte(f.key + "wrong")
						case "zero create":
							point.Kvs[0].CreateRevision = 0
						case "backward mod":
							point.Kvs[0].ModRevision = point.Kvs[0].CreateRevision - 1
						case "future mod":
							point.Kvs[0].ModRevision = r.Header.Revision + 1
						}
					}
					return r, nil
				}}
				var entry *CommandIssuerEntry
				var err error
				if mode == "load" {
					entry, err = b.LoadExecIssuer(context.Background(), registryCertificateID)
				} else {
					entry, err = b.RegisterExecIssuer(context.Background())
				}
				require.Nil(t, entry)
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrConflict)
			})
		}
	}
	// Identity/restore point shape is checked even when base comparisons passed.
	for _, index := range []int{0, 1} {
		for _, defect := range []string{"missing", "lease", "value", "key", "count", "more", "nil kv", "nested revision", "revision"} {
			t.Run("metadata/"+string(rune('0'+index))+"/"+defect, func(t *testing.T) {
				f := newRegistryFixture(t)
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: registryReads(f.key), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					p := r.Responses[index].GetResponseRange()
					switch defect {
					case "missing":
						p.Kvs = nil
						p.Count = 0
					case "lease":
						p.Kvs[0].Lease = 42
					case "value":
						p.Kvs[0].Value = []byte("other")
					case "key":
						p.Kvs[0].Key = []byte("other")
					case "count":
						p.Count++
					case "more":
						p.More = true
					case "nil kv":
						p.Kvs[0] = nil
					case "nested revision":
						p.Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
					case "revision":
						p.Kvs[0].ModRevision = r.Header.Revision + 1
					}
					return r, nil
				}}
				entry, err := f.b.LoadExecIssuer(context.Background(), registryCertificateID)
				require.Nil(t, entry)
				require.Error(t, err)
			})
		}
	}
}

func TestExecIssuerRegistryReplyLoss(t *testing.T) {
	for _, mode := range []string{"committed reply lost", "final read lost", "final absent"} {
		t.Run(mode, func(t *testing.T) {
			f := newRegistryFixture(t)
			original := f.b.client.KV
			match := putsKey(f.key)
			if mode == "final read lost" {
				match = registryReads(f.key)
			}
			intercepted := false
			f.b.client.KV = &faultKV{KV: original, match: match, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				intercepted = true
				if mode == "final absent" {
					_, err := f.raw.Delete(context.Background(), f.key)
					require.NoError(t, err)
					return r, nil
				}
				return nil, context.DeadlineExceeded
			}}
			entry, err := f.b.RegisterExecIssuer(context.Background())
			require.Nil(t, entry)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.True(t, intercepted)
			f.b.client.KV = original
			stored := registryRaw(t, f)
			if mode == "final absent" {
				require.Empty(t, stored.Kvs)
				return
			}
			require.Len(t, stored.Kvs, 1)
			kv := stored.Kvs[0]
			loaded, err := f.b.LoadExecIssuer(context.Background(), registryCertificateID)
			require.NoError(t, err)
			require.NotNil(t, loaded)
			require.Equal(t, kv.CreateRevision, loaded.Revision)
			replay, err := f.b.RegisterExecIssuer(context.Background())
			require.NoError(t, err)
			require.Equal(t, loaded, replay)
			require.Equal(t, kv, registryRaw(t, f).Kvs[0], "retry must preserve original immutable body and first revision")
		})
	}
}

type registryResult struct {
	entry *CommandIssuerEntry
	err   error
}

func registryCompetitor(t *testing.T, f *registryFixture) *Backend {
	t.Helper()
	wire := f.sign(t, func(c *controlprotocol.CommandIssuerCertificateClaims) {
		c.PublicKey = ed25519.NewKeyFromSeed([]byte("34567890123456789012345678901234")).Public().(ed25519.PublicKey)
	})
	other := *f.b
	other.execIssuer = &registryProvider{wire: wire}
	other.client = f.raw
	return &other
}
func TestExecIssuerRegistryCompetition(t *testing.T) {
	f := newRegistryFixture(t)
	other := registryCompetitor(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	start := make(chan struct{})
	done := make(chan registryResult, 2)
	finished := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("registry competitors failed to join")
		}
	})
	for _, b := range []*Backend{f.b, other} {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			<-start
			e, err := b.RegisterExecIssuer(ctx)
			done <- registryResult{e, err}
		}(b)
	}
	go func() { wg.Wait(); close(finished) }()
	close(start)
	wins, conflicts := 0, 0
	var winner *CommandIssuerEntry
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			if r.err == nil {
				wins++
				require.NotNil(t, r.entry)
				winner = r.entry
			} else {
				require.Nil(t, r.entry)
				require.ErrorIs(t, r.err, ErrConflict)
				conflicts++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("registry competition timed out")
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 1, conflicts)
	require.Len(t, registryRaw(t, f).Kvs, 1)
	loaded, err := f.b.LoadExecIssuer(context.Background(), registryCertificateID)
	require.NoError(t, err)
	require.Equal(t, winner, loaded)
}

func TestExecIssuerRegistryDelayedCAS(t *testing.T) {
	f := newRegistryFixture(t)
	other := registryCompetitor(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	original := f.b.client.KV
	var hook *faultKV
	hook = &faultKV{KV: original, match: putsKey(f.key), before: func() {
		close(reached)
		<-release
		if os.Getenv("SANDBOX_TEST_ISSUER_FATAL") == "1" {
			require.Same(t, hook, f.b.client.KV, "registry hook restored before worker exit")
			t.Fatal("injected registry worker Fatal")
		}
	}}
	f.b.client.KV = hook
	done := make(chan registryResult, 1)
	finished := make(chan struct{})
	if os.Getenv("SANDBOX_TEST_ISSUER_FATAL") == "1" {
		t.Cleanup(func() {
			select {
			case <-finished:
				t.Log("registry worker joined before fixture cleanup")
			default:
				t.Error("registry worker still running at fixture cleanup")
			}
		})
	}
	t.Cleanup(func() {
		unlock()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("registry worker failed bounded join")
			return
		}
		f.b.client.KV = original
	})
	go func() { defer close(finished); e, err := f.b.RegisterExecIssuer(ctx); done <- registryResult{e, err} }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("registry CAS not intercepted")
	}
	if os.Getenv("SANDBOX_TEST_ISSUER_FATAL") == "1" {
		t.Fatal("injected registry parent Fatal")
	}
	// A true missing snapshot while the complete first CAS has not reached etcd
	// is not a terminal failure; another valid body may win the absent-key CAS.
	absent, err := other.LoadExecIssuer(context.Background(), registryCertificateID)
	require.NoError(t, err)
	require.Nil(t, absent)
	winner, err := other.RegisterExecIssuer(context.Background())
	require.NoError(t, err)
	require.NotNil(t, winner)
	kv := registryRaw(t, f).Kvs[0]
	unlock()
	select {
	case r := <-done:
		require.Nil(t, r.entry)
		require.ErrorIs(t, r.err, ErrConflict)
	case <-time.After(5 * time.Second):
		t.Fatal("delayed registry CAS did not return")
	}
	require.Equal(t, kv, registryRaw(t, f).Kvs[0], "late complete CAS must not overwrite winning certificate")
}
func TestExecIssuerRegistryDelayedCASCleanupAfterFatal(t *testing.T) {
	if os.Getenv("TEST_ETCD_ENDPOINTS") == "" {
		t.Skip("set TEST_ETCD_ENDPOINTS to a real three-member etcd cluster")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestExecIssuerRegistryDelayedCAS$", "-test.v")
	cmd.Env = append(os.Environ(), "SANDBOX_TEST_ISSUER_FATAL=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.NoError(t, ctx.Err(), string(output))
	require.Contains(t, string(output), "injected registry parent Fatal")
	require.Contains(t, string(output), "injected registry worker Fatal")
	require.Contains(t, string(output), "registry worker joined before fixture cleanup")
	require.NotContains(t, string(output), "registry worker still running")
	require.NotContains(t, string(output), "registry hook restored before worker exit")
	require.NotContains(t, string(output), "DATA RACE")
}

// Corruption on CAS Else is rejected before a later read could conceal it.
func TestExecIssuerRegistryRejectsCorruptElse(t *testing.T) {
	f := newRegistryFixture(t)
	entry, err := f.b.RegisterExecIssuer(context.Background())
	require.NoError(t, err)
	require.NotNil(t, entry)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.key), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
		require.NoError(t, e)
		require.False(t, r.Succeeded)
		r.Responses[2].GetResponseRange().Kvs[0].Lease = 42
		return r, nil
	}}
	entry, err = f.b.RegisterExecIssuer(context.Background())
	require.Nil(t, entry)
	require.ErrorIs(t, err, ErrCorruptRecord)
}
