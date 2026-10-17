package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This host protocol model authenticates metadata and exercises the actual
// client RPC boundary. It deliberately configures no physical cleanup client,
// issuer, TaskClaim, worker partition or runtime permission.
type releasedReadTestFixture struct {
	b         *Backend
	server    *workerTestServer
	seed      cleanupBirthTestSeed
	keys      [4]string
	root      ed25519.PrivateKey
	authority string
}

func newReleasedReadTestFixture(t *testing.T) *releasedReadTestFixture {
	t.Helper()
	server, options := workerTestEndpoint(t)
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	options.WorkerControl = nil
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "released-read", Roots: []ed25519.PublicKey{root}}
	options.Clock = goodAuthorityClock()
	options.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 1}
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })

	seed := seedCleanupBirth(t, &backfillBootstrapFixture{b: b, server: server})
	// The generic birth fixture permits opaque runtime identities. Signed
	// termination requires the production UUID profile, fixed before this
	// fixture's actual immutable cohort is created at its common revision.
	seed.task.CreationIntentID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	seed.task.Runtime.UID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	seed.task.Runtime.BootID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	seed.intent.Task = seed.task
	seed.wires[0], err = encodeTaskRecord(seed.task)
	require.NoError(t, err)
	seed.wires[1], err = encodeCleanupIntentRecord(seed.intent)
	require.NoError(t, err)
	var deletes, puts []clientv3.Op
	for i := 0; i < 4; i++ {
		deletes = append(deletes, clientv3.OpDelete(seed.keys[i]))
		puts = append(puts, clientv3.OpPut(seed.keys[i], seed.wires[i]))
	}
	_, err = b.client.Txn(context.Background()).Then(deletes...).Commit()
	require.NoError(t, err)
	cohort, err := b.client.Txn(context.Background()).Then(puts...).Commit()
	require.NoError(t, err)
	seed.birth = cohort.Header.Revision
	keys, err := b.readViewKeys(seed.task.WorkspaceHash, seed.task.Reference.SandboxID)
	require.NoError(t, err)
	f := &releasedReadTestFixture{b: b, server: server, seed: seed, keys: keys, root: rootKey, authority: options.Identity.RuntimeID}
	return f
}

func (f *releasedReadTestFixture) seedCurrent(t *testing.T, phase SandboxPhase) {
	t.Helper()
	task := f.seed.task
	owner := WorkspaceOwnerRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, SandboxID: task.Reference.SandboxID, IntentID: task.CreationIntentID, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, Runtime: &task.Runtime}
	fence := WorkspaceFenceRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation}
	placement := SandboxPlacementRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: task.Reference.RestoreEpoch, Partition: task.Reference.Partition, Generation: task.Generation}
	control := SandboxControlRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: task.Reference.RestoreEpoch, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, Phase: phase, Snapshot: task.Snapshot, Runtime: &task.Runtime, ExpiresAt: task.ExpiresAt}

	ops := make([]clientv3.Op, 4)
	for i, record := range []interface{ Validate() error }{&owner, &fence, &placement, &control} {
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		ops[i] = clientv3.OpPut(f.keys[i], wire)
	}
	_, err := f.b.client.Txn(context.Background()).Then(ops...).Commit()
	require.NoError(t, err)
}

func TestSandboxReleasedReadInput(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		result, err := b.ReadSandboxReleased(context.Background(), "sandbox")
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	f := newReleasedReadTestFixture(t)
	_, err := f.b.ReadSandboxReleased(nil, f.seed.task.Reference.SandboxID)
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = f.b.ReadSandboxReleased(context.Background(), "../outside")
	require.ErrorIs(t, err, ErrInvalidRecord)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.b.ReadSandboxReleased(ctx, f.seed.task.Reference.SandboxID)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSandboxReleasedReadSharedCapacity(t *testing.T) {
	f := newReleasedReadTestFixture(t)
	var calls []*snapshotReadCall
	t.Cleanup(func() {
		for _, call := range calls {
			call.finish()
		}
	})
	for i := 0; i < 256; i++ {
		call, err := f.b.registerSnapshotRead(context.Background())
		require.NoError(t, err)
		calls = append(calls, call)
	}
	before := len(backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server}))
	result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
	require.Nil(t, result)
	require.ErrorIs(t, err, ErrSnapshotReadCapacity)
	require.Len(t, backfillBootstrapRequests(&backfillBootstrapFixture{server: f.server}), before, "capacity refusal must precede all native reads")
}

func TestSandboxReleasedReadCurrent(t *testing.T) {
	for _, phase := range []SandboxPhase{PhaseActive, PhaseDestroying, PhaseCleanupPending} {
		t.Run(string(phase), func(t *testing.T) {
			f := newReleasedReadTestFixture(t)
			f.seedCurrent(t, phase)
			before := f.server.point(f.keys[2])
			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.NoError(t, err)
			require.Nil(t, result, "complete current ownership must never authenticate Released")
			require.Equal(t, before, f.server.point(f.keys[2]))
			view, err := f.b.ReadSandboxView(context.Background(), f.seed.task.Reference.SandboxID)
			require.NoError(t, err, "the original four-point reader retains its current-state contract")
			require.NotNil(t, view)
		})
	}
}

func TestSandboxReleasedReadCurrentCorruption(t *testing.T) {
	for _, fault := range []string{"missing-owner", "missing-control", "missing-fence", "owner-generation", "owner-epoch", "owner-lease", "owner-json", "control-json", "link-json"} {
		t.Run(fault, func(t *testing.T) {
			f := newReleasedReadTestFixture(t)
			f.seedCurrent(t, PhaseActive)
			want := ErrCorruptRecord
			switch fault {
			case "missing-owner", "missing-control", "missing-fence":
				i := map[string]int{"missing-owner": 0, "missing-fence": 1, "missing-control": 3}[fault]
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(f.keys[i])).Commit()
				require.NoError(t, err)
			case "owner-generation", "owner-epoch":
				var owner WorkspaceOwnerRecord
				require.NoError(t, decodeDomainRecord(f.server.point(f.keys[0]), &owner))
				if fault == "owner-generation" {
					owner.Generation++
				} else {
					owner.RestoreEpoch = "other-epoch"
					want = ErrIdentityMismatch
				}
				wire, err := encodeDomainRecord(owner)
				require.NoError(t, err)
				f.server.put(f.keys[0], wire, 0)
			case "owner-lease":
				f.server.put(f.keys[0], string(f.server.point(f.keys[0]).Value), 999)
			case "owner-json":
				f.server.put(f.keys[0], "{}", 0)
			case "control-json":
				f.server.put(f.keys[3], "{}", 0)
			case "link-json":
				f.server.put(f.seed.keys[2], "{}", 0)
			}

			result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
			require.Nil(t, result)
			require.ErrorIs(t, err, want, "a false Released observation must not hide corrupt current records")
		})
	}
}

func TestSandboxReleasedReadUnsealedHistory(t *testing.T) {
	f := newReleasedReadTestFixture(t)
	f.seedCurrent(t, PhaseCleanupPending)
	_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(f.keys[0]), clientv3.OpDelete(f.keys[3])).Commit()
	require.NoError(t, err)
	f.server.put(f.seed.keys[6], cleanupBirthTestRelease(t, f.seed), 0)
	result, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
	require.Nil(t, result)
	require.Error(t, err, "codec-valid release metadata without signed termination is never Released")
	view, err := f.b.ReadSandboxView(context.Background(), f.seed.task.Reference.SandboxID)
	require.Nil(t, view)
	require.ErrorIs(t, err, ErrCorruptRecord, "the original reader must still reject released holes")
}

func TestSandboxReleasedReadCloseJoinsClient(t *testing.T) {
	f := newReleasedReadTestFixture(t)
	f.seedCurrent(t, PhaseActive)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 1 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		entered <- struct{}{}
		<-release
		return r, err
	}}
	done, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := f.b.ReadSandboxReleased(context.Background(), f.seed.task.Reference.SandboxID)
		done <- err
	}()
	workerTestReceive(t, entered)
	go func() { closed <- f.b.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while its original client call was blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	require.ErrorIs(t, workerTestReceive(t, done), context.Canceled)
	require.NoError(t, workerTestReceive(t, closed))
}

// These fixed profile caps account for the entire successful eight-RPC path,
// using maximal 1024-byte keys and protobuf scalars. Runtime dispatch must
// additionally preflight each concrete request and reject arbitrary profiles.
func TestSandboxReleasedReadWireBudget(t *testing.T) {
	caps := [][]int{{4096}, {4096, 1024, 4096, 2048}, {4096}, {4096, 4096, 2048, 2048}, {8192}, {16384, 16384}, {4096, 8192, 32768, 4096, 8192, 8192, 8192, maxRecordBytes, 8192}, {4096, 1024, 4096, 4096, 4096, 4096, 4096}}
	total := 0
	for i, profile := range caps {
		// Enrolled readers include identity/restore points in their replies.
		profile = append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, profile...)
		reply := cleanupBirthPointReply(profile)
		require.LessOrEqual(t, reply.Size(), 256*1024, "RPC %d", i+1)
		total += reply.Size()
	}
	require.LessOrEqual(t, total, 512*1024)
	t.Logf("maximal-key eight-RPC response envelope: %d bytes", total)

	// A generic domain profile must retain its original four-pin restriction.
	o := validOptions(t)
	b := &Backend{namespace: o.Namespace}
	b.identityKey, _ = b.namespace.Key("meta", "identity")
	b.restoreKey, _ = b.namespace.Key("meta", "restore_epoch")
	key := b.namespace.Root() + "retained-history"
	pins := make([]clientv3.Cmp, 5)
	for i := range pins {
		pins[i] = clientv3.Compare(clientv3.ModRevision(key), "=", int64(1))
	}
	require.ErrorIs(t, b.readViewPreflight(nil, nil, pins), ErrInvalidRecord)
}
