package etcd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func validateTaskArchiveRecovery(o *TaskWorkspaceArchiveRecoveryOptions) (uint64, error) {
	if o == nil {
		return 0, nil
	}
	l := o.limits()
	i, err := taskArchiveIndexBytes(l.Entries)
	if err != nil || l.Jobs == 0 || l.Jobs > taskArchiveRecoverySlots || l.JobBytes < 2*taskArchiveRecoveryMetadataBytes+1536 || l.JobBytes > 1<<40 || l.TotalBytes < l.JobBytes || i > math.MaxUint64-4*taskArchiveRecoveryMetadataBytes {
		return 0, errTaskArchive
	}
	memory := i + 4*taskArchiveRecoveryMetadataBytes
	if l.JobIndexBytes < memory || l.TotalIndexBytes < memory || memory > uint64(math.MaxInt) || len(o.Directory) > 4096 || !filepath.IsAbs(o.Directory) || filepath.Clean(o.Directory) != o.Directory || !recoverySafeString(o.Directory) {
		return 0, errTaskArchive
	}
	return memory, nil
}

func recoveryLimitsDigest(o TaskWorkspaceArchiveRecoveryOptions) string {
	l := o.limits()
	parts := []string{"sandbox.workspace-archive-recovery.v1", o.Directory}
	for _, n := range []uint64{l.Jobs, l.JobBytes, l.TotalBytes, l.Entries, l.JobIndexBytes, l.TotalIndexBytes} {
		parts = append(parts, strconv.FormatUint(n, 10))
	}
	h := framedDigest(parts...)
	return hex.EncodeToString(h[:])
}

func newTaskArchiveRecoveryStore(o TaskWorkspaceArchiveRecoveryOptions) (*taskArchiveStore, error) {
	return newTaskArchiveRecoveryStoreContext(context.Background(), o)
}

func newTaskArchiveRecoveryStoreContext(ctx context.Context, o TaskWorkspaceArchiveRecoveryOptions) (*taskArchiveStore, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errTaskArchive
	}
	memory, err := validateTaskArchiveRecovery(&o)
	if err != nil {
		return nil, err
	}
	root, identity, err := taskArchiveRecoveryOpenRoot(o.Directory)
	if err != nil {
		return nil, err
	}
	i, _ := taskArchiveIndexBytes(o.Entries)
	s := &taskArchiveStore{root: root, limits: o.limits(), indexBytes: i, spools: make(map[*taskArchiveSpool]struct{}), recovery: &taskArchiveRecoveryStore{root: identity, directory: o.Directory, limitsDigest: recoveryLimitsDigest(o), memoryBytes: memory, gate: make(chan struct{}, 1)}}
	s.self = s
	s.recovery.gate <- struct{}{}
	if err = s.scanRecovery(ctx); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if s.recovery.storeID == "" {
		var id [32]byte
		if _, err = rand.Read(id[:]); err != nil {
			return nil, errors.Join(err, root.Close())
		}
		s.recovery.storeID = hex.EncodeToString(id[:])
	}
	return s, nil
}

func recoverySlotName(slot uint32) string { return fmt.Sprintf("slot-%02d", slot) }

func recoveryName(name string, jobs uint64) (uint32, uint8, error) {
	if len(name) < 11 || len(name) > 12 || !strings.HasPrefix(name, "slot-") || name[5] < '0' || name[5] > '9' || name[6] < '0' || name[6] > '9' {
		return 0, 0, errTaskArchive
	}
	slot := uint32(name[5]-'0')*10 + uint32(name[6]-'0')
	if uint64(slot) >= jobs {
		return 0, 0, errTaskArchive
	}
	switch name[7:] {
	case ".tar":
		return slot, 1, nil
	case ".json":
		return slot, 2, nil
	case ".next":
		return slot, 4, nil
	}
	return 0, 0, errTaskArchive
}

func (s *taskArchiveStore) chargeRecovery() error {
	memory := s.recovery.memoryBytes
	if s.jobs >= s.limits.Jobs || s.disk > s.limits.TotalBytes-s.limits.JobBytes || s.memory > s.limits.TotalIndexBytes-memory {
		return errTaskArchive
	}
	s.jobs++
	s.disk += s.limits.JobBytes
	s.memory += memory
	return nil
}

func (s *taskArchiveStore) recoveryBuffers() *taskArchiveRecoveryBuffers {
	if s.recovery.buffers == nil {
		s.recovery.buffers = &taskArchiveRecoveryBuffers{}
	}
	return s.recovery.buffers
}

func recoveryRead(f *os.File, input []byte) ([]byte, error) {
	st, err := f.Stat()
	if err != nil || st.Size() <= 0 || st.Size() > taskArchiveRecoveryWireBound || len(input) != taskArchiveRecoveryMetadataBytes {
		return nil, errors.Join(errTaskArchive, err)
	}
	n, err := f.ReadAt(input[:int(st.Size())], 0)
	if err != nil || n != int(st.Size()) {
		return nil, errors.Join(errTaskArchive, err)
	}
	// Check both length and descriptor identity again after the bounded read.
	after, err := f.Stat()
	if err != nil || after.Size() != st.Size() {
		return nil, errors.Join(errTaskArchive, err)
	}
	return input[:n], nil
}

func (s *taskArchiveStore) scanRecovery(ctx context.Context) error {
	names, err := s.root.Readdirnames(193)
	// For n>0, os.File can return a short batch with nil error; EOF is reported
	// by the next read. The combined allocation and read count remain <=193.
	for err == nil && len(names) < 193 {
		more, readErr := s.root.Readdirnames(193 - len(names))
		names = append(names, more...)
		err = readErr
	}
	if len(names) > 192 || err != io.EOF {
		return errors.Join(errTaskArchive, err)
	}
	var present [taskArchiveRecoverySlots]uint8
	for _, name := range names {
		slot, kind, err := recoveryName(name, s.limits.Jobs)
		if err != nil || present[slot]&kind != 0 {
			return errTaskArchive
		}
		present[slot] |= kind
	}
	// All occupied slots are charged before any metadata or archive is opened.
	for slot, files := range present {
		if files != 0 {
			if err := s.chargeRecovery(); err != nil {
				return err
			}
			s.recovery.slots[slot].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, Quarantined: true}
		}
	}
	var content [taskArchiveRecoverySlots]uint64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot, kind, _ := recoveryName(name, s.limits.Jobs)
		limit := uint64(taskArchiveRecoveryMetadataBytes)
		if kind == 1 {
			limit = s.limits.JobBytes - 2*taskArchiveRecoveryMetadataBytes
		}
		size, err := taskArchiveRecoveryContentSize(s.root, name)
		if err != nil || size > limit || size > s.limits.JobBytes-content[slot] {
			return errors.Join(errTaskArchive, err)
		}
		content[slot] += size
	}
	var index []byte
	for slot, files := range present {
		if files == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		base := recoverySlotName(uint32(slot))
		f, _, err := taskArchiveRecoveryOpen(s.root, base+".json")
		if err != nil {
			continue
		}
		buf := s.recoveryBuffers()
		in, err := recoveryRead(f, buf.input[:])
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			continue
		}
		m, err := decodeTaskArchiveRecovery(in, buf.output[:])
		if err != nil || m.Slot != uint32(slot) {
			continue
		}
		if s.recovery.storeID != "" && s.recovery.storeID != m.StoreID || m.LimitsDigest != s.recovery.limitsDigest {
			return errTaskArchive
		}
		s.recovery.storeID = m.StoreID
		if files != 3 || m.Root != s.recovery.root || m.Artifact == nil || m.State == "pending" {
			continue
		}
		tarf, id, err := taskArchiveRecoveryOpen(s.root, base+".tar")
		if err != nil {
			continue
		}
		if index == nil {
			index = make([]byte, int(s.indexBytes))
		} else {
			clear(index)
		}
		err = s.verifyColdTar(ctx, tarf, base+".tar", id, m, index)
		err = errors.Join(err, tarf.Close())
		if err == nil {
			s.recovery.slots[slot].diagnostic = recoveryDiagnostic(m)
		}
	}
	return errors.Join(ctx.Err(), taskArchiveRecoveryCheckRoot(s.root, s.recovery.directory, s.recovery.root))
}

func (s *taskArchiveStore) verifyColdTar(ctx context.Context, f *os.File, name string, id taskArchiveIdentity, m taskArchiveRecoveryManifest, index []byte) error {
	if m.Artifact == nil || m.Received == nil || id != (taskArchiveIdentity{m.Artifact.Dev, m.Artifact.Ino}) || m.Received.Bytes > s.limits.JobBytes-2*taskArchiveRecoveryMetadataBytes || m.Received.Entries > s.limits.Entries {
		return errTaskArchive
	}
	st, err := f.Stat()
	if err != nil || st.Size() < 0 || uint64(st.Size()) != m.Received.Bytes {
		return errors.Join(errTaskArchive, err)
	}
	h := sha256.New()
	var scratch [32768]byte
	n, err := io.CopyBuffer(h, taskArchiveContextReader{ctx, io.NewSectionReader(f, 0, st.Size())}, scratch[:])
	if err != nil || uint64(n) != m.Received.Bytes || hex.EncodeToString(h.Sum(nil)) != m.Received.SHA256 {
		return errors.Join(errTaskArchive, err)
	}
	entries, err := taskArchiveValidateTar(ctx, f, m.Received.Bytes, s.limits.Entries, index)
	if err != nil || entries != m.Received.Entries {
		return errors.Join(errTaskArchive, err)
	}
	return taskArchiveCheck(s.root, name, f, id)
}

func recoveryDiagnostic(m taskArchiveRecoveryManifest) taskArchiveRecoveryDiagnostic {
	d := taskArchiveRecoveryDiagnostic{Occupied: true, State: 1}
	copy(d.TaskID[:], m.Seed.Task.TaskID)
	store, _ := hex.DecodeString(m.StoreID)
	copy(d.StoreID[:], store)
	if m.Received != nil {
		d.State, d.Bytes, d.Entries = 2, m.Received.Bytes, m.Received.Entries
		h, _ := hex.DecodeString(m.Received.SHA256)
		copy(d.SHA256[:], h)
	}
	if m.Binding != nil {
		d.State = 3
		p, _ := hex.DecodeString(m.Binding.Plan.Digest)
		t, _ := hex.DecodeString(m.Binding.TargetDigest)
		copy(d.PlanDigest[:], p)
		copy(d.TargetDigest[:], t)
	}
	return d
}

func (s *taskArchiveStore) coldSnapshot(slot uint32) (taskArchiveRecoveryDiagnostic, error) {
	if s == nil || s.self != s || s.recovery == nil || uint64(slot) >= s.limits.Jobs {
		return taskArchiveRecoveryDiagnostic{}, errTaskArchive
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovery.slots[slot].diagnostic, nil
}

// An initialization error returns the already closed original spool only as a
// charged-attempt marker, never file/witness authority. Precharge errors return nil.
func (s *taskArchiveStore) reserveRecovery(ctx context.Context, seed taskArchiveRecoverySeed) (*taskArchiveSpool, error) {
	if s == nil || s.self != s || s.recovery == nil || ctx == nil || seed.validate() != nil {
		return nil, errTaskArchive
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errTaskArchive
	}
	var slot uint32
	for ; uint64(slot) < s.limits.Jobs; slot++ {
		if !s.recovery.slots[slot].diagnostic.Occupied {
			break
		}
	}
	if uint64(slot) == s.limits.Jobs || s.chargeRecovery() != nil {
		s.mu.Unlock()
		return nil, errTaskArchive
	}
	s.recovery.slots[slot].diagnostic = taskArchiveRecoveryDiagnostic{Occupied: true, Quarantined: true}
	owned, cancel := context.WithCancel(ctx)
	life, closeCancel := context.WithCancel(context.Background())
	r := &taskArchiveSpool{store: s, name: recoverySlotName(slot) + ".tar", ctx: owned, cancel: cancel, active: true, ownerDone: make(chan struct{}), recovery: &taskArchiveRecoveryOwner{slot: slot, ctx: life, cancel: closeCancel}}
	r.self = r
	s.spools[r] = struct{}{}
	s.mu.Unlock()
	err := r.initializeRecovery(seed)
	s.mu.Lock()
	r.active = false
	close(r.ownerDone)
	s.mu.Unlock()
	if err != nil {
		return r, errors.Join(err, r.close())
	}
	return r, nil
}

func (r *taskArchiveSpool) recoveryGate(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.recovery.ctx.Done():
		return nil, r.recovery.ctx.Err()
	case <-r.store.recovery.gate:
		if err := errors.Join(ctx.Err(), r.recovery.ctx.Err()); err != nil {
			r.store.recovery.gate <- struct{}{}
			return nil, err
		}
		return func() { r.store.recovery.gate <- struct{}{} }, nil
	}
}

func (r *taskArchiveSpool) initializeRecovery(seed taskArchiveRecoverySeed) error {
	unlock, err := r.recoveryGate(r.ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s := r.store
	m := taskArchiveRecoveryManifest{Version: 1, Slot: r.recovery.slot, State: "pending", StoreID: s.recovery.storeID, LimitsDigest: s.recovery.limitsDigest, Root: s.recovery.root, Seed: seed}
	r.recoveryProbe("pending-before")
	if err = r.commitRecovery(r.ctx, m, false); err != nil {
		return err
	}
	r.file, r.identity, err = taskArchiveCreate(s.root, r.name)
	if err != nil {
		return err
	}
	r.recoveryProbe("artifact-created")
	m.Artifact = &taskArchiveRecoveryArtifact{r.identity.dev, r.identity.ino}
	if err = r.commitRecovery(r.ctx, m, true); err != nil {
		return err
	}
	r.index = make([]byte, int(s.indexBytes))
	return r.ctx.Err()
}

func (r *taskArchiveSpool) commitRecovery(ctx context.Context, next taskArchiveRecoveryManifest, existing bool) error {
	s, recovery := r.store, r.store.recovery
	if err := errors.Join(ctx.Err(), r.recovery.ctx.Err(), taskArchiveRecoveryCheckRoot(s.root, recovery.directory, recovery.root)); err != nil {
		return err
	}
	buf := s.recoveryBuffers()
	base := recoverySlotName(r.recovery.slot)
	if existing {
		f, _, err := taskArchiveRecoveryOpen(s.root, base+".json")
		if err != nil {
			return err
		}
		in, readErr := recoveryRead(f, buf.input[:])
		closeErr := f.Close()
		old, encodeErr := encodeTaskArchiveRecovery(r.recovery.manifest, buf.output[:])
		if readErr != nil || closeErr != nil || encodeErr != nil || !bytes.Equal(in, old) {
			return errors.Join(errTaskArchive, readErr, closeErr, encodeErr)
		}
	}
	wire, err := encodeTaskArchiveRecovery(next, buf.output[:])
	if err != nil {
		return err
	}
	prefix := next.State
	if prefix == "pending" && next.Artifact != nil {
		prefix = "artifact"
	}
	if _, err = taskArchiveRecoveryCommit(s.root, base, wire, func(stage string) { r.recoveryProbe(prefix + "-" + stage) }); err != nil {
		return err
	}
	// This durable generation remains history even when the postguard fails.
	r.recovery.manifest = next
	s.mu.Lock()
	s.recovery.slots[r.recovery.slot].diagnostic = recoveryDiagnostic(next)
	s.mu.Unlock()
	return errors.Join(ctx.Err(), r.recovery.ctx.Err(), taskArchiveRecoveryCheckRoot(s.root, recovery.directory, recovery.root))
}

func (r *taskArchiveSpool) archiveBytesLimit() uint64 {
	if r.store.recovery != nil {
		return r.store.limits.JobBytes - 2*taskArchiveRecoveryMetadataBytes
	}
	return r.store.limits.JobBytes
}

// Called only while the original validated claim/draft chain remains in the
// receive admission. Fixed roles replace neither permanent history nor receipts.
func originalArchiveRecoverySeed(c *TaskClaim, d *taskArchiveDraft) (taskArchiveRecoverySeed, error) {
	if c == nil || c.self != c || d == nil || d.finalSync == nil || d.finalSync != c.finalSyncDraft || d.finalSync.archive != d || d.finalSync.record.Claim != c.reference {
		return taskArchiveRecoverySeed{}, errTaskArchive
	}
	task, err := encodeTaskRecord(d.finalSync.record.Task)
	if err != nil {
		return taskArchiveRecoverySeed{}, err
	}
	seed := taskArchiveRecoverySeed{Task: c.reference.Task, TaskBirth: c.birth, TaskDigest: workspaceSyncIntentDigest(task), ClaimID: c.reference.ClaimID, WorkerID: c.reference.WorkerID, LeaseID: c.reference.LeaseID, ClaimRevision: c.reference.CreateRevision, ClaimDigest: workspaceSyncIntentDigest(c.value), FinalSyncAttempt: d.finalSync.record.AttemptID, FinalSyncRevision: d.finalSync.revision, FinalSyncDigest: workspaceSyncIntentDigest(d.finalSync.value), ArchiveCommand: d.reference().CommandID, ArchiveRevision: d.revision, ArchiveDigest: workspaceSyncIntentDigest(d.value), IssuerRevision: d.issuerRevision, IssuerDigest: workspaceSyncIntentDigest(d.issuerValue), TicketDigest: d.ticket.Digest()}
	return seed, seed.validate()
}

// The receive owner has already physically returned before this registration.
// Add/Wait is sealed under the same admission mutex as spool Close, so metadata
// waiters and IO cannot outlive the descriptor they actually use.
func (r *taskArchiveSpool) beginRecoveryMetadata(ctx context.Context) (context.Context, func(), error) {
	if r == nil || r.self != r || r.store == nil || r.store.self != r.store || r.recovery == nil || ctx == nil {
		return nil, nil, errTaskArchive
	}
	s := r.store
	s.mu.Lock()
	if s.closed || r.closing || r.active || r.recovery.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.Unlock()
		return nil, nil, errors.Join(errTaskArchive, ctx.Err(), r.recovery.ctx.Err())
	}
	r.recovery.metadata.Add(1)
	s.mu.Unlock()
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.recovery.ctx, cancel)
	return bounded, func() { stop(); cancel(); r.recovery.metadata.Done() }, nil
}

func (r *taskArchiveSpool) sealOriginalReceived(ctx context.Context, original *taskReceivedArchive) error {
	if r != nil && r.self == r && r.store != nil && r.store.self == r.store && r.store.recovery == nil {
		return nil
	}
	if r != nil && r.ctx != nil && r.ctx.Err() != nil {
		return r.ctx.Err() // receiving and received seal retain original session expiry
	}
	bounded, finish, err := r.beginRecoveryMetadata(ctx)
	if err != nil {
		return err
	}
	defer finish()
	s := r.store
	s.mu.Lock()
	valid := original != nil && original.self == original && original.origin == r && r.witness == original && r.started && r.consumer == nil
	s.mu.Unlock()
	if !valid {
		return errTaskArchive
	}
	unlock, err := r.recoveryGate(bounded)
	if err != nil {
		return err
	}
	defer unlock()
	m, d := r.recovery.manifest, original.diagnostics
	if m.State != "pending" || m.Artifact == nil || m.Seed.TicketDigest != d.TicketDigest || d.Bytes < 1536 || d.Bytes > r.archiveBytesLimit() || d.Entries == 0 || d.Entries > s.limits.Entries || !validHexDigest(d.SHA256) {
		return errTaskArchive
	}
	if err := taskArchiveCheck(s.root, r.name, r.file, r.identity); err != nil {
		return err
	}
	st, err := r.file.Stat()
	if err != nil || st.Size() < 0 || uint64(st.Size()) != d.Bytes {
		return errors.Join(errTaskArchive, err)
	}
	if err := errors.Join(bounded.Err(), r.file.Sync()); err != nil {
		return err
	}
	r.recoveryProbe("tar-fsync")
	if err := taskArchiveCheck(s.root, r.name, r.file, r.identity); err != nil {
		return err
	}
	m.State = "received"
	m.Received = &taskArchiveRecoveryReceived{d.Bytes, d.Entries, d.SHA256}
	return r.commitRecovery(bounded, m, true)
}

func recoverySameBinding(a, b taskArchiveRecoveryBinding) bool {
	ap, bp := a.Plan, b.Plan
	if a.Local != b.Local || a.TargetDigest != b.TargetDigest || ap.Provider != bp.Provider || ap.StorageIdentityHash != bp.StorageIdentityHash || ap.Bucket != bp.Bucket || ap.Prefix != bp.Prefix || ap.Hash != bp.Hash || ap.Partition != bp.Partition || ap.DeleteExtraneous != bp.DeleteExtraneous || ap.Digest != bp.Digest || len(ap.Exclude) != len(bp.Exclude) {
		return false
	}
	for i := range ap.Exclude {
		if ap.Exclude[i] != bp.Exclude[i] {
			return false
		}
	}
	return true
}

func (r *taskArchiveSpool) sealOriginalBinding(ctx context.Context, source *PreparedTaskWorkspaceSyncSource, body *taskWorkspaceSyncBody) error {
	if r != nil && r.self == r && r.store != nil && r.store.self == r.store && r.store.recovery == nil {
		return nil
	}
	bounded, finish, err := r.beginRecoveryMetadata(ctx)
	if err != nil {
		return err
	}
	defer finish()
	s := r.store
	s.mu.Lock()
	valid := source != nil && source.self == source && r.consumer == source && source.archive != nil && source.archive.witness == r.witness && source.archive.owner != nil && source.archive.owner.spool == r
	s.mu.Unlock()
	if !valid || !source.originalLocalBody(body) || body.localTarget == nil || source.plan.validate() != nil {
		return errTaskArchive
	}
	body.mu.Lock()
	active := body.active && !body.closed && body.ctx.Err() == nil
	body.mu.Unlock()
	if !active {
		return errTaskArchive
	}
	unlock, err := r.recoveryGate(bounded)
	if err != nil {
		return err
	}
	defer unlock()
	if err := errors.Join(source.local(bounded), body.localTarget.check(bounded)); err != nil {
		return err
	}
	if err := r.checkRecoveryRoot(); err != nil {
		return err
	}
	m := r.recovery.manifest
	if m.Received == nil || m.Seed.Task != source.archive.owner.claim.reference.Task || m.Seed.ClaimID != source.archive.owner.claim.reference.ClaimID || m.Seed.TicketDigest != source.diagnostics.TicketDigest || *m.Received != (taskArchiveRecoveryReceived{source.diagnostics.Bytes, source.diagnostics.Entries, source.diagnostics.SHA256}) {
		return errTaskArchive
	}
	local, err := encodeOperationMetadata(body.localTarget.pin, 16384)
	if err != nil {
		return err
	}
	binding := taskArchiveRecoveryBinding{recoveryPlanWire(source.plan), body.localTarget.pin, workspaceSyncIntentDigest(local)}
	if m.State == "bound" {
		if m.Binding == nil || !recoverySameBinding(*m.Binding, binding) {
			return ErrIdentityMismatch
		}
	} else {
		if m.State != "received" || m.Binding != nil {
			return errTaskArchive
		}
		m.State, m.Binding = "bound", &binding
		r.recoveryProbe("bound-before")
		if err := r.commitRecovery(bounded, m, true); err != nil {
			return err
		}
	}
	// Both actual original source history and the physical target are checked
	// after the durable bound generation, immediately before the first reserve.
	if err := source.guard(bounded); err != nil {
		return err
	}
	if err := body.localTarget.check(bounded); err != nil {
		return err
	}
	r.recoveryProbe("bound-postguard")
	return errors.Join(bounded.Err(), r.recovery.ctx.Err(), source.local(bounded), body.localTarget.ctx.Err())
}

func (r *taskArchiveSpool) recoveryProbe(stage string) {
	if r.store.recovery != nil && r.store.recovery.probe != nil {
		r.store.recovery.probe(stage)
	}
}

func (r *taskArchiveSpool) checkRecoveryRoot() error {
	if r.store.recovery == nil {
		return nil
	}
	return taskArchiveRecoveryCheckRoot(r.store.root, r.store.recovery.directory, r.store.recovery.root)
}
