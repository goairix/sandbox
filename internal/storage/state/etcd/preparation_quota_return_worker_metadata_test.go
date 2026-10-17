package etcd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// These tests exercise ordinary point metadata, never decoded quota authority.
// Session, partition, owner and Range share are original backend capabilities;
// the host listener measures actual native gRPC requests, not etcd semantics.
func coldQuotaMetadataPoints(t *testing.T, f *coldQuotaAdmissionFixture, caps []int) []string {
	t.Helper()
	keys := make([]string, len(caps))
	for i, cap := range caps {
		key, err := f.b.namespace.Key("metadata-test", fmt.Sprintf("point-%02d", i))
		require.NoError(t, err)
		keys[i] = key
		f.server.put(key, strings.Repeat("h", cap), 0)
	}
	return keys
}

func coldQuotaMetadataRead(f *coldQuotaAdmissionFixture, keys []string, caps []int) ([]*mvccpb.KeyValue, error) {
	return f.b.readColdQuotaWorkerPoints(f.owner, f.session, f.part, f.ranges, keys, caps)
}

func coldQuotaMetadataRequests(f *coldQuotaAdmissionFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

func coldQuotaMetadataResetRequests(f *coldQuotaAdmissionFixture) {
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
}

type coldQuotaMetadataSnapshot struct {
	ranges, claims workerBucket
	operation      *workerCall
	lost           bool
	registered     bool
}

// Copy lifecycle state under its original lock; assertions run after unlocking
// so a failing assertion cannot strand backend cleanup behind the same mutex.
func coldQuotaMetadataState(f *coldQuotaAdmissionFixture) coldQuotaMetadataSnapshot {
	f.b.archiveLife.mu.Lock()
	defer f.b.archiveLife.mu.Unlock()
	_, registered := f.b.archiveLife.enrolled[f.owner]
	return coldQuotaMetadataSnapshot{ranges: f.ranges.bucket, claims: f.claims.bucket, operation: f.session.operation, lost: f.session.lost, registered: registered}
}

// Omitting any original fence, charging Claim, adding pre/postguard RPCs or
// retaining the old 16 KiB receive cap breaks this actual read-boundary test.
func TestColdQuotaWorkerMetadataPaidW20AndLargeHistory(t *testing.T) {
	for _, caps := range [][]int{
		{65536, 65536, 65536},
		{8192, 16384, 8192, 8192, 4096, 4096, 65536, 4096, 4096, 8192, 4096, 4096, 4096, 4096},
	} {
		t.Run(fmt.Sprintf("points-%d", len(caps)), func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			keys := coldQuotaMetadataPoints(t, f, caps)
			coldQuotaMetadataResetRequests(f)
			connection := f.b.client.ActiveConnection()
			originalContext := f.owner.context
			originalClaim := f.claims.bucket

			points, err := coldQuotaMetadataRead(f, keys, caps)
			require.NoError(t, err)
			require.Len(t, points, len(keys))
			for i, point := range points {
				require.NotNil(t, point)
				require.Equal(t, keys[i], string(point.Key))
				require.Equal(t, strings.Repeat("h", caps[i]), string(point.Value))
			}
			require.Same(t, connection, f.b.client.ActiveConnection())
			require.Same(t, originalContext, f.owner.context)
			require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
			require.Zero(t, host.count("/etcdserverpb.KV/Range"))
			require.Zero(t, host.count("/etcdserverpb.Lease/LeaseGrant"))

			requests := coldQuotaMetadataRequests(f)
			require.Len(t, requests, 1)
			request := requests[0]
			require.Len(t, request.Compare, 20)
			require.Equal(t, 24+2*len(keys), len(request.Compare)+len(request.Success)+len(request.Failure))
			all := append([]string{f.b.identityKey, f.b.restoreKey}, keys...)
			require.Equal(t, taskProtoPoints(all), request.Success)
			require.Equal(t, taskProtoPoints(all), request.Failure)
			for _, fence := range []taskFence{f.b.workerControl.profile, f.session.guard, f.part.fence, f.ranges.fence} {
				coldQuotaWorkerJournalRequireFence(t, request, fence)
			}
			for _, comparison := range request.Compare {
				require.NotEqual(t, f.claims.fence.key, string(comparison.Key))
			}
			base := taskProtoComparisons(f.b.baseComparisons())
			for _, comparison := range base {
				require.Contains(t, request.Compare, comparison)
			}

			state := coldQuotaMetadataState(f)
			require.Zero(t, state.ranges.tokens)
			require.Equal(t, originalClaim, state.claims)
			require.Nil(t, state.operation)
			require.False(t, state.lost)
			require.True(t, state.registered)
			select {
			case <-f.owner.done:
				t.Fatal("a metadata phase cannot finish the caller's enrolled owner")
			default:
			}
		})
	}
}

func TestColdQuotaWorkerMetadataIgnoresOriginalClaimState(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	keys := coldQuotaMetadataPoints(t, f, []int{4096})
	f.b.archiveLife.mu.Lock()
	f.claims.bucket.tokens = 0
	f.claims.lost = true
	original := f.claims.bucket
	f.b.archiveLife.mu.Unlock()
	// Even the remote Claim fence is stale: metadata owns only the original
	// Range lane, so reading or validating Claim would reject this valid read.
	f.server.put(f.claims.fence.key, "changed-claim-fence", 0)
	points, err := coldQuotaMetadataRead(f, keys, []int{4096})
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
	require.Equal(t, original, coldQuotaMetadataState(f).claims)
}

// Invalid local inputs and unpaid attempts must not consume earned fractions,
// move the earning origin, register a call or poison the original live session.
func TestColdQuotaWorkerMetadataLocalRefusalDoesNotCharge(t *testing.T) {
	tests := map[string]func(*testing.T, *coldQuotaAdmissionFixture, *[]string, *[]int){
		"nil-owner": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) { f.owner = nil },
		"copied-owner": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			copy := *f.owner
			f.owner = &copy
		},
		"nil-session":   func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) { f.session = nil },
		"nil-partition": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) { f.part = nil },
		"nil-range":     func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) { f.ranges = nil },
		"copied-session": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			copy := *f.session
			copy.self = &copy
			f.session = &copy
		},
		"copied-partition": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			copy := *f.part
			copy.self = &copy
			f.part = &copy
		},
		"copied-range": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			copy := *f.ranges
			copy.self = &copy
			f.ranges = &copy
		},
		"wrong-role": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) { f.ranges = f.claims },
		"range-insufficient": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			f.ranges.bucket.tokens = 262143
		},
		"empty-points":    func(_ *testing.T, _ *coldQuotaAdmissionFixture, keys *[]string, caps *[]int) { *keys, *caps = nil, nil },
		"mismatched-caps": func(_ *testing.T, _ *coldQuotaAdmissionFixture, _ *[]string, caps *[]int) { *caps = nil },
		"zero-cap":        func(_ *testing.T, _ *coldQuotaAdmissionFixture, _ *[]string, caps *[]int) { (*caps)[0] = 0 },
		"over-record-cap": func(_ *testing.T, _ *coldQuotaAdmissionFixture, _ *[]string, caps *[]int) { (*caps)[0] = 65537 },
		"foreign-key": func(_ *testing.T, _ *coldQuotaAdmissionFixture, keys *[]string, _ *[]int) {
			(*keys)[0] = "/foreign/point"
		},
		"request-over-128k": func(_ *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			// This is a local projection only: it never becomes stored worker
			// authority, and still fits the old per-message 256 KiB precheck.
			f.b.workerControl.profile.value = strings.Repeat("x", 131072)
		},
		"fifteen-points": func(t *testing.T, f *coldQuotaAdmissionFixture, keys *[]string, caps *[]int) {
			*caps = make([]int, 15)
			for i := range *caps {
				(*caps)[i] = 1
			}
			*keys = coldQuotaMetadataPoints(t, f, *caps)
		},
		"aggregate-over-budget": func(t *testing.T, f *coldQuotaAdmissionFixture, keys *[]string, caps *[]int) {
			*caps = []int{65536, 65536, 65536, 65536}
			*keys = coldQuotaMetadataPoints(t, f, *caps)
		},
		"combined-budget-only": func(t *testing.T, f *coldQuotaAdmissionFixture, keys *[]string, caps *[]int) {
			*caps = []int{65536, 65536, 65536, 60000}
			*keys = coldQuotaMetadataPoints(t, f, *caps)
			all := append([]string{f.b.identityKey, f.b.restoreKey}, (*keys)...)
			comparisons := append(f.b.baseComparisons(), f.b.workerControl.profile.comparisons()...)
			for _, fence := range []taskFence{f.session.guard, f.part.fence, f.ranges.fence} {
				comparisons = append(comparisons, fence.comparisons()...)
			}
			request := &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: taskProtoPoints(all), Failure: taskProtoPoints(all)}
			values := []string{f.b.identityValue, f.b.restoreEpoch, strings.Repeat("x", 65536), strings.Repeat("x", 65536), strings.Repeat("x", 65536), ""}
			// Find the first cap whose worst reply plus W20 request exceeds
			// one original charge, while each message separately still fits.
			for cap := 60000; cap <= 65536; cap++ {
				values[5] = strings.Repeat("x", cap)
				reply := taskProtoResponse(all, values)
				if request.Size()+reply.Size() <= 262144 {
					continue
				}
				require.LessOrEqual(t, request.Size(), 131072)
				require.LessOrEqual(t, reply.Size(), 262144)
				(*caps)[3] = cap
				return
			}
			t.Fatal("fixture could not isolate the aggregate-only boundary")
		},
		"other-session-range": func(t *testing.T, f *coldQuotaAdmissionFixture, _ *[]string, _ *[]int) {
			s, err := f.b.OpenWorkerSession(context.Background(), "metadata-other-session")
			require.NoError(t, err)
			f.ranges, err = f.b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetRange, 1)
			require.NoError(t, err)
			f.ranges.bucket.last, f.ranges.bucket.tokens = time.Now().Add(time.Hour), 262144
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			original := *f
			caps := []int{4096}
			keys := coldQuotaMetadataPoints(t, f, caps)
			mutate(t, f, &keys, &caps)
			original.b.archiveLife.mu.Lock()
			ranges, claims := original.ranges.bucket, original.claims.bucket
			original.b.archiveLife.mu.Unlock()
			before := host.count("/etcdserverpb.KV/Txn")

			points, err := coldQuotaMetadataRead(f, keys, caps)
			require.Error(t, err)
			require.Nil(t, points)
			require.Equal(t, before, host.count("/etcdserverpb.KV/Txn"))
			state := coldQuotaMetadataState(&original)
			require.Equal(t, ranges, state.ranges)
			require.Equal(t, claims, state.claims)
			require.Nil(t, state.operation)
			require.False(t, state.lost)
		})
	}
}

// Every four-field worker fence participates in the actual Txn. A false W20
// cannot identify which authority changed and must conservatively lose session.
func TestColdQuotaWorkerMetadataRemoteFenceLoss(t *testing.T) {
	for _, kind := range []string{"profile", "session", "partition", "range"} {
		for _, field := range []string{"value", "create", "mod", "lease"} {
			t.Run(kind+"-"+field, func(t *testing.T) {
				f, host := newColdQuotaRPCHost(t)
				keys := coldQuotaMetadataPoints(t, f, []int{4096})
				fences := map[string]taskFence{"profile": f.b.workerControl.profile, "session": f.session.guard, "partition": f.part.fence, "range": f.ranges.fence}
				f.server.mu.Lock()
				kv := f.server.values[fences[kind].key]
				switch field {
				case "value":
					kv.Value = append(kv.Value, 'x')
				case "create":
					kv.CreateRevision++
				case "mod":
					kv.ModRevision++
				case "lease":
					kv.Lease++
				}
				f.server.mu.Unlock()

				points, err := coldQuotaMetadataRead(f, keys, []int{4096})
				require.Nil(t, points)
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				require.ErrorIs(t, err, ErrGuardExpired)
				require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
				state := coldQuotaMetadataState(f)
				require.True(t, state.lost)
				require.Zero(t, state.ranges.tokens)
				require.Equal(t, uint64(1), state.claims.tokens)
			})
		}
	}
}

// These mutations occur at the real serialized ACK boundary. A successful Txn
// boolean alone cannot legitimize an alien key, invalid MVCC or extra graph.
func TestColdQuotaWorkerMetadataUnknownACKLosesSession(t *testing.T) {
	for _, mode := range []string{"header", "cluster", "revision", "shape", "nested-header", "key", "mvcc", "identity", "restore", "actual-cap", "nested-txn", "transport"} {
		t.Run(mode, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			keys := coldQuotaMetadataPoints(t, f, []int{4096})
			if mode == "transport" {
				host.mu.Lock()
				host.fault = status.Error(codes.Unavailable, "lost metadata acknowledgement")
				host.mu.Unlock()
			} else {
				f.server.mu.Lock()
				f.server.txnReply = func(_ *pb.TxnRequest, response *pb.TxnResponse) {
					switch mode {
					case "header":
						response.Header = nil
					case "cluster":
						response.Header.ClusterId++
					case "revision":
						response.Header.Revision = 0
					case "shape":
						response.Responses = response.Responses[:2]
					case "nested-header":
						response.Responses[2].GetResponseRange().Header = &pb.ResponseHeader{ClusterId: 99, Revision: response.Header.Revision}
					case "key":
						response.Responses[2].GetResponseRange().Kvs[0].Key = []byte(keys[0] + "-alien")
					case "mvcc":
						response.Responses[2].GetResponseRange().Kvs[0].ModRevision = response.Header.Revision + 1
					case "identity":
						response.Responses[0].GetResponseRange().Kvs[0].Value = []byte("other-identity")
					case "restore":
						response.Responses[1].GetResponseRange().Kvs[0].Value = []byte("other-restore")
					case "actual-cap":
						response.Responses[2].GetResponseRange().Kvs[0].Value = []byte(strings.Repeat("x", 4097))
					case "nested-txn":
						response.Responses[2] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: &pb.TxnResponse{}}}
					}
				}
				f.server.mu.Unlock()
			}

			points, err := coldQuotaMetadataRead(f, keys, []int{4096})
			require.Nil(t, points)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
			state := coldQuotaMetadataState(f)
			require.True(t, state.lost)
			require.Zero(t, state.ranges.tokens)
			require.Nil(t, state.operation)
			require.True(t, state.registered)
		})
	}
}

// The response is below 256 KiB but one byte above the charge remaining after
// the actual request. ResourceExhausted must arise at gRPC's receive boundary;
// merely checking the decoded business cap would not enforce the paid cohort.
func TestColdQuotaWorkerMetadataNativeReceiveUsesActualRequestRemainder(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	caps := []int{65536, 65536, 65536, 4096}
	keys := coldQuotaMetadataPoints(t, f, caps)
	f.server.mu.Lock()
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		target := 262144 - request.Size() + 1
		for _, op := range response.Responses[2:] {
			op.GetResponseRange().Kvs[0].Value = []byte(strings.Repeat("x", 65536))
		}
		last := response.Responses[len(response.Responses)-1].GetResponseRange().Kvs[0]
		for response.Size() > target {
			last.Value = last.Value[:len(last.Value)-1]
		}
		// Payload/tag varint thresholds can skip a byte. A skipped target is
		// still strictly over the dynamic receive limit and below 256 KiB.
		for response.Size() < target {
			last.Value = append(last.Value, 'x')
		}
	}
	f.server.mu.Unlock()
	points, err := coldQuotaMetadataRead(f, keys, caps)
	require.Nil(t, points)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
	require.True(t, coldQuotaMetadataState(f).lost)
}

func TestColdQuotaWorkerMetadataWireRefusalDoesNotPreserveSession(t *testing.T) {
	for _, prefix := range [][]byte{
		{0x98, 0x06, 0x00}, // Unknown scalar field 99.
		{0x10, 0x01},       // Duplicate Succeeded field before canonical ACK.
	} {
		t.Run(fmt.Sprintf("prefix-%x", prefix), func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			keys := coldQuotaMetadataPoints(t, f, []int{4096})
			response := taskProtoResponse(append([]string{f.b.identityKey, f.b.restoreKey}, keys...), []string{f.b.identityValue, f.b.restoreEpoch, strings.Repeat("h", 4096)})
			wire, err := response.Marshal()
			require.NoError(t, err)
			host.mu.Lock()
			host.wire = append(append([]byte(nil), prefix...), wire...)
			host.mu.Unlock()

			points, err := coldQuotaMetadataRead(f, keys, []int{4096})
			require.Nil(t, points)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.True(t, coldQuotaMetadataState(f).lost)
			require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
		})
	}
}

// The interceptor invokes the real native call first, then withholds its actual
// return after cancellation. That distinguishes a join from a context-only wait.
func coldQuotaMetadataIntercept(t *testing.T, f *coldQuotaAdmissionFixture, after func(context.Context, *pb.TxnResponse, error) error) {
	t.Helper()
	original := f.b.client
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second, DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
		err := invoke(ctx, method, request, reply, connection, options...)
		if method != "/etcdserverpb.KV/Txn" {
			return err
		}
		return after(ctx, reply.(*pb.TxnResponse), err)
	})}})
	require.NoError(t, err)
	f.b.client = transport
	t.Cleanup(func() {
		select {
		case <-f.owner.done:
		default:
			f.owner.finish()
		}
		f.b.client = original
		require.NoError(t, f.b.Close())
		if transport.Ctx().Err() == nil {
			require.NoError(t, transport.Close())
		}
		if original.Ctx().Err() == nil {
			require.NoError(t, original.Close())
		}
	})
}

// This fixture owns conditional enrolled cleanup because Close's caller must
// finish that owner after the native return. The older admission fixture owns
// unconditional cleanup and therefore cannot be finished early by this test.
func coldQuotaMetadataJoinFixture(t *testing.T) *coldQuotaAdmissionFixture {
	t.Helper()
	b, server := workerTestBackend(t)
	b.enrolledPublication = &EnrolledPublicationOptions{MaxActive: 4}
	ctx := context.WithValue(context.Background(), coldQuotaAdmissionContextKey{}, "original-enrolled-owner")
	owner, err := b.registerEnrolledPublication(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-owner.done:
		default:
			owner.finish()
		}
	})
	session, err := b.OpenWorkerSession(context.Background(), "quota-metadata-join")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(context.Background(), session, 7)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	b.archiveLife.mu.Lock()
	frozen := time.Now().Add(time.Hour)
	ranges.bucket.last, ranges.bucket.tokens = frozen, 262144
	claims.bucket.last, claims.bucket.tokens = frozen, 1
	b.archiveLife.mu.Unlock()
	return &coldQuotaAdmissionFixture{b: b, server: server, owner: owner, session: session, part: part, ranges: ranges, claims: claims}
}

func TestColdQuotaWorkerMetadataActualReturnRetainsCallAndOwner(t *testing.T) {
	for _, mode := range []string{"release-session", "close"} {
		t.Run(mode, func(t *testing.T) {
			f := coldQuotaMetadataJoinFixture(t)
			keys := coldQuotaMetadataPoints(t, f, []int{4096})
			entered, allow := make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(allow) })
			var hits atomic.Int32
			coldQuotaMetadataIntercept(t, f, func(_ context.Context, _ *pb.TxnResponse, err error) error {
				hits.Add(1)
				close(entered)
				<-allow
				return err
			})
			finished := make(chan error, 1)
			go func() {
				_, err := coldQuotaMetadataRead(f, keys, []int{4096})
				if mode == "close" {
					f.owner.finish()
				}
				finished <- err
			}()
			coldQuotaJournalAwait(t, entered)
			f.b.archiveLife.mu.Lock()
			call := f.session.operation
			f.b.archiveLife.mu.Unlock()
			require.NotNil(t, call)
			require.Equal(t, "original-enrolled-owner", call.ctx.Value(coldQuotaAdmissionContextKey{}))
			stopped := make(chan error, 1)
			go func() {
				if mode == "close" {
					stopped <- f.b.Close()
				} else {
					stopped <- f.b.ReleaseWorkerSession(context.Background(), f.session)
				}
			}()
			coldQuotaJournalAwait(t, call.ctx.Done())
			select {
			case <-call.done:
				t.Fatal("cancelled metadata returned ownership before the actual native return")
			default:
			}
			select {
			case err := <-stopped:
				t.Fatalf("%s returned before the original native call: %v", mode, err)
			default:
			}

			state := coldQuotaMetadataState(f)
			require.Same(t, call, state.operation)
			require.Zero(t, state.ranges.tokens)
			require.Equal(t, uint64(1), state.claims.tokens)
			require.True(t, state.registered)
			// A duplicate shares the same original session but cannot spend a
			// second Range credit or dispatch while the native owner is busy.
			_, duplicateErr := coldQuotaMetadataRead(f, keys, []int{4096})
			require.Error(t, duplicateErr)
			require.Equal(t, int32(1), hits.Load())
			release.Do(func() { close(allow) })
			require.ErrorIs(t, workerTestReceive(t, finished), ErrOutcomeUnknown)
			require.NoError(t, workerTestReceive(t, stopped))
			coldQuotaJournalAwait(t, call.done)
		})
	}
}

func TestColdQuotaWorkerMetadataRPCRejectsMutationAndMixedCohorts(t *testing.T) {
	for _, kind := range []string{"put-success", "delete-failure", "nested", "empty", "range-option"} {
		t.Run(kind, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			request := &pb.TxnRequest{Success: taskProtoPoints([]string{f.b.identityKey}), Failure: taskProtoPoints([]string{f.b.identityKey})}
			switch kind {
			case "put-success":
				request.Success = []*pb.RequestOp{taskProtoPut(f.b.identityKey, "forbidden", 0)}
			case "delete-failure":
				request.Failure = []*pb.RequestOp{{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: &pb.DeleteRangeRequest{Key: []byte(f.b.identityKey)}}}}
			case "nested":
				request.Success = []*pb.RequestOp{{Request: &pb.RequestOp_RequestTxn{RequestTxn: request}}}
			case "empty":
				request.Success, request.Failure = nil, nil
			case "range-option":
				request.Success[0].GetRequestRange().Serializable = true
			}
			response, err := f.b.coldQuotaWorkerMetadataRPC(call, request)
			require.Error(t, err)
			require.Nil(t, response)
			require.Zero(t, host.count("/etcdserverpb.KV/Txn"))
		})
	}
	for _, first := range []string{"metadata", "journal", "stage"} {
		t.Run("cohort-"+first, func(t *testing.T) {
			f, host := newColdQuotaRPCHost(t)
			call, _, err := f.begin()
			require.NoError(t, err)
			defer call.finish()
			request := &pb.TxnRequest{Success: taskProtoPoints([]string{f.b.identityKey}), Failure: taskProtoPoints([]string{f.b.identityKey})}
			switch first {
			case "metadata":
				_, err = f.b.coldQuotaWorkerMetadataRPC(call, request)
			case "journal":
				_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
			case "stage":
				_, err = f.b.coldQuotaWorkerGrantRPC(call)
			}
			require.NoError(t, err)
			beforeTxn, beforeGrant := host.count("/etcdserverpb.KV/Txn"), host.count("/etcdserverpb.Lease/LeaseGrant")
			_, err = f.b.coldQuotaWorkerMetadataRPC(call, request)
			require.Error(t, err)
			if first == "metadata" {
				_, err = f.b.coldQuotaWorkerJournalRPC(call, request)
				require.Error(t, err)
				_, err = f.b.coldQuotaWorkerGrantRPC(call)
				require.Error(t, err)
			}
			require.Equal(t, beforeTxn, host.count("/etcdserverpb.KV/Txn"))
			require.Equal(t, beforeGrant, host.count("/etcdserverpb.Lease/LeaseGrant"))
		})
	}
}

// Metadata's 64 KiB value permission is scoped to its own codec. Existing
// Journal/guard/Commit must still reject the same large native response.
func TestColdQuotaWorkerMetadataDoesNotWidenOldPhaseLimits(t *testing.T) {
	f, host := newColdQuotaRPCHost(t)
	keys := coldQuotaMetadataPoints(t, f, []int{20000})
	call, _, err := f.begin()
	require.NoError(t, err)
	defer call.finish()
	_, err = f.b.coldQuotaWorkerJournalRPC(call, coldQuotaRPCPointRequest(keys[0]))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, 1, host.count("/etcdserverpb.KV/Txn"))
}
