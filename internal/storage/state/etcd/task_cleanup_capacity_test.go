package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// A peer owns one real session and partition. Its sole test goroutine rotates
// over 25 disjoint original Claim shares; readiness never changes a bucket.
// These are metadata reservation calls, not physical cleanup authority.
type cleanupCapacityPeer struct {
	b       *Backend
	session *WorkerSession
	part    *WorkerPartitionClaim
	shares  []*WorkerBudgetShare
	next    int
	lease   *creationFaultLease

	rpcs      atomic.Int64
	claimPuts atomic.Int64
	dropped   atomic.Int64
	dropRev   atomic.Int64
	turns     int
	conflicts int
	unknown   *cleanupSlotState
	lastOwner *workerDueOwner
}

type cleanupCapacityTaskState struct {
	ref      TaskReference
	evidence *cleanupSlotEvidence
	selected *cleanupSlotState
	occupied *cleanupSlotState
}

// Only actual successful upstream reserve responses can be withheld. All
// requests still reach etcd; there is no synthetic success or authority hook.
func (p *cleanupCapacityPeer) observeNative(dropFirstReserve bool) {
	reserve := false
	p.b.client.KV = &faultKV{KV: p.b.client.KV, match: func(ops []clientv3.Op) bool {
		p.rpcs.Add(1)
		reserve = false
		for _, op := range ops {
			if !op.IsPut() {
				continue
			}
			key := string(op.KeyBytes())
			if strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/claim") {
				p.claimPuts.Add(1)
			}
			if strings.HasPrefix(key, p.b.namespace.Root()+"cleanup-execution-slots/") {
				reserve = true
			}
		}
		return true
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if dropFirstReserve && reserve && err == nil && r != nil && r.Succeeded && p.dropped.CompareAndSwap(0, 1) {
			p.dropRev.Store(r.Header.Revision)
			return nil, errors.New("capacity fixture: actual committed reserve ACK withheld")
		}
		return r, err
	}}
	p.lease = &creationFaultLease{Lease: p.b.client.Lease}
	p.b.client.Lease = p.lease
}

// Each turn earns and consumes one original credit, retains the native owner
// through RPC return/postcheck, then joins it before any later turn or renewal.
// The callback is test orchestration of existing private protocol methods only.
func (p *cleanupCapacityPeer) turn(ctx context.Context, work func(*workerDueOwner) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.b.archiveLife.mu.Lock()
		remaining := time.Until(p.session.deadline)
		p.b.archiveLife.mu.Unlock()
		if remaining < 15*time.Second {
			if err := p.b.RenewWorkerSession(ctx, p.session); err != nil {
				return err
			}
		}

		now := time.Now()
		var ready *WorkerBudgetShare
		var earliest time.Time
		p.b.archiveLife.mu.Lock()
		for n := 0; n < len(p.shares); n++ {
			i := (p.next + n) % len(p.shares)
			at := dueWorkerCreditAt(p.shares[i].bucket, now, 1)
			if !at.After(now) {
				ready = p.shares[i]
				p.next = (i + 1) % len(p.shares)
				break
			}
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
		p.b.archiveLife.mu.Unlock()
		if ready == nil {
			timer := time.NewTimer(time.Until(earliest))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}

		owner, stop, err := p.b.beginExpiredTaskOwner(ctx, p.part, ready)
		if err != nil {
			return err
		}
		p.turns++
		p.lastOwner = owner
		before := p.rpcs.Load()
		err = work(owner)
		post := owner.postcheck()
		owner.finish()
		stop()
		<-owner.done
		if count := p.rpcs.Load() - before; count > 2 {
			return fmt.Errorf("charged turn sent %d RPCs, maximum 2", count)
		}
		return errors.Join(err, post)
	}
}

func (p *cleanupCapacityPeer) prepare(ctx context.Context, task *cleanupCapacityTaskState) error {
	var bundle *taskClaimBundle
	if err := p.turn(ctx, func(o *workerDueOwner) error {
		var err error
		bundle, err = p.b.loadCleanupClaimBundle(o, task.ref)
		return err
	}); err != nil {
		return err
	}
	if err := p.turn(ctx, func(o *workerDueOwner) error {
		var err error
		task.evidence, err = p.b.loadCleanupSlotEvidence(o, bundle)
		return err
	}); err != nil {
		return err
	}
	return p.turn(ctx, func(o *workerDueOwner) error {
		if err := p.b.initializeCleanupSlot(o, task.evidence); err != nil {
			return err
		}
		var err error
		task.selected, err = p.b.readCleanupSlot(o, task.ref)
		return err
	})
}

// Every conflict yields its native owner and consumes another real credit for
// rotation. An uncertain successful reserve is read at its exact candidate;
// it never enters the conflict/rotation branch or creates another attempt.
func (p *cleanupCapacityPeer) occupy(ctx context.Context, task *cleanupCapacityTaskState, expectFull bool) error {
	seen := make(map[uint32]bool)
	for candidate := 0; candidate < 32; candidate++ {
		selected := task.selected
		if seen[selected.record.Slot] {
			return fmt.Errorf("candidate repeated before full 32-slot traversal")
		}
		seen[selected.record.Slot] = true
		err := p.turn(ctx, func(o *workerDueOwner) error {
			return p.b.reserveCleanupSlot(o, task.evidence, selected)
		})
		if err == nil || errors.Is(err, ErrOutcomeUnknown) {
			unknown := errors.Is(err, ErrOutcomeUnknown)
			if err = p.turn(ctx, func(o *workerDueOwner) error {
				var readErr error
				task.occupied, readErr = p.b.readCleanupKnownState(o, selected)
				return readErr
			}); err != nil {
				return err
			}
			if task.occupied.record.Selection != selected.record.Selection || task.occupied.record.Incarnation != selected.record.Incarnation {
				return fmt.Errorf("reserve changed candidate or incarnation")
			}
			if unknown {
				if task.occupied.execution.mod != p.dropRev.Load() {
					return fmt.Errorf("unknown resolution does not match actual commit revision")
				}
				p.unknown = task.occupied
			}
			if expectFull {
				return fmt.Errorf("33rd Task unexpectedly occupied slot %d", selected.record.Slot)
			}
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		p.conflicts++
		if candidate == 31 {
			break
		}
		if err = p.turn(ctx, func(o *workerDueOwner) error {
			if err := p.b.rotateCleanupSlot(o, selected); err != nil {
				return err
			}
			var readErr error
			task.selected, readErr = p.b.readCleanupSlot(o, task.ref)
			return readErr
		}); err != nil {
			return err
		}
	}
	if !expectFull {
		return fmt.Errorf("fewer than 32 Tasks exhausted all slots")
	}
	return nil
}

// This producer uses original domain APIs and genuine signed ordinary runtime
// metadata. It creates no Pod and supplies no Kernel/physical permission.
func cleanupCapacityTask(t *testing.T, ctx context.Context, b *Backend, root ed25519.PrivateKey, workspace WorkspaceIdentity, ordinal int) TaskReference {
	t.Helper()
	in := acquisitionInput(t, fmt.Sprintf("capacity-%d", ordinal))
	in.Workspace = workspace
	acquired, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	claim, err := b.ClaimCreation(ctx, workspace, in.IntentID, "capacity-producer", 30*time.Second)
	require.NoError(t, err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ReleaseCreationClaim(cleanup, claim))
	}()
	dispatch, err := b.DeclareRuntimeDispatch(ctx, claim, dispatchInputForDeclaration())
	require.NoError(t, err)
	certificate := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", RuntimeReference{
		ID: fmt.Sprintf("capacity-runtime-%d", ordinal), UID: fmt.Sprintf("capacity-uid-%d", ordinal), BootID: fmt.Sprintf("capacity-boot-%d", ordinal),
	})
	bound, err := b.BindRuntime(ctx, claim, certificate)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mounted, err := b.ConsumeRuntimeMount(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	preparation, err := b.loadRuntimePreparation(ctx, workspace, in.IntentID, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	proof, err := controlprotocol.SignReadyReceipt(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), certificate, controlprotocol.ReadyReceiptClaims{
		Version: 1, CertificateDigest: preparation.Binding.Record.CertificateDigest,
		Claim:         controlprotocol.ClaimReference{ClaimID: claim.reference.ClaimID, CreateRevision: claim.reference.CreateRevision, LeaseID: claim.reference.LeaseID},
		DataGateEpoch: claim.control.DataGateEpoch, GateState: "open", MountAttempt: preparation.Mount.Record.MountAttempt,
		ObservedAt: now, ValidUntil: now.Add(5 * time.Second),
	})
	require.NoError(t, err)
	published, err := b.PublishRuntime(ctx, claim, proof)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	control, err := b.LoadControl(ctx, workspace.Partition(), in.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := b.namespace.sandboxKeys(workspace.Partition(), in.SandboxID, control.Snapshot.Version)
	require.NoError(t, err)
	points, err := b.readDomain(ctx, controlKey)
	require.NoError(t, err)
	require.NotNil(t, points[0])
	stage, ref, err := b.PrepareDestroy(ctx, BeginDestroyInput{SandboxID: in.SandboxID, RequestID: fmt.Sprintf("capacity-destroy-%d", ordinal), ExpectedControlRevision: points[0].ModRevision}, 30*time.Second)
	require.NoError(t, err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ReleaseStage(cleanup, stage))
	}()
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	return ref
}

// Observation reads are test evidence, not admission or discovery authority.
// Preserve every E/S byte and native revision for the full/lease-loss checks.
func cleanupCapacityRows(t *testing.T, ctx context.Context, raw *clientv3.Client, root string) map[string]*mvccpb.KeyValue {
	t.Helper()
	response, err := raw.Get(ctx, root, clientv3.WithPrefix())
	require.NoError(t, err)
	rows := make(map[string]*mvccpb.KeyValue)
	for _, kv := range response.Kvs {
		key := string(kv.Key)
		if strings.HasPrefix(key, root+"cleanup-execution-slots/") || strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/execution") {
			rows[key] = kv
		}
	}
	return rows
}

func TestCleanupPersistent32RealTasks(t *testing.T) {
	require.NotEmpty(t, os.Getenv("TEST_ETCD_ENDPOINTS"), "real three-member fixture is mandatory; missing configuration is not a skipped PASS")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	base, raw := integrationBackend(t) // Exactly one cell for every producer and worker.
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options := integrationOptions(base, raw)
	options.RequestTimeout = time.Second
	options.Clock = goodAuthorityClock()
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	options.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
	profileKey, err := base.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	_, err = raw.Put(ctx, profileKey, workerTestProfile(options))
	require.NoError(t, err)
	producer, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	var groups [4][]*cleanupCapacityTaskState
	var extra *cleanupCapacityTaskState
	// This bounded pure search only chooses routing; every persisted Task is
	// subsequently produced by the original atomic lifecycle above.
	for candidate := 0; candidate < 65536; candidate++ {
		workspace, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("capacity/%d/", candidate))
		require.NoError(t, err)
		partition := int(workspace.Partition())
		if partition >= 4 || len(groups[partition]) >= 8 && (partition != 0 || extra != nil) {
			continue
		}
		ordinal := partition*8 + len(groups[partition])
		if len(groups[partition]) == 8 {
			ordinal = 32
		}
		task := &cleanupCapacityTaskState{ref: cleanupCapacityTask(t, ctx, producer, root, workspace, ordinal)}
		if ordinal == 32 {
			extra = task
		} else {
			groups[partition] = append(groups[partition], task)
		}
		if extra != nil && len(groups[0])+len(groups[1])+len(groups[2])+len(groups[3]) == 32 {
			break
		}
	}
	require.NotNil(t, extra)
	var peers [4]*cleanupCapacityPeer
	for i := range peers {
		require.Len(t, groups[i], 8)
		b, err := New(ctx, options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		session, err := b.OpenWorkerSession(ctx, fmt.Sprintf("capacity-worker-%d", i))
		require.NoError(t, err)
		part, err := b.ClaimWorkerPartition(ctx, session, uint8(i))
		require.NoError(t, err)
		peer := &cleanupCapacityPeer{b: b, session: session, part: part}
		peers[i] = peer
		for slot := i * 25; slot < (i+1)*25; slot++ {
			share, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, uint16(slot))
			require.NoError(t, err)
			peer.shares = append(peer.shares, share)
		}
		peer.observeNative(i == 0)
	}

	ready, start := make(chan struct{}, 4), make(chan struct{})
	results := make(chan error, 4)
	var workers sync.WaitGroup
	var release sync.Once
	unblock := func() { release.Do(func() { close(start) }) }
	// Register before launching. Fatal/timeout must cancel, unblock and join all
	// four native owners before any Backend cleanup can acquire their lifetimes.
	defer func() {
		cancel()
		unblock()
		workers.Wait()
	}()
	for i, peer := range peers {
		workers.Add(1)
		go func(p *cleanupCapacityPeer, tasks []*cleanupCapacityTaskState) {
			defer workers.Done()
			var result error
			defer func() { results <- result }()
			for _, task := range tasks {
				if result = p.prepare(ctx, task); result != nil {
					return
				}
			}
			ready <- struct{}{}
			select {
			case <-ctx.Done():
				result = ctx.Err()
				return
			case <-start:
			}
			for _, task := range tasks {
				if result = p.occupy(ctx, task, false); result != nil {
					return
				}
			}
		}(peer, groups[i])
	}
	for i := 0; i < 4; i++ {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("prepare barrier failed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	unblock()
	for i := 0; i < 4; i++ {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	workers.Wait()

	occupied := make(map[uint32]TaskReference)
	for _, tasks := range groups {
		for _, task := range tasks {
			s := task.occupied
			require.NotNil(t, s)
			require.Equal(t, "occupied", s.record.Phase)
			require.Equal(t, task.ref, s.record.Task)
			require.Equal(t, cleanupSlotTestIncarnation(s.record), s.record.Incarnation)
			require.Equal(t, task.evidence.bundle.birth, s.record.TaskBirth)
			require.NotNil(t, s.slot)
			require.Zero(t, s.slot.lease)
			require.Zero(t, s.execution.lease)
			require.Greater(t, s.slot.create, s.execution.create)
			require.Equal(t, s.slot.mod, s.execution.mod)
			_, duplicate := occupied[s.record.Slot]
			require.False(t, duplicate)
			occupied[s.record.Slot] = task.ref
		}
	}
	require.Len(t, occupied, 32)
	require.Equal(t, int64(1), peers[0].dropped.Load())
	require.NotNil(t, peers[0].unknown)
	full := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Len(t, full, 64)

	p := peers[0]
	require.NoError(t, p.prepare(ctx, extra))
	beforeConflicts := p.conflicts
	require.NoError(t, p.occupy(ctx, extra, true))
	require.Equal(t, 32, p.conflicts-beforeConflicts)
	require.Nil(t, extra.occupied)
	require.Equal(t, "selecting", extra.selected.record.Phase)
	afterFull := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Len(t, afterFull, 65)
	require.True(t, extra.selected.execution.matches(afterFull[extra.selected.execution.key]), "33rd Task must retain its exact selecting tuple after all 32 refusals")
	for key, kv := range full {
		require.Equal(t, kv, afterFull[key], key)
	}

	// The stale selecting tuple of the ACK-lost Task cannot reserve another
	// cell or rotate after its unique occupied E has already committed.
	first := groups[0][0]
	require.NoError(t, p.turn(ctx, func(o *workerDueOwner) error {
		if err := p.b.reserveCleanupSlot(o, first.evidence, first.selected); !errors.Is(err, ErrConflict) {
			return fmt.Errorf("same Task stale reserve: %w", err)
		}
		if err := p.b.rotateCleanupSlot(o, first.selected); !errors.Is(err, ErrConflict) {
			return fmt.Errorf("same Task stale rotation: %w", err)
		}
		return nil
	}))
	require.Equal(t, afterFull, cleanupCapacityRows(t, ctx, raw, base.namespace.Root()))

	// The sole deliberate raw E/S mutation is this named ABA experiment.
	// Same bytes at a later S birth must not authenticate the old occupation.
	abas := first.occupied
	_, err = raw.Delete(ctx, abas.slot.key)
	require.NoError(t, err)
	_, err = raw.Put(ctx, abas.slot.key, abas.slot.value)
	require.NoError(t, err)
	require.NoError(t, p.turn(ctx, func(o *workerDueOwner) error {
		_, err := p.b.readCleanupKnownState(o, abas)
		if err == nil {
			return errors.New("same-byte slot ABA accepted")
		}
		return nil
	}))
	beforeRelease := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Equal(t, afterFull[abas.execution.key], beforeRelease[abas.execution.key])
	require.Greater(t, beforeRelease[abas.slot.key].CreateRevision, abas.execution.mod)

	for i, peer := range peers {
		t.Logf("metadata peer=%d charged-turns=%d actual-txn=%d conflicts=%d actual-ACK-drops=%d", i, peer.turns, peer.rpcs.Load(), peer.conflicts, peer.dropped.Load())
		require.Zero(t, peer.claimPuts.Load(), "reservation-only scope; not a managed Claim gate proof")
		require.Zero(t, peer.lease.grants.Load(), "observation begins after original session creation")
		require.NoError(t, peer.b.ReleaseWorkerSession(ctx, peer.session))
		unexpected, stop, err := peer.b.beginExpiredTaskOwner(ctx, peer.part, peer.shares[0])
		if unexpected != nil {
			unexpected.finish()
		}
		if stop != nil {
			stop()
		}
		require.Error(t, err)
		require.Error(t, peer.b.rotateCleanupSlot(peer.lastOwner, groups[i][0].selected))
	}
	require.Equal(t, beforeRelease, cleanupCapacityRows(t, ctx, raw, base.namespace.Root()))
	for _, kv := range beforeRelease {
		require.Zero(t, kv.Lease)
	}
}
