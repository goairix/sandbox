package etcd

import (
	"errors"
	"time"
)

// cleanupColdQuotaTurn branches before the default Claim owner. Metadata pays
// Range only; journal/Resolve/Stage pay both original shares, while final P
// deletion keeps its existing Claim-only CAS. Every call returns before yield.
func (w *DueWorker) cleanupColdQuotaTurn(j *cleanupLocalJob) (time.Time, error) {
	if j.step == "cold-quota-complete" {
		if err := w.validateCleanupColdQuotaVerified(j); err != nil {
			w.cleanupTurnFailed(j, err)
		} else {
			j.step = "done"
			w.cleanupTurnCompleted(j)
		}
		return time.Now(), nil
	}
	if w.coldQuota != nil && w.coldQuota.job != j {
		return time.Now().Add(100 * time.Millisecond), nil
	}
	if err := w.beginCleanupColdQuota(j); err != nil {
		if errors.Is(err, ErrConflict) {
			return time.Now().Add(100 * time.Millisecond), nil
		}
		w.cleanupTurnFailed(j, err)
		return time.Time{}, nil
	}
	s := w.coldQuota
	if _, _, err := w.cleanupColdQuotaOwnedBytes(); err != nil {
		w.cleanupTurnFailed(j, err)
		return time.Time{}, nil
	}
	claimOnly := j.step == "cold-quota-pending-delete"
	mutation := j.step == "cold-quota-journal" || j.step == "cold-quota-cohort" || j.step == "cold-quota-recovery" && s.recovery != nil && s.recovery.phase == coldQuotaRecoveryWantResolve
	if !claimOnly {
		if at := w.readyRangeAt(s.ranges, 2); time.Now().Before(at) {
			return at, nil
		}
	}
	if mutation || claimOnly {
		if share, at := w.readyClaimFor(time.Now(), s.claims); share == nil {
			return at, nil
		}
	}
	err := w.stepCleanupColdQuota(j, s)
	if err == nil {
		_, _, err = w.cleanupColdQuotaOwnedBytes()
	}
	if err != nil {
		// Readiness refused before dispatch above. Any later error is an actual
		// typed/native result, never permission to retry the same RPC in a loop.
		// Journal and Stage return uncertainty to their owner; unlike metadata,
		// they do not themselves invalidate this resident worker's session.
		// Their actual calls and once-only cleanup have already joined here.
		if errors.Is(err, ErrOutcomeUnknown) {
			w.backend.archiveLife.mu.Lock()
			w.session.lost = true
			w.session.cancel()
			w.backend.archiveLife.mu.Unlock()
		}
		w.cleanupTurnFailed(j, err)
	} else {
		w.cleanupTurnCompleted(j)
	}
	return time.Now(), nil
}

func (w *DueWorker) stepCleanupColdQuota(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace) error {
	b := w.backend
	switch j.step {
	case "cold-origin", "cold-quota-history":
		if s.cursor == nil {
			if j.coldQuota.workspace == (WorkspaceIdentity{}) && j.target == nil {
				j.step = "cold-quota-task"
				return w.readCleanupColdQuotaTask(j, s)
			}
			workspace := j.coldQuota.workspace
			if j.target != nil {
				workspace = j.target.workspaceIdentity()
			}
			task, _, err := w.cleanupColdQuotaTask(j)
			if err != nil {
				return err
			}
			cursor, err := b.newColdQuotaWorkerHistory(s.owner, workspace, task.CreationIntentID, j.task, w.session, j.partition, s.ranges)
			if err != nil {
				return err
			}
			s.cursor, s.history = cursor, cursor.history
		}
		history, err := b.stepColdQuotaWorkerHistory(s.owner, s.cursor, w.session, j.partition, s.ranges)
		if err != nil {
			return err
		}
		if j.step == "cold-origin" && s.cursor.phase == coldQuotaWorkerHistoryWantRelease {
			if err = w.captureCleanupColdQuotaOrigin(j, s.history); err != nil {
				return err
			}
			w.releaseCleanupColdQuota(j)
			j.step = "issuer"
			return nil
		}
		if history == nil {
			return nil
		}
		if err = w.captureCleanupColdQuotaOrigin(j, history); err != nil {
			return err
		}
		s.recovery, err = b.newColdQuotaWorkerRecovery(s.owner, history, w.session, j.partition, s.ranges, s.claims)
		if err == nil {
			j.step = "cold-quota-recovery"
		}
		return err
	case "cold-quota-task":
		return w.readCleanupColdQuotaTask(j, s)
	case "cold-quota-snapshot":
		return w.readCleanupColdQuotaSnapshot(j, s)
	case "cold-quota-recovery":
		if err := w.validateCleanupColdQuotaOrigin(j); err != nil {
			return err
		}
		result, err := b.stepColdQuotaWorkerRecovery(s.owner, s.history, s.recovery, w.session, j.partition, s.ranges, s.claims)
		if err != nil || result == nil {
			return err
		}
		s.recovered = result
		if result.entry != nil {
			return w.publishCleanupColdQuota(j, s, result.entry)
		}
		s.prepare, err = b.newColdQuotaWorkerPrepare(s.owner, s.history, w.session, j.partition, s.ranges)
		if err == nil {
			j.step = "cold-quota-prepare"
		}
		return err
	case "cold-quota-prepare":
		if err := w.validateCleanupColdQuotaOrigin(j); err != nil {
			return err
		}
		prepared, err := b.stepColdQuotaWorkerPrepare(s.owner, s.history, s.prepare, w.session, j.partition, s.ranges)
		if err == nil && prepared != nil {
			s.prepared, j.step = prepared, "cold-quota-journal"
		}
		return err
	case "cold-quota-journal":
		if err := w.validateCleanupColdQuotaOrigin(j); err != nil {
			return err
		}
		permit, err := b.journalColdRuntimeQuotaReturnWorker(s.owner, s.prepared, w.session, j.partition, s.ranges, s.claims, s.recovered.previous, s.recovered.aborted)
		if err == nil {
			s.journal, j.step = permit, "cold-quota-cohort"
		}
		return err
	case "cold-quota-cohort":
		if err := w.validateCleanupColdQuotaOrigin(j); err != nil {
			return err
		}
		permit, err := b.continueColdRuntimeQuotaReturnWorker(s.owner, s.journal, w.session, j.partition, s.ranges, s.claims)
		if err != nil {
			return err
		}
		s.permit = permit
		s.postread, err = b.newColdQuotaWorkerPostread(s.owner, permit, w.session, j.partition, s.ranges)
		if err == nil {
			j.step = "cold-quota-postread"
		}
		return err
	case "cold-quota-postread":
		entry, err := b.stepColdQuotaWorkerPostread(s.owner, s.permit, s.postread, w.session, j.partition, s.ranges)
		if err != nil || entry == nil {
			return err
		}
		return w.publishCleanupColdQuota(j, s, entry)
	case "cold-quota-intent":
		return w.readCleanupColdQuotaDetachedIntent(j, s)
	case "cold-quota-pending-birth":
		birth, err := b.readCleanupBirth(j.ctx, j.partition, s.ranges, j.task.TaskID)
		if err == nil {
			if birth.record != j.coldQuota.record || birth.birth != j.coldQuota.birth || birth.pending == nil {
				return ErrConflict
			}
			if birth.detachedReleaseRecord != nil && birth.execution != nil && birth.execution.Phase != "settled" {
				return ErrInvalidRecord
			}
			s.birth, j.step = birth, "cold-quota-pending-release"
		}
		return err
	case "cold-quota-pending-release":
		if s.birth != nil && s.birth.detachedReleaseRecord != nil {
			if err := b.validateCleanupBirthRelease(j.partition, s.birth); err != nil {
				return err
			}
			// All earlier protocol calls and identified cleanup already joined.
			// Keep only the compact verified attribution plus fresh birth while
			// replacing this graph: two thirteen-point histories cannot coexist.
			s.history, s.cursor = nil, nil
			s.recovery, s.recovered = nil, nil
			s.prepare, s.prepared = nil, nil
			s.journal, s.permit, s.postread = nil, nil, nil
			s.release = nil
			j.step = "cold-quota-pending-profile"
			return nil
		}
		release, err := b.readCleanupBirthRelease(j.ctx, j.partition, s.ranges, s.birth)
		if err == nil {
			s.release, j.step = release, "cold-quota-pending-delete"
		}
		return err
	case "cold-quota-pending-profile":
		return w.readCleanupColdQuotaPendingProfile(j, s)
	case "cold-quota-pending-delete":
		if err := b.deleteCleanupPending(j.ctx, j.partition, s.claims, s.birth, s.release, j); err != nil {
			return err
		}
		if s.release.profile != nil {
			pin := j.coldQuota.verified.release
			j.result.Release = &TaskOwnerReleaseEntry{Task: j.task, CreateRevision: pin.CreateRevision, SHA256: pin.Digest}
		}
		w.releaseCleanupColdQuota(j)
		j.result.Outcome, j.result.Reason, j.step = OutcomeCommitted, "complete", "done"
		return nil
	default:
		return ErrInvalidRecord
	}
}

// Quota-only and cold joined-Run paths read immutable Task/snapshot metadata;
// they cannot load the deleted mutable owner/control/runtime-index bundle.
func (w *DueWorker) readCleanupColdQuotaTask(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace) error {
	key, err := w.backend.namespace.taskKey(j.task)
	if err != nil {
		return err
	}
	points, err := w.backend.readColdQuotaWorkerPoints(s.owner, w.session, j.partition, s.ranges, []string{key}, []int{4096})
	if err != nil {
		return err
	}
	var record TaskRecord
	if decodeTaskRecord(points[0], &record) != nil || record.Reference != j.task {
		return ErrCorruptRecord
	}
	c := &j.coldQuota
	if c.self != nil && (c.self != j || c.record != record || c.birth != points[0].CreateRevision) || j.state != nil && j.state.record.TaskBirth != points[0].CreateRevision {
		return ErrInvalidRecord
	}
	c.self, c.record, c.birth = j, record, points[0].CreateRevision
	j.step = "cold-quota-snapshot"
	return nil
}

func (w *DueWorker) readCleanupColdQuotaSnapshot(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace) error {
	b, task := w.backend, j.coldQuota.record
	_, snapshotKey, err := b.namespace.sandboxKeys(j.task.Partition, j.task.SandboxID, task.Snapshot.Version)
	if err != nil {
		return err
	}
	placementKey, err := b.namespace.placementKey(j.task.SandboxID)
	if err != nil {
		return err
	}
	points, err := b.readColdQuotaWorkerPoints(s.owner, w.session, j.partition, s.ranges, []string{snapshotKey, placementKey}, []int{maxRecordBytes, 4096})
	if err != nil {
		return err
	}
	placement, err := b.decodePlacement(points[1], j.task.SandboxID)
	if err != nil {
		return err
	}
	snapshot, err := decodeSnapshot(points[0], j.task.Partition, j.task.SandboxID, task.Snapshot, placement)
	if err != nil || snapshot == nil {
		return ErrCorruptRecord
	}
	if snapshot.Version == 1 && snapshot.LocalCleanup == nil {
		if b.detachedCleanup == nil {
			return ErrInvalidConfiguration
		}
		// This classifies metadata only. The next separately paid intent read
		// derives ordinary identity; the full signed cursor still authenticates
		// Task/snapshot/intent/publication before any refund or settlement.
		j.step = "cold-quota-intent"
		return nil
	}
	if snapshot.LocalCleanup == nil {
		return ErrCorruptRecord
	}
	workspace, err := snapshot.LocalCleanup.workspace()
	if err != nil || b.validateWorkspaceBinding(workspace) != nil || workspace.Hash() != task.WorkspaceHash {
		return ErrCorruptRecord
	}
	j.coldQuota.workspace, j.step = workspace, "cold-quota-history"
	return nil
}

// The original immutable Task selects this actual mutable permanent intent.
// Its RequestHash derives an ordinary detached identity, never an effect token.
// Paid full history must still authenticate all of these source bindings.
func (w *DueWorker) readCleanupColdQuotaDetachedIntent(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace) error {
	b, task := w.backend, j.coldQuota.record
	key, err := b.namespace.intentKey(j.task.Partition, task.CreationIntentID)
	if err != nil {
		return err
	}
	points, err := b.readColdQuotaWorkerPoints(s.owner, w.session, j.partition, s.ranges, []string{key}, []int{4096})
	if err != nil {
		return err
	}
	kv := points[0]
	var record CreationIntentRecord
	if !operationPermanentKV(kv) || kv.Version <= 0 || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || decodeDomainRecord(kv, &record) != nil {
		return ErrCorruptRecord
	}
	wire, err := encodeDomainRecord(&record)
	if err != nil || wire != string(kv.Value) {
		return ErrCorruptRecord
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return err
	}
	workspace, err := detachedIdentityFromRequestHash(identity.StorageID, record.RequestHash)
	if err != nil {
		return err
	}
	intent, err := b.decodeCreationIntent(kv, workspace, task.CreationIntentID)
	if err != nil || intent == nil || intent.Phase != "published" || intent.SandboxID != j.task.SandboxID || intent.Generation != task.Generation || workspace.Partition() != j.task.Partition || workspace.Hash() != task.WorkspaceHash {
		return ErrCorruptRecord
	}
	j.coldQuota.workspace, j.step = workspace, "cold-quota-history"
	return nil
}

// A verified detached refund rejoins the original actor's exact E/S read.
// This route cannot start a Run, claim its physical work, or load the deleted
// owner/control/index. Unjoined/changed E remains a refusal with P intact.
func (w *DueWorker) routeCleanupDetachedQuotaOccupation(j *cleanupLocalJob) (bool, error) {
	s := w.coldQuota
	if !j.coldQuota.quotaOnly || s == nil || s.job != j || s.history == nil || s.history.profileRelease == nil {
		return false, nil
	}
	if _, err := w.backend.detachedCleanupOccupationProof(j); err != nil {
		return true, err
	}
	if j.state == nil {
		j.step = "cold-quota-pending-birth"
		return true, nil
	}
	r := j.state.record
	if r.Phase == "settled" {
		if r.Release == nil || *r.Release != j.coldQuota.verified.release {
			return true, ErrCorruptRecord
		}
		j.step = "cold-quota-pending-birth"
		return true, nil
	}
	if r.Phase != "occupied" || r.Run == nil || !r.Run.Joined {
		return true, ErrInvalidRecord
	}
	j.step = "terminal-history"
	return true, nil
}

// Reuse the original seven paid groups with the same original owner/members.
// No standalone loader RPC or old mutable slot read is hidden in this phase.
// The fresh union is joined to both birth and the same job's verified refund
// before it can supply the existing pending-delete CAS.
func (w *DueWorker) readCleanupColdQuotaPendingProfile(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace) error {
	b := w.backend
	if s.birth == nil || s.birth.detachedReleaseRecord == nil || w.validateCleanupColdQuotaVerified(j) != nil {
		return ErrInvalidRecord
	}
	if s.cursor == nil {
		cursor, err := b.newColdQuotaWorkerHistory(s.owner, j.coldQuota.workspace, j.coldQuota.record.CreationIntentID, j.task, w.session, j.partition, s.ranges)
		if err != nil {
			return err
		}
		s.cursor, s.history = cursor, cursor.history
	}
	history, err := b.stepColdQuotaWorkerHistory(s.owner, s.cursor, w.session, j.partition, s.ranges)
	if err != nil || history == nil {
		return err
	}
	if err := w.captureCleanupColdQuotaOrigin(j, history); err != nil {
		return err
	}
	release := &cleanupBirthReleaseEvidence{origin: b, partition: j.partition, birth: s.birth, profile: history.profileRelease}
	release.self = release
	if _, err := matchCleanupBirthReleaseEvidence(s.birth, release); err != nil {
		return err
	}
	if err := b.validateCleanupColdQuotaPending(j, j.partition, s.birth, release); err != nil {
		return err
	}
	s.release, j.step = release, "cold-quota-pending-delete"
	return nil
}

func (w *DueWorker) publishCleanupColdQuota(j *cleanupLocalJob, s *cleanupColdQuotaWorkspace, entry *ColdRuntimeQuotaReturnEntry) error {
	if s != w.coldQuota || s.job != j || entry == nil || s.history == nil {
		return ErrInvalidRecord
	}
	binding, err := coldQuotaReturnRelease(s.history)
	if err != nil {
		return err
	}
	if err := w.captureCleanupColdQuotaOrigin(j, s.history); err != nil {
		return err
	}
	j.coldQuota.verified = &cleanupColdQuotaVerified{job: j, entry: *entry, release: cleanupColdQuotaReleaseStamp(binding.kv)}
	if err := w.validateCleanupColdQuotaVerified(j); err != nil {
		j.coldQuota.verified = nil
		return err
	}
	if j.coldQuota.quotaOnly {
		if binding.detached != nil {
			// Retain this original opaque proof while the same actor reads and
			// settles any Joined occupation. P requires its later fresh birth
			// and separately paid reauthentication, not this initial hint/proof.
			j.step = "observe"
			return nil
		}
		j.step = "cold-quota-pending-birth"
		return nil
	}
	if j.state != nil && j.state.record.Phase == "occupied" {
		// The cold detached settlement consumes this same opaque release after
		// its next charged terminal/settlement turn. Keep the one shared graph
		// until finishCleanupJob runs after the actual native owner has joined.
		if binding.detached == nil {
			w.releaseCleanupColdQuota(j)
		}
		j.step = "settle"
	} else {
		w.releaseCleanupColdQuota(j)
		// A terminal occupation needs no further physical settlement. Publish
		// its compact verified state before the next actor turn finishes it.
		j.step = "cold-quota-complete"
	}
	return nil
}
