package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// Inspect the actual protobuf passed to gRPC, after clientv3 builds the Txn.
// A separate pre-Get or a serializable-only/empty Txn cannot pass this check.
func interceptFenceRPC(t *testing.T, b *Backend, guard string, check func(*pb.TxnRequest, *pb.TxnResponse)) *int {
	t.Helper()
	calls := new(int)
	transport, err := clientv3.New(clientv3.Config{Endpoints: b.client.Endpoints(), DialTimeout: 5 * time.Second, DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoke(ctx, method, req, reply, cc, opts...)
		txn, ok := req.(*pb.TxnRequest)
		if !ok {
			return err
		}
		for _, cmp := range txn.Compare {
			if string(cmp.Key) == guard {
				require.NoError(t, err)
				*calls++
				check(txn, reply.(*pb.TxnResponse))
				break
			}
		}
		return err
	})}})
	require.NoError(t, err)
	original := b.client.KV
	b.client.KV = transport.KV
	t.Cleanup(func() { b.client.KV = original; require.NoError(t, transport.Close()) })
	return calls
}

func TestFenceChecksLinearizableAtRPCBoundary(t *testing.T) {
	for _, path := range []string{"renew", "bind replay", "consume replay"} {
		for _, changed := range []bool{false, true} {
			name := path + " success"
			if changed {
				name = path + " changed guard"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var b *Backend
				var raw *clientv3.Client
				var guard, value string
				var leaseID int64
				var action func() error
				comparisons := 28 // four base + all 24 original creation comparisons
				if path == "renew" {
					var in AcquireIntentInput
					b, raw, in, _ = operationControlFixture(t, "plain")
					c := admittedOperation(t, b, raw, in.SandboxID, OperationMutation)
					guard, value, leaseID = c.guardKey, c.value, c.Reference().LeaseID
					action = func() error { return b.RenewOperation(ctx, c) }
					comparisons = 30
				} else {
					var c *CreationClaim
					var wire []byte
					b, raw, _, c, wire, _ = preparationFixture(t, "plain")
					_, err := b.BindRuntime(ctx, c, wire)
					require.NoError(t, err)
					_, err = b.ConsumeRuntimeMount(ctx, c)
					require.NoError(t, err)
					guard, value, leaseID = c.guardKey, c.value, c.Reference().LeaseID
					action = func() error {
						if path == "bind replay" {
							r, err := b.BindRuntime(ctx, c, wire)
							if err == nil {
								require.True(t, r.Replay)
							}
							return err
						}
						r, err := b.ConsumeRuntimeMount(ctx, c)
						if err == nil {
							require.True(t, r.Replay)
						}
						return err
					}
				}
				if changed {
					_, err := raw.Delete(ctx, guard)
					require.NoError(t, err)
					_, err = raw.Put(ctx, guard, value, clientv3.WithLease(clientv3.LeaseID(leaseID)))
					require.NoError(t, err)
				}
				calls := interceptFenceRPC(t, b, guard, func(req *pb.TxnRequest, _ *pb.TxnResponse) {
					require.Len(t, req.Compare, comparisons)
					require.LessOrEqual(t, len(req.Compare)+len(req.Success)+len(req.Failure), 64)
					require.LessOrEqual(t, req.Size(), 256*1024)
					for _, branch := range [][]*pb.RequestOp{req.Success, req.Failure} {
						require.Len(t, branch, 1, "fence Txn itself must carry the linearizable point read")
						r := branch[0].GetRequestRange()
						require.NotNil(t, r, "fence check must remain read-only")
						require.False(t, r.Serializable)
						require.Equal(t, b.identityKey, string(r.Key))
						require.Empty(t, r.RangeEnd)
						require.Zero(t, r.Revision)
					}
				})
				err := action()
				if changed {
					require.ErrorIs(t, err, ErrConflict)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, 1, *calls)
			})
		}
	}
}

func TestFenceCheckRejectsMalformedPointResponse(t *testing.T) {
	for _, path := range []string{"renew", "preparation replay"} {
		for _, defect := range []string{"top revision", "no responses", "nil response", "wrong type", "more", "count", "missing point", "nil point", "key", "future revision", "nested cluster", "nested revision", "identity lease", "identity value"} {
			t.Run(path+"/"+defect, func(t *testing.T) {
				ctx := context.Background()
				var b *Backend
				var guard string
				var action func() error
				var cap *OperationCapability
				if path == "renew" {
					backend, raw, in, _ := operationControlFixture(t, "plain")
					b = backend
					cap = admittedOperation(t, b, raw, in.SandboxID, OperationData)
					guard = cap.guardKey
					action = func() error { return b.RenewOperation(ctx, cap) }
				} else {
					backend, _, _, c, _, _ := preparationFixture(t, "plain")
					b = backend
					guard = c.guardKey
					action = func() error { return b.checkPreparationReplay(ctx, c) }
				}
				lease := &creationFaultLease{Lease: b.client.Lease}
				b.client.Lease = lease
				t.Cleanup(func() { b.client.Lease = lease.Lease })
				calls := interceptFenceRPC(t, b, guard, func(_ *pb.TxnRequest, r *pb.TxnResponse) {
					// On the old implementation even this malformed empty response is accepted.
					if len(r.Responses) == 0 {
						return
					}
					point := r.Responses[0].GetResponseRange()
					switch defect {
					case "top revision":
						r.Header.Revision = 0
					case "no responses":
						r.Responses = nil
					case "nil response":
						r.Responses[0] = nil
					case "wrong type":
						r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{}}}
					case "more":
						point.More = true
					case "count":
						point.Count++
					case "missing point":
						point.Kvs = nil
						point.Count = 0
					case "nil point":
						point.Kvs[0] = nil
					case "key":
						point.Kvs[0].Key = []byte("wrong")
					case "future revision":
						point.Kvs[0].ModRevision = r.Header.Revision + 1
					case "nested cluster":
						point.Header = &pb.ResponseHeader{ClusterId: r.Header.ClusterId + 1, Revision: r.Header.Revision}
					case "nested revision":
						point.Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
					case "identity lease":
						point.Kvs[0].Lease = 123
					case "identity value":
						point.Kvs[0].Value = []byte("changed")
					}
				})
				require.Error(t, action())
				require.Equal(t, 1, *calls)
				require.Zero(t, lease.keeps.Load())
				if cap != nil {
					require.True(t, cap.lost)
					require.Error(t, action())
					require.Equal(t, 1, *calls)
				}
			})
		}
	}
}
