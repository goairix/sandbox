package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
)

type dueWatchSubscription struct {
	Partition  *WorkerPartitionClaim
	Range      *WorkerBudgetShare
	Revision   int64
	WindowEnd  time.Time
	ReplayFrom int64
}

type dueWatchPartitionResult struct {
	Partition                                               uint8
	Revision, ReplayFrom, ObservedRevision, CoveredRevision int64
	Listed, Created, Synchronized                           bool
	Invalidation                                            string
}

type dueWatchBatchResult struct {
	Idle, Invalidated bool
	Frames, Events    int
	WireBytes         uint64
	Partitions        []dueWatchPartitionResult
}

func (o *dueWatchOwner) headerValid(h *pb.ResponseHeader) bool {
	return h != nil && h.ClusterId == o.call.session.origin.clusterID && h.MemberId > 0 && h.RaftTerm > 0 && h.Revision > 0 && h.Revision < math.MaxInt64 && len(h.XXX_unrecognized) == 0
}

func (o *dueWatchOwner) notifyGoalLocked() {
	if o.createdCount != len(o.subscriptions) {
		return
	}
	for _, sub := range o.subscriptions {
		v := &o.cache.views[sub.Partition.partition]
		if !v.bridge || v.covered < o.pre[sub.Partition.partition] {
			return
		}
	}
	select {
	case o.goal <- struct{}{}:
	default:
	}
}

func (o *dueWatchOwner) send() {
	defer close(o.sendDone)
	for _, sub := range o.subscriptions {
		prefix := fmt.Sprintf("%sp/%02x/due/", o.call.session.origin.namespace.Root(), sub.Partition.partition)
		request := &pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: &pb.WatchCreateRequest{Key: []byte(prefix), RangeEnd: []byte(prefix + sub.WindowEnd.Format(dueTimeLayout) + "0"), StartRevision: sub.ReplayFrom, WatchId: int64(sub.Partition.partition) + 1, ProgressNotify: true, Fragment: true}}}
		if err := o.stream.Send(request); err != nil {
			if o.streamCtx.Err() == nil {
				o.sendErr = err
			}
			return
		}
	}
	select {
	case <-o.created:
	case <-o.streamCtx.Done():
		return
	}
	// The pinned server drops early progress requests while watchers sync;
	// after all ACKs, retry finitely rather than assuming a later reply exists.
	for i := 0; i < 10; i++ {
		request := &pb.WatchRequest{RequestUnion: &pb.WatchRequest_ProgressRequest{ProgressRequest: &pb.WatchProgressRequest{}}}
		if err := o.stream.Send(request); err != nil {
			if o.streamCtx.Err() == nil {
				o.sendErr = err
			}
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-o.streamCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
	<-o.streamCtx.Done()
}

func (o *dueWatchOwner) validateEvents(sub dueWatchSubscription, events []*mvccpb.Event, header *pb.ResponseHeader) error {
	if len(events) == 0 || len(events) > 118 {
		return ErrCorruptRecord
	}
	next := o.cache.views[sub.Partition.partition].sub.ReplayFrom
	last := next
	for i, event := range events {
		if event == nil || event.Kv == nil || event.PrevKv != nil || len(event.XXX_unrecognized) != 0 {
			return ErrCorruptRecord
		}
		kv := event.Kv
		if kv.ModRevision < next || kv.ModRevision < last || kv.ModRevision > header.Revision || kv.ModRevision == math.MaxInt64 || len(kv.Key) > maxKeyBytes || len(kv.Value) > dueRecordBytes || len(kv.XXX_unrecognized) != 0 {
			return ErrCorruptRecord
		}
		last = kv.ModRevision
		for j := 0; j < i; j++ {
			if events[j].Kv.ModRevision == kv.ModRevision && bytes.Equal(events[j].Kv.Key, kv.Key) {
				return ErrCorruptRecord
			}
		}
		expiry, err := dueKeyExpiry(o.call.session.origin.namespace, sub.Partition.partition, string(kv.Key))
		if err != nil || expiry.After(sub.WindowEnd) {
			return ErrCorruptRecord
		}
		switch event.Type {
		case mvccpb.PUT:
			candidate, err := decodeDueCandidate(o.call.session.origin.namespace, kv, header.Revision)
			if err != nil || candidate.Partition != sub.Partition.partition || candidate.RestoreEpoch != o.call.session.origin.restoreEpoch {
				return ErrCorruptRecord
			}
		case mvccpb.DELETE:
			if kv.Lease != 0 || kv.CreateRevision != 0 || kv.Version != 0 || len(kv.Value) != 0 {
				return ErrCorruptRecord
			}
		default:
			return ErrCorruptRecord
		}
	}
	return nil
}

func (o *dueWatchOwner) receive() {
	defer close(o.recvDone)
	var assembly []*mvccpb.Event
	var assemblyHeader *pb.ResponseHeader
	var assemblyID int64
	assemblyBytes := 0
	b := o.call.session.origin
	fail := func(err error) { o.recvErr = err }
	for {
		r, err := o.stream.Recv()
		if err != nil {
			if assembly != nil {
				fail(ErrCorruptRecord)
			} else if o.streamCtx.Err() == nil {
				fail(err)
			}
			return
		}
		if !o.headerValid(r.Header) || r.Canceled || r.CompactRevision != 0 || r.CancelReason != "" {
			fail(ErrCorruptRecord)
			return
		}
		// A genuine all-watch progress certificate proves one complete revision.
		// Validate against immutable starts, since equal certificates may repeat
		// after ReplayFrom has already advanced to that revision plus one.
		if r.WatchId == -1 {
			if r.Created || r.Fragment || len(r.Events) != 0 || assembly != nil || o.createdCount != len(o.subscriptions) {
				fail(ErrCorruptRecord)
				return
			}
			b.archiveLife.mu.Lock()
			valid := true
			for _, sub := range o.subscriptions {
				v := &o.cache.views[sub.Partition.partition]
				if r.Header.Revision < o.starts[sub.Partition.partition] || r.Header.Revision < v.covered {
					valid = false
					break
				}
			}
			if valid {
				for _, sub := range o.subscriptions {
					v := &o.cache.views[sub.Partition.partition]
					v.covered = r.Header.Revision
					v.sub.ReplayFrom = r.Header.Revision + 1
					v.bridge = true
				}
				o.progress = true
				o.notifyGoalLocked()
			}
			b.archiveLife.mu.Unlock()
			if !valid {
				fail(ErrCorruptRecord)
				return
			}
			continue
		}
		if r.WatchId <= 0 || r.WatchId > 256 {
			fail(ErrCorruptRecord)
			return
		}
		partition := uint8(r.WatchId - 1)
		b.archiveLife.mu.Lock()
		v := &o.cache.views[partition]
		if v.sub.Partition == nil {
			b.archiveLife.mu.Unlock()
			fail(ErrCorruptRecord)
			return
		}
		if r.Created {
			if v.created || r.Fragment || len(r.Events) != 0 || assembly != nil || r.Header.Revision < v.sub.Revision {
				b.archiveLife.mu.Unlock()
				fail(ErrCorruptRecord)
				return
			}
			v.created = true
			o.createdCount++
			if o.createdCount == len(o.subscriptions) {
				close(o.created)
			}
			o.notifyGoalLocked()
			b.archiveLife.mu.Unlock()
			continue
		}
		if !v.created {
			b.archiveLife.mu.Unlock()
			fail(ErrCorruptRecord)
			return
		}
		if len(r.Events) == 0 {
			if r.Fragment || assembly != nil || r.Header.Revision < o.starts[partition] || r.Header.Revision < v.covered {
				b.archiveLife.mu.Unlock()
				fail(ErrCorruptRecord)
				return
			}
			v.covered = r.Header.Revision
			v.sub.ReplayFrom = r.Header.Revision + 1
			v.bridge = true
			o.progress = true
			o.notifyGoalLocked()
			b.archiveLife.mu.Unlock()
			continue
		}
		if assembly != nil {
			if r.WatchId != assemblyID || r.Header.ClusterId != assemblyHeader.ClusterId || r.Header.MemberId != assemblyHeader.MemberId || r.Header.Revision != assemblyHeader.Revision || r.Header.RaftTerm != assemblyHeader.RaftTerm {
				b.archiveLife.mu.Unlock()
				fail(ErrCorruptRecord)
				return
			}
			assemblyBytes += o.codec.lastBytes
			if len(assembly)+len(r.Events) > 118 || assemblyBytes > maxMutationBytes {
				b.archiveLife.mu.Unlock()
				fail(ErrCorruptRecord)
				return
			}
			assembly = append(assembly, r.Events...)
		} else if r.Fragment {
			assembly = make([]*mvccpb.Event, 0, 118)
			assembly = append(assembly, r.Events...)
			assemblyHeader, assemblyID, assemblyBytes = r.Header, r.WatchId, o.codec.lastBytes
		}
		if r.Fragment {
			b.archiveLife.mu.Unlock()
			continue
		}
		events := r.Events
		if assembly != nil {
			events = assembly
		}
		if err = o.validateEvents(v.sub, events, r.Header); err == nil {
			err = o.cache.applyLocked(events, partition, r.Header.Revision)
		}
		if err != nil {
			b.archiveLife.mu.Unlock()
			fail(err)
			return
		}
		// Historical response headers may be newer than these actual events.
		// Resume at the last whole event revision, never at that header's tail.
		v.covered = events[len(events)-1].Kv.ModRevision
		v.sub.ReplayFrom = v.covered + 1
		v.bridge = true
		o.notifyGoalLocked()
		assembly, assemblyHeader, assemblyBytes = nil, nil, 0
		b.archiveLife.mu.Unlock()
	}
}

func (b *Backend) runDueWatchBatch(ctx context.Context, cache *dueWorkerCache) (result dueWatchBatchResult, resultErr error) {
	o, err := b.beginDueWatch(ctx, cache)
	if err != nil {
		return result, err
	}
	defer o.finish()
	defer func() {
		b.archiveLife.mu.Lock()
		if resultErr != nil {
			cache.invalidateLocked("watch resync")
			result.Invalidated = true
		}
		result.Frames, result.Events, result.WireBytes = o.codec.frames, o.codec.events, o.codec.bytes
		result.Idle = o.codec.events == 0 && !o.progress
		result.Partitions = cache.statusLocked().Partitions
		b.archiveLife.mu.Unlock()
	}()
	// PRE is the catch-up goal. POST validates current authority after physical
	// stream joins; unrelated writes between them are only an observed tail.
	for start := 0; start < len(o.subscriptions); {
		end := dueWatchGroupEnd(o.subscriptions, start)
		revision, err := b.dueWatchGuard(o, o.subscriptions[start:end])
		if err != nil {
			return result, err
		}
		for _, sub := range o.subscriptions[start:end] {
			o.pre[sub.Partition.partition] = revision
		}
		start = end
	}
	deadline, _ := o.call.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return result, context.DeadlineExceeded
	}
	o.streamCtx, o.streamCancel = context.WithTimeout(o.call.ctx, min(500*time.Millisecond, remaining/2))
	o.stream, err = pb.NewWatchClient(b.client.ActiveConnection()).Watch(o.streamCtx, grpc.MaxCallRecvMsgSize(maxMutationBytes), grpc.MaxCallSendMsgSize(maxMutationBytes), grpc.ForceCodec(&o.codec))
	if err == nil {
		o.sendDone, o.recvDone = make(chan struct{}), make(chan struct{})
		go o.send()
		go o.receive()
		select {
		case <-o.goal:
		case <-o.recvDone:
		case <-o.sendDone:
		case <-o.streamCtx.Done():
		}
		o.streamCancel()
		<-o.sendDone
		<-o.recvDone
		_ = o.stream.CloseSend()
		err = errors.Join(o.sendErr, o.recvErr)
	}
	b.archiveLife.mu.Lock()
	stopped := o.locallyStoppedLocked()
	b.archiveLife.mu.Unlock()
	if stopped {
		return result, context.Canceled
	}
	for start := 0; start < len(o.subscriptions); {
		end := dueWatchGroupEnd(o.subscriptions, start)
		revision, guardErr := b.dueWatchGuard(o, o.subscriptions[start:end])
		if guardErr != nil {
			return result, errors.Join(err, guardErr)
		}
		for _, sub := range o.subscriptions[start:end] {
			o.post[sub.Partition.partition] = revision
		}
		start = end
	}
	if err != nil {
		return result, err
	}
	b.archiveLife.mu.Lock()
	if o.locallyStoppedLocked() {
		b.archiveLife.mu.Unlock()
		return result, context.Canceled
	}
	for _, sub := range o.subscriptions {
		v := &cache.views[sub.Partition.partition]
		v.observed = o.post[sub.Partition.partition]
		v.synchronized = v.listed && v.created && v.bridge && v.covered >= o.pre[sub.Partition.partition]
		v.invalidation = ""
	}
	cache.published = true
	b.archiveLife.mu.Unlock()
	return result, nil
}
