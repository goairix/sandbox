package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncIntentBudgetBounds(t *testing.T) {
	require.NoError(t, validateTaskWorkspaceSyncBudget(nil))
	valid := TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}
	require.NoError(t, validateTaskWorkspaceSyncBudget(&valid))
	for _, mutation := range []func(*TaskWorkspaceSyncBudgetOptions){
		func(o *TaskWorkspaceSyncBudgetOptions) { o.BudgetID = uuid.Nil.String() },
		func(o *TaskWorkspaceSyncBudgetOptions) { o.BudgetID = "invalid" },
		func(o *TaskWorkspaceSyncBudgetOptions) { o.SlotsPerPartition = 0 },
		func(o *TaskWorkspaceSyncBudgetOptions) { o.SlotsPerPartition = 65 },
		func(o *TaskWorkspaceSyncBudgetOptions) { o.MaxIntentBytes = 4095 },
		func(o *TaskWorkspaceSyncBudgetOptions) { o.MaxIntentBytes = 16385 },
	} {
		bad := valid
		mutation(&bad)
		require.ErrorIs(t, validateTaskWorkspaceSyncBudget(&bad), ErrInvalidConfiguration)
	}
	require.EqualValues(t, 256*(4096+512+2*1024), valid.logicalByteBound())
	valid.SlotsPerPartition, valid.MaxIntentBytes = 64, 16384
	require.EqualValues(t, 256*64*(16384+512+2*1024), valid.logicalByteBound())
}

func workspaceSyncIntentRecordFixture(t *testing.T) TaskWorkspaceSyncIntentRecord {
	t.Helper()
	w := workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")
	ref := TaskWorkspaceSyncIntentReference{Task: TaskReference{Namespace: "/test/scope/cell/", RestoreEpoch: "epoch", TaskID: uuid.NewString(), SandboxID: uuid.NewString(), Partition: w.partition}, BudgetID: uuid.NewString(), IntentID: uuid.NewString()}
	digest := strings.Repeat("a", 64)
	return TaskWorkspaceSyncIntentRecord{taskWorkspaceSyncIntentAttribution: taskWorkspaceSyncIntentAttribution{Version: 1, Reference: ref, BudgetRevision: 1, Kind: "put_object", FinalSyncAttemptID: uuid.NewString(), CommandID: uuid.NewString(), TicketDigest: digest, ArchiveSHA256: digest, PlanDigest: digest, WorkspaceHash: w.hash, Provider: w.provider, StorageIdentityHash: w.storageIdentityHash, Bucket: w.bucket, Prefix: w.prefix, RelativePath: "file", Mode: 0600, Size: 3, Offset: 512, BodySHA256: digest}, Claim: TaskClaimReference{Task: ref.Task, ClaimID: uuid.NewString(), WorkerID: "worker", CreateRevision: 1, LeaseID: 2}}
}

func TestTaskWorkspaceSyncIntentStrictRecords(t *testing.T) {
	record := workspaceSyncIntentRecordFixture(t)
	value, err := encodeWorkspaceSyncIntent(record, 4096)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(value), CreateRevision: 9, ModRevision: 9, Version: 1}
	decoded, err := decodeWorkspaceSyncIntent(kv, 4096)
	require.NoError(t, err)
	require.Equal(t, record, decoded)
	for _, fault := range []string{"lease", "version", "mod", "extra", "duplicate", "missing", "null", "noncanonical", "bound", "claim-extra", "revision"} {
		t.Run(fault, func(t *testing.T) {
			bad := *kv
			switch fault {
			case "lease":
				bad.Lease = 1
			case "version":
				bad.Version = 2
			case "mod":
				bad.ModRevision++
			case "extra":
				bad.Value = []byte(strings.Replace(value, `"version":1`, `"version":1,"extra":0`, 1))
			case "duplicate":
				bad.Value = []byte(strings.Replace(value, `"version":1`, `"version":1,"version":1`, 1))
			case "missing":
				bad.Value = []byte(strings.Replace(value, `"kind":"put_object",`, "", 1))
			case "null":
				bad.Value = []byte(strings.Replace(value, `"claim":{`, `"claim":null,"discard":{`, 1))
			case "noncanonical":
				bad.Value = append([]byte(" "), bad.Value...)
			case "bound":
				bad.Value = []byte(strings.Repeat("x", 4097))
			case "claim-extra":
				bad.Value = []byte(strings.Replace(value, `"claim":{`, `"claim":{"actual_worker_uid":"forged",`, 1))
			case "revision":
				bad.Value = []byte(strings.Replace(value, `"revision":0`, `"revision":9`, 1))
			}
			_, err := decodeWorkspaceSyncIntent(&bad, 4096)
			require.Error(t, err)
		})
	}
	for _, size := range []int{4096, 16384} {
		large := record
		large.RelativePath = strings.Repeat("x", workspaceSyncMaxPathBytes)
		encoded, err := encodeWorkspaceSyncIntent(large, size)
		if size == 4096 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), size)
		}
	}
}

func TestTaskWorkspaceSyncIntentRejectsUnoriginalBeforeRPC(t *testing.T) {
	var source *PreparedTaskWorkspaceSyncSource
	_, err := source.reservePutIntent(context.Background(), nil)
	require.Error(t, err)
	_, source = syncSourceHostIO(t, nil)
	entry := &taskWorkspaceSyncEntry{origin: source, kind: '0'}
	entry.self = entry
	body := &taskWorkspaceSyncBody{source: source, entry: entry, ctx: context.Background()}
	body.self = body
	source.current, source.body = entry, body
	copyBody := taskWorkspaceSyncBody{self: body, source: source, entry: entry, ctx: body.ctx}
	for _, candidate := range []*taskWorkspaceSyncBody{nil, {}, &copyBody} {
		_, err = source.reservePutIntent(context.Background(), candidate)
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	body.bytes = 1
	_, err = source.reservePutIntent(context.Background(), body)
	require.ErrorIs(t, err, ErrInvalidRecord)
	body.bytes, body.closed = 0, true
	_, err = source.reservePutIntent(context.Background(), body)
	require.ErrorIs(t, err, ErrInvalidRecord)
	body.closed = false
	_, err = source.reservePutIntent(context.Background(), body)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}

func TestTaskWorkspaceSyncIntentActualWireBounds(t *testing.T) {
	n, err := NewNamespace("/test", "scope", "cell")
	require.NoError(t, err)
	r := workspaceSyncIntentRecordFixture(t)
	pin := &taskWorkspaceSyncBudgetPin{options: TaskWorkspaceSyncBudgetOptions{r.Reference.BudgetID, 64, 16384}, key: n.Root() + "meta/workspace_sync_budget", value: "cfg", create: 1, mod: 1}
	b := &Backend{namespace: n, identityKey: n.Root() + "meta/identity", restoreKey: n.Root() + "meta/restore_epoch", identityValue: "identity", restoreEpoch: "epoch", workspaceSyncBudget: pin}
	d := &taskWorkspaceSyncIntentDraft{record: r}
	require.NoError(t, b.prepareWorkspaceSyncIntent(d))
	cmps := make([]clientv3.Cmp, 0, 56)
	for i := 0; i < 50; i++ {
		cmps = append(cmps, clientv3.Compare(clientv3.Version(fmt.Sprintf("%shistory/%02d", n.Root(), i)), "=", 1))
	}
	prefix := fmt.Sprintf("%sp/%02x/sandboxes/%s/operations/", n.Root(), r.Reference.Task.Partition, r.Reference.Task.SandboxID)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(prefix)))
	cmps = append(cmps, pin.comparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(d.intentKey), "=", 0), clientv3.Compare(clientv3.Version(d.slotKey), "=", 0))
	keys := []string{b.identityKey, b.restoreKey, pin.key, d.intentKey, d.slotKey}
	require.NoError(t, b.preflightWorkspaceSyncIntent(cmps, d, keys))
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(d.intentKey, d.value, 0), taskProtoPut(d.slotKey, d.slotValue, 0)}, Failure: taskProtoPoints(keys)}
	require.Equal(t, 63, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.LessOrEqual(t, request.Size(), maxMutationBytes)
	t.Logf("host actual protobuf request=%d bytes nodes=63 (root excluded by existing helper)", request.Size())
	request.Failure = append(request.Failure, taskProtoRange(b.identityKey), taskProtoRange(b.restoreKey))
	require.ErrorIs(t, taskCheckWire(n, []*pb.TxnRequest{request}, nil), ErrInvalidMutation)
	request.Failure = request.Failure[:5]
	request.Success[0] = taskProtoPut(d.intentKey, strings.Repeat("x", maxMutationBytes), 0)
	require.ErrorIs(t, taskCheckWire(n, []*pb.TxnRequest{request}, nil), ErrInvalidMutation)
	require.ErrorIs(t, taskCheckWire(n, nil, []*pb.TxnResponse{taskProtoResponse([]string{d.intentKey}, []string{strings.Repeat("x", maxMutationBytes)})}), ErrInvalidMutation)
}

func TestTaskWorkspaceSyncIntentWholePrefixPredicate(t *testing.T) {
	n, err := NewNamespace("/test", "scope", "cell")
	require.NoError(t, err)
	key := n.Root() + "p/ab/sandboxes/sandbox/operations/"
	valid := clientv3.Compare(clientv3.Version(key), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(key))
	c := pb.Compare(valid)
	require.True(t, taskWorkspaceSyncEmptyPrefixComparison(n, &c))
	for _, mutation := range []func(*pb.Compare){
		func(c *pb.Compare) { c.Key = []byte(strings.ReplaceAll(key, "/ab/", "/AB/")) },
		func(c *pb.Compare) { c.Key = []byte(strings.ReplaceAll(key, "operations/", "other/")) },
		func(c *pb.Compare) { c.Key = []byte(strings.ReplaceAll(key, "sandbox/", "../")) },
		func(c *pb.Compare) { c.RangeEnd = []byte(clientv3.GetPrefixRangeEnd(n.Root())) },
		func(c *pb.Compare) { c.Result = pb.Compare_NOT_EQUAL },
		func(c *pb.Compare) { c.TargetUnion = &pb.Compare_Version{Version: 1} },
		func(c *pb.Compare) { c.Target = pb.Compare_CREATE },
		func(c *pb.Compare) { c.Key = []byte("/foreign/p/ab/sandboxes/sandbox/operations/") },
		func(c *pb.Compare) {
			c.Key = []byte(n.Root() + "p/ab/sandboxes/" + strings.Repeat("x", 1024) + "/operations/")
		},
	} {
		bad := pb.Compare(valid)
		mutation(&bad)
		require.False(t, taskWorkspaceSyncEmptyPrefixComparison(n, &bad))
	}
}
