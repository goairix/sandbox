package etcd

import (
	"bytes"
	"container/list"
	"context"
	"strings"
)

type snapshotCacheKey struct {
	partition uint8
	sandboxID string
	version   string
	digest    string
}

type snapshotCacheEntry struct {
	key    snapshotCacheKey
	record *SandboxSnapshotRecord
	weight uint64
}

// All state is protected by Backend.archiveLife.mu, including admission and
// the final publication check. Pointer ownership rejects diagnostic copies of
// Backend without copying a mutex. Maps and retained payloads allocate on use.
type snapshotReadCache struct {
	origin  *Backend
	budget  uint64
	used    uint64
	lru     list.List
	entries map[snapshotCacheKey]*list.Element
	calls   map[*snapshotReadCall]struct{}
	misses  map[snapshotCacheKey]*snapshotReadMiss
}

type snapshotReadCall struct {
	backend *Backend
	context context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	ended   bool
}

type snapshotReadMiss struct {
	context context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	record  *SandboxSnapshotRecord
	err     error
}

func newSnapshotReadCache(b *Backend, budget uint64) *snapshotReadCache {
	if budget == 0 {
		budget = 64 * 1024 * 1024
	}
	return &snapshotReadCache{origin: b, budget: budget}
}

func cloneSnapshotRecord(r *SandboxSnapshotRecord) *SandboxSnapshotRecord {
	if r == nil {
		return nil
	}
	return &SandboxSnapshotRecord{Version: r.Version, SandboxID: strings.Clone(r.SandboxID), Snapshot: SnapshotReference{Version: strings.Clone(r.Snapshot.Version), Digest: strings.Clone(r.Snapshot.Digest)}, Payload: bytes.Clone(r.Payload)}
}

func (c *snapshotReadCache) lookup(key snapshotCacheKey) *SandboxSnapshotRecord {
	e := c.entries[key]
	if e == nil {
		return nil
	}
	c.lru.MoveToFront(e)
	return cloneSnapshotRecord(e.Value.(*snapshotCacheEntry).record)
}

func (c *snapshotReadCache) store(key snapshotCacheKey, record *SandboxSnapshotRecord) {
	if record == nil {
		return
	}
	// Includes retained string/payload bytes and conservative entry, map bucket,
	// list element, allocation and pointer overhead, even for empty payloads.
	weight := uint64(512 + 2*(len(key.sandboxID)+len(key.version)+len(key.digest)) + len(record.SandboxID) + len(record.Snapshot.Version) + len(record.Snapshot.Digest) + len(record.Payload))
	if weight > c.budget {
		return
	}
	if e := c.entries[key]; e != nil {
		c.used -= e.Value.(*snapshotCacheEntry).weight
		delete(c.entries, key)
		c.lru.Remove(e)
	}
	for c.used+weight > c.budget {
		e := c.lru.Back()
		entry := e.Value.(*snapshotCacheEntry)
		delete(c.entries, entry.key)
		c.used -= entry.weight
		c.lru.Remove(e)
	}
	key.sandboxID, key.version, key.digest = strings.Clone(key.sandboxID), strings.Clone(key.version), strings.Clone(key.digest)
	if c.entries == nil {
		c.entries = make(map[snapshotCacheKey]*list.Element)
	}
	c.entries[key] = c.lru.PushFront(&snapshotCacheEntry{key: key, record: cloneSnapshotRecord(record), weight: weight})
	c.used += weight
}

func (b *Backend) registerSnapshotRead(ctx context.Context) (*snapshotReadCall, error) {
	if ctx == nil || b == nil || b.archiveLife == nil || b.snapshotReads == nil || b.snapshotReads.origin != b {
		return nil, ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(b.snapshotReads.calls) >= 256 {
		return nil, ErrSnapshotReadCapacity
	}
	owned, cancel := context.WithCancel(ctx)
	call := &snapshotReadCall{backend: b, context: owned, cancel: cancel, done: make(chan struct{})}
	if b.snapshotReads.calls == nil {
		b.snapshotReads.calls = make(map[*snapshotReadCall]struct{})
	}
	b.snapshotReads.calls[call] = struct{}{}
	return call, nil
}

func (c *snapshotReadCall) finish() {
	b := c.backend
	b.archiveLife.mu.Lock()
	if !c.ended {
		c.ended = true
		c.cancel()
		delete(b.snapshotReads.calls, c)
		close(c.done)
	}
	b.archiveLife.mu.Unlock()
}

func (b *Backend) snapshotRecord(call *snapshotReadCall, key snapshotCacheKey, placement SandboxPlacementRecord) (*SandboxSnapshotRecord, error) {
	record, miss, err := b.startSnapshotRead(call, key, placement)
	if err != nil || record != nil {
		return record, err
	}
	select {
	case <-call.context.Done():
		return nil, call.context.Err()
	case <-miss.done:
		if err := call.context.Err(); err != nil {
			return nil, err
		}
		return cloneSnapshotRecord(miss.record), miss.err
	}
}

func (b *Backend) startSnapshotRead(call *snapshotReadCall, key snapshotCacheKey, placement SandboxPlacementRecord) (*SandboxSnapshotRecord, *snapshotReadMiss, error) {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed {
		return nil, nil, context.Canceled
	}
	if err := call.context.Err(); err != nil {
		return nil, nil, err
	}
	if record := b.snapshotReads.lookup(key); record != nil {
		return record, nil, nil
	}
	miss := b.snapshotReads.misses[key]
	if miss == nil {
		if len(b.snapshotReads.misses) >= 16 {
			return nil, nil, ErrSnapshotReadCapacity
		}
		ctx, cancel := context.WithTimeout(context.Background(), b.requestTimeout)
		miss = &snapshotReadMiss{context: ctx, cancel: cancel, done: make(chan struct{})}
		if b.snapshotReads.misses == nil {
			b.snapshotReads.misses = make(map[snapshotCacheKey]*snapshotReadMiss)
		}
		b.snapshotReads.misses[key] = miss
		go b.fillSnapshot(key, placement, miss)
	}
	return nil, miss, nil
}

func (b *Backend) fillSnapshot(key snapshotCacheKey, placement SandboxPlacementRecord, miss *snapshotReadMiss) {
	record, err := b.loadSnapshotRead(miss.context, key, placement)
	b.archiveLife.mu.Lock()
	if b.archiveLife.closed {
		record = nil
		err = context.Canceled
	} else if contextErr := miss.context.Err(); contextErr != nil {
		record = nil
		err = contextErr
	}
	if err == nil && record != nil {
		b.snapshotReads.store(key, record)
	}
	miss.record, miss.err = record, err
	delete(b.snapshotReads.misses, key)
	miss.cancel()
	close(miss.done)
	b.archiveLife.mu.Unlock()
}
