package etcd

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// A fresh page starts at the exact B frontier. One CAS covers at most one
// legacy birth and any adjacent post-cutoff rows, whose new producers publish
// P atomically. This relies on quiesced old writers and immutable legacy I/T/
// link/receipt without GC; it is not an audit of arbitrary database writes.
// EOF permits completion only when this CAS covers every returned row.
func (b *Backend) cleanupBackfillAdvancePlan(p *WorkerPartitionClaim, page *cleanupLegacyPage, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (cleanupBackfillRecord, bool, error) {
	if page == nil || page.self != page || page.origin != b {
		return cleanupBackfillRecord{}, false, ErrInvalidRecord
	}
	start, end, err := b.cleanupLegacyBounds(p, page.state, nil)
	if err != nil {
		return cleanupBackfillRecord{}, false, err
	}
	prior := page.state.record
	if page.fence != page.state.fence || page.revision != prior.ReadViewRevision || page.start != start || page.end != end || len(page.rows) > cleanupLegacyPageEntries || page.eof != (page.next == nil) {
		return cleanupBackfillRecord{}, false, ErrInvalidRecord
	}
	if !page.eof && len(page.rows) != cleanupLegacyPageEntries {
		return cleanupBackfillRecord{}, false, ErrInvalidRecord
	}

	// Validate all row structure, including the suffix this invocation cannot
	// cover. A copied/foreign page or continuation never creates a new frontier.
	last := prior.Frontier
	legacy := -1
	covered := len(page.rows)
	for i, row := range page.rows {
		if !validPreparationUUID(row.taskID) || row.taskID <= last || row.birth <= 0 || row.birth > page.revision {
			return cleanupBackfillRecord{}, false, ErrInvalidRecord
		}
		last = row.taskID
		if row.birth <= prior.LegacyCutoffRevision {
			if legacy < 0 {
				legacy = i
			} else if covered == len(page.rows) {
				covered = i
			}
		}
	}
	if page.next != nil {
		nextStart, nextEnd, err := b.cleanupLegacyBounds(p, page.state, page.next)
		prefix := fmt.Sprintf("%sp/%02x/cleanup-intents/", b.namespace.Root(), p.partition)
		c := &page.next.cursor
		if err != nil || nextEnd != end || nextStart != prefix+last+"\x00" || c.self != c || c.origin != b || c.partition != p || c.share != page.ranges {
			return cleanupBackfillRecord{}, false, ErrInvalidRecord
		}
	}

	terminal := false
	if legacy < 0 {
		if birth != nil || release != nil {
			return cleanupBackfillRecord{}, false, ErrInvalidRecord
		}
	} else {
		if err := b.validateCleanupBirthEvidence(p, birth); err != nil {
			return cleanupBackfillRecord{}, false, err
		}
		row := page.rows[legacy]
		if birth.record.Reference.TaskID != row.taskID || birth.birth != row.birth || birth.revision < page.revision {
			return cleanupBackfillRecord{}, false, ErrInvalidRecord
		}
		if release != nil {
			if release.self != release || release.origin != b || release.partition != p || release.birth != birth {
				return cleanupBackfillRecord{}, false, ErrInvalidRecord
			}
			if err := b.validateCleanupBirthRelease(p, birth); err != nil {
				return cleanupBackfillRecord{}, false, err
			}
			if err := matchCleanupBirthReleaseProof(birth, release.proof); err != nil {
				return cleanupBackfillRecord{}, false, err
			}
			terminal = birth.execution == nil || birth.execution.Phase == "settled"
		}
	}

	next := prior
	if covered > 0 {
		next.Frontier = page.rows[covered-1].taskID
	}
	if page.eof && covered == len(page.rows) {
		next.Phase = "complete"
	}
	if next == prior || next.Validate() != nil {
		return cleanupBackfillRecord{}, false, ErrInvalidRecord
	}
	return next, terminal, nil
}

// Admission checks both the original discovery share and the Claim share before
// consuming credit. The same real session must remain live with the existing
// cleanup CAS margin (4 request timeouts + 1 second); no clock is extended.
func (b *Backend) validateCleanupBackfillAdvance(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, page *cleanupLegacyPage, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := b.cleanupBackfillAdvancePlan(p, page, birth, release); err != nil {
		return err
	}
	if b.requestTimeout <= 0 || b.requestTimeout > 3*time.Second {
		return ErrInvalidConfiguration
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err := b.cleanupBirthMembership(p, page.ranges, 4*b.requestTimeout+time.Second); err != nil {
		return err
	}
	if !b.validWorkerBudget(claims) || claims.kind != WorkerBudgetClaim || claims.session != p.session {
		return ErrInvalidRecord
	}
	if claims.release != nil || claims.released || claims.lost {
		return ErrGuardExpired
	}
	return ctx.Err()
}

// Advance and P publication/deletion share one native Claim CAS. Unknown ACKs
// invalidate the original session: only a fresh paid B read under a new owner
// may adopt durable coverage, never this invocation's provenance or pointers.
// finish retains ownership until the actual client return, including Close.
func (b *Backend) advanceCleanupBackfill(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, page *cleanupLegacyPage, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (_ *cleanupBackfillState, resultErr error) {
	if err := b.validateCleanupBackfillAdvance(ctx, p, claims, page, birth, release); err != nil {
		return nil, err
	}
	o, stop, err := b.beginExpiredTaskOwner(ctx, p, claims)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer o.finish()
	knownConflict := false
	defer func() {
		if resultErr != nil && o.call.started && !knownConflict {
			o.call.lose()
		}
	}()

	next, mutation, err := b.cleanupBackfillAdvanceMutation(o, page, birth, release)
	if err != nil {
		return nil, err
	}
	if err = b.preflightCleanupBackfillAdvance(mutation); err != nil {
		return nil, err
	}
	ops := make([]clientv3.Op, len(mutation.Writes))
	for i, write := range mutation.Writes {
		if write.Delete {
			ops[i] = clientv3.OpDelete(write.Key)
		} else {
			ops[i] = clientv3.OpPut(write.Key, string(write.Value))
		}
	}
	if _, err = b.cleanupOwnerComparisons(o); err != nil {
		return nil, err
	}
	rpc, cancel := o.call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return nil, err
	}
	o.call.started = true
	response, err := b.client.Txn(rpc).If(mutation.Comparisons...).Then(ops...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		if err = o.postcheck(); err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		knownConflict = true
		return nil, ErrConflict
	}
	if !cleanupBackfillAdvanceACK(response, mutation) {
		return nil, ErrOutcomeUnknown
	}
	if err = o.postcheck(); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}

	write := mutation.Writes[len(mutation.Writes)-1]
	state := &cleanupBackfillState{origin: b, partition: p, record: next, fence: taskFence{key: write.Key, value: string(write.Value), create: page.fence.create, mod: response.Header.Revision}}
	state.self = state
	return state, nil
}

// W20, exact B and immutable T/I/receipt are always fenced. Optional E/release/P
// each contribute their full native fence or exact absence. Link was checked at
// the permanent common birth; it is never used as an independent authority.
func (b *Backend) cleanupBackfillAdvanceMutation(o *workerDueOwner, page *cleanupLegacyPage, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (cleanupBackfillRecord, Mutation, error) {
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	if err = b.validateCleanupBackfillAdvance(o.call.ctx, o.partition, o.share, page, birth, release); err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	next, terminal, err := b.cleanupBackfillAdvancePlan(o.partition, page, birth, release)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	cmps = append(cmps, page.fence.comparisons()...)
	var writes []Write
	if birth != nil {
		for _, fence := range []taskFence{birth.task, birth.intent, birth.receipt} {
			cmps = append(cmps, fence.comparisons()...)
		}
		keys, _, err := b.cleanupBirthCohortKeys(birth.record.Reference, birth.initial)
		if err != nil {
			return cleanupBackfillRecord{}, Mutation{}, err
		}
		for i, fence := range []*taskFence{birth.executionFence, birth.release, birth.pending} {
			key := []string{keys[4], keys[6], keys[5]}[i]
			if fence == nil {
				cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
			} else {
				cmps = append(cmps, fence.comparisons()...)
			}
		}
		if terminal && birth.pending != nil {
			writes = append(writes, Write{Key: keys[5], Delete: true})
		} else if !terminal && birth.pending == nil {
			value, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: birth.record.Reference, TaskDigest: workerIdentityDigest(birth.task.value)})
			if err != nil {
				return cleanupBackfillRecord{}, Mutation{}, err
			}
			writes = append(writes, Write{Key: keys[5], Value: []byte(value)})
		}
	}
	value, err := encodeCleanupBackfill(next)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	writes = append(writes, Write{Key: page.fence.key, Value: []byte(value)})
	return next, Mutation{Comparisons: cmps, Writes: writes}, nil
}

// Validate every write ACK before publishing B's new native fence. A deletion
// compares a present immutable P, so anything other than exactly one deletion
// is uncertain. No previous values are requested or accepted on either write.
func cleanupBackfillAdvanceACK(response *clientv3.TxnResponse, mutation Mutation) bool {
	if response == nil || response.Header == nil || len(response.Responses) != len(mutation.Writes) {
		return false
	}
	for _, cmp := range mutation.Comparisons {
		var revision int64
		switch v := cmp.TargetUnion.(type) {
		case *pb.Compare_CreateRevision:
			revision = v.CreateRevision
		case *pb.Compare_ModRevision:
			revision = v.ModRevision
		}
		if response.Header.Revision <= revision {
			return false
		}
	}
	for i, write := range mutation.Writes {
		op := response.Responses[i]
		if op == nil {
			return false
		}
		if write.Delete {
			del := op.GetResponseDeleteRange()
			if del == nil || del.Deleted != 1 || len(del.PrevKvs) != 0 || !operationNestedHeader(del.Header, response.Header) {
				return false
			}
		} else {
			put := op.GetResponsePut()
			if put == nil || put.PrevKv != nil || !operationNestedHeader(put.Header, response.Header) {
				return false
			}
		}
	}
	return true
}
