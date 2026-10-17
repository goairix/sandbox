//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type cleanupExecutorCloseResult struct {
	task   state.TaskReference
	result state.CleanupLocalResult
	err    error
}

// The original Task is real; the additional references exercise only public
// queue syntax and capacity. They carry no Task history or runtime authority.
// This worker uses the genuine Kernel/profile built by the common initializer.
func cleanupExecutorCloseWorker(t *testing.T, parent context.Context, s cleanupExecutorSetup, options state.Options, tlsConfig *tls.Config) {
	t.Helper()
	require.Contains(t, []string{"stop", "backend-close"}, s.CloseMode)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	proxy := newCleanupExecutorEtcdProxy(t, tlsConfig, s.Task, "close-join")
	options.Endpoints = []string{proxy.endpoint()}
	options.RequestTimeout = 3 * time.Second
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	shares := make([]uint16, 32)
	for i := range shares {
		shares[i] = uint16(i)
	}
	w, err := b.StartDueWorker(ctx, state.DueWorkerOptions{WorkerID: "cleanup-close-" + s.CloseMode, Partitions: []uint8{s.Task.Partition}, RangeSlots: []uint16{0}, ClaimSlots: shares, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	defer w.Stop()

	results := make(chan cleanupExecutorCloseResult, 33)
	var executes sync.WaitGroup
	var closeDone chan struct{}
	// Caller cancellation only requests shutdown. Even a Fatal path releases
	// the remote barrier, collects actual Execute returns and joins Stop/Close.
	defer func() {
		cancel()
		proxy.unblockCloseRead()
		_ = w.Stop()
		executes.Wait()
		if closeDone != nil {
			<-closeDone
		}
		proxy.close(t)
	}()
	start := func(task state.TaskReference) {
		executes.Add(1)
		go func() {
			defer executes.Done()
			result, err := w.ExecuteCleanupLocal(ctx, task)
			results <- cleanupExecutorCloseResult{task: task, result: result, err: err}
		}()
	}
	start(s.Task)
	select {
	case <-proxy.closeEntered:
	case early := <-results:
		t.Fatalf("real Task returned before its actual admission read: %+v error=%v", early.result, early.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	// One admitting job occupies the sole actor. Exactly one of the next32
	// public calls must fail; that refusal witnesses a full table without a
	// private getter or guessing which goroutine won the registration race.
	queueDeadline, stopQueue := context.WithTimeout(ctx, 2*time.Second)
	defer stopQueue()
	for i := 0; i < 32; i++ {
		ref := s.Task
		ref.TaskID = uuid.NewString()
		start(ref)
	}
	var refused cleanupExecutorCloseResult
	select {
	case refused = <-results:
		require.ErrorIs(t, refused.err, state.ErrConflict)
		require.NotEqual(t, s.Task, refused.task)
	case <-queueDeadline.Done():
		t.Fatal("public queue did not reach exactly32 registered calls within the read barrier")
	}
	before := proxy.snapshot()
	retryCtx, stopRetry := context.WithTimeout(queueDeadline, 500*time.Millisecond)
	_, retryErr := w.ExecuteCleanupLocal(retryCtx, refused.task)
	stopRetry()
	require.ErrorIs(t, retryErr, state.ErrConflict)
	afterRetry := proxy.snapshot()
	// Background Watch/KeepAlive stream counts are not Task admission. Check
	// only the bounded calls/effects whose absence the queue gate promises.
	require.Equal(t, before.AdmissionReads, afterRetry.AdmissionReads)
	require.Equal(t, before.GrantIDs, afterRetry.GrantIDs)
	require.Equal(t, before.ClaimPuts, afterRetry.ClaimPuts)
	require.Equal(t, before.ArchivePuts, afterRetry.ArchivePuts)
	require.Equal(t, before.FaultMatched, afterRetry.FaultMatched)
	require.Equal(t, before.FaultCommitted, afterRetry.FaultCommitted)
	require.Equal(t, before.FaultJoined, afterRetry.FaultJoined)
	require.Equal(t, before.CloseReadJoined, afterRetry.CloseReadJoined)
	select {
	case extra := <-results:
		t.Fatalf("expected32 retained public calls, got another return: %+v error=%v", extra.result, extra.err)
	case <-proxy.closeReturned:
		t.Fatal("RPC barrier expired before capacity and close observations")
	default:
	}
	require.Len(t, before.GrantIDs, 1, "only original worker-session Lease exists")
	require.Zero(t, before.ClaimPuts)
	require.Zero(t, before.ArchivePuts)
	require.Equal(t, 1, before.AdmissionReads)
	t.Logf("close-join mode=%s public32 registered (1 admitting +31 queued), unique33 refused, same-ref retry refused with native counters unchanged: %+v", s.CloseMode, before)

	closed := make(chan error, 1)
	closeDone = make(chan struct{})
	go func() {
		defer close(closeDone)
		if s.CloseMode == "stop" {
			closed <- w.Stop()
		} else {
			closed <- b.Close()
		}
	}()
	// Do not release the server barrier first: the original close operation
	// must cancel its owned RPC/jobs. Remote handler joining is checked below
	// separately and is never attributed to Backend ownership.
	for i := 0; i < 32; i++ {
		select {
		case result := <-results:
			require.ErrorIs(t, result.err, context.Canceled)
			require.NotEqual(t, state.OutcomeCommitted, result.result.Outcome)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	executes.Wait()
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	<-closeDone
	t.Logf("close-join mode=%s actual Stop/Backend.Close returned error=%v", s.CloseMode, closeErr)
	require.True(t, closeErr == nil || errors.Is(closeErr, state.ErrOutcomeUnknown) && errors.Is(closeErr, context.Canceled), "%v", closeErr)
	select {
	case <-w.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	workerErr := w.Stop()
	status := w.Status()
	t.Logf("close-join mode=%s actual joined worker Stop error=%v LastError=%q", s.CloseMode, workerErr, status.LastError)
	require.False(t, status.Running)
	require.False(t, status.Synchronized)
	// Backend.Close seals the original session before the actor's next live
	// check. Its cleanup outcome is separate from the worker's guard-loss
	// outcome; only this exact backend-close path may publish invalid here.
	switch {
	case workerErr == nil:
		require.Equal(t, "stopped", string(status.Phase))
	case errors.Is(workerErr, state.ErrOutcomeUnknown) && errors.Is(workerErr, context.Canceled):
		require.Equal(t, "unknown", string(status.Phase))
	case s.CloseMode == "backend-close" && errors.Is(workerErr, state.ErrGuardExpired) && !errors.Is(workerErr, state.ErrOutcomeUnknown):
		require.Equal(t, "invalid", string(status.Phase))
	default:
		t.Fatalf("unexpected joined worker outcome: %v, phase=%s", workerErr, status.Phase)
	}
	wantLastError := ""
	if workerErr != nil {
		wantLastError = workerErr.Error()
		if len(wantLastError) > 256 {
			wantLastError = wantLastError[:256]
		}
	}
	require.Equal(t, wantLastError, status.LastError, "original dueWorkerError maps at most256 bytes")
	backendErr := b.Close()
	if s.CloseMode == "backend-close" {
		require.Equal(t, closeErr, backendErr, "repeated Close returns the original joined outcome")
	} else {
		require.NoError(t, backendErr)
	}
	proxy.unblockCloseRead()
	proxy.close(t)
	final := proxy.snapshot()
	require.Equal(t, 1, final.CloseReadJoined)
	require.Equal(t, before.GrantIDs, final.GrantIDs)
	require.Equal(t, 1, final.AdmissionReads)
	require.Zero(t, final.ClaimPuts)
	require.Zero(t, final.ArchivePuts)
	t.Logf("close-join mode=%s all32 Execute returned; worker Done/status=%s; Backend.Close returned; fixture proxy actual handler/pumps joined: %+v", s.CloseMode, status.Phase, final)
}

// The parent observes no cleanup effects, then explicitly tears down the
// still-live genuine runtime. That fixture kill is not an observer cleanup
// proof. Every OS child is actually waited, including on assertion failure.
func cleanupExecutorCloseParent(t *testing.T, ctx context.Context, s cleanupExecutorSetup, admin *clientv3.Client, api *cleanupExecutorAPI, supervisor *exec.Cmd, waited *bool) {
	t.Helper()
	setup := filepath.Join(s.Directory, "worker-close.json")
	require.NoError(t, os.WriteFile(setup, observerNativeJSON(t, s), 0600))
	worker := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=110s")
	worker.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+setup)
	worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
	require.NoError(t, worker.Start())
	workerErr := worker.Wait()
	t.Logf("close-join mode=%s original worker actual Wait=%v", s.CloseMode, workerErr)
	require.NoError(t, workerErr)

	prefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	read, stop := context.WithTimeout(ctx, 2*time.Second)
	response, err := admin.Txn(read).Then(clientv3.OpGet(prefix+"execution"), clientv3.OpGet(prefix+"claim"), clientv3.OpGet(prefix+"workspace-archive-command"), clientv3.OpGet(s.Binding.Namespace+"cleanup-execution-slots/", clientv3.WithPrefix())).Commit()
	stop()
	require.NoError(t, err)
	require.Len(t, response.Responses, 4)
	for _, point := range response.Responses {
		require.Empty(t, point.GetResponseRange().Kvs)
	}
	api.mu.Lock()
	patches, deletes, upgrades, present := api.patches, api.deletes, api.upgrades, api.pod != nil
	api.mu.Unlock()
	require.Zero(t, patches)
	require.Zero(t, deletes)
	require.Zero(t, upgrades)
	require.True(t, present)
	require.NoError(t, supervisor.Process.Kill())
	supervisorErr := supervisor.Wait()
	*waited = true
	var exited *exec.ExitError
	require.ErrorAs(t, supervisorErr, &exited)
	require.Equal(t, syscall.SIGKILL, exited.Sys().(syscall.WaitStatus).Signal())
	t.Logf("close-join mode=%s parent no Claim/E/S/archive/provider/bridge effects; fixture supervisor actual Wait=%v", s.CloseMode, supervisorErr)
}
