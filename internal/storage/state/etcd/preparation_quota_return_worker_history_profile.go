package etcd

// Detached history uses the original paid point reader and its complete W20
// preflight, payment and actual-return join. Task is part of this same head
// transaction so snapshot routing never trusts a reconstructed native envelope.
// The local v1 head remains the original two-point group in the phases file.
func (b *Backend) readColdQuotaWorkerHistoryDetachedHead(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	termination, provider, _, err := b.namespace.localRuntimeReleaseKeys(origin.task)
	if err != nil {
		return err
	}
	taskKey, err := b.namespace.taskKey(origin.task)
	if err != nil {
		return err
	}
	keys := []string{termination, provider, taskKey}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, keys, []int{16384, 16384, 4096})
	if err != nil {
		return err
	}
	head := &detachedRuntimeHistorySnapshot{termination: values[0], provider: values[1]}
	intent, err := b.decodeDetachedRuntimeHistoryHead(origin.task, head)
	if err != nil {
		return err
	}
	if _, err := b.decodeDetachedRuntimeTask(intent, values[2]); err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.detachedHead, state.actualTask = head, values[2]
	state.phase = coldQuotaWorkerHistoryWantSignedHistory
	return nil
}

// The full thirteen-point join is B's existing pure native/signed history
// decoder. Its union is retained only after cold reservation/publication origin
// matching and the original post-return owner/member/context revalidation.
// The cursor and the returned history borrow the same union. Every downstream
// writer selects that opaque profile explicitly; none treats a nil local proof
// as permission to invent a v1 Complete or use an unsupported record builder.
func (b *Backend) readColdQuotaWorkerHistoryDetachedSigned(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	intent, err := b.decodeDetachedRuntimeHistoryHead(origin.task, state.detachedHead)
	if err != nil {
		return err
	}
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(intent, state.actualTask)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, keys, caps)
	if err != nil {
		return err
	}
	proof, err := b.decodeTaskOwnerReleaseProof(origin.task, state.actualTask, state.releaseKV, state.detachedHead.termination, state.detachedHead.provider, values)
	if err != nil {
		return err
	}
	if !coldQuotaDetachedHistoryOriginMatches(state.history, proof) {
		return ErrConflict
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.releaseProof = proof
	state.history.profileRelease = proof
	state.releaseKV, state.detachedHead, state.actualTask = nil, nil, nil
	state.phase = coldQuotaWorkerHistoryDone
	return nil
}

// Match the already authenticated v2 branch to the independently paid cold
// preparation/publication history. This checks attribution, not a current
// permission to refund. In particular quota's partition may differ from its
// Owner's partition, as allowed by the original preparation protocol.
func coldQuotaDetachedHistoryOriginMatches(history *coldQuotaReturnHistory, proof *taskOwnerReleaseProof) bool {
	if history == nil || history.release != nil || history.publication == nil || history.publication.entry == nil || proof == nil || proof.local != nil || proof.detached == nil {
		return false
	}
	release := proof.detached
	if release.history == nil || history.publication.journal == nil {
		return false
	}
	// Point9 is the publication in B's fixed thirteen-point table. It must
	// also be the complete native source retained by the earlier paid group;
	// an equal record value after delete/recreate cannot replace that source.
	publicationPoint := release.history.history[9]
	if publicationPoint == nil || publicationPoint.Version != history.publication.journal.Version || !ownTaskFence(history.publication.journal).matches(publicationPoint) {
		return false
	}
	r, publication := history.reservation, history.publication.entry.Record
	if !coldCompletionMatches(r, publication) {
		return false
	}
	t, task := release.termination, release.record.Task
	return task.Namespace == publication.Attempt.Namespace &&
		task.RestoreEpoch == publication.RestoreEpoch &&
		task.Partition == r.Owner.Partition && task.SandboxID == r.Owner.SandboxID &&
		release.record.TaskBirth == t.TaskBirth &&
		t.CreationIntentID == r.Owner.IntentID &&
		t.Current.WorkspaceHash == r.Owner.WorkspaceHash && t.Current.Generation == r.Owner.Generation &&
		t.Current.DataGateEpoch == publication.DataGateEpoch && RuntimeReference(t.Current.Runtime) == publication.Runtime &&
		t.NodeUID == publication.NodeUID && t.NodeBootID == publication.NodeBootID &&
		t.EnrollmentDigest == publication.EnrollmentDigest && t.ObserverCertificateDigest == publication.ObserverCertificateDigest &&
		t.Phase == "cleanup-complete"
}
