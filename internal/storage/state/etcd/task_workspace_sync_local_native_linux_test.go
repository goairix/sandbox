//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
	if os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_FOREIGN_INITIALIZER") == "1" {
		os.Exit(workspaceSyncLocalForeignInitializer())
	}
	if os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE") != "1" {
		os.Exit(m.Run())
	}
	code := workspaceSyncLocalNativeMain(m)
	os.Exit(code)
}

// A distinct test-only UID0 process creates the foreign ownership fixture.
// It never bootstraps or supplies the original nonprivileged worker authority.
func workspaceSyncLocalForeignInitializer() int {
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		return 1
	}
	root := "/work/foreign-test"
	if err := os.Mkdir(root, 0777); err != nil {
		return 1
	}
	if err := os.Chmod(root, 0777); err != nil {
		return 1
	}
	file, err := os.OpenFile(root+"/root-file", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
	if err != nil {
		return 1
	}
	_, writeErr := file.Write([]byte("foreign preserve exact bytes"))
	chmodErr := file.Chmod(0666)
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Join(writeErr, chmodErr, syncErr, closeErr) != nil {
		return 1
	}
	var st syscall.Stat_t
	if err := syscall.Stat(root+"/root-file", &st); err != nil || st.Uid != 0 || st.Gid != 0 || st.Nlink != 1 {
		return 1
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "CapEff:\t0000000000000000") || !strings.Contains(string(status), "NoNewPrivs:\t1") {
		return 1
	}
	fmt.Printf("foreign fixture helper uid=0 gid=0 caps0=true NNP=true nlink=%d dev=%d ino=%d fileUID=%d mode=%o fsyncClose=true\n", st.Nlink, st.Dev, st.Ino, st.Uid, st.Mode&0777)
	if err := os.WriteFile(root+"/ready", []byte("helper actual file fsync and Close completed"), 0644); err != nil {
		return 1
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(root + "/cleanup-ready"); err == nil {
			if err := errors.Join(os.Remove(root+"/ready"), os.Remove(root+"/cleanup-ready")); err != nil {
				return 1
			}
			if err := os.Remove(root); err != nil {
				return 1
			}
			fmt.Println("foreign UID0 fixture helper exact paths removed; original initializer process ending")
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 1
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
	if os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_FOREIGN_FIXTURE") == "1" {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat("/work/foreign-test/ready"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "foreign fixture helper did not complete")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
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
			case <-time.After(3 * time.Second):
				_ = s.cmd.Process.Kill()
				select {
				case <-s.done:
				case <-time.After(3 * time.Second):
					fmt.Printf("native etcd child name=%s pid=%d owner_pid=%d joined=false retained=true\n", s.name, s.cmd.Process.Pid, os.Getpid())
					code = 1
					// Bounded stop attempts are not a join. Preserve this original
					// process/log owner indefinitely until actual Wait is observed.
					<-s.done
				}
			}
			fmt.Printf("native etcd child name=%s pid=%d joined=true exit=%v\n", s.name, s.cmd.Process.Pid, s.waitErr)
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
		go func() {
			s.waitErr = s.cmd.Wait()
			if os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_PENDING_PROOF") == "1" && s.name == "local1" {
				// Explicit fixture seam delays only publication after a real Wait
				// returned. It does not model an unkillable kernel child.
				fmt.Printf("native child wait name=%s pid=%d actual_wait_complete=true publication_pending=true\n", s.name, s.cmd.Process.Pid)
				for {
					if _, err := os.Stat("/work/release-local1-join"); err == nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			close(s.done)
		}()
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
	return workspaceSyncLocalNativeBodyData(t, nil, 0)
}

func workspaceSyncLocalNativeBodyData(t *testing.T, data []byte, entries uint64) (*quiescenceNativeFixture, *PreparedTaskWorkspaceSyncSource, *taskWorkspaceSyncBody, *taskWorkspaceSyncLocalTarget) {
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
	var f *quiescenceNativeFixture
	var s *PreparedTaskWorkspaceSyncSource
	var body *taskWorkspaceSyncBody
	if data == nil {
		f, s, body = workspaceSyncIntentBodyWithSetup(t, plan, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 2, 4096}, setup)
	} else {
		var prepared *PreparedTaskWorkspaceArchive
		var wire *deliveryNativeWire
		f, prepared, wire = deliveryNativeFixtureWithData(t, 0, func(TaskRecord) TaskFinalSyncPlan {
			p, err := plan.FinalSyncPlan()
			require.NoError(t, err)
			return p
		}, &TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 2, 4096}, setup, data, entries, 60*time.Second)
		done := deliveryBegin(f, prepared)
		deliveryWaitSent(t, wire, done)
		close(wire.body)
		r := deliveryWait(t, done)
		require.NoError(t, r.err)
		require.NoError(t, <-wire.done)
		var err error
		s, err = f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
		require.NoError(t, err)
		entry, err := s.next(f.ctx)
		require.NoError(t, err)
		for entry.kind == tar.TypeDir {
			entry, err = s.next(f.ctx)
			require.NoError(t, err)
		}
		body, err = s.openBody(f.ctx, entry)
		require.NoError(t, err)
	}
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

func TestTaskWorkspaceSyncLocalNativePostguardClose(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	f, s, body, target := workspaceSyncLocalNativeBody(t)
	require.NoError(t, s.bindLocalPut(f.ctx, body, target))
	_, err := s.reservePutIntent(f.ctx, body)
	require.NoError(t, err)
	p, err := s.prepareLocalPut(f.ctx, body, target)
	require.NoError(t, err)
	observed, resume := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.probeComplete = func() { close(observed); <-resume }
	p.mu.Unlock()
	guardDone := make(chan error, 1)
	go func() { guardDone <- p.guard() }()
	<-observed // callback is after a real final physical target.check
	free := s.archive.owner.nativeMu.TryLock()
	if free {
		s.archive.owner.nativeMu.Unlock()
	}
	joined := make(chan error, 3)
	go func() { joined <- p.Close() }()
	go func() { joined <- target.Close() }()
	go func() { joined <- s.Close() }()
	joinedCount := 0
	select {
	case err := <-joined:
		joinedCount++
		t.Errorf("Close returned while the original postguard owner was pending: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	require.NoError(t, <-guardDone)
	for i := joinedCount; i < 3; i++ {
		require.NoError(t, <-joined)
	}
	require.True(t, free, "nativeMu must be released before the final physical postguard")
	t.Log("actual postguard observed; nativeMu free; original guard retained owner/source/target Close registrations")
}

func TestTaskWorkspaceSyncLocalPutNative(t *testing.T) {
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("explicit owned Linux native fixture required")
	}
	t.Run("nested-and-successive-original-files", func(t *testing.T) {
		f, s, body, target := workspaceSyncLocalNativeBody(t)
		for _, expected := range []string{"hello", "bye"} {
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			_, err := s.reservePutIntent(f.ctx, body)
			require.NoError(t, err)
			w, err := s.putLocal(f.ctx, body, target)
			require.NoError(t, err)
			require.NotNil(t, w)
			require.True(t, w.original())
			require.Same(t, w, w.self)
			require.Same(t, s, w.origin.source)
			require.Equal(t, int64(len(expected)), w.bytes)
			actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
			require.NoError(t, err)
			require.Equal(t, expected, string(actual))
			st, err := os.Stat(filepath.Join(target.pin.Root, body.entry.path))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(body.entry.mode), st.Mode().Perm())
			fds, err := os.ReadDir("/proc/self/fd")
			require.NoError(t, err)
			for _, descriptor := range fds {
				link, err := os.Readlink("/proc/self/fd/" + descriptor.Name())
				if err == nil {
					require.False(t, strings.HasPrefix(link, target.pin.Root+"/"), "all child file and parent FDs physically closed: %s", link)
				}
			}
			t.Logf("original file bytes=%d SHA256=%s mode=%o dev=%d inode=%d; scoped child FD inventory empty", w.bytes, w.sha256, st.Mode().Perm(), w.dev, w.ino)
			_, err = s.putLocal(f.ctx, body, target)
			require.Error(t, err, "original successful file dispatch is never repeated")
			require.NoError(t, body.Close())
			if expected == "hello" {
				entry, err := s.next(f.ctx)
				require.NoError(t, err, "success releases IO owner without canceling source")
				body, err = s.openBody(f.ctx, entry)
				require.NoError(t, err)
			}
		}
	})
	for _, contents := range []string{"", strings.Repeat("chunk", 15000)} {
		name := "zero"
		if contents != "" {
			name = "multichunk"
		}
		t.Run(name, func(t *testing.T) {
			data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/deep", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/deep/nested", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/deep/nested/file", Typeflag: tar.TypeReg, Mode: 0640, Size: int64(len(contents))}}, []string{"", "", "", contents})
			f, s, body, target := workspaceSyncLocalNativeBodyData(t, data, 4)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			_, err := s.reservePutIntent(f.ctx, body)
			require.NoError(t, err)
			w, err := s.putLocal(f.ctx, body, target)
			require.NoError(t, err)
			require.Equal(t, int64(len(contents)), w.bytes)
			require.Equal(t, body.entry.sha256, w.sha256)
			actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
			require.NoError(t, err)
			require.Equal(t, contents, string(actual))
			t.Logf("actual regular file bytes=%d SHA256=%s mode=0640 filesystem=%s original FD closed", w.bytes, w.sha256, target.pin.FilesystemName)
		})
	}
	t.Run("mode-zero-preserved", func(t *testing.T) {
		data := archiveTarBytes(t, []*tar.Header{{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "workspace/opaque", Typeflag: tar.TypeReg, Mode: 0, Size: 6}}, []string{"", "opaque"})
		f, s, body, target := workspaceSyncLocalNativeBodyData(t, data, 2)
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		w, err := s.putLocal(f.ctx, body, target)
		require.NoError(t, err, "a scoped identity observation must not require archive read permission")
		require.True(t, w.original())
		path := filepath.Join(target.pin.Root, body.entry.path)
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.EqualValues(t, 0, st.Mode().Perm())
		require.NoError(t, os.Chmod(path, 0600), "test-only inspection after witnessing original mode 0000")
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "opaque", string(actual))
	})
	for _, fault := range []string{"overwrite", "symlink", "hardlink", "fifo", "parent-symlink", "parent-mode", "body-hash", "no-ack", "unknown", "root-mode", "ownership"} {
		t.Run(fault, func(t *testing.T) {
			f, s, body, target := workspaceSyncLocalNativeBody(t)
			require.NoError(t, os.Mkdir(filepath.Join(target.pin.Root, "z"), 0700))
			path := filepath.Join(target.pin.Root, body.entry.path)
			other := filepath.Join(target.pin.Root, "original")
			var foreign syscall.Stat_t
			switch fault {
			case "ownership":
				require.NoError(t, syscall.Stat("/work/foreign-test/root-file", &foreign))
				require.EqualValues(t, 0, foreign.Uid)
				require.EqualValues(t, 1, foreign.Nlink)
				require.NoError(t, os.Rename("/work/foreign-test/root-file", path))
				t.Cleanup(func() {
					require.NoError(t, os.WriteFile("/work/foreign-test/cleanup-ready", []byte("original worker moved foreign leaf; helper may remove exact empty fixture"), 0644))
					deadline := time.Now().Add(3 * time.Second)
					for {
						_, err := os.Stat("/work/foreign-test")
						if os.IsNotExist(err) {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("original foreign fixture helper did not remove exact paths")
						}
						time.Sleep(20 * time.Millisecond)
					}
				})
			case "overwrite", "hardlink":
				require.NoError(t, os.WriteFile(path, []byte("preserve this long content"), 0600))
				if fault == "hardlink" {
					require.NoError(t, os.Link(path, other))
				}
			case "symlink":
				require.NoError(t, os.WriteFile(other, []byte("preserve"), 0600))
				require.NoError(t, os.Symlink(other, path))
			case "fifo":
				require.NoError(t, syscall.Mkfifo(path, 0600))
			case "parent-symlink":
				require.NoError(t, os.Remove(filepath.Join(target.pin.Root, "z")))
				require.NoError(t, os.Symlink(archiveTestDirectory(t), filepath.Join(target.pin.Root, "z")))
			case "parent-mode":
				require.NoError(t, os.Chmod(filepath.Join(target.pin.Root, "z"), 0750))
			}
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			if fault == "unknown" {
				workspaceSyncIntentSetKV(s, &faultKV{KV: f.b.client.KV, match: workspaceSyncIntentPutMatch, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					return nil, context.DeadlineExceeded
				}})
			}
			if fault != "no-ack" {
				_, err := s.reservePutIntent(f.ctx, body)
				if fault == "unknown" {
					require.ErrorIs(t, err, ErrOutcomeUnknown)
				} else {
					require.NoError(t, err)
				}
			}
			if fault == "body-hash" {
				_, err := s.archive.owner.spool.file.WriteAt([]byte("j"), body.entry.offset)
				require.NoError(t, err)
			}
			if fault == "root-mode" {
				require.NoError(t, os.Chmod(target.pin.Root, 0750))
			}
			w, err := s.putLocal(f.ctx, body, target)
			if fault == "overwrite" {
				require.NoError(t, err)
				require.NotNil(t, w)
				actual, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "hello", string(actual))
			} else {
				require.Error(t, err)
				require.Nil(t, w)
				if fault == "no-ack" || fault == "unknown" || fault == "root-mode" {
					_, err := os.Lstat(path)
					require.True(t, os.IsNotExist(err), "no original ACK/current gate means no leaf effect")
				}
				if fault == "hardlink" || fault == "symlink" {
					actual, err := os.ReadFile(other)
					require.NoError(t, err)
					if fault == "hardlink" {
						require.Equal(t, "preserve this long content", string(actual))
					} else {
						require.Equal(t, "preserve", string(actual))
					}
				}
				if fault == "ownership" {
					actual, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Equal(t, "foreign preserve exact bytes", string(actual))
					var current syscall.Stat_t
					require.NoError(t, syscall.Stat(path, &current))
					require.Equal(t, foreign.Dev, current.Dev)
					require.Equal(t, foreign.Ino, current.Ino)
					require.Equal(t, foreign.Uid, current.Uid)
					require.Equal(t, foreign.Gid, current.Gid)
					require.Equal(t, foreign.Mode, current.Mode)
					require.EqualValues(t, 1, current.Nlink)
					require.NoError(t, target.worker.ValidateCurrent())
					t.Logf("real worker uid=%d gid=%d caps0; foreign file uid=%d nlink=%d inode=%d mode=%o bytes preserved before truncate", target.pin.UID, target.pin.GID, current.Uid, current.Nlink, current.Ino, current.Mode&0777)
				}
			}
			if body.intent != nil {
				for _, key := range []string{body.intent.intentKey, body.intent.slotKey} {
					got, err := f.raw.Get(f.ctx, key)
					require.NoError(t, err)
					require.Len(t, got.Kvs, 1, "permanent intent/slot remain charged")
				}
			}
		})
	}
	for _, route := range []string{"body", "source", "target", "claim", "backend", "cancel", "root-after-write", "target-after-file-close"} {
		t.Run("io-pending-"+route, func(t *testing.T) {
			f, s, body, target := workspaceSyncLocalNativeBody(t)
			require.NoError(t, s.bindLocalPut(f.ctx, body, target))
			_, err := s.reservePutIntent(f.ctx, body)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			observed, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			s.mu.Lock()
			s.localIOComplete = func(stage string) {
				want := "write"
				if route == "target-after-file-close" {
					want = "close"
				}
				if stage == want {
					once.Do(func() { close(observed); <-resume })
				}
			}
			s.mu.Unlock()
			putDone := make(chan error, 1)
			go func() {
				w, err := s.putLocal(ctx, body, target)
				if w != nil {
					err = errors.Join(err, errors.New("canceled physical IO issued witness"))
				}
				putDone <- err
			}()
			select {
			case <-observed:
			case err := <-putDone:
				t.Fatalf("put ended before actual syscall observation: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("actual IO did not reach bounded pause")
			}
			_, err = body.Read(make([]byte, 1))
			require.Error(t, err)
			_, err = s.next(f.ctx)
			require.Error(t, err)
			_, err = s.putLocal(f.ctx, body, target)
			require.Error(t, err)
			joined := make(chan error, 2)
			switch route {
			case "body":
				go func() { joined <- body.Close() }()
			case "source":
				go func() { joined <- s.Close() }()
			case "target", "target-after-file-close":
				go func() { joined <- target.Close() }()
			case "claim":
				go func() { joined <- f.b.ReleaseTaskClaim(f.ctx, f.c) }()
			case "backend":
				go func() { joined <- f.b.Close() }()
				go func() { joined <- f.b.Close() }()
			case "cancel":
				cancel()
			case "root-after-write":
				require.NoError(t, os.Chmod(target.pin.Root, 0750))
			}
			joinedCount := 0
			select {
			case err := <-joined:
				joinedCount++
				t.Errorf("Close returned before physical owner finished: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			_, err = s.archive.owner.spool.file.Stat()
			require.NoError(t, err, "original spool FD remains open while real put is pending")
			free := s.archive.owner.nativeMu.TryLock()
			if free {
				s.archive.owner.nativeMu.Unlock()
			}
			require.True(t, free, "nativeMu is free during actual filesystem IO")
			close(resume)
			require.Error(t, <-putDone)
			if route != "cancel" && route != "root-after-write" {
				wantJoins := 1
				if route == "backend" {
					wantJoins = 2
				}
				for joinedCount < wantJoins {
					require.NoError(t, <-joined)
					joinedCount++
				}
			}
			t.Logf("actual syscall completion pause; Close route=%s waited original IO; witness=nil; syscall cleanup joined", route)
		})
	}
	t.Run("real-write-error-retains-partial-charge", func(t *testing.T) {
		f, s, body, target := workspaceSyncLocalNativeBody(t)
		require.NoError(t, s.bindLocalPut(f.ctx, body, target))
		_, err := s.reservePutIntent(f.ctx, body)
		require.NoError(t, err)
		var original syscall.Rlimit
		require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original))
		limited := original
		limited.Cur = 2
		wasIgnored := signal.Ignored(syscall.SIGXFSZ)
		signal.Ignore(syscall.SIGXFSZ)
		defer func() {
			if wasIgnored {
				signal.Ignore(syscall.SIGXFSZ)
			} else {
				signal.Reset(syscall.SIGXFSZ)
			}
		}()
		require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited))
		defer func() { require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original)) }()
		w, putErr := s.putLocal(f.ctx, body, target)
		require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original))
		require.ErrorIs(t, putErr, syscall.EFBIG)
		require.Nil(t, w)
		actual, err := os.ReadFile(filepath.Join(target.pin.Root, body.entry.path))
		require.NoError(t, err)
		require.Equal(t, "he", string(actual), "actual short write leaves charged partial leaf")
		w, err = s.putLocal(f.ctx, body, target)
		require.Error(t, err)
		require.Nil(t, w)
		for _, key := range []string{body.intent.intentKey, body.intent.slotKey} {
			got, err := f.raw.Get(f.ctx, key)
			require.NoError(t, err)
			require.Len(t, got.Kvs, 1)
		}
		t.Log("real EFBIG after 2 written bytes; original soft rlimit restored; no witness/retry/refund")
	})
}
