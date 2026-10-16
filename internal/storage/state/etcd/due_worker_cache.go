package etcd

import (
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

const dueCacheKeys = 10000
const dueCacheDynamicBytes = 40 * 1024 * 1024
const dueCacheReservedBytes = 24 * 1024 * 1024 // fixed8 + transient16, including retained page

type dueCacheEntry struct {
	key, value string
	candidate  DueCandidateRecord
	charge     uint64
	heap       int
	live       bool
}

type dueCacheView struct {
	sub                                   dueWatchSubscription
	listed, created, bridge, synchronized bool
	covered, observed                     int64
	invalidation                          string
}

// All mutation uses the existing archiveLife.mu. Fixed arrays prevent map,
// queue and heap growth beyond the accounted original worker reservation.
type dueWorkerCache struct {
	origin           *Backend
	self             *dueWorkerCache
	session          *WorkerSession
	entries          [dueCacheKeys]dueCacheEntry
	index            [32768]int32
	heap             [dueCacheKeys]int
	free             [dueCacheKeys]int
	count, freeCount int
	bytes            uint64
	views            [256]dueCacheView
	pageSource       *dueWorkerScan
	published        bool
}

type dueWorkerCacheStatus struct {
	OwnedBytes uint64
	Keys       int
	Partitions []dueWatchPartitionResult
}

func newDueWorkerCache(b *Backend, s *WorkerSession) (*dueWorkerCache, error) {
	if !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerSession(s) {
		return nil, ErrInvalidRecord
	}
	if b.archiveLife.closed || s.release != nil {
		return nil, ErrGuardExpired
	}
	if err := s.live(); err != nil {
		return nil, err
	}
	if s.dueCache != nil {
		return nil, ErrConflict
	}
	c := &dueWorkerCache{origin: b, session: s, freeCount: dueCacheKeys}
	c.self = c
	for i := range c.free {
		c.free[i] = dueCacheKeys - 1 - i
	}
	s.dueCache = c
	return c, nil
}

func (c *dueWorkerCache) validLocked() bool {
	return c != nil && c.self == c && c.origin != nil && c.origin.validWorkerSession(c.session) && c.session.dueCache == c
}

func dueCacheHash(key string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h = (h ^ uint64(key[i])) * 1099511628211
	}
	return h
}

func (c *dueWorkerCache) find(key string) (int, int) {
	start, empty := int(dueCacheHash(key)&32767), -1
	for n := 0; n < len(c.index); n++ {
		cell := (start + n) & 32767
		value := c.index[cell]
		if value == 0 {
			if empty >= 0 {
				cell = empty
			}
			return -1, cell
		}
		if value == -1 {
			if empty < 0 {
				empty = cell
			}
			continue
		}
		if c.entries[value-1].key == key {
			return int(value - 1), cell
		}
	}
	return -1, empty
}

func (c *dueWorkerCache) less(a, b int) bool {
	left, right := &c.entries[c.heap[a]], &c.entries[c.heap[b]]
	if !left.candidate.ExpiresAt.Equal(right.candidate.ExpiresAt) {
		return left.candidate.ExpiresAt.Before(right.candidate.ExpiresAt)
	}
	return left.key < right.key
}
func (c *dueWorkerCache) swap(a, b int) {
	c.heap[a], c.heap[b] = c.heap[b], c.heap[a]
	c.entries[c.heap[a]].heap, c.entries[c.heap[b]].heap = a, b
}
func (c *dueWorkerCache) up(pos int) {
	for pos > 0 {
		parent := (pos - 1) / 2
		if !c.less(pos, parent) {
			break
		}
		c.swap(pos, parent)
		pos = parent
	}
}
func (c *dueWorkerCache) down(pos int) {
	for {
		child := pos*2 + 1
		if child >= c.count {
			return
		}
		if child+1 < c.count && c.less(child+1, child) {
			child++
		}
		if !c.less(child, pos) {
			return
		}
		c.swap(child, pos)
		pos = child
	}
}
func (c *dueWorkerCache) remove(key string) {
	slot, cell := c.find(key)
	if slot < 0 {
		return
	}
	pos := c.entries[slot].heap
	c.bytes -= c.entries[slot].charge
	c.count--
	if pos < c.count {
		replacement := c.heap[c.count]
		c.heap[pos] = replacement
		c.entries[replacement].heap = pos
		c.up(pos)
		c.down(c.entries[replacement].heap)
	}
	c.heap[c.count] = 0
	c.index[cell] = -1
	c.entries[slot] = dueCacheEntry{}
	c.free[c.freeCount] = slot
	c.freeCount++
}

func dueCacheCharge(key, value string) uint64 { return uint64(4*len(key) + 3*len(value) + 1024) }

func (c *dueWorkerCache) put(key, value string, candidate DueCandidateRecord) error {
	slot, cell := c.find(key)
	charge := dueCacheCharge(key, value)
	old := uint64(0)
	if slot >= 0 {
		old = c.entries[slot].charge
	}
	if cell < 0 || slot < 0 && c.count == dueCacheKeys || c.bytes-old+charge > dueCacheDynamicBytes {
		return ErrConflict
	}
	if slot < 0 {
		c.freeCount--
		slot = c.free[c.freeCount]
		c.index[cell] = int32(slot + 1)
		c.heap[c.count] = slot
		c.entries[slot].heap = c.count
		c.count++
	}
	pos := c.entries[slot].heap
	candidate.SandboxID = strings.Clone(candidate.SandboxID)
	candidate.WorkspaceHash = strings.Clone(candidate.WorkspaceHash)
	candidate.RestoreEpoch = strings.Clone(candidate.RestoreEpoch)
	candidate.IntentID = strings.Clone(candidate.IntentID)
	c.entries[slot] = dueCacheEntry{key: strings.Clone(key), value: strings.Clone(value), candidate: candidate, charge: charge, heap: pos, live: true}
	c.bytes = c.bytes - old + charge
	c.up(pos)
	return nil
}

func (c *dueWorkerCache) invalidateLocked(reason string) {
	if len(reason) > 256 {
		reason = reason[:256]
	}
	c.entries = [dueCacheKeys]dueCacheEntry{}
	c.index = [32768]int32{}
	c.heap = [dueCacheKeys]int{}
	c.count, c.bytes, c.freeCount = 0, 0, dueCacheKeys
	for i := range c.free {
		c.free[i] = dueCacheKeys - 1 - i
	}
	for i := range c.views {
		v := &c.views[i]
		if v.sub.Partition == nil {
			continue
		}
		v.listed, v.created, v.bridge, v.synchronized = false, false, false, false
		v.covered, v.sub.ReplayFrom = v.sub.Revision, v.sub.Revision+1
		v.invalidation = reason
	}
	c.pageSource = nil
	c.published = false
}

func (c *dueWorkerCache) invalidate(reason string) {
	if c == nil || c.origin == nil {
		return
	}
	c.origin.archiveLife.mu.Lock()
	defer c.origin.archiveLife.mu.Unlock()
	if c.validLocked() {
		c.invalidateLocked(reason)
	}
}

func (c *dueWorkerCache) addPage(scan *dueWorkerScan) error {
	if c == nil || c.origin == nil || scan == nil {
		return ErrInvalidRecord
	}
	b := c.origin
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !c.validLocked() || b.archiveLife.closed || c.session.release != nil || c.session.operation != nil || scan.backend != b || !b.validWorkerPartition(scan.partition) || !b.validWorkerBudget(scan.ranges) || scan.partition.session != c.session || scan.ranges.session != c.session || scan.ranges.kind != WorkerBudgetRange || !b.validDueCheckpoint(scan.checkpoint, scan.partition) || scan.checkpoint.record.Pending != nil || !scan.started || len(scan.page) > 118 {
		return ErrInvalidRecord
	}
	if err := c.session.live(); err != nil {
		return err
	}
	if c.pageSource != nil && c.pageSource != scan && len(c.pageSource.page) > 0 {
		c.invalidateLocked("retained page overflow")
		return ErrConflict
	}
	r := scan.checkpoint.record
	v := &c.views[scan.partition.partition]
	if v.sub.Partition != nil && (v.sub.Revision != r.Revision || v.sub.WindowEnd != r.WindowEnd) {
		for i := range c.entries {
			if c.entries[i].live && c.entries[i].candidate.Partition == r.Partition {
				c.remove(c.entries[i].key)
			}
		}
		*v = dueCacheView{}
	}
	if v.sub.Partition == nil {
		*v = dueCacheView{sub: dueWatchSubscription{Partition: scan.partition, Range: scan.ranges, Revision: r.Revision, WindowEnd: r.WindowEnd, ReplayFrom: r.ReplayFrom}, covered: r.Revision}
	}
	for _, candidate := range scan.page {
		key, err := b.namespace.dueCandidateKey(candidate)
		if err != nil || candidate.Partition != r.Partition || candidate.RestoreEpoch != b.restoreEpoch || candidate.ExpiresAt.After(r.WindowEnd) {
			c.invalidateLocked("invalid page")
			return ErrCorruptRecord
		}
		value, err := encodeDueCandidate(candidate)
		if err != nil {
			c.invalidateLocked("invalid page")
			return err
		}
		if err = c.put(key, value, candidate); err != nil {
			c.invalidateLocked("cache overflow")
			return err
		}
	}
	v.listed = v.listed || scan.cursor == nil
	v.invalidation = ""
	c.pageSource = scan
	c.published = false
	return nil
}

func (c *dueWorkerCache) peek() (DueCandidateRecord, bool) {
	if c == nil || c.origin == nil {
		return DueCandidateRecord{}, false
	}
	c.origin.archiveLife.mu.Lock()
	defer c.origin.archiveLife.mu.Unlock()
	if !c.validLocked() || !c.published || c.count == 0 || c.session.lost || c.session.released || c.session.ctx.Err() != nil || c.session.release != nil || c.origin.archiveLife.closed {
		return DueCandidateRecord{}, false
	}
	for i := range c.views {
		if c.views[i].sub.Partition != nil && !c.views[i].synchronized {
			return DueCandidateRecord{}, false
		}
	}
	return c.entries[c.heap[0]].candidate, true
}

func (c *dueWorkerCache) settled(candidate DueCandidateRecord) {
	if c == nil || c.origin == nil {
		return
	}
	c.origin.archiveLife.mu.Lock()
	defer c.origin.archiveLife.mu.Unlock()
	if !c.validLocked() {
		return
	}
	key, err := c.origin.namespace.dueCandidateKey(candidate)
	if err != nil {
		return
	}
	slot, _ := c.find(key)
	if slot >= 0 && c.entries[slot].candidate == candidate {
		c.remove(key)
	}
}

func (c *dueWorkerCache) statusLocked() dueWorkerCacheStatus {
	status := dueWorkerCacheStatus{OwnedBytes: dueCacheReservedBytes + c.bytes, Keys: c.count, Partitions: make([]dueWatchPartitionResult, 0, 256)}
	for i := range c.views {
		v := &c.views[i]
		if v.sub.Partition == nil {
			continue
		}
		status.Partitions = append(status.Partitions, dueWatchPartitionResult{Partition: uint8(i), Revision: v.sub.Revision, ReplayFrom: v.sub.ReplayFrom, ObservedRevision: v.observed, CoveredRevision: v.covered, Listed: v.listed, Created: v.created, Synchronized: v.synchronized, Invalidation: v.invalidation})
	}
	return status
}

func (c *dueWorkerCache) snapshot() dueWorkerCacheStatus {
	if c == nil || c.origin == nil {
		return dueWorkerCacheStatus{}
	}
	c.origin.archiveLife.mu.Lock()
	defer c.origin.archiveLife.mu.Unlock()
	if !c.validLocked() {
		return dueWorkerCacheStatus{}
	}
	return c.statusLocked()
}

// All changes are first validated by the receiver. Any capacity rejection
// invalidates the complete view, so no partially merged frame can be scheduled.
func (c *dueWorkerCache) applyLocked(events []*mvccpb.Event, partition uint8, revision int64) error {
	for _, event := range events {
		key := string(event.Kv.Key)
		if event.Type == mvccpb.DELETE {
			c.remove(key)
			continue
		}
		candidate, err := decodeDueCandidate(c.origin.namespace, event.Kv, revision)
		if err != nil || candidate.Partition != partition {
			return ErrCorruptRecord
		}
		if err = c.put(key, string(event.Kv.Value), candidate); err != nil {
			return err
		}
	}
	return nil
}
