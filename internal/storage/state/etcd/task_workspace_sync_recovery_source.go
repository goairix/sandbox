package etcd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *taskArchiveStore) registerRecovery(ctx context.Context, slot uint32) (*taskWorkspaceSyncRecoverySource, error) {
	if s == nil || s.self != s || s.recovery == nil || ctx == nil || uint64(slot) >= s.limits.Jobs {
		return nil, ErrInvalidRecord
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.recovery.slots[slot].diagnostic
	if s.closed || ctx.Err() != nil || !d.Occupied || d.Quarantined || d.State != 3 || s.recovery.slots[slot].owner != nil {
		return nil, ErrConflict
	}
	// Duplicate cold slots must not create two cooperative writers for the same
	// task/target under one fresh claim. The fixed array needs no task map.
	for _, candidate := range s.recovery.slots {
		if candidate.owner != nil && candidate.diagnostic.TaskID == d.TaskID {
			return nil, ErrConflict
		}
	}
	for r := range s.spools {
		if r.recovery != nil && (r.recovery.slot == slot || s.recovery.slots[r.recovery.slot].diagnostic.TaskID == d.TaskID) {
			return nil, ErrConflict
		}
	}
	owned, cancel := context.WithCancel(ctx)
	r := &taskWorkspaceSyncRecoverySource{store: s, slot: slot, diagnostic: d, ctx: owned, cancel: cancel, done: make(chan struct{})}
	r.self = r
	s.recovery.slots[slot].owner = r
	go r.own()
	return r, nil
}

func (s *taskWorkspaceSyncRecoverySource) own() {
	<-s.ctx.Done()
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
	s.ops.Wait()
	if s.parentStop != nil {
		s.parentStop()
	}
	if s.target != nil {
		s.err = errors.Join(s.err, s.target.Close())
	}
	if s.file != nil {
		s.err = errors.Join(s.err, s.file.Close())
	}
	s.index = nil
	s.store.mu.Lock()
	// The slot stays charged. Only its live owner is cleared after real joins.
	s.store.recovery.slots[s.slot].owner = nil
	close(s.done)
	s.store.mu.Unlock()
}

func (s *taskWorkspaceSyncRecoverySource) begin(ctx context.Context) error {
	if s == nil || s.self != s || ctx == nil {
		return ErrInvalidRecord
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.busy || s.ctx.Err() != nil || ctx.Err() != nil {
		return ErrGuardExpired
	}
	s.busy = true
	s.ops.Add(1)
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
	s.ops.Done()
}

func (s *taskWorkspaceSyncRecoverySource) Close() error {
	if s == nil || s.self != s {
		return ErrInvalidRecord
	}
	s.cancel()
	<-s.done
	return s.err
}

func (s *taskWorkspaceSyncRecoverySource) load(ctx context.Context) error {
	r := s.store.recovery
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-r.gate:
	}
	defer func() {
		// decode owns strings/slices; no canonical wire escapes this gate. Drop
		// both metadata arrays before history/query/body/parent-handle storage.
		r.buffers = nil
		r.gate <- struct{}{}
	}()
	if err := errors.Join(ctx.Err(), s.ctx.Err(), taskArchiveRecoveryCheckRoot(s.store.root, r.directory, r.root)); err != nil {
		return err
	}
	f, _, err := taskArchiveRecoveryOpen(s.store.root, recoverySlotName(s.slot)+".json")
	if err != nil {
		return err
	}
	buf := s.store.recoveryBuffers()
	in, err := recoveryRead(f, buf.input[:])
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	m, err := decodeTaskArchiveRecovery(in, buf.output[:])
	if err != nil || m.Slot != s.slot || m.State != "bound" || m.StoreID != r.storeID || m.Root != r.root || m.LimitsDigest != r.limitsDigest || recoveryDiagnostic(m) != s.diagnostic {
		return errors.Join(ErrIdentityMismatch, err)
	}
	s.file, s.identity, err = taskArchiveRecoveryOpen(s.store.root, recoverySlotName(s.slot)+".tar")
	if err != nil {
		return err
	}
	s.index = make([]byte, int(s.store.indexBytes))
	before, err := workspaceSyncRecoveryArchiveStamp(s.file)
	if err != nil {
		return err
	}
	if err := s.store.verifyColdTar(ctx, s.file, recoverySlotName(s.slot)+".tar", s.identity, m, s.index); err != nil {
		return err
	}
	s.stamp, err = workspaceSyncRecoveryArchiveStamp(s.file)
	if err != nil || before != s.stamp {
		return errors.Join(ErrIdentityMismatch, err)
	}
	s.manifest = m
	s.plan, err = m.Binding.Plan.plan()
	return err
}

func (b *Backend) prepareTaskWorkspaceSyncRecovery(ctx context.Context, c *TaskClaim, slot uint32, destination *controltransport.Destination, worker *launcher.KernelBoundary) (_ *taskWorkspaceSyncRecoverySource, resultErr error) {
	if b == nil || ctx == nil || !b.validTaskClaim(c) || b.archiveStore == nil || b.archiveStore.recovery == nil || b.workspaceSyncBudget == nil || b.workspaceSyncBudget.profile != 2 || destination == nil || worker == nil {
		return nil, ErrInvalidRecord
	}
	if err := taskArchiveRecoverySupported(); err != nil {
		return nil, err
	}
	s, err := b.archiveStore.registerRecovery(ctx, slot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, s.Close())
		}
	}()
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	s.parentStop = context.AfterFunc(c.parentCtx, s.cancel)
	// Current claim/operator/prefix/budget precede metadata IO. Full historical
	// dual-drain and all 50 pins are acquired separately after parsing.
	err = b.guardTaskWorkspaceRecoveryClaim(s.ctx, c)
	if err == nil {
		err = s.load(s.ctx)
	}
	if err == nil {
		var q *PreparedTaskQuiescence
		q, err = b.PrepareAfterDualDrain(s.ctx, c, destination)
		if err == nil {
			s.authority, err = b.pinTaskArchiveRecovery(s.ctx, c, s.manifest, q)
		}
		if err == nil {
			_, err = s.authority.queryProducer(s.ctx, destination)
		}
		if err == nil {
			s.target, err = openTaskWorkspaceSyncLocalTarget(s.ctx, worker, s.plan, s.manifest.Binding.Local.Root)
		}
		if err == nil && !workspaceSyncRecoverySameTarget(s.manifest.Binding.Local, s.target.pin) {
			err = ErrIdentityMismatch
		}
	}
	if err == nil {
		s.cursor = newWorkspaceSyncArchiveCursor(s.manifest.Received.Bytes, s.manifest.Received.Entries, s.index, s.plan, s.readAt)
	}
	s.end()
	if err != nil {
		return nil, err
	}
	if err := s.restoreCheckpoint(s.ctx); err != nil {
		return nil, err
	}
	go s.monitor()
	return s, nil
}

func (b *Backend) guardTaskWorkspaceRecoveryClaim(ctx context.Context, c *TaskClaim) error {
	c.mu.Lock()
	if err := c.live(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	cmps := append(b.baseComparisons(), c.comparisons()...)
	c.mu.Unlock()
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return err
	}
	cmps = append(cmps, clientv3.Compare(clientv3.Version(prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(prefix)))
	cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	keys := []string{b.identityKey}
	_, err = workspaceSyncNativeTxn(ctx, b, cmps, workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{b.identityValue}), false)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) monitor() {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			if err := s.begin(s.ctx); err != nil {
				if s.ctx.Err() != nil {
					return
				}
				if s.local(s.ctx) != nil {
					s.cancel()
					return
				}
				timer.Reset(250 * time.Millisecond)
				continue
			}
			err := s.guard(s.ctx)
			s.end()
			if err != nil {
				s.cancel()
				return
			}
			timer.Reset(250 * time.Millisecond)
		}
	}
}

func workspaceSyncRecoverySameTarget(a, b taskWorkspaceSyncLocalAttribution) bool {
	x, y := a.Worker, b.Worker
	if x.BootID != y.BootID || x.PIDNamespaceDev != y.PIDNamespaceDev || x.PIDNamespaceIno != y.PIDNamespaceIno || x.MountNamespaceDev != y.MountNamespaceDev || x.MountNamespaceIno != y.MountNamespaceIno {
		return false
	}
	a.Worker = b.Worker
	return a == b
}

// local never performs IO and may run while nativeMu is held. Source begin/end
// owns descriptor lifetime; physical checks deliberately run outside nativeMu.
func (s *taskWorkspaceSyncRecoverySource) local(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || s.ctx.Err() != nil {
		return ErrGuardExpired
	}
	return s.authority.local(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) physical(ctx context.Context) error {
	if err := s.local(ctx); err != nil {
		return err
	}
	r := s.store.recovery
	if err := taskArchiveRecoveryCheckRoot(s.store.root, r.directory, r.root); err != nil {
		return err
	}
	if err := taskArchiveCheck(s.store.root, recoverySlotName(s.slot)+".tar", s.file, s.identity); err != nil {
		return err
	}
	stamp, err := workspaceSyncRecoveryArchiveStamp(s.file)
	if err != nil || stamp != s.stamp {
		return errors.Join(ErrIdentityMismatch, err)
	}
	if s.target != nil {
		return errors.Join(s.target.check(ctx), s.local(ctx))
	}
	return s.local(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) comparisons(charge *taskWorkspaceSyncRecoveryCharge) []clientv3.Cmp {
	a, b := s.authority, s.authority.backend
	cmps := append([]clientv3.Cmp(nil), a.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(a.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(a.prefix)))
	cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	if charge != nil {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(charge.intentKey), "=", charge.birth), clientv3.Compare(clientv3.ModRevision(charge.intentKey), "=", charge.birth), clientv3.Compare(clientv3.Value(charge.intentKey), "=", charge.value))
	}
	return cmps
}

func (s *taskWorkspaceSyncRecoverySource) native(ctx context.Context, cmps []clientv3.Cmp, success []clientv3.Op, protoSuccess []*pb.RequestOp, reply *pb.TxnResponse, emptyFailure bool) (*clientv3.TxnResponse, error) {
	a := s.authority
	a.nativeMu.Lock()
	defer a.nativeMu.Unlock()
	if err := s.local(ctx); err != nil {
		return nil, err
	}
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	r, err := workspaceSyncNativeTxn(bounded, a.backend, cmps, success, protoSuccess, reply, emptyFailure)
	return r, errors.Join(err, s.local(ctx))
}

func (s *taskWorkspaceSyncRecoverySource) guard(ctx context.Context) error {
	if err := s.physical(ctx); err != nil {
		return err
	}
	var charge *taskWorkspaceSyncRecoveryCharge
	if s.body != nil {
		charge = s.body.charge
	}
	cmps := s.comparisons(charge)
	if charge != nil {
		g := charge.generation
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(charge.slotKey), "=", g.create), clientv3.Compare(clientv3.ModRevision(charge.slotKey), "=", g.mod), clientv3.Compare(clientv3.Value(charge.slotKey), "=", g.value))
	}
	keys := []string{s.authority.backend.identityKey}
	_, err := s.native(ctx, cmps, workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, []string{s.authority.backend.identityValue}), false)
	if err != nil {
		return err
	}
	return s.physical(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) readAt(ctx context.Context, data []byte, offset int64) (int, error) {
	if offset < 0 || uint64(offset) > s.manifest.Received.Bytes || uint64(len(data)) > s.manifest.Received.Bytes-uint64(offset) {
		return 0, ErrInvalidRecord
	}
	total := 0
	for len(data) > 0 {
		if err := s.physical(ctx); err != nil {
			return total, err
		}
		chunk := data
		if len(chunk) > 32768 {
			chunk = chunk[:32768]
		}
		n, err := s.file.ReadAt(chunk, offset+int64(total))
		total += n
		if after := s.physical(ctx); after != nil {
			return total, errors.Join(err, after)
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
		data = data[n:]
	}
	return total, nil
}

func (s *taskWorkspaceSyncRecoverySource) next(ctx context.Context) (*workspaceSyncArchiveEntry, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	if s.body != nil && !s.body.settled {
		return nil, ErrConflict
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	s.current, s.body = nil, nil
	e, err := s.cursor.next(ctx)
	if err != nil {
		if err == io.EOF {
			if after := s.guard(ctx); after != nil {
				return nil, after
			}
		}
		return nil, err
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	s.current = e
	return e, nil
}

type workspaceSyncRecoveryReadAt struct {
	source *taskWorkspaceSyncRecoverySource
	ctx    context.Context
}

func (r workspaceSyncRecoveryReadAt) ReadAt(data []byte, offset int64) (int, error) {
	return r.source.readAt(r.ctx, data, offset)
}

func (s *taskWorkspaceSyncRecoverySource) openBody(ctx context.Context, e *workspaceSyncArchiveEntry) (*taskWorkspaceSyncRecoveryBody, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	if e == nil || s.current != e || e.kind != tar.TypeReg || s.body != nil {
		return nil, ErrInvalidRecord
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(s.ctx)
	body := &taskWorkspaceSyncRecoveryBody{source: s, entry: e, ctx: owned, cancel: cancel, hash: sha256.New(), reader: io.NewSectionReader(workspaceSyncRecoveryReadAt{s, owned}, e.offset, e.size)}
	body.self, s.body = body, body
	return body, nil
}

func (b *taskWorkspaceSyncRecoveryBody) readOwned(ctx context.Context, data []byte) (int, error) {
	if err := errors.Join(b.ctx.Err(), b.source.guard(ctx)); err != nil {
		return 0, err
	}
	if len(data) > 32768 {
		data = data[:32768]
	}
	n, err := b.reader.Read(data)
	b.bytes += int64(n)
	b.hash.Write(data[:n])
	if after := b.source.guard(ctx); after != nil {
		return n, errors.Join(err, after)
	}
	if b.bytes == b.entry.size && (err == nil || err == io.EOF) {
		if hex.EncodeToString(b.hash.Sum(nil)) != b.entry.sha256 {
			return n, ErrIdentityMismatch
		}
		b.complete = true
		return n, io.EOF
	}
	if err != nil {
		return n, errors.Join(io.ErrUnexpectedEOF, err)
	}
	return n, nil
}

func (b *taskWorkspaceSyncRecoveryBody) Read(data []byte) (int, error) {
	if b == nil || b.self != b {
		return 0, ErrInvalidRecord
	}
	if err := b.source.begin(b.ctx); err != nil {
		return 0, err
	}
	defer b.source.end()
	b.mu.Lock()
	if b.closed || b.active {
		b.mu.Unlock()
		return 0, ErrInvalidRecord
	}
	b.active, b.done = true, make(chan struct{})
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.active = false; close(b.done); b.mu.Unlock() }()
	return b.readOwned(b.ctx, data)
}

func (b *taskWorkspaceSyncRecoveryBody) Close() error {
	if b == nil || b.self != b {
		return ErrInvalidRecord
	}
	b.mu.Lock()
	b.closed = true
	active, done := b.active, b.done
	if active {
		b.cancel()
	}
	b.mu.Unlock()
	if active {
		<-done
	}
	b.cancel()
	if !b.complete {
		b.source.cancel()
		return errTaskArchive
	}
	return nil
}
