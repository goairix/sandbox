//go:build linux

package etcd

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var workspaceSyncLocalNativeWorker *launcher.KernelBoundary

// Only this explicit owned CGO0 container mode hardens the process. Ordinary
// host tests retain their existing bootstrap-free behavior.
func TestMain(m *testing.M) {
	if os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE") != "1" {
		os.Exit(m.Run())
	}
	code := workspaceSyncLocalNativeMain(m)
	os.Exit(code)
}

type workspaceSyncLocalServer struct {
	cmd     *exec.Cmd
	log     *os.File
	done    chan struct{}
	waitErr error
	name    string
}

func workspaceSyncLocalNativeMain(m *testing.M) (code int) {
	code = 1
	worker, err := launcher.BootstrapWorkspaceSyncWorker()
	if err != nil {
		fmt.Fprintln(os.Stderr, "native worker bootstrap:", err)
		return
	}
	workspaceSyncLocalNativeWorker = worker
	if err := os.MkdirAll("/work/tmp", 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if err := os.Setenv("TMPDIR", "/work/tmp"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	servers := make([]*workspaceSyncLocalServer, 0, 3)
	defer func() {
		for _, s := range servers {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-s.done:
				fmt.Printf("native etcd child name=%s pid=%d joined=true exit=%v\n", s.name, s.cmd.Process.Pid, s.waitErr)
			case <-time.After(3 * time.Second):
				_ = s.cmd.Process.Kill()
				select {
				case <-s.done:
					fmt.Printf("native etcd child name=%s pid=%d killed=true joined=true exit=%v\n", s.name, s.cmd.Process.Pid, s.waitErr)
				case <-time.After(3 * time.Second):
					fmt.Printf("native etcd child name=%s pid=%d joined=false retained=true\n", s.name, s.cmd.Process.Pid)
					code = 1
				}
			}
			if err := s.log.Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
			log, err := os.Open(s.log.Name())
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
				continue
			}
			fmt.Printf("BEGIN RAW ETCD CHILD %s\n", s.name)
			if _, err := io.Copy(os.Stdout, log); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
			fmt.Printf("END RAW ETCD CHILD %s\n", s.name)
			if err := log.Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
	}()
	cluster := "local1=http://127.0.0.1:12380,local2=http://127.0.0.1:22380,local3=http://127.0.0.1:32380"
	endpoints := make([]string, 0, 3)
	for i := 1; i <= 3; i++ {
		name, client, peer := fmt.Sprintf("local%d", i), fmt.Sprintf("http://127.0.0.1:%d2379", i), fmt.Sprintf("http://127.0.0.1:%d2380", i)
		log, err := os.Create("/work/" + name + ".log")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		cmd := exec.Command("/usr/local/bin/etcd", "--name", name, "--data-dir", "/work/"+name, "--listen-client-urls", client, "--advertise-client-urls", client, "--listen-peer-urls", peer, "--initial-advertise-peer-urls", peer, "--initial-cluster", cluster, "--initial-cluster-token", "workspace-sync-local-owned", "--initial-cluster-state", "new")
		cmd.Stdout, cmd.Stderr = log, log
		cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			fmt.Fprintln(os.Stderr, err)
			return
		}
		s := &workspaceSyncLocalServer{cmd: cmd, log: log, done: make(chan struct{}), name: name}
		servers = append(servers, s) // retain each original child before next start
		go func() { s.waitErr = s.cmd.Wait(); close(s.done) }()
		endpoints = append(endpoints, client)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := false
	probeClient := &http.Client{Timeout: 500 * time.Millisecond}
	for ctx.Err() == nil {
		ready = true
		for _, endpoint := range endpoints {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			response, err := probeClient.Do(req)
			if err != nil {
				ready = false
				break
			}
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 1024))
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(data), `"health":"true"`) {
				ready = false
				break
			}
		}
		if ready {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintln(os.Stderr, "owned three-member cluster not ready:", ctx.Err())
		return
	}
	if err := os.Setenv("TEST_ETCD_ENDPOINTS", strings.Join(endpoints, ",")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	taskWorkspaceSyncLocalOwnershipVerifier = func(t *testing.T) {
		t.Helper()
		require.NoError(t, worker.ValidateCurrent())
		require.Len(t, servers, 3)
		require.Equal(t, strings.Join(endpoints, ","), os.Getenv("TEST_ETCD_ENDPOINTS"))
		for _, s := range servers {
			select {
			case <-s.done:
				t.Fatalf("original child %s exited: %v", s.name, s.waitErr)
			default:
			}
			require.NoError(t, s.cmd.Process.Signal(syscall.Signal(0)))
		}
		client, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: time.Second})
		require.NoError(t, err)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		members, err := client.MemberList(ctx)
		require.NoError(t, err)
		require.Len(t, members.Members, 3)
		seen := make(map[uint64]bool, 3)
		for i, endpoint := range endpoints {
			status, err := client.Status(ctx, endpoint)
			require.NoError(t, err)
			require.NotNil(t, status.Header)
			require.Equal(t, members.Header.ClusterId, status.Header.ClusterId)
			require.False(t, seen[status.Header.MemberId])
			seen[status.Header.MemberId] = true
			found := false
			for _, member := range members.Members {
				if member.ID == status.Header.MemberId {
					require.Equal(t, servers[i].name, member.Name)
					require.Equal(t, []string{endpoint}, member.ClientURLs)
					found = true
				}
			}
			require.True(t, found)
			t.Logf("original local child name=%s pid=%d endpoint=%s member=%x cluster=%x", servers[i].name, servers[i].cmd.Process.Pid, endpoint, status.Header.MemberId, status.Header.ClusterId)
		}
	}
	code = m.Run()
	return
}

func workspaceSyncLocalNativeBody(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")})
	require.NoError(t, err)
	setup := func(t *testing.T) (*Backend, *clientv3.Client, AcquireIntentInput, *CreationClaim, []byte, ed25519.PrivateKey) {
		original, raw := integrationBackend(t)
		root := ed25519.NewKeyFromSeed(make([]byte, 32))
		options := integrationOptions(original, raw)
		options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
		options.Clock = goodAuthorityClock()
		base, err := New(context.Background(), options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, base.Close()) })
		archiveTrackCleanup(base)
		input := acquisitionInput(t, "local-source")
		input.Workspace = plan.workspace
		result, err := base.AcquireIntent(context.Background(), input)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
		claim, err := base.ClaimCreation(context.Background(), input.Workspace, input.IntentID, "local-preparer", 30*time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = base.ReleaseCreationClaim(context.Background(), claim) })
		dispatch, err := base.DeclareRuntimeDispatch(context.Background(), claim, dispatchInputForDeclaration())
		require.NoError(t, err)
		wire := preparationCertificate(t, base, dispatch.Entry.Record, root, "plain", RuntimeReference{ID: uuid.NewString(), UID: uuid.NewString(), BootID: uuid.NewString()})
		return base, raw, input, claim, wire, root
	}
	f, s, body := workspaceSyncIntentBodyWithSetup(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 2, 4096}, setup)
	root := archiveTestDirectory(t)
	target, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, plan, root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return f, s, body, target
}

func TestTaskWorkspaceSyncLocalNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	t.Run("readonly-attribution-original-gate", func(t *testing.T) {
		f, s, body, target := workspaceSyncLocalNativeBody(t)
		entries, err := os.ReadDir(target.pin.Root)
		require.NoError(t, err)
		require.Empty(t, entries, "constructor did not create provider files")
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		result, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		require.True(t, body.intent.acknowledged)
		record, err := f.b.LoadTaskWorkspaceSyncIntent(f.ctx, result.Reference)
		require.NoError(t, err)
		require.Equal(t, uint32(2), record.Record.Version)
		require.Equal(t, target.pin, *record.Record.Local)
		p, err := s.prepareLocalPut(f.ctx, body, target)
		require.NoError(t, err)
		require.Equal(t, 59, p.nodes, "50 original pins plus prefix/budget and bounded point branches")
		t.Logf("native readonly gate originalPins=%d helperNodes=%d requestBytes=%d actualResponseBytes=%d successReplyBound=%d failureReplyBound=%d intentBytes=%d slotBytes=%d", len(s.archive.owner.comparisons), p.nodes, p.requestBytes, p.responseBytes, p.successBytes, p.failureBytes, len(p.draft.value), len(p.draft.slotValue))
		_, err = s.prepareLocalPut(f.ctx, body, target)
		require.Error(t, err)
		_, err = body.Read(make([]byte, 1))
		require.Error(t, err, "original IO owner excludes ordinary Read")
		_, err = s.next(f.ctx)
		require.Error(t, err)
		copyOwner := &taskWorkspaceSyncLocalPut{self: p, source: s, body: body, target: target}
		require.Error(t, copyOwner.Close())
		require.NoError(t, p.Close())
		entries, err = os.ReadDir(target.pin.Root)
		require.NoError(t, err)
		require.Empty(t, entries, "Task1 gate has no provider effects")
	})
	t.Run("target-worker-plan-mismatches", func(t *testing.T) {
		f, s, body, target := workspaceSyncLocalNativeBody(t)
		copyWorker := &launcher.KernelBoundary{}
		reflect.ValueOf(copyWorker).Elem().Set(reflect.ValueOf(workspaceSyncLocalNativeWorker).Elem())
		_, err := openTaskWorkspaceSyncLocalTarget(f.ctx, copyWorker, s.plan, target.pin.Root)
		require.Error(t, err)
		copyTarget := &taskWorkspaceSyncLocalTarget{self: target, worker: target.worker, pin: target.pin, plan: target.plan}
		require.Error(t, s.bindLocalPut(f.ctx, body, copyTarget))
		require.Error(t, copyTarget.Close())
		wrongPlan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: s.plan.workspace, Exclude: []string{"other"}})
		require.NoError(t, err)
		wrong, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, wrongPlan, target.pin.Root)
		require.NoError(t, err)
		defer wrong.Close()
		require.Error(t, s.bindLocalPut(f.ctx, body, wrong))
		second, err := openTaskWorkspaceSyncLocalTarget(f.ctx, workspaceSyncLocalNativeWorker, s.plan, archiveTestDirectory(t))
		require.NoError(t, err)
		defer second.Close()
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err = s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		_, err = s.prepareLocalPut(f.ctx, body, second)
		require.Error(t, err)
	})
	for _, fault := range []string{"symlink", "permissions", "replacement", "root-after-ack"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncLocalNativeBody(t)
			root := target.pin.Root
			switch fault {
			case "symlink":
				link := filepath.Join(archiveTestDirectory(t), "link")
				require.NoError(t, os.Symlink(root, link))
				_, err := openTaskWorkspaceSyncLocalTarget(f.ctx, target.worker, s.plan, link)
				require.Error(t, err)
				return
			case "permissions":
				require.NoError(t, os.Chmod(root, 0750))
			case "replacement":
				require.NoError(t, os.Rename(root, root+"-original"))
				require.NoError(t, os.Mkdir(root, 0700))
				defer os.Remove(root + "-original")
			case "root-after-ack":
				require.NoError(t, s.bindLocalPut(f.ctx, body, target))
				_, err := s.reservePutIntent(f.ctx, body)
				require.NoError(t, err)
				require.NoError(t, os.Chmod(root, 0750))
				p, err := s.prepareLocalPut(f.ctx, body, target)
				require.Error(t, err)
				require.Nil(t, p)
				return
			}
			require.Error(t, s.bindLocalPut(f.ctx, body, target))
			require.Nil(t, body.intent)
		})
	}
	for _, fault := range []string{"unknown", "unknown-paired-aba", "paired-aba", "slot-corrupt", "restore", "whole-prefix", "claim-lost"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncLocalNativeBody(t)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			if strings.HasPrefix(fault, "unknown") {
				workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}})
			}
			_, err := s.reservePutIntent(f.ctx, body)
			if strings.HasPrefix(fault, "unknown") {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
			} else {
				require.NoError(t, err)
			}
			d := body.intent
			if fault == "paired-aba" || fault == "unknown-paired-aba" {
				_, err = f.raw.Txn(f.ctx).Then(clientv3.OpDelete(d.intentKey), clientv3.OpDelete(d.slotKey)).Commit()
				require.NoError(t, err)
				_, err = f.raw.Txn(f.ctx).Then(clientv3.OpPut(d.intentKey, d.value), clientv3.OpPut(d.slotKey, d.slotValue)).Commit()
				require.NoError(t, err)
			}
			err = nil
			switch fault {
			case "slot-corrupt":
				_, err = f.raw.Put(f.ctx, d.slotKey, "corrupt")
			case "restore":
				deferRestore(t, f)
				_, err = f.raw.Put(f.ctx, f.b.restoreKey, "different-epoch")
			case "whole-prefix":
				_, err = f.raw.Put(f.ctx, s.archive.owner.prefix+"zz/future", "present")
			case "claim-lost":
				_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
			}
			require.NoError(t, err)
			if strings.HasPrefix(fault, "unknown") {
				_, err = s.reservePutIntent(f.ctx, body)
				require.NoError(t, err, "diagnostic arbitration can observe matching metadata")
				require.False(t, d.acknowledged, "arbitration never grants authority")
			}
			p, err := s.prepareLocalPut(f.ctx, body, target)
			require.Error(t, err)
			require.Nil(t, p)
			entries, err := os.ReadDir(target.pin.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
	t.Run("owner-close-waits-actual-fstat", func(t *testing.T) {
		f, s, body, target := workspaceSyncLocalNativeBody(t)
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		p, err := s.prepareLocalPut(f.ctx, body, target)
		require.NoError(t, err)
		require.NoError(t, p.beginIO())
		// Pause only after a real readonly kernel observation completed; this
		// seam proves registration/waiting and never invents a successful IO.
		require.NoError(t, target.check(f.ctx))
		joined := make(chan error, 3)
		var started sync.WaitGroup
		started.Add(3)
		go func() { started.Done(); joined <- p.Close() }()
		go func() { started.Done(); joined <- target.Close() }()
		go func() { started.Done(); joined <- s.Close() }()
		started.Wait()
		select {
		case err := <-joined:
			t.Fatalf("close returned before actual owner completion: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		p.finishIO()
		for i := 0; i < 3; i++ {
			require.NoError(t, <-joined)
		}
		require.True(t, errors.Is(p.ctx.Err(), context.Canceled))
	})
}
