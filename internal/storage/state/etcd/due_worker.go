package etcd

import (
	"context"
	"errors"
	"time"
)

// DueWorkerOptions assigns exact fixed scheduling shares to one resident
// original session. The slices are copied; no other slots are probed on conflict.
type DueWorkerOptions struct {
	// WorkerID is the bounded diagnostic identity for this original session.
	WorkerID string
	// Partitions contains 1..256 distinct logical partition IDs.
	Partitions []uint8
	// RangeSlots and ClaimSlots contain distinct IDs below 20 and 100.
	RangeSlots, ClaimSlots []uint16
	// RelistSlot is one lifetime-owned fixed slot below four.
	RelistSlot uint16
	// StageTTL is integral seconds in [1s,24h]; zero selects 30s.
	StageTTL time.Duration
}

// DueWorkerPhase describes local discovery/admission state, not task execution.
type DueWorkerPhase string

// DueWorkerPartitionStatus is an owned diagnostic snapshot without authority.
// CoveredRevision is proved coverage; ObservedRevision can include a newer tail
// from the post-guard, which will be replayed by a subsequent finite batch.
type DueWorkerPartitionStatus struct {
	Partition                                               uint8
	Phase                                                   DueWorkerPhase
	Revision, ReplayFrom, ObservedRevision, CoveredRevision int64
	WindowEnd                                               time.Time
	Frontier                                                string
	Synchronized                                            bool
	Queued                                                  int
	OldestExpiry                                            time.Time
	Pending                                                 StageReference
	Invalidation                                            string
}

// DueWorkerStatus reports bounded local progress. Synchronized never asserts
// physical sandbox destruction; permanent tasks remain the executor's input.
type DueWorkerStatus struct {
	Session               WorkerSessionReference
	Running, Synchronized bool
	Phase                 DueWorkerPhase
	Partitions            []DueWorkerPartitionStatus
	OwnedBytes            uint64
	Queued                int
	Renewals              uint64
	LastError             string
}

// DueWorker owns one serial resident consumer and its original session. It
// shares the etcd package's transaction and lifetime invariants; no diagnostic
// reference can reconstruct the private scheduling capabilities it holds.
type DueWorker struct {
	backend        *Backend
	self           *DueWorker
	session        *WorkerSession
	ctx            context.Context
	cancel         context.CancelFunc
	options        DueWorkerOptions
	ranges, claims []*WorkerBudgetShare
	relist         *WorkerBudgetShare
	scans          []*dueWorkerScan
	cache          *dueWorkerCache

	// Only the actor accesses scan/round-robin state. Status, windowAt and
	// completion publication use archiveLife.mu, the existing lifetime lock.
	partNext, claimNext int
	windowAt            time.Time
	status              DueWorkerStatus
	err                 error
	workDone, done      chan struct{}
}

func copyDueWorkerOptions(options DueWorkerOptions) (DueWorkerOptions, error) {
	if !validDomainSegment(options.WorkerID) || len(options.Partitions) == 0 || len(options.Partitions) > 256 || len(options.RangeSlots) == 0 || len(options.RangeSlots) > 20 || len(options.ClaimSlots) == 0 || len(options.ClaimSlots) > 100 || options.RelistSlot >= 4 {
		return options, ErrInvalidRecord
	}
	if options.StageTTL == 0 {
		options.StageTTL = 30 * time.Second
	}
	if options.StageTTL < time.Second || options.StageTTL > 24*time.Hour || options.StageTTL%time.Second != 0 {
		return options, ErrInvalidRecord
	}
	var partitions [256]bool
	for _, p := range options.Partitions {
		if partitions[p] {
			return options, ErrInvalidRecord
		}
		partitions[p] = true
	}
	for _, shares := range []struct {
		slots []uint16
		limit uint16
	}{{options.RangeSlots, 20}, {options.ClaimSlots, 100}} {
		var seen [100]bool
		for _, slot := range shares.slots {
			if slot >= shares.limit || seen[slot] {
				return options, ErrInvalidRecord
			}
			seen[slot] = true
		}
	}
	options.Partitions = append([]uint8(nil), options.Partitions...)
	options.RangeSlots = append([]uint16(nil), options.RangeSlots...)
	options.ClaimSlots = append([]uint16(nil), options.ClaimSlots...)
	return options, nil
}

// StartDueWorker initializes one original session and starts the actual due
// consumer. Failure joins initialization and once-only cleanup before returning.
// Configured session TTL must exceed two request timeouts plus one second.
func (b *Backend) StartDueWorker(ctx context.Context, options DueWorkerOptions) (*DueWorker, error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	options, err := copyDueWorkerOptions(options)
	if err != nil {
		return nil, err
	}
	if b.workerControl.options.SessionTTL <= 2*b.requestTimeout+time.Second {
		return nil, ErrInvalidConfiguration
	}
	s, err := b.OpenWorkerSession(ctx, options.WorkerID)
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(s.ctx)
	w := &DueWorker{backend: b, session: s, ctx: owned, cancel: cancel, options: options, workDone: make(chan struct{}), done: make(chan struct{}), status: DueWorkerStatus{Session: s.Reference(), Running: true, Phase: "starting"}}
	w.self = w

	// Registration precedes all further native startup work, so Close and
	// external original releases also join a partially initialized actor.
	b.archiveLife.mu.Lock()
	registered := b.validWorkerSession(s) && !b.archiveLife.closed && s.release == nil
	if registered {
		s.dueWorker = w
	}
	b.archiveLife.mu.Unlock()
	if !registered {
		cancel()
		return nil, errors.Join(context.Canceled, b.releaseWorkerSession(ctx, s))
	}
	if err = w.initialize(); err != nil {
		w.finish(err)
		return nil, w.err
	}
	w.publishStatus("listing", nil)
	go w.run()
	return w, nil
}

func (w *DueWorker) initialize() error {
	b := w.backend
	for _, kind := range []WorkerBudgetKind{WorkerBudgetRelist, WorkerBudgetRange, WorkerBudgetClaim} {
		slots := []uint16{w.options.RelistSlot}
		if kind == WorkerBudgetRange {
			slots = w.options.RangeSlots
		}
		if kind == WorkerBudgetClaim {
			slots = w.options.ClaimSlots
		}
		for _, slot := range slots {
			if err := w.renewIfNeeded(w.ctx); err != nil {
				return err
			}
			share, err := b.AcquireWorkerBudget(w.ctx, w.session, kind, slot)
			if err != nil {
				return err
			}
			switch kind {
			case WorkerBudgetRelist:
				w.relist = share
			case WorkerBudgetRange:
				w.ranges = append(w.ranges, share)
			case WorkerBudgetClaim:
				w.claims = append(w.claims, share)
			}
		}
	}
	cache, err := newDueWorkerCache(b, w.session)
	if err != nil {
		return err
	}
	w.cache = cache
	for i, partition := range w.options.Partitions {
		if err = w.renewIfNeeded(w.ctx); err != nil {
			return err
		}
		p, claimErr := b.ClaimWorkerPartition(w.ctx, w.session, partition)
		if claimErr != nil {
			return claimErr
		}
		scan, scanErr := newDueWorkerScan(b, p, w.ranges[i%len(w.ranges)], w.claims[0], w.options.StageTTL)
		if scanErr != nil {
			return scanErr
		}
		w.scans = append(w.scans, scan)
	}
	return nil
}

// Done closes after actual work, original cleanup and terminal publication.
func (w *DueWorker) Done() <-chan struct{} {
	if w == nil || w.self != w {
		return nil
	}
	return w.done
}

// Stop cancels and physically joins this original consumer and once cleanup.
// Repeated callers receive the same terminal result.
func (w *DueWorker) Stop() error {
	if w == nil || w.self != w || w.backend == nil {
		return ErrInvalidRecord
	}
	w.cancel()
	<-w.done
	return w.err
}

// Status returns an owned bounded copy under the original lifetime lock.
func (w *DueWorker) Status() DueWorkerStatus {
	if w == nil || w.self != w || w.backend == nil {
		return DueWorkerStatus{}
	}
	w.backend.archiveLife.mu.Lock()
	defer w.backend.archiveLife.mu.Unlock()
	status := w.status
	status.Partitions = append([]DueWorkerPartitionStatus(nil), status.Partitions...)
	return status
}

func dueWorkerError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 256 {
		text = text[:256]
	}
	return text
}

// pauseDiscovery changes current publication, not historical coverage. The
// serial actor may still revalidate a selected candidate, but another cached
// turn must obtain a fresh post-guard publication before scheduling again.
func (w *DueWorker) pauseDiscovery(phase DueWorkerPhase) {
	w.backend.archiveLife.mu.Lock()
	w.pauseDiscoveryLocked(phase)
	w.backend.archiveLife.mu.Unlock()
}

// Requires archiveLife.mu; also used by the original pre-Grant binding after
// its pending CAS is positively acknowledged, before native Grant dispatch.
func (w *DueWorker) pauseDiscoveryLocked(phase DueWorkerPhase) {
	if w.cache != nil {
		w.cache.published = false
	}
	w.status.Synchronized, w.status.Phase = false, phase
	for i := range w.status.Partitions {
		w.status.Partitions[i].Synchronized = false
		w.status.Partitions[i].Phase = phase
	}
}

// Only the serial actor calls this while scans are changing. Cache fields and
// the published copy are read together under archiveLife.mu; Status never races
// a scan mutation or returns a borrowed mutable cache/source-page slice.
func (w *DueWorker) publishStatus(phase DueWorkerPhase, workErr error) {
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	status := DueWorkerStatus{Session: w.session.reference, Running: true, Synchronized: len(w.scans) == len(w.options.Partitions), Phase: phase, Renewals: w.status.Renewals, LastError: w.status.LastError, Partitions: make([]DueWorkerPartitionStatus, 0, len(w.scans))}
	if workErr != nil {
		status.LastError = dueWorkerError(workErr)
	}
	if w.cache != nil {
		status.OwnedBytes, status.Queued = dueCacheReservedBytes+w.cache.bytes, w.cache.count
	}
	// Aggregate the fixed cache once, rather than scanning all entries for
	// each partition while the shared lifetime lock is held.
	var queued [256]int
	var oldest [256]time.Time
	if w.cache != nil {
		for i := range w.cache.entries {
			entry := &w.cache.entries[i]
			if !entry.live {
				continue
			}
			partition := entry.candidate.Partition
			queued[partition]++
			if oldest[partition].IsZero() || entry.candidate.ExpiresAt.Before(oldest[partition]) {
				oldest[partition] = entry.candidate.ExpiresAt
			}
		}
	}
	for _, scan := range w.scans {
		partition := scan.partition.partition
		row := DueWorkerPartitionStatus{Partition: partition, Phase: phase, Pending: scan.pending, Queued: queued[partition], OldestExpiry: oldest[partition]}
		if scan.checkpoint != nil {
			r := scan.checkpoint.record
			row.Revision, row.ReplayFrom, row.WindowEnd, row.Frontier = r.Revision, r.ReplayFrom, r.WindowEnd, r.Frontier
			if r.Pending != nil {
				row.Pending = r.Pending.Reference
			}
		}
		if w.cache != nil {
			v := &w.cache.views[row.Partition]
			row.ObservedRevision, row.CoveredRevision = v.observed, v.covered
			row.Synchronized = scan.listed && v.synchronized && w.cache.published && row.Pending == (StageReference{})
			row.Invalidation = v.invalidation
			if v.sub.Partition != nil {
				row.ReplayFrom = v.sub.ReplayFrom
			}
		}
		status.Synchronized = status.Synchronized && row.Synchronized
		status.Partitions = append(status.Partitions, row)
	}
	w.status = status
}
