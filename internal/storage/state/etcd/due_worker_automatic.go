package etcd

import (
	"context"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// DueWorkerCleanupPartitionStatus reports finite polling/backfill progress.
// These observations confer no execution authority or pending Watch coverage.
type DueWorkerCleanupPartitionStatus struct {
	Partition                              uint8
	BackfillPhase                          string
	LegacyCutoffRevision, ReadViewRevision int64
	Frontier                               string
	PendingRevision                        int64
	NextSweep                              time.Time
}

// DueWorkerCleanupStatus is an owned bounded diagnostic snapshot. Polling has
// backlog-dependent latency; completion is not a promise to notice new P in 30s.
type DueWorkerCleanupStatus struct {
	Enabled    bool
	Phase      string
	Active     int
	Queued     int
	Rejected   uint64
	LastError  string
	Partitions []DueWorkerCleanupPartitionStatus
}

type dueWorkerAutomaticPartition struct {
	scan            *dueWorkerScan
	backfill        *cleanupBackfillState
	cursor          *cleanupPendingCursor
	pendingRevision int64
	nextSweep       time.Time
	backfillAt      time.Time
}

// One actor owns one materialized pending page and one active proof phase.
// Cursor/checkpoint rows are fixed by the configured partitions. Pending and
// legacy turns may overlap that page, but never create another proof batch.
type dueWorkerAutomatic struct {
	partitions                []dueWorkerAutomaticPartition
	pendingNext, backfillNext int
	active, pendingPart       int
	preferBackfill, backfill  bool
	phase                     string
	page                      cleanupPendingPage
	pageReady                 bool
	index                     int
	candidate                 cleanupPendingRecord
	legacy                    *cleanupLegacyPage
	observation               *cleanupBackfillObservation
	birth                     *cleanupBirthEvidence
	indexBirth                *cleanupBirthIndexEvidence
	release                   *cleanupBirthReleaseEvidence
	rejected                  uint64
	lastError                 string
}

// Physical admission is checked by StartDueWorker before its first RPC. This
// initializer only attaches bounded metadata state to the already owned scans.
func (w *DueWorker) initializeAutomatic() error {
	if !w.options.AutomaticCleanup {
		return nil
	}
	if w.automatic != nil || w.cache == nil || len(w.scans) == 0 || len(w.scans) > 256 {
		return ErrInvalidRecord
	}
	a := &dueWorkerAutomatic{active: -1, pendingPart: -1}
	for _, scan := range w.scans {
		a.partitions = append(a.partitions, dueWorkerAutomaticPartition{scan: scan})
	}
	w.backend.archiveLife.mu.Lock()
	keyLimit, byteLimit := 9631, uint64(36<<20)
	if w.backend.cleanupColdQuotaEnabled() {
		keyLimit, byteLimit = 9567, 32<<20
	}
	if !w.cache.validLocked() || w.cache.session != w.session || w.cache.count > keyLimit || w.cache.bytes > byteLimit {
		w.backend.archiveLife.mu.Unlock()
		return ErrInvalidRecord
	}
	w.cache.automatic = true
	w.cache.coldQuota = w.backend.cleanupColdQuotaEnabled()
	w.backend.archiveLife.mu.Unlock()
	w.automatic = a
	_, err := w.automaticRetainedBytes()
	return err
}

// A turn never waits for a physical job or retries a native call. Every phase
// returns its actual owner before the loop renews or offers another lane work.
func (w *DueWorker) automaticTurn(ctx context.Context) (time.Time, error) {
	if w == nil || w.self != w || ctx == nil || w.automatic == nil {
		return time.Time{}, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if _, err := w.automaticRetainedBytes(); err != nil {
		return time.Time{}, err
	}
	a, now := w.automatic, time.Now()
	if a.phase == "" {
		wake := now.Add(30 * time.Second)
		selected := false
		for _, backfill := range []bool{a.preferBackfill, !a.preferBackfill} {
			var at time.Time
			if backfill {
				selected, at = w.automaticSelectBackfill(now)
			} else {
				selected, at = w.automaticSelectPending(now)
			}
			if selected {
				break
			}
			if !at.IsZero() && at.Before(wake) {
				wake = at
			}
		}
		if !selected {
			return wake, nil
		}
	}
	var at time.Time
	var err error
	if a.backfill {
		at, err = w.automaticBackfillTurn(ctx)
	} else {
		at, err = w.automaticPendingTurn(ctx)
	}
	if err != nil {
		if !w.automaticHealthy(err) {
			return time.Time{}, err
		}
		a.lastError = dueWorkerError(err)
		part := &a.partitions[a.active]
		switch {
		case errors.Is(err, rpctypes.ErrCompacted) && a.backfill:
			a.legacy, a.birth, a.release = nil, nil, nil
			a.indexBirth = nil
			a.phase = "backfill-refresh-read"
		case errors.Is(err, rpctypes.ErrCompacted):
			part.cursor, part.pendingRevision = nil, 0
			part.nextSweep = now.Add(30 * time.Second)
			a.page, a.pageReady, a.index, a.pendingPart = cleanupPendingPage{}, false, 0, -1
			w.automaticFinishPhase()
		case a.backfill:
			part.backfillAt = now.Add(30 * time.Second)
			if errors.Is(err, ErrConflict) {
				part.backfill, part.backfillAt = nil, now.Add(100*time.Millisecond)
			}
			w.automaticFinishPhase()
		default:
			a.rejected++
			w.automaticAdvancePending()
		}
		at = now.Add(100 * time.Millisecond)
	}
	if _, err = w.automaticRetainedBytes(); err != nil {
		return time.Time{}, err
	}
	if at.IsZero() {
		at = now
	}
	return at, nil
}

// Typed validation is local only when the original reader's actual postguard
// preserved its session. Unknown transport/identity/lifetime errors stop both
// discovery domains; an outer corruption sentinel cannot conceal them.
func (w *DueWorker) automaticHealthy(err error) bool {
	var transport *enrolledPointTransportError
	if errors.As(err, &transport) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrGuardExpired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	return !b.archiveLife.closed && w.ctx.Err() == nil && b.validWorkerSession(w.session) && w.session.release == nil && w.session.live() == nil
}

func (w *DueWorker) automaticRangeAt(part *dueWorkerAutomaticPartition) time.Time {
	return w.readyRangeAt(part.scan.ranges, 1)
}

func (w *DueWorker) automaticFinishPhase() {
	a := w.automatic
	a.preferBackfill = !a.backfill
	a.phase, a.active = "", -1
	a.birth, a.release, a.legacy, a.observation = nil, nil, nil, nil
	a.indexBirth = nil
	a.candidate = cleanupPendingRecord{}
}

// Called under archiveLife.mu by publication, never by Status on mutable actor
// state. The caller subsequently copies this bounded slice for each observer.
func (w *DueWorker) automaticStatusLocked() DueWorkerCleanupStatus {
	s := DueWorkerCleanupStatus{Enabled: w.options.AutomaticCleanup}
	if a := w.automatic; a != nil {
		s.Phase, s.Rejected, s.LastError = a.phase, a.rejected, a.lastError
		s.Queued = max(len(a.page.Candidates)-a.index, 0)
		for _, part := range a.partitions {
			row := DueWorkerCleanupPartitionStatus{Partition: part.scan.partition.partition, PendingRevision: part.pendingRevision, NextSweep: part.nextSweep}
			if part.backfill != nil {
				r := part.backfill.record
				row.BackfillPhase, row.LegacyCutoffRevision, row.ReadViewRevision, row.Frontier = r.Phase, r.LegacyCutoffRevision, r.ReadViewRevision, r.Frontier
			}
			s.Partitions = append(s.Partitions, row)
		}
	}
	for _, job := range w.cleanupJobs {
		if job != nil {
			s.Active++
		}
	}
	return s
}

// Retained bodies are charged separately from bounded in-flight protobufs in
// the existing 16MiB transient reservation. Per-partition 8KiB, hint 4KiB,
// registry reference 4KiB, one 1MiB evidence phase and 64KiB fixed state total
// 3670016 bytes, below the 4MiB reservation. This measures ownership, not RSS.
func (w *DueWorker) automaticRetainedBytes() (uint64, error) {
	a := w.automatic
	if a == nil || len(a.partitions) > 256 || len(a.page.Candidates) > 80 || a.index < 0 || a.index > len(a.page.Candidates) {
		return 0, ErrInvalidRecord
	}
	total := uint64(64 << 10)
	for _, part := range a.partitions {
		charge := uint64(1024)
		if part.backfill != nil {
			r, f := part.backfill.record, part.backfill.fence
			charge += uint64(len(f.key) + len(f.value) + len(r.Namespace) + len(r.RestoreEpoch) + len(r.IdentityDigest) + len(r.Frontier))
		}
		if part.cursor != nil {
			charge += uint64(len(part.cursor.cursor.start) + len(part.cursor.cursor.end))
		}
		if charge > 8192 {
			return 0, ErrInvalidRecord
		}
		total += charge
	}
	// Consumed entries are cleared immediately so their routing strings are not
	// retained during the rest of the sweep. Only the unconsumed tail owns data.
	for _, hint := range a.page.Candidates[a.index:] {
		if hint.Validate() != nil {
			return 0, ErrInvalidRecord
		}
		ref := hint.Reference
		charge := uint64(512 + 3*(len(ref.Namespace)+len(ref.RestoreEpoch)+len(ref.TaskID)+len(ref.SandboxID)+len(hint.TaskDigest)))
		if charge > 4096 {
			return 0, ErrInvalidRecord
		}
		total += charge
	}
	proofBytes, err := automaticEvidenceBytes(a)
	if err != nil || proofBytes > 1<<20 {
		return 0, ErrInvalidRecord
	}
	total += proofBytes
	w.backend.archiveLife.mu.Lock()
	for _, job := range w.cleanupJobs {
		if job != nil {
			total += 4096 // only the added retained Task identity; original job owns its physical graph
		}
	}
	w.backend.archiveLife.mu.Unlock()
	if total > 4<<20 {
		return 0, ErrInvalidRecord
	}
	return total, nil
}

// Returned release evidence retains the release KV/record and termination,
// not the provider KV or nine historical responses. Charge four copies of
// their bounded canonical bytes plus object overhead before another phase.
func automaticEvidenceBytes(a *dueWorkerAutomatic) (uint64, error) {
	charge := uint64(64 << 10) // active B observation, two legacy rows and candidate routing
	if index := a.indexBirth; index != nil {
		// Index-only evidence replaces the normal birth/release phase. Five
		// fences at 1024-key/4096-value bytes, four copies plus 4096 overhead
		// each, and the routing reserve total at most 188416 bytes.
		if a.birth != nil || a.release != nil {
			return 0, ErrInvalidRecord
		}
		for _, f := range []*taskFence{&index.task, &index.intent, &index.link, &index.receipt, index.pending} {
			if f != nil {
				if len(f.key) > maxKeyBytes || len(f.value) > 4096 {
					return 0, ErrInvalidRecord
				}
				charge += uint64(4096 + 4*(len(f.key)+len(f.value)))
			}
		}
	}
	if birth := a.birth; birth != nil {
		for _, f := range []*taskFence{&birth.task, &birth.intent, &birth.link, &birth.receipt, birth.executionFence, birth.pending, birth.release} {
			if f != nil {
				if len(f.key) > maxKeyBytes || len(f.value) > 8192 {
					return 0, ErrInvalidRecord
				}
				charge += uint64(4096 + 4*(len(f.key)+len(f.value)))
			}
		}
	}
	if release := a.release; release != nil {
		if release.proof == nil || release.proof.kv == nil {
			return 0, ErrInvalidRecord
		}
		proof := release.proof
		record, err := encodeLocalOwnerRelease(proof.record)
		if err != nil {
			return 0, err
		}
		termination, err := p.EncodeTaskRuntimeTerminationIntent(proof.termination)
		if err != nil || len(termination) > 16384 || len(proof.kv.Key) > maxKeyBytes || len(proof.kv.Value) > 8192 {
			return 0, ErrInvalidRecord
		}
		charge += uint64(4096 + 4*(len(record)+len(termination)+len(proof.kv.Key)+len(proof.kv.Value)))
	}
	return charge, nil
}
