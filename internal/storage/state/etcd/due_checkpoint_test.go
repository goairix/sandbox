package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These tests drive the finite consumer that the resident actor will use.
// The actual bounded gRPC Range/control/Stage paths remain in the execution;
// credit is earned deterministically, without a sleeping scheduler in tests.
func dueCheckpointTestKey(f *duePageFixture) string {
	return f.b.namespace.Root() + "p/aa/due-worker-checkpoint"
}

func dueCheckpointTestRecord(f *duePageFixture, page DuePage) dueCheckpointRecord {
	return dueCheckpointRecord{
		Version: 1, Namespace: f.b.namespace.Root(), RestoreEpoch: f.b.restoreEpoch,
		IdentityDigest: workerIdentityDigest(f.b.identityValue), ProfileID: workerProfileID,
		ProfileDigest: workerIdentityDigest(f.b.workerControl.profile.value), Partition: 0xaa,
		WriterSessionID: f.session.reference.SessionID, WriterPartitionRevision: f.partition.fence.create,
		Revision: page.Revision, ReplayFrom: page.Revision + 1, WindowEnd: page.WindowEnd, Phase: "list",
	}
}

func dueCheckpointTestWrite(t *testing.T, f *duePageFixture, previous *dueCheckpoint, record dueCheckpointRecord) (*dueCheckpoint, error) {
	t.Helper()
	call, err := f.b.beginWorkerCall(context.Background(), f.session)
	require.NoError(t, err)
	defer call.finish()
	return f.b.writeDueCheckpoint(call, f.partition, nil, previous, record)
}

func dueCheckpointTestRaw(t *testing.T, f *duePageFixture) dueCheckpointRecord {
	t.Helper()
	kv := f.server.point(dueCheckpointTestKey(f))
	require.NotNil(t, kv)
	var record dueCheckpointRecord
	require.NoError(t, json.Unmarshal(kv.Value, &record))
	return record
}

func dueCheckpointTestReopen(t *testing.T, f *duePageFixture) {
	t.Helper()
	require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
	s, err := f.b.OpenWorkerSession(context.Background(), "replacement-due-worker")
	require.NoError(t, err)
	p, err := f.b.ClaimWorkerPartition(context.Background(), s, 0xaa)
	require.NoError(t, err)
	ranges, err := f.b.AcquireWorkerBudget(context.Background(), s, WorkerBudgetRange, 0)
	require.NoError(t, err)
	f.session, f.partition, f.share = s, p, ranges
}

func dueCheckpointTestScan(t *testing.T, f *dueExpiryFixture) *dueWorkerScan {
	t.Helper()
	scan, err := newDueWorkerScan(f.b, f.partition, f.share, f.claimShare, 30*time.Second)
	require.NoError(t, err)
	return scan
}

func dueCheckpointTestReopenExpiry(t *testing.T, f *dueExpiryFixture) {
	t.Helper()
	dueCheckpointTestReopen(t, f.duePageFixture)
	claims, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	f.claimShare = claims
}

func dueCheckpointTestPendingPut(ops []clientv3.Op) bool {
	for _, op := range ops {
		if !op.IsPut() || !bytes.HasSuffix(op.KeyBytes(), []byte("/due-worker-checkpoint")) {
			continue
		}
		var record dueCheckpointRecord
		if json.Unmarshal(op.ValueBytes(), &record) == nil && record.Pending != nil {
			return true
		}
	}
	return false
}

func dueCheckpointTestClearPut(ops []clientv3.Op) bool {
	for _, op := range ops {
		if !op.IsPut() || !bytes.HasSuffix(op.KeyBytes(), []byte("/due-worker-checkpoint")) {
			continue
		}
		var record dueCheckpointRecord
		if json.Unmarshal(op.ValueBytes(), &record) == nil && record.Pending == nil && record.Frontier != "" {
			return true
		}
	}
	return false
}

func TestDueCheckpointStrictSchemaAndBounds(t *testing.T) {
	f := newDuePageFixture(t)
	rows := f.seed(t, 1, f.now)
	f.earn()
	page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	record := dueCheckpointTestRecord(f, page)
	record.Frontier, err = f.b.namespace.dueCandidateKey(rows[0])
	require.NoError(t, err)
	wire, err := encodeDueCheckpoint(record)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 8192)
	kv := &mvccpb.KeyValue{Key: []byte(dueCheckpointTestKey(f)), Value: []byte(wire), Version: 1, CreateRevision: page.Revision, ModRevision: page.Revision}
	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"unknown-field": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"version":1`), []byte(`"version":1,"extra":0`), 1)
		},
		"duplicate-field": func(k *mvccpb.KeyValue) {
			k.Value = bytes.Replace(k.Value, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		},
		"missing-pending": func(k *mvccpb.KeyValue) { k.Value = bytes.Replace(k.Value, []byte(`,"pending":null`), nil, 1) },
		"noncanonical":    func(k *mvccpb.KeyValue) { k.Value = append(k.Value, ' ') },
		"oversized":       func(k *mvccpb.KeyValue) { k.Value = bytes.Repeat([]byte("x"), 8193) },
		"leased":          func(k *mvccpb.KeyValue) { k.Lease = 1 },
		"future-mod":      func(k *mvccpb.KeyValue) { k.ModRevision++ },
		"zero-create":     func(k *mvccpb.KeyValue) { k.CreateRevision = 0 },
		"zero-version":    func(k *mvccpb.KeyValue) { k.Version = 0 },
		"wrong-key":       func(k *mvccpb.KeyValue) { k.Key = append(k.Key, 'x') },
	} {
		t.Run(name, func(t *testing.T) {
			bad := workerTestKV(kv)
			mutate(bad)
			_, err := f.b.decodeDueCheckpoint(bad, page.Revision)
			require.Error(t, err)
		})
	}
	for name, mutate := range map[string]func(*dueCheckpointRecord){
		"version":                    func(r *dueCheckpointRecord) { r.Version = 2 },
		"epoch":                      func(r *dueCheckpointRecord) { r.RestoreEpoch = "other" },
		"namespace":                  func(r *dueCheckpointRecord) { r.Namespace += "other/" },
		"identity":                   func(r *dueCheckpointRecord) { r.IdentityDigest = strings.Repeat("f", 64) },
		"profile":                    func(r *dueCheckpointRecord) { r.ProfileDigest = strings.Repeat("f", 64) },
		"profile-id":                 func(r *dueCheckpointRecord) { r.ProfileID = "other" },
		"partition":                  func(r *dueCheckpointRecord) { r.Partition++ },
		"revision":                   func(r *dueCheckpointRecord) { r.Revision = 0 },
		"replay-header-substitution": func(r *dueCheckpointRecord) { r.ReplayFrom++ },
		"revision-overflow":          func(r *dueCheckpointRecord) { r.Revision = math.MaxInt64; r.ReplayFrom = math.MinInt64 },
		"phase":                      func(r *dueCheckpointRecord) { r.Phase = "healthy" },
		"writer":                     func(r *dueCheckpointRecord) { r.WriterSessionID = "copied-reference" },
		"writer-revision":            func(r *dueCheckpointRecord) { r.WriterPartitionRevision = 0 },
		"frontier-nul":               func(r *dueCheckpointRecord) { r.Frontier += "\x00" },
		"frontier-other-partition":   func(r *dueCheckpointRecord) { r.Frontier = strings.Replace(r.Frontier, "/p/aa/", "/p/ab/", 1) },
		"frontier-outside-window": func(r *dueCheckpointRecord) {
			c := rows[0]
			c.ExpiresAt = r.WindowEnd.Add(time.Second)
			r.Frontier, _ = f.b.namespace.dueCandidateKey(c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			mutate(&bad)
			wire, err := json.Marshal(bad)
			require.NoError(t, err)
			k := workerTestKV(kv)
			k.Value = wire
			_, err = f.b.decodeDueCheckpoint(k, page.Revision)
			require.Error(t, err)
		})
	}
	got, err := f.b.decodeDueCheckpoint(kv, page.Revision)
	require.NoError(t, err)
	require.Equal(t, record, got)
	// The row can grow beyond generic workerPoints' 2048-byte cap while
	// remaining valid under its dedicated permanent decoder.
	max := record
	max.Pending = &duePendingRecord{Candidate: rows[0], Reference: StageReference{Namespace: record.Namespace, Partition: 0xaa, RequestID: uuid.NewString(), StageID: "destroy", AttemptID: uuid.NewString(), Digest: strings.Repeat("b", 64), RestoreEpoch: record.RestoreEpoch}, Task: TaskReference{Namespace: record.Namespace, RestoreEpoch: record.RestoreEpoch, Partition: 0xaa, TaskID: uuid.NewString(), SandboxID: rows[0].SandboxID}}
	_, err = encodeDueCheckpoint(max)
	require.NoError(t, err)
	for _, mutate := range []func(*duePendingRecord){
		func(p *duePendingRecord) { p.Reference.StageID = "create" },
		func(p *duePendingRecord) { p.Reference.AttemptID = "other" },
		func(p *duePendingRecord) { p.Reference.Partition++ },
		func(p *duePendingRecord) { p.Task.SandboxID = "other" },
		func(p *duePendingRecord) { p.Task.TaskID = "other" },
	} {
		bad := max
		p := *max.Pending
		bad.Pending = &p
		mutate(&p)
		_, err := encodeDueCheckpoint(bad)
		require.Error(t, err)
	}
}

func TestDueCheckpointFencedCASAndRecreatedRow(t *testing.T) {
	for _, change := range []string{"none", "same-value-rewrite", "recreated", "partition", "profile"} {
		t.Run(change, func(t *testing.T) {
			f := newDuePageFixture(t)
			rows := f.seed(t, 1, f.now)
			f.earn()
			page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
			require.NoError(t, err)
			record := dueCheckpointTestRecord(f, page)
			saved, err := dueCheckpointTestWrite(t, f, nil, record)
			require.NoError(t, err)
			key := dueCheckpointTestKey(f)
			before := f.server.point(key)
			switch change {
			case "same-value-rewrite":
				f.server.put(key, string(before.Value), 0)
			case "recreated":
				f.server.mu.Lock()
				delete(f.server.values, key)
				f.server.mu.Unlock()
				f.server.put(key, string(before.Value), 0)
			case "partition", "profile":
				fence := f.partition.fence
				if change == "profile" {
					fence = f.b.workerControl.profile
				}
				f.server.put(fence.key, fence.value, fence.lease)
			}
			record.Frontier, err = f.b.namespace.dueCandidateKey(rows[0])
			require.NoError(t, err)
			_, err = dueCheckpointTestWrite(t, f, saved, record)
			if change == "none" {
				require.NoError(t, err)
				after := f.server.point(key)
				require.Equal(t, before.CreateRevision, after.CreateRevision)
				require.Greater(t, after.ModRevision, before.ModRevision)
			} else {
				require.Error(t, err)
				require.Empty(t, dueCheckpointTestRaw(t, f).Frontier)
			}
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			for _, q := range f.server.requests {
				for _, op := range q.Success {
					put := op.GetRequestPut()
					if put == nil || string(put.Key) != key {
						continue
					}
					require.Zero(t, put.Lease)
					require.Len(t, q.Success, 1)
					require.Len(t, q.Failure, 4)
					require.Contains(t, []int{17, 20}, len(q.Compare))
					require.LessOrEqual(t, q.Size(), 128*1024)
				}
			}
		})
	}
}

func TestDueCheckpointReadRejectsMalformedNativeEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*pb.TxnResponse){
		"outer-cluster":  func(r *pb.TxnResponse) { r.Header.ClusterId++ },
		"outer-revision": func(r *pb.TxnResponse) { r.Header.Revision = 0 },
		"missing-op":     func(r *pb.TxnResponse) { r.Responses = nil },
		"wrong-op":       func(r *pb.TxnResponse) { r.Responses = taskProtoPutResponse(1).Responses },
		"count":          func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Count++ },
		"more":           func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().More = true },
		"duplicate-kv": func(r *pb.TxnResponse) {
			point := r.Responses[0].GetResponseRange()
			point.Kvs = append(point.Kvs, workerTestKV(point.Kvs[0]))
			point.Count = 2
		},
		"nested-revision": func(r *pb.TxnResponse) { r.Responses[0].GetResponseRange().Header.Revision++ },
		"oversized-row": func(r *pb.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Value = bytes.Repeat([]byte("x"), 8193)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDuePageFixture(t)
			f.earn()
			page, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
			require.NoError(t, err)
			_, err = dueCheckpointTestWrite(t, f, nil, dueCheckpointTestRecord(f, page))
			require.NoError(t, err)
			originalKV := f.b.client.KV
			f.b.client.KV = &faultKV{KV: originalKV, match: func(ops []clientv3.Op) bool {
				return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == dueCheckpointTestKey(f)
			}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				mutate((*pb.TxnResponse)(r))
				return r, nil
			}}
			got, err := f.b.readDueCheckpoint(context.Background(), f.partition)
			require.Error(t, err)
			require.Nil(t, got)
			f.b.client.KV = originalKV
		})
	}
}

func TestDueCheckpointNewOwnerActualPageResume(t *testing.T) {
	f := newDuePageFixture(t)
	rows := f.seed(t, 120, f.now.Add(-time.Second))
	f.earn()
	first, err := f.b.ReadDuePage(context.Background(), f.partition, f.share, nil)
	require.NoError(t, err)
	record := dueCheckpointTestRecord(f, first)
	record.Frontier, err = f.b.namespace.dueCandidateKey(rows[117])
	require.NoError(t, err)
	saved, err := dueCheckpointTestWrite(t, f, nil, record)
	require.NoError(t, err)
	oldSession := f.session.Reference()
	dueCheckpointTestReopen(t, f)
	require.NotEqual(t, oldSession.SessionID, f.session.Reference().SessionID)
	f.earn()
	_, err = f.b.ReadDuePage(context.Background(), f.partition, f.share, first.Next)
	require.Error(t, err, "a replacement cannot adopt the original cursor")
	_, err = f.b.resumeDuePage(context.Background(), f.partition, f.share, saved)
	require.Error(t, err, "even a copied checkpoint handle must be reread under current original fences")
	loaded, err := f.b.readDueCheckpoint(context.Background(), f.partition)
	require.NoError(t, err)
	// A later write changes the current response header, never saved fixed R.
	f.server.put(f.b.identityKey, f.b.identityValue, 0)
	f.earn()
	last, err := f.b.resumeDuePage(context.Background(), f.partition, f.share, loaded)
	require.NoError(t, err)
	require.Equal(t, first.Revision, last.Revision)
	require.Equal(t, first.WindowEnd, last.WindowEnd)
	require.Equal(t, rows[118:], last.Candidates)
	require.Nil(t, last.Next)
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var actual *pb.RangeRequest
	for _, q := range f.server.requests {
		if len(q.Success) == 1 && q.Success[0].GetRequestRange() != nil && len(q.Success[0].GetRequestRange().RangeEnd) != 0 {
			actual = q.Success[0].GetRequestRange()
		}
	}
	require.NotNil(t, actual)
	require.Equal(t, record.Frontier+"\x00", string(actual.Key))
	require.Equal(t, first.Revision, actual.Revision)
	require.Equal(t, int64(118), actual.Limit)
	require.Equal(t, pb.RangeRequest_NONE, actual.SortOrder)
}

func TestDueCheckpointFutureFrontierSurvivesRestart(t *testing.T) {
	f := newDueExpiryFixture(t)
	// Keep the full authoritative metadata coherent while moving expiry into
	// the future. The real consumer must refuse early work and retain it.
	oldKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
	f.server.mu.Lock()
	delete(f.server.values, oldKey)
	f.server.mu.Unlock()
	f.control.ExpiresAt = f.now.Add(30 * time.Second)
	wire, err := encodeDomainRecord(f.control)
	require.NoError(t, err)
	f.server.put(f.keys[1], wire, 0)
	f.candidate = dueCandidateForControl(0xaa, f.control)
	key, _ := f.b.namespace.dueCandidateKey(f.candidate)
	wire, err = encodeDueCandidate(f.candidate)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	f.earnClaim()
	err = scan.step(context.Background())
	require.True(t, err == nil || errors.Is(err, ErrOperationAdmissionClosed))
	saved := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Empty(t, saved.Frontier, "early candidate is not safely covered")
	require.Nil(t, saved.Pending)
	dueCheckpointTestReopenExpiry(t, f)
	loaded, err := f.b.readDueCheckpoint(context.Background(), f.partition)
	require.NoError(t, err)
	f.earn()
	page, err := f.b.resumeDuePage(context.Background(), f.partition, f.share, loaded)
	require.NoError(t, err)
	require.Equal(t, []DueCandidateRecord{f.candidate}, page.Candidates)
	require.Equal(t, saved.Revision, page.Revision)
	// Expiry can arrive without any new etcd write to this candidate.
	f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: f.candidate.ExpiresAt.Add(time.Second)}, nil
	})
	restarted := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, restarted.step(context.Background()))
	f.earnClaim()
	require.NoError(t, restarted.step(context.Background()))
	require.Nil(t, f.server.point(key), "restarted finite consumer executes actual atomic expiry")
	require.Equal(t, key, dueCheckpointTestRaw(t, f.duePageFixture).Frontier)
}

func TestDueCheckpointPendingBeforeGrant(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	lease := f.b.client.Lease
	var pending dueCheckpointRecord
	f.b.client.Lease = &workerTestLease{Lease: lease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		pending = dueCheckpointTestRaw(t, f.duePageFixture)
		require.NotNil(t, pending.Pending, "durable exact attempt precedes native Grant")
		require.Equal(t, f.candidate, pending.Pending.Candidate)
		require.Empty(t, pending.Frontier)
		return lease.Grant(ctx, ttl)
	}}
	f.earnClaim()
	require.NoError(t, scan.step(context.Background()))
	require.NotNil(t, pending.Pending)
	ref := pending.Pending.Reference
	outcome, err := f.b.ResolveStage(context.Background(), ref)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	for _, key := range taskDestroyKeys(t, f.b, pending.Pending.Task) {
		require.NotNil(t, f.server.point(key), "real finite consumer creates all permanent task evidence")
	}
	final := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Nil(t, final.Pending)
	dueKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
	require.Equal(t, dueKey, final.Frontier)
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var pendingTxn *pb.TxnRequest
	for _, q := range f.server.requests {
		for _, op := range q.Success {
			if p := op.GetRequestPut(); p != nil && string(p.Key) == dueCheckpointTestKey(f.duePageFixture) {
				var row dueCheckpointRecord
				require.NoError(t, json.Unmarshal(p.Value, &row))
				if row.Pending != nil {
					pendingTxn = q
				}
			}
		}
	}
	require.NotNil(t, pendingTxn)
	require.Len(t, pendingTxn.Compare, 24, "same original claim owner plus exact checkpoint CAS")
	require.Len(t, pendingTxn.Success, 1)
	require.Len(t, pendingTxn.Failure, 4)
	require.LessOrEqual(t, pendingTxn.Size(), 128*1024)
}

func TestDueCheckpointUnknownPendingWriteDoesNotGrant(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	originalKV := f.b.client.KV
	f.b.client.KV = &faultKV{KV: originalKV, match: dueCheckpointTestPendingPut, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	before, _, _ := f.server.counts()
	f.earnClaim()
	require.ErrorIs(t, scan.step(context.Background()), ErrOutcomeUnknown)
	after, _, _ := f.server.counts()
	require.Equal(t, before, after, "unknown checkpoint acknowledgement dispatches ZERO Stage Grants")
	pending := dueCheckpointTestRaw(t, f.duePageFixture)
	require.NotNil(t, pending.Pending)
	require.Empty(t, pending.Frontier)
	f.b.client.KV = originalKV
	dueCheckpointTestReopenExpiry(t, f)
	before, _, _ = f.server.counts()
	_ = dueCheckpointTestScan(t, f)
	after, _, _ = f.server.counts()
	require.Equal(t, before, after, "constructor only arbitrates original reference")
	outcome, err := f.b.ResolveStage(context.Background(), pending.Pending.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome, "actual receipt CAS settles the unstarted original attempt")
}

func TestDueCheckpointUnknownAdmissionRestoresExactAttempt(t *testing.T) {
	for _, phase := range []string{"Grant", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := newDueExpiryFixture(t)
			scan := dueCheckpointTestScan(t, f)
			f.earn()
			require.NoError(t, scan.step(context.Background()))
			originalKV, originalLease := f.b.client.KV, f.b.client.Lease
			if phase == "Grant" {
				f.b.client.Lease = &workerTestLease{Lease: originalLease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
					_, err := originalLease.Grant(ctx, ttl)
					require.NoError(t, err)
					return nil, context.DeadlineExceeded
				}}
			} else {
				f.b.client.KV = &faultKV{KV: originalKV, match: putsKey(f.keys[1]), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
			}
			f.earnClaim()
			require.ErrorIs(t, scan.step(context.Background()), ErrOutcomeUnknown)
			pending := dueCheckpointTestRaw(t, f.duePageFixture)
			require.NotNil(t, pending.Pending)
			require.Empty(t, pending.Frontier)
			require.Equal(t, pending.Revision+1, pending.ReplayFrom)
			f.b.client.KV, f.b.client.Lease = originalKV, originalLease
			dueCheckpointTestReopenExpiry(t, f)
			before, _, _ := f.server.counts()
			_ = dueCheckpointTestScan(t, f)
			after, _, _ := f.server.counts()
			require.Equal(t, before, after, "new authority cannot retry before exact original arbitration")
			_, receipt, err := f.b.stageKeys(pending.Pending.Reference)
			require.NoError(t, err)
			require.NotNil(t, f.server.point(receipt), "constructor must actually settle original reference, not drop pending")
			outcome, err := f.b.ResolveStage(context.Background(), pending.Pending.Reference)
			require.NoError(t, err)
			expected := OutcomeAborted
			if phase == "commit" {
				expected = OutcomeCommitted
			}
			require.Equal(t, expected, outcome)
			for _, key := range taskDestroyKeys(t, f.b, pending.Pending.Task) {
				if phase == "commit" {
					require.NotNil(t, f.server.point(key))
				} else {
					require.Nil(t, f.server.point(key))
				}
			}
		})
	}
}

func TestDueCheckpointCommitThenCheckpointCrash(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	originalKV := f.b.client.KV
	// Simulate process loss BEFORE final frontier dispatch, after actual
	// business commit. The permanent pending row must still name that task.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.b.client.KV = &faultKV{KV: originalKV, match: dueCheckpointTestClearPut, before: cancel}
	f.earnClaim()
	require.Error(t, scan.step(ctx))
	pending := dueCheckpointTestRaw(t, f.duePageFixture)
	require.NotNil(t, pending.Pending)
	require.Empty(t, pending.Frontier)
	dueKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
	require.Nil(t, f.server.point(dueKey))
	for _, key := range taskDestroyKeys(t, f.b, pending.Pending.Task) {
		require.NotNil(t, f.server.point(key))
	}
	f.b.client.KV = originalKV
	dueCheckpointTestReopenExpiry(t, f)
	before, _, _ := f.server.counts()
	restarted := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, restarted.step(context.Background()))
	f.earnClaim()
	require.NoError(t, restarted.step(context.Background()))
	after, _, _ := f.server.counts()
	require.Equal(t, before, after, "historical settled candidate cannot create a second task")
	final := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Nil(t, final.Pending)
	require.Equal(t, dueKey, final.Frontier)
	require.Equal(t, pending.Revision, final.Revision)
}

func TestDueCheckpointUnknownResolverPreservesPending(t *testing.T) {
	f := newDueExpiryFixture(t)
	scan := dueCheckpointTestScan(t, f)
	f.earn()
	require.NoError(t, scan.step(context.Background()))
	originalKV := f.b.client.KV
	f.b.client.KV = &faultKV{KV: originalKV, match: dueCheckpointTestPendingPut, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	f.earnClaim()
	require.ErrorIs(t, scan.step(context.Background()), ErrOutcomeUnknown)
	pending := dueCheckpointTestRaw(t, f.duePageFixture)
	require.NotNil(t, pending.Pending)
	f.b.client.KV = originalKV
	dueCheckpointTestReopenExpiry(t, f)
	_, receipt, err := f.b.stageKeys(pending.Pending.Reference)
	require.NoError(t, err)
	f.b.client.KV = &faultKV{KV: originalKV, match: putsKey(receipt), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, r.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	before, _, _ := f.server.counts()
	_, err = newDueWorkerScan(f.b, f.partition, f.share, f.claimShare, 30*time.Second)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	after, _, _ := f.server.counts()
	require.Equal(t, before, after)
	retained := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Equal(t, pending.Pending, retained.Pending)
	require.Empty(t, retained.Frontier)
	f.b.client.KV = originalKV
	dueCheckpointTestReopenExpiry(t, f)
	_ = dueCheckpointTestScan(t, f)
	outcome, err := f.b.ResolveStage(context.Background(), pending.Pending.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
}
