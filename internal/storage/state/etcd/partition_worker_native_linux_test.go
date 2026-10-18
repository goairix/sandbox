package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

const partitionWorkerNativeFlag = "SANDBOX_PARTITION_WORKER_CONTROL_NATIVE"
const partitionWorkerNativeChildFlag = "SANDBOX_PARTITION_WORKER_CONTROL_CHILD"

// Output is bounded and read only after the original Cmd.Wait has joined its
// writers. This fixture has one Wait owner for every successfully started PID.
type partitionWorkerNativeOutput struct {
	buffer  bytes.Buffer
	dropped int
}

func (o *partitionWorkerNativeOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 64*1024 - o.buffer.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = o.buffer.Write(p[:remaining])
	}
	o.dropped += n - remaining
	return n, nil
}

type partitionWorkerNativeProcess struct {
	cmd            *exec.Cmd
	done           chan struct{}
	err            error
	stdout, stderr partitionWorkerNativeOutput
	stopOnce       sync.Once
	t              *testing.T
}

func startPartitionWorkerNativeProcess(t *testing.T, cmd *exec.Cmd) *partitionWorkerNativeProcess {
	t.Helper()
	p := &partitionWorkerNativeProcess{cmd: cmd, done: make(chan struct{}), t: t}
	cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
	require.NoError(t, cmd.Start())
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(p.stop)
	return p
}

func (p *partitionWorkerNativeProcess) stop() {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
		default:
			if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				p.t.Errorf("kill owned PID %d: %v", p.cmd.Process.Pid, err)
			}
		}
		<-p.done
		p.t.Logf("owned child PID=%d Wait=%v stdout_dropped=%d stderr_dropped=%d", p.cmd.Process.Pid, p.err, p.stdout.dropped, p.stderr.dropped)
		if p.stdout.buffer.Len() != 0 {
			p.t.Logf("joined stdout: %s", p.stdout.buffer.String())
		}
		if p.stderr.buffer.Len() != 0 {
			p.t.Logf("joined stderr: %s", p.stderr.buffer.String())
		}
	})
}

func partitionWorkerNativeRequest(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 3*time.Second)
}

func partitionWorkerNativePause(t *testing.T, ctx context.Context, delay time.Duration) {
	t.Helper()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatalf("bounded native wait: %v", ctx.Err())
	}
}

func startPartitionWorkerNativeCluster(t *testing.T, ctx context.Context) []string {
	t.Helper()
	endpoints, _ := startPartitionWorkerNativeClusterWithData(t, ctx, t.TempDir(), 0)
	return endpoints
}

// Scale tests use a runner-owned disk path and explicit backend quota. The
// original small native fixtures retain their default temp directory/quota.
// Every child is still joined by its original process owner before data removal.
func startPartitionWorkerNativeClusterWithData(t *testing.T, ctx context.Context, root string, quota int64) ([]string, []*partitionWorkerNativeProcess) {
	t.Helper()
	clientURLs := []string{"http://127.0.0.1:23791", "http://127.0.0.1:23792", "http://127.0.0.1:23793"}
	peerURLs := []string{"http://127.0.0.1:23801", "http://127.0.0.1:23802", "http://127.0.0.1:23803"}
	var members []string
	for i := range peerURLs {
		members = append(members, fmt.Sprintf("worker-etcd-%d=%s", i+1, peerURLs[i]))
	}
	token := uuid.NewString()
	var children []*partitionWorkerNativeProcess
	for i := range clientURLs {
		cmd := exec.Command("/fixture/etcd", "--name", fmt.Sprintf("worker-etcd-%d", i+1), "--data-dir", filepath.Join(root, fmt.Sprintf("member-%d", i+1)), "--listen-client-urls", clientURLs[i], "--advertise-client-urls", clientURLs[i], "--listen-peer-urls", peerURLs[i], "--initial-advertise-peer-urls", peerURLs[i], "--initial-cluster", strings.Join(members, ","), "--initial-cluster-state", "new", "--initial-cluster-token", token, "--logger", "zap", "--log-level", "error")
		if quota > 0 {
			cmd.Args = append(cmd.Args, fmt.Sprintf("--quota-backend-bytes=%d", quota))
		}
		children = append(children, startPartitionWorkerNativeProcess(t, cmd))
	}
	raw, err := clientv3.New(clientv3.Config{Endpoints: clientURLs, DialTimeout: time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		for _, child := range children {
			select {
			case <-child.done:
				child.stop() // Wait is joined before any output is inspected.
				t.Fatalf("owned etcd PID %d exited during startup: %v", child.cmd.Process.Pid, child.err)
			default:
			}
		}
		request, stop := context.WithTimeout(startup, 500*time.Millisecond)
		list, err := raw.MemberList(request)
		stop()
		ready := err == nil && list.Header != nil && list.Header.ClusterId != 0 && len(list.Members) == 3
		seen := make(map[uint64]bool, 3)
		if ready {
			for _, endpoint := range clientURLs {
				request, stop = context.WithTimeout(startup, 500*time.Millisecond)
				status, e := raw.Status(request, endpoint)
				stop()
				if e != nil || status.Header == nil || status.Header.ClusterId != list.Header.ClusterId || status.Header.MemberId == 0 || status.Version != "3.6.14" {
					ready = false
					break
				}
				seen[status.Header.MemberId] = true
			}
		}
		if ready && len(seen) == 3 {
			return clientURLs, children
		}
		if startup.Err() != nil {
			t.Fatalf("three actual etcd 3.6.14 members did not become ready: %v", startup.Err())
		}
		partitionWorkerNativePause(t, startup, 100*time.Millisecond)
	}
}

func partitionWorkerNativeBackend(t *testing.T, ctx context.Context, options Options) *Backend {
	t.Helper()
	b, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b
}

func partitionWorkerNativeTTL(t *testing.T, ctx context.Context, raw *clientv3.Client, s *WorkerSession) int64 {
	t.Helper()
	request, stop := partitionWorkerNativeRequest(ctx)
	defer stop()
	r, err := raw.TimeToLive(request, clientv3.LeaseID(s.Reference().LeaseID))
	require.NoError(t, err)
	require.NotNil(t, r.ResponseHeader)
	require.Greater(t, r.TTL, int64(0))
	require.GreaterOrEqual(t, r.GrantedTTL, int64(3))
	require.LessOrEqual(t, r.GrantedTTL, int64(6), "the fixture's actual native minimum must fit the frozen expiry window; production permits actual TTL up to 60s")
	require.Equal(t, s.ttlSeconds, r.GrantedTTL)
	return r.GrantedTTL
}

func TestPartitionWorkerControlNative(t *testing.T) {
	if os.Getenv(partitionWorkerNativeFlag) != "1" {
		t.Skip("requires the fresh isolated partition-worker native collector")
	}
	require.Equal(t, "arm64", runtime.GOARCH)
	if input := os.Getenv(partitionWorkerNativeChildFlag); input != "" {
		partitionWorkerNativeChild(t, input)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	endpoints := startPartitionWorkerNativeCluster(t, ctx)
	t.Setenv("TEST_ETCD_ENDPOINTS", strings.Join(endpoints, ","))
	seed, raw := integrationBackend(t) // Validates three actual distinct members.
	options := integrationOptions(seed, raw)
	options.RequestTimeout = 3 * time.Second
	options.WorkerControl = &WorkerControlOptions{SessionTTL: 3 * time.Second}
	profile := workerProfileRecord{Version: 1, ProfileID: workerProfileID, Namespace: seed.namespace.Root(), RestoreEpoch: seed.restoreEpoch, IdentityDigest: workerIdentityDigest(seed.identityValue), PartitionCount: 256, RelistSlots: 4, RangeSlots: 20, RangeBytesPerSecond: 1048576, RangeBurstBytes: 262144, ClaimSlots: 100, ClaimPerSecond: 1, ClaimBurst: 1}
	wire, err := encodeOperationMetadata(profile, workerRecordBytes)
	require.NoError(t, err)
	profileKey, err := seed.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	request, stop := partitionWorkerNativeRequest(ctx)
	provisioned, err := raw.Txn(request).If(append(seed.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(profileKey), "=", 0))...).Then(clientv3.OpPut(profileKey, wire)).Commit()
	stop()
	require.NoError(t, err)
	require.True(t, provisioned.Succeeded)
	require.NotNil(t, provisioned.Header)

	t.Run("competition-original-renew-revoke", func(t *testing.T) {
		backends := []*Backend{partitionWorkerNativeBackend(t, ctx, options), partitionWorkerNativeBackend(t, ctx, options)}
		var sessions [2]*WorkerSession
		for i, b := range backends {
			sessions[i], err = b.OpenWorkerSession(ctx, "reused-worker-id")
			require.NoError(t, err)
			partitionWorkerNativeTTL(t, ctx, raw, sessions[i])
		}
		require.NotEqual(t, sessions[0].Reference().SessionID, sessions[1].Reference().SessionID)
		type result struct {
			index int
			claim *WorkerPartitionClaim
			err   error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for i, b := range backends {
			go func(index int, backend *Backend) {
				<-start
				p, e := backend.ClaimWorkerPartition(ctx, sessions[index], 0x42)
				results <- result{index, p, e}
			}(i, b)
		}
		close(start)
		first, second := workerTestReceive(t, results), workerTestReceive(t, results)
		winner, loser := first, second
		if winner.err != nil {
			winner, loser = loser, winner
		}
		require.NoError(t, winner.err)
		require.NotNil(t, winner.claim)
		require.ErrorIs(t, loser.err, ErrConflict)
		require.Nil(t, loser.claim)
		original := sessions[winner.index].Reference()
		require.NoError(t, backends[winner.index].RenewWorkerSession(ctx, sessions[winner.index]))
		require.Equal(t, original, sessions[winner.index].Reference())
		partitionWorkerNativeTTL(t, ctx, raw, sessions[winner.index])
		require.NoError(t, backends[loser.index].RenewWorkerSession(ctx, sessions[loser.index]))
		require.NoError(t, backends[winner.index].ReleaseWorkerSession(ctx, sessions[winner.index]))
		successor, err := backends[loser.index].ClaimWorkerPartition(ctx, sessions[loser.index], 0x42)
		require.NoError(t, err)
		require.Greater(t, successor.Reference().CreateRevision, winner.claim.Reference().CreateRevision)
		_ = backends[winner.index].ReleaseWorkerPartition(ctx, winner.claim)
		key, _ := seed.namespace.workerPartitionKey(0x42)
		request, stop := partitionWorkerNativeRequest(ctx)
		point, err := raw.Get(request, key)
		stop()
		require.NoError(t, err)
		require.Len(t, point.Kvs, 1)
		require.Equal(t, sessions[loser.index].Reference().LeaseID, point.Kvs[0].Lease)
		t.Logf("native partition CAS winner_session=%s original_lease=%d successor_session=%s", original.SessionID, original.LeaseID, sessions[loser.index].Reference().SessionID)
	})

	t.Run("fixed-shares-and-earned-credit", func(t *testing.T) {
		backends := []*Backend{partitionWorkerNativeBackend(t, ctx, options), partitionWorkerNativeBackend(t, ctx, options)}
		var sessions [5]*WorkerSession
		var shares [4]*WorkerBudgetShare
		for i := range sessions {
			b := backends[i%2]
			sessions[i], err = b.OpenWorkerSession(ctx, fmt.Sprintf("budget-worker-%d", i))
			require.NoError(t, err)
			partitionWorkerNativeTTL(t, ctx, raw, sessions[i])
			if i < len(shares) {
				shares[i], err = b.AcquireWorkerBudget(ctx, sessions[i], WorkerBudgetRelist, uint16(i))
				require.NoError(t, err)
				again, err := b.AcquireWorkerBudget(ctx, sessions[i], WorkerBudgetRelist, uint16(i))
				require.NoError(t, err)
				require.Same(t, shares[i], again)
			}
		}
		b, s := backends[0], sessions[4]
		for slot := uint16(0); slot < 4; slot++ {
			_, err := b.AcquireWorkerBudget(ctx, s, WorkerBudgetRelist, slot)
			require.ErrorIs(t, err, ErrConflict)
		}
		require.NoError(t, backends[0].RenewWorkerSession(ctx, sessions[0]))
		require.NoError(t, backends[0].ReleaseWorkerBudget(ctx, shares[0]))
		relist, err := b.AcquireWorkerBudget(ctx, s, WorkerBudgetRelist, 0)
		require.NoError(t, err)
		var rates [2]*WorkerBudgetShare
		for i, kind := range []WorkerBudgetKind{WorkerBudgetRange, WorkerBudgetClaim} {
			for slot := uint16(0); slot < 2; slot++ {
				share, err := b.AcquireWorkerBudget(ctx, s, kind, slot)
				require.NoError(t, err)
				b.archiveLife.mu.Lock()
				initial := share.bucket.tokens
				b.archiveLife.mu.Unlock()
				require.Zero(t, initial, "new shares publish an empty bucket; naturally elapsed time may earn later credit")
				require.Equal(t, s.Reference().LeaseID, share.Reference().Session.LeaseID)
				if slot == 0 {
					rates[i] = share
				}
			}
		}
		p, err := b.ClaimWorkerPartition(ctx, s, 0x43)
		require.NoError(t, err)
		require.NoError(t, b.RunWorkerRelist(ctx, p, relist, func(context.Context) error { return nil }))
		require.NoError(t, b.RenewWorkerSession(ctx, s))
		partitionWorkerNativePause(t, ctx, 1100*time.Millisecond)
		for i, amount := range []uint32{262144, 1} {
			charged := time.Now()
			require.NoError(t, b.ConsumeWorkerBudget(ctx, rates[i], amount))
			err := b.ConsumeWorkerBudget(ctx, rates[i], amount)
			if err == nil {
				minimum := 250 * time.Millisecond
				if i == 1 {
					minimum = time.Second
				}
				require.GreaterOrEqual(t, time.Since(charged), minimum, "a repeated full charge must be earned by actual elapsed time")
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
		}
		t.Log("native four fixed relist slots competed across two backends; one session held multiple original rate shares")
	})

	t.Run("actual-worker-kill-wait-expiry", func(t *testing.T) { partitionWorkerNativeCrash(t, ctx, seed, raw, options) })

	for _, changed := range []string{"guard", "restore", "identity"} {
		t.Run("original-fence/"+changed, func(t *testing.T) {
			b := partitionWorkerNativeBackend(t, ctx, options)
			s, err := b.OpenWorkerSession(ctx, "fence-worker")
			require.NoError(t, err)
			partitionWorkerNativeTTL(t, ctx, raw, s)
			key := b.identityKey
			if changed == "guard" {
				key, _ = b.namespace.workerSessionKey(s.Reference().SessionID)
			}
			if changed == "restore" {
				key = b.restoreKey
			}
			request, stop := partitionWorkerNativeRequest(ctx)
			before, err := raw.Get(request, key)
			stop()
			require.NoError(t, err)
			require.Len(t, before.Kvs, 1)
			old := before.Kvs[0]
			var restoreOnce sync.Once
			var restoreErr error
			restore := func() error {
				restoreOnce.Do(func() {
					request, stop := context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					_, restoreErr = raw.Put(request, key, string(old.Value), clientv3.WithLease(clientv3.LeaseID(old.Lease)))
				})
				return restoreErr
			}
			t.Cleanup(func() { require.NoError(t, restore()) })
			value := string(old.Value)
			if changed == "identity" {
				value += " "
			}
			if changed == "restore" {
				value += "-changed"
			}
			request, stop = partitionWorkerNativeRequest(ctx)
			_, err = raw.Put(request, key, value, clientv3.WithLease(clientv3.LeaseID(old.Lease)))
			stop()
			require.NoError(t, err)
			require.Error(t, b.RenewWorkerSession(ctx, s))
			require.NoError(t, restore())
			require.Error(t, b.RenewWorkerSession(ctx, s), "restoring bytes cannot revive an original lost session")
			t.Logf("native original %s fence rejected and remained terminal", changed)
		})
	}
	request, stop = partitionWorkerNativeRequest(ctx)
	finalProfile, err := raw.Get(request, profileKey)
	stop()
	require.NoError(t, err)
	require.Len(t, finalProfile.Kvs, 1)
	require.Equal(t, wire, string(finalProfile.Kvs[0].Value))
	require.Equal(t, int64(0), finalProfile.Kvs[0].Lease)
	require.Equal(t, int64(1), finalProfile.Kvs[0].Version)
	require.Equal(t, provisioned.Header.Revision, finalProfile.Kvs[0].CreateRevision)
	require.Equal(t, finalProfile.Kvs[0].CreateRevision, finalProfile.Kvs[0].ModRevision)
	require.NoError(t, ctx.Err())
}

type partitionWorkerNativeChildInput struct {
	Endpoints                   []string
	Prefix, Scope, Cell         string
	Identity                    Identity
	RestoreEpoch, Report, Nonce string
}

type partitionWorkerNativeChildReport struct {
	Nonce      string
	Session    WorkerSessionReference
	Partition  WorkerPartitionReference
	GrantedTTL int64
}

func partitionWorkerNativeChild(t *testing.T, input string) {
	var in partitionWorkerNativeChildInput
	require.NoError(t, json.Unmarshal([]byte(input), &in))
	require.True(t, validPreparationUUID(in.Nonce))
	require.True(t, filepath.IsAbs(in.Report) && filepath.Clean(in.Report) == in.Report && strings.HasPrefix(in.Report, "/tmp/") && filepath.Base(in.Report) == "worker-report.json")
	n, err := NewNamespace(in.Prefix, in.Scope, in.Cell)
	require.NoError(t, err)
	in.Identity.RestoreEpoch = in.RestoreEpoch
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	b, err := New(ctx, Options{Endpoints: in.Endpoints, Namespace: n, Identity: in.Identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, WorkerControl: &WorkerControlOptions{SessionTTL: 3 * time.Second}})
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	s, err := b.OpenWorkerSession(ctx, "native-crash-worker")
	require.NoError(t, err)
	p, err := b.ClaimWorkerPartition(ctx, s, 0xd3)
	require.NoError(t, err)
	require.LessOrEqual(t, s.ttlSeconds, int64(6))
	report, err := json.Marshal(partitionWorkerNativeChildReport{Nonce: in.Nonce, Session: s.Reference(), Partition: p.Reference(), GrantedTTL: s.ttlSeconds})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(in.Report+".tmp", report, 0600))
	require.NoError(t, os.Rename(in.Report+".tmp", in.Report))
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("parent did not kill its owned worker within the bounded child lifetime")
		case <-ticker.C:
			require.NoError(t, b.RenewWorkerSession(ctx, s))
		}
	}
}

func partitionWorkerNativeCrash(t *testing.T, ctx context.Context, seed *Backend, raw *clientv3.Client, options Options) {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "worker-report.json")
	in := partitionWorkerNativeChildInput{Endpoints: options.Endpoints, Prefix: seed.namespace.prefix, Scope: seed.namespace.scope, Cell: seed.namespace.cell, Identity: options.Identity, RestoreEpoch: seed.restoreEpoch, Report: reportPath, Nonce: uuid.NewString()}
	encoded, err := json.Marshal(in)
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(executable, "-test.run", "^TestPartitionWorkerControlNative$", "-test.v", "-test.timeout", "30s")
	cmd.Env = append(os.Environ(), partitionWorkerNativeChildFlag+"="+string(encoded))
	child := startPartitionWorkerNativeProcess(t, cmd)
	ready, stopReady := context.WithTimeout(ctx, 10*time.Second)
	defer stopReady()
	var report partitionWorkerNativeChildReport
	for {
		wire, e := os.ReadFile(reportPath)
		if e == nil {
			require.NoError(t, json.Unmarshal(wire, &report))
			break
		}
		require.True(t, os.IsNotExist(e), "read owned report: %v", e)
		select {
		case <-child.done:
			child.stop()
			t.Fatalf("owned worker exited before its original report: %v", child.err)
		default:
		}
		partitionWorkerNativePause(t, ready, 50*time.Millisecond)
	}
	require.Equal(t, in.Nonce, report.Nonce)
	require.Equal(t, seed.namespace.Root(), report.Session.Namespace)
	require.Equal(t, seed.restoreEpoch, report.Session.RestoreEpoch)
	require.Equal(t, "native-crash-worker", report.Session.WorkerID)
	require.Equal(t, report.Session, report.Partition.Session)
	require.Equal(t, uint8(0xd3), report.Partition.Partition)
	require.GreaterOrEqual(t, report.GrantedTTL, int64(3))
	require.LessOrEqual(t, report.GrantedTTL, int64(6))
	guardKey, err := seed.namespace.workerSessionKey(report.Session.SessionID)
	require.NoError(t, err)
	partitionKey, err := seed.namespace.workerPartitionKey(0xd3)
	require.NoError(t, err)
	request, stop := partitionWorkerNativeRequest(ctx)
	guard, err := raw.Get(request, guardKey)
	stop()
	require.NoError(t, err)
	require.Len(t, guard.Kvs, 1)
	var guardRecord workerSessionRecord
	require.NoError(t, decodeWorkerSessionRecord(seed.namespace, guard.Kvs[0], &guardRecord))
	require.Equal(t, report.Session.LeaseID, guard.Kvs[0].Lease)
	require.Equal(t, report.Session.CreateRevision, guard.Kvs[0].CreateRevision)
	request, stop = partitionWorkerNativeRequest(ctx)
	partition, err := raw.Get(request, partitionKey)
	stop()
	require.NoError(t, err)
	require.Len(t, partition.Kvs, 1)
	var partitionRecord workerPartitionRecord
	require.NoError(t, decodeWorkerPartitionRecord(seed.namespace, partition.Kvs[0], &partitionRecord))
	require.Equal(t, report.Session.LeaseID, partition.Kvs[0].Lease)
	require.Equal(t, report.Partition.CreateRevision, partition.Kvs[0].CreateRevision)
	b := partitionWorkerNativeBackend(t, ctx, options)
	contender, err := b.OpenWorkerSession(ctx, "native-crash-worker")
	require.NoError(t, err)
	_, err = b.ClaimWorkerPartition(ctx, contender, 0xd3)
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, b.ReleaseWorkerSession(ctx, contender))
	child.stop() // Actual SIGKILL and original Cmd.Wait; no graceful Lease revoke.
	var killed *exec.ExitError
	require.ErrorAs(t, child.err, &killed)
	status, ok := child.cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Equal(t, syscall.SIGKILL, status.Signal())
	expiry, cancelExpiry := context.WithTimeout(ctx, time.Duration(report.GrantedTTL)*time.Second+5*time.Second)
	defer cancelExpiry()
	for {
		request, stop := partitionWorkerNativeRequest(expiry)
		ttl, e := raw.TimeToLive(request, clientv3.LeaseID(report.Session.LeaseID))
		stop()
		require.NoError(t, e)
		if ttl.TTL < 0 {
			request, stop = partitionWorkerNativeRequest(expiry)
			points, e := raw.Txn(request).Then(clientv3.OpGet(guardKey), clientv3.OpGet(partitionKey)).Commit()
			stop()
			require.NoError(t, e)
			require.Len(t, points.Responses, 2)
			require.Empty(t, points.Responses[0].GetResponseRange().Kvs)
			require.Empty(t, points.Responses[1].GetResponseRange().Kvs)
			break
		}
		partitionWorkerNativePause(t, expiry, 100*time.Millisecond)
	}
	successor, err := b.OpenWorkerSession(ctx, "native-crash-worker")
	require.NoError(t, err)
	require.NotEqual(t, report.Session.SessionID, successor.Reference().SessionID)
	require.NotEqual(t, report.Session.LeaseID, successor.Reference().LeaseID)
	p, err := b.ClaimWorkerPartition(ctx, successor, 0xd3)
	require.NoError(t, err)
	require.Greater(t, p.Reference().CreateRevision, report.Partition.CreateRevision)
	t.Logf("native actual worker PID=%d SIGKILL+Wait original_lease=%d granted_TTL=%d expired; new_session=%s took partition d3", child.cmd.Process.Pid, report.Session.LeaseID, report.GrantedTTL, successor.Reference().SessionID)
}
