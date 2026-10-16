package etcd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestWorkerRecords(t *testing.T) {
	t.Run("session-key-record-envelope-binding", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker-a")
		require.NoError(t, err)
		key, err := b.namespace.Key("workers", "sessions", session.Reference().SessionID)
		require.NoError(t, err)
		kv := server.point(key)
		require.NotNil(t, kv)
		require.LessOrEqual(t, len(kv.Value), 2048)
		var original workerSessionRecord
		require.NoError(t, decodeWorkerSessionRecord(b.namespace, kv, &original))
		wire, err := encodeWorkerSessionRecord(original)
		require.NoError(t, err)
		require.Equal(t, string(kv.Value), wire)
		for name, mutate := range map[string]func(*mvccpb.KeyValue){
			"key":               func(k *mvccpb.KeyValue) { k.Key = []byte(key + "-other") },
			"outside-namespace": func(k *mvccpb.KeyValue) { k.Key = []byte("/other/workers/sessions/" + session.Reference().SessionID) },
			"lease":             func(k *mvccpb.KeyValue) { k.Lease++ },
			"permanent":         func(k *mvccpb.KeyValue) { k.Lease = 0 },
			"rewrite":           func(k *mvccpb.KeyValue) { k.ModRevision++ },
			"missing-create":    func(k *mvccpb.KeyValue) { k.CreateRevision = 0 },
			"version":           func(k *mvccpb.KeyValue) { k.Version = 2 },
			"schema": func(k *mvccpb.KeyValue) {
				k.Value = bytes.Replace(k.Value, []byte(`"version":1`), []byte(`"version":2`), 1)
			},
			"root-slash": func(k *mvccpb.KeyValue) {
				k.Value = bytes.Replace(k.Value, []byte(`"namespace":"/test/authority/cell/"`), []byte(`"namespace":"/"`), 1)
			},
			"unknown": func(k *mvccpb.KeyValue) {
				k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"unknown":1}`)...)
			},
			"duplicate": func(k *mvccpb.KeyValue) {
				k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"version":1}`)...)
			},
			"noncanonical": func(k *mvccpb.KeyValue) { k.Value = append([]byte(" "), k.Value...) },
			"oversize":     func(k *mvccpb.KeyValue) { k.Value = bytes.Repeat([]byte("x"), 2049) },
		} {
			t.Run(name, func(t *testing.T) {
				bad := workerTestKV(kv)
				mutate(bad)
				dst := original
				require.ErrorIs(t, decodeWorkerSessionRecord(b.namespace, bad, &dst), ErrCorruptRecord)
				require.Equal(t, original, dst, "failed decode must not mutate caller output")
			})
		}
		require.ErrorIs(t, decodeWorkerSessionRecord(b.namespace, nil, &original), ErrCorruptRecord)
		require.ErrorIs(t, decodeWorkerSessionRecord(b.namespace, kv, nil), ErrCorruptRecord)
	})
	t.Run("partition-key-is-exact", func(t *testing.T) {
		b, server := workerTestBackend(t)
		session, err := b.OpenWorkerSession(context.Background(), "worker-a")
		require.NoError(t, err)
		claim, err := b.ClaimWorkerPartition(context.Background(), session, 0xab)
		require.NoError(t, err)
		key, err := b.namespace.Key("p", "ab", "worker")
		require.NoError(t, err)
		kv := server.point(key)
		require.NotNil(t, kv)
		require.Equal(t, session.Reference().LeaseID, kv.Lease)
		require.Equal(t, claim.Reference().CreateRevision, kv.CreateRevision)
		var original workerPartitionRecord
		require.NoError(t, decodeWorkerPartitionRecord(b.namespace, kv, &original))
		for _, wrong := range []string{"p/aa/worker", "p/AB/worker", "p/ab/workers", "p/ab/worker/extra"} {
			bad := workerTestKV(kv)
			bad.Key = []byte(b.namespace.Root() + wrong)
			dst := original
			require.ErrorIs(t, decodeWorkerPartitionRecord(b.namespace, bad, &dst), ErrCorruptRecord)
			require.Equal(t, original, dst)
		}
	})
}

func TestWorkerProfilePin(t *testing.T) {
	for _, fault := range []string{"zero-outer-revision", "outer-before-profile", "nested-cluster", "nested-revision", "omitted-nested-header"} {
		t.Run("native-read-header/"+fault, func(t *testing.T) {
			server, options := workerTestEndpoint(t)
			key, _ := options.Namespace.Key("meta", "worker_control_budget")
			hits := 0
			before := server.revision
			server.txnReply = func(request *pb.TxnRequest, reply *pb.TxnResponse) {
				for i, op := range request.Success {
					read := op.GetRequestRange()
					if read == nil || string(read.Key) != key {
						continue
					}
					hits++
					point := reply.Responses[i].GetResponseRange()
					switch fault {
					case "zero-outer-revision", "outer-before-profile":
						revision := int64(0)
						if fault == "outer-before-profile" {
							revision = server.values[key].ModRevision - 1
						}
						reply.Header.Revision = revision
						// Match nested revisions to isolate the outer-positive/KV
						// revision bound from nested-header mismatch detection.
						for _, response := range reply.Responses {
							if r := response.GetResponseRange(); r != nil && r.Header != nil {
								r.Header.Revision = revision
							}
						}
					case "nested-cluster":
						point.Header.ClusterId++
					case "nested-revision":
						point.Header.Revision++
					case "omitted-nested-header":
						point.Header = nil
					}
				}
			}
			b, err := New(context.Background(), options)
			if b != nil {
				t.Cleanup(func() { require.NoError(t, b.Close()) })
			}
			if fault == "omitted-nested-header" {
				require.NoError(t, err, "native Txn may legitimately omit operation headers")
				require.NotNil(t, b)
			} else {
				require.Error(t, err, "malformed actual profile response must fail closed in New")
				require.Nil(t, b)
			}
			server.mu.Lock()
			after, observed := server.revision, hits
			requests := append([]*pb.TxnRequest(nil), server.requests...)
			server.mu.Unlock()
			require.Equal(t, 1, observed, "fault injection is limited to the actual profile read")
			require.Equal(t, before, after)
			grants, keeps, revokes := server.counts()
			require.Equal(t, []int{0, 0, 0}, []int{grants, keeps, revokes})
			for _, request := range requests {
				for _, branch := range [][]*pb.RequestOp{request.Success, request.Failure} {
					for _, op := range branch {
						require.Nil(t, op.GetRequestPut())
						require.Nil(t, op.GetRequestDeleteRange())
					}
				}
			}
		})
	}
	t.Run("New-is-read-only-and-options-owned", func(t *testing.T) {
		server, options := workerTestEndpoint(t)
		before := server.revision
		b, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		require.Equal(t, before, server.revision)
		grants, keeps, revokes := server.counts()
		require.Equal(t, []int{0, 0, 0}, []int{grants, keeps, revokes})
		options.WorkerControl.SessionTTL = 60 * time.Second
		session, err := b.OpenWorkerSession(context.Background(), "worker")
		require.NoError(t, err)
		leaseID := session.Reference().LeaseID
		server.mu.Lock()
		actualTTL := server.leases[leaseID]
		requests := append([]*pb.TxnRequest(nil), server.requests...)
		server.mu.Unlock()
		require.Equal(t, int64(15), actualTTL)
		for _, request := range requests {
			require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
			require.LessOrEqual(t, request.Size(), 256*1024)
		}
	})
	t.Run("disabled-does-not-require-profile", func(t *testing.T) {
		server, options := workerTestEndpoint(t)
		options.WorkerControl = nil
		key, _ := options.Namespace.Key("meta", "worker_control_budget")
		delete(server.values, key)
		b, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		_, err = b.OpenWorkerSession(context.Background(), "worker")
		require.Error(t, err)
		grants, _, _ := server.counts()
		require.Zero(t, grants)
	})
	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"missing": nil,
		"lease":   func(k *mvccpb.KeyValue) { k.Lease = 100 },
		"rewrite": func(k *mvccpb.KeyValue) { k.ModRevision++ },
		"version": func(k *mvccpb.KeyValue) { k.Version = 2 },
		"unknown": func(k *mvccpb.KeyValue) {
			k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"extra":1}`)...)
		},
		"duplicate": func(k *mvccpb.KeyValue) {
			k.Value = append(bytes.TrimSuffix(k.Value, []byte("}")), []byte(`,"version":1}`)...)
		},
		"whitespace": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, ' ') },
		"rate": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"claim_slots":100`), []byte(`"claim_slots":101`), 1)
		},
		"namespace": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte("/test/authority/cell/"), []byte("/test/authority/other/"), 1)
		},
		"restore": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"restore_epoch":"epoch"`), []byte(`"restore_epoch":"other"`), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, options := workerTestEndpoint(t)
			key, _ := options.Namespace.Key("meta", "worker_control_budget")
			if mutate == nil {
				delete(server.values, key)
			} else {
				mutate(server.values[key])
			}
			b, err := New(context.Background(), options)
			if b != nil {
				_ = b.Close()
			}
			require.ErrorIs(t, err, ErrIdentityMismatch)
			grants, _, _ := server.counts()
			require.Zero(t, grants)
		})
	}
	t.Run("TTL-validation-before-network", func(t *testing.T) {
		for _, ttl := range []time.Duration{-time.Second, time.Second, 3*time.Second + time.Nanosecond, 61 * time.Second} {
			options := validOptions(t)
			options.WorkerControl = &WorkerControlOptions{SessionTTL: ttl}
			require.ErrorIs(t, validateOptions(options), ErrInvalidConfiguration)
		}
		for _, ttl := range []time.Duration{0, 3 * time.Second, 15 * time.Second, 60 * time.Second} {
			options := validOptions(t)
			options.WorkerControl = &WorkerControlOptions{SessionTTL: ttl}
			require.NoError(t, validateOptions(options))
		}
	})
	t.Run("preflight-before-Grant", func(t *testing.T) {
		b, server := workerTestBackend(t)
		b.identityValue = strings.Repeat("x", maxMutationBytes+1)
		_, err := b.OpenWorkerSession(context.Background(), "worker")
		require.Error(t, err)
		grants, _, _ := server.counts()
		require.Zero(t, grants)
	})
}
