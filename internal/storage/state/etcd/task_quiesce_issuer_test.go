package etcd

import (
	"context"
	"crypto/ed25519"
	"errors"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

// This recording transport checks the private composition's outgoing RPCs.
// It always aborts before usable registration evidence, with an unsealed model
// tuple; it does not fabricate a public claim, Prepared, or native outcome.
type quiescenceIssuerRecordingKV struct {
	clientv3.KV
	commit func([]clientv3.Cmp, []clientv3.Op, []clientv3.Op) (*clientv3.TxnResponse, error)
}
type quiescenceIssuerRecordingTxn struct {
	clientv3.Txn
	k       *quiescenceIssuerRecordingKV
	c       []clientv3.Cmp
	yes, no []clientv3.Op
}

func (k *quiescenceIssuerRecordingKV) Txn(context.Context) clientv3.Txn {
	return &quiescenceIssuerRecordingTxn{k: k}
}
func (t *quiescenceIssuerRecordingTxn) If(c ...clientv3.Cmp) clientv3.Txn  { t.c = c; return t }
func (t *quiescenceIssuerRecordingTxn) Then(o ...clientv3.Op) clientv3.Txn { t.yes = o; return t }
func (t *quiescenceIssuerRecordingTxn) Else(o ...clientv3.Op) clientv3.Txn { t.no = o; return t }
func (t *quiescenceIssuerRecordingTxn) Commit() (*clientv3.TxnResponse, error) {
	return t.k.commit(t.c, t.yes, t.no)
}
func TestTaskQuiesceMetadataIssuerRPCFences(t *testing.T) {
	for _, phase := range []string{"register", "read"} {
		t.Run(phase, func(t *testing.T) {
			b, c, d := quiescenceBuilderFixture(t)
			now := time.Now().UTC()
			d.deadline = now.Add(20 * time.Second)
			c.deadline = d.deadline
			c.parentCtx = context.Background()
			certificate := append([]byte(nil), d.issuer.Record.Certificate...)
			d.certificate = certificate
			d.issuer = nil
			b.clusterID = 7
			b.identityKey, _ = b.namespace.Key("identity")
			b.identityValue = "value"
			b.restoreKey, _ = b.namespace.Key("restore")
			b.restoreEpoch = c.reference.Task.RestoreEpoch
			b.requestTimeout = time.Second
			root := ed25519.NewKeyFromSeed(make([]byte, 32))
			var e error
			b.taskQuiescenceVerifier, e = p.NewManagementVerifier(p.TrustBinding{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, AuthorityID: d.claims.Context.Current.AuthorityID, Target: d.claims.Context.Current.Target}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, e)
			b.publicationVerifier, e = p.NewPublicationVerifier(p.TrustBinding{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, AuthorityID: d.claims.Context.Current.AuthorityID, Target: d.claims.Context.Current.Target}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, e)
			b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
			sentinel := errors.New("recorded boundary stop")
			writes, reads := 0, 0
			b.client = &clientv3.Client{KV: &quiescenceIssuerRecordingKV{commit: func(cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				if len(yes) == 1 && yes[0].IsPut() {
					writes++
					want := append(b.baseComparisons(), c.comparisons()...)
					want = append(want, clientv3.Compare(clientv3.CreateRevision(string(yes[0].KeyBytes())), "=", 0))
					require.Equal(t, want, cmps)
					require.Equal(t, 49, len(cmps)+len(yes)+len(no))
					if phase == "register" {
						return nil, sentinel
					}
					return &clientv3.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 7, Revision: 5}, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{}}}}}, nil
				}
				if len(yes) == 3 {
					reads++
					require.Equal(t, append(b.baseComparisons(), c.comparisons()...), cmps)
					require.Equal(t, 50, len(cmps)+len(yes)+len(no))
					return nil, sentinel
				}
				require.Len(t, yes, 1)
				require.True(t, yes[0].IsGet())
				return &clientv3.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 7, Revision: 5}, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Count: 1, Kvs: []*mvccpb.KeyValue{{Key: []byte(b.identityKey), Value: []byte(b.identityValue), CreateRevision: 1, ModRevision: 1}}}}}}}, nil
			}}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			e = b.signTaskQuiescence(ctx, c, d)
			require.ErrorIs(t, e, sentinel)
			require.Equal(t, 1, writes)
			if phase == "read" {
				require.Equal(t, 1, reads)
			} else {
				require.Zero(t, reads)
			}
			require.Nil(t, d.issuer)
		})
	}
}

type quiescenceCertificateHook struct {
	*quiescenceProvider
	certificate func(context.Context) ([]byte, error)
}

func (q *quiescenceCertificateHook) Certificate(ctx context.Context) ([]byte, error) {
	return q.certificate(ctx)
}
func TestTaskQuiesceNativeIssuerClaimFences(t *testing.T) {
	for _, when := range []string{"certificate", "read"} {
		for _, loss := range []string{"revoke", "replace"} {
			t.Run(when+"/"+loss, func(t *testing.T) {
				f := newQuiescenceNativeFixture(t)
				originalKV := f.b.client.KV
				defer func() { f.b.client.KV = originalKV }()
				key, e := f.b.namespace.commandIssuerKey(f.issuer.old.identity.CertificateID())
				require.NoError(t, e)
				lose := func() {
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
					if loss == "replace" {
						fresh, e := f.b.ClaimTask(f.ctx, f.c.reference.Task, "issuer-replacement", 30*time.Second)
						require.NoError(t, e)
						t.Cleanup(func() { taskReleaseClaim(t, f.b, fresh) })
					}
				}
				if when == "certificate" {
					f.b.taskQuiescenceIssuer = &quiescenceCertificateHook{quiescenceProvider: f.issuer, certificate: func(ctx context.Context) ([]byte, error) {
						wire, e := f.issuer.Certificate(ctx)
						require.NoError(t, e)
						lose()
						return wire, nil
					}}
				} else {
					f.b.client.KV = &faultKV{KV: originalKV, match: putsKey(key), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
						require.NoError(t, e)
						require.True(t, r.Succeeded)
						lose()
						return r, nil
					}}
				}
				result, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
				require.Error(t, e)
				require.Nil(t, result.Prepared)
				require.Zero(t, f.issuer.calls)
				saved := append([]byte(nil), f.c.quiescenceDraft.certificate...)
				require.NotEmpty(t, saved)
				entry, e := f.raw.Get(f.ctx, key)
				require.NoError(t, e)
				if when == "certificate" {
					require.Empty(t, entry.Kvs)
				} else {
					require.Len(t, entry.Kvs, 1)
				}
				// A later caller cannot replace the pinned certificate after original loss.
				f.issuer.old.wire = nil
				again, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
				require.Error(t, e)
				require.Nil(t, again.Prepared)
				require.Equal(t, saved, f.c.quiescenceDraft.certificate)
			})
		}
	}
}
