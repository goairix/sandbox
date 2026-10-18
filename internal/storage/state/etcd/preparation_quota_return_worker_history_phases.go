package etcd

// Each helper owns one complete original loader group. All caps below are
// value bytes from the closed read-phase delta, not transport frame limits.
// The paid reader owns W20 and actual native-call joining; helpers reuse the
// existing pure business validators and publish only after binding revalidation.
func (b *Backend) coldQuotaWorkerHistoryPublicationKeys(origin coldQuotaWorkerHistoryOrigin) ([]string, error) {
	j, proof, err := b.namespace.runtimePublicationKeys(origin.workspace.Partition(), origin.intent)
	if err != nil {
		return nil, err
	}
	enrollment, activation, err := b.namespace.runtimeEnrollmentKeys(origin.workspace.Partition(), origin.intent)
	if err != nil {
		return nil, err
	}
	return []string{j, proof, enrollment, activation}, nil
}

func (b *Backend) readColdQuotaWorkerHistoryLocate(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	keys, err := b.coldQuotaWorkerHistoryPublicationKeys(origin)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, keys, []int{8192, 16384, 8192, 8192})
	if err != nil {
		return err
	}
	if values[0] == nil {
		for _, point := range values {
			if point != nil {
				return ErrCorruptRecord
			}
		}
		return ErrConflict
	}
	var located RuntimeEnrolledPublicationRecord
	if !enrolledImmutableKV(values[0]) || decodeEnrolledRecord(values[0].Value, &located) != nil {
		return ErrCorruptRecord
	}
	if located.IntentID != origin.intent || located.WorkspaceHash != origin.workspace.Hash() || located.Attempt.Namespace != b.namespace.Root() || located.Attempt.Partition != origin.workspace.Partition() || located.RestoreEpoch != b.restoreEpoch {
		return ErrCorruptRecord
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.located = located
	state.phase = coldQuotaWorkerHistoryWantPublication
	return nil
}

func (b *Backend) readColdQuotaWorkerHistoryPublication(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	keys, err := b.coldQuotaWorkerHistoryPublicationKeys(origin)
	if err != nil {
		return err
	}
	receipt, err := b.runtimeDispatchReceiptKey(state.located.Attempt)
	if err != nil {
		return err
	}
	preparation, err := b.coldHistoricalKeys(origin.workspace.Partition(), origin.intent, state.located)
	if err != nil {
		return err
	}
	keys = append(append(keys, receipt), preparation...)
	caps := []int{8192, 16384, 8192, 8192, 4096, 4096, 65536, 4096, 4096, 8192, 4096, 4096, 4096}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, keys, caps)
	if err != nil {
		return err
	}
	entry, err := b.decodeColdHistoricalPublication(values, keys, origin.workspace, origin.intent, state.located)
	if err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	// Authentication above includes the original signatures and located==full
	// binding. Quota history needs only this projection and its two CAS points;
	// do not retain the temporary proof/enrollment/preparation payload graph.
	state.history.publication = &coldPublicationHistory{
		entry:   &RuntimeEnrolledPublicationEntry{Record: entry.Record, Reference: entry.Reference},
		journal: values[0], receipt: values[4],
	}
	state.located = RuntimeEnrolledPublicationRecord{}
	state.phase = coldQuotaWorkerHistoryWantReservation
	return nil
}

func (b *Backend) readColdQuotaWorkerHistoryReservation(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	reservation, work, err := b.namespace.coldPreparationKeys(origin.workspace.Partition(), origin.intent)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, []string{reservation, work}, []int{16384, 16384})
	if err != nil {
		return err
	}
	var record ColdPreparationRecord
	if coldDecode(values[0], &record) != nil || !b.validColdReservation(record, origin.workspace, origin.intent) || record.Phase != "settled" || !coldCompletionMatches(record, state.history.publication.entry.Record) {
		return ErrConflict
	}
	// Preserve the public settlement wrapper's source-error precedence: a bad
	// work marker or transaction birth must fail before paying for a receipt.
	if err := b.validateColdSettlementSource(record, values[0], values[1], state.history.publication); err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.history.reservation = record
	state.history.reservedKV, state.history.workKV = values[0], values[1]
	state.phase = coldQuotaWorkerHistoryWantSettlement
	return nil
}

func (b *Backend) readColdQuotaWorkerHistorySettlement(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	history := state.history
	key, err := b.runtimeDispatchReceiptKey(history.reservation.Settlement)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, []string{key}, []int{4096})
	if err != nil {
		return err
	}
	ref, err := b.decodeColdSettlement(history.reservation, history.reservedKV, history.workKV, history.publication, values[0])
	if err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	// One canonical immutable receipt satisfies both original A4 and A5 reads.
	// Its full reference and original MVCC point remain the final CAS sources.
	history.settlement, history.settledKV = ref, values[0]
	state.phase = coldQuotaWorkerHistoryWantRelease
	return nil
}

func (b *Backend) readColdQuotaWorkerHistoryRelease(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	_, _, key, err := b.namespace.localRuntimeReleaseKeys(origin.task)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, []string{key}, []int{8192})
	if err != nil {
		return err
	}
	if values[0] == nil {
		return ErrConflict
	}
	profile, err := decodeTaskOwnerReleaseProfile(origin.task, values[0])
	if err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.releaseProfile, state.releaseKV = profile, values[0]
	state.phase = coldQuotaWorkerHistoryWantHead
	return nil
}

func (b *Backend) readColdQuotaWorkerHistoryHead(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	if state.releaseProfile == taskOwnerReleaseDetached {
		return b.readColdQuotaWorkerHistoryDetachedHead(state, origin)
	}
	if state.releaseProfile != taskOwnerReleaseLocal {
		return ErrInvalidRecord
	}

	termination, provider, _, err := b.namespace.localRuntimeReleaseKeys(origin.task)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, []string{termination, provider}, []int{16384, 16384})
	if err != nil {
		return err
	}
	snapshot := &localRuntimeHistorySnapshot{termination: values[0], provider: values[1]}
	if _, err := b.decodeLocalRuntimeHistoryHead(origin.task, snapshot); err != nil {
		return err
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.snapshot = snapshot
	state.phase = coldQuotaWorkerHistoryWantSignedHistory
	return nil
}

func (b *Backend) readColdQuotaWorkerHistorySigned(state *coldQuotaWorkerHistory, origin coldQuotaWorkerHistoryOrigin) error {
	if state.releaseProfile == taskOwnerReleaseDetached {
		return b.readColdQuotaWorkerHistoryDetachedSigned(state, origin)
	}
	if state.releaseProfile != taskOwnerReleaseLocal {
		return ErrInvalidRecord
	}

	r, err := b.decodeLocalRuntimeHistoryHead(origin.task, state.snapshot)
	if err != nil {
		return err
	}
	keys, err := b.localRuntimeHistoryKeys(r, origin.task)
	if err != nil {
		return err
	}
	values, err := b.readColdQuotaWorkerPoints(origin.owner, origin.session, origin.partition, origin.ranges, keys, localRuntimeHistoryCaps())
	if err != nil {
		return err
	}
	snapshot := *state.snapshot
	copy(snapshot.history[:], values)
	release, err := b.decodeLocalOwnerReleaseProof(origin.task, state.releaseKV, &snapshot)
	if err != nil {
		return err
	}
	if !coldQuotaReturnOriginMatches(state.history.reservation, state.history.publication.entry.Record, release) {
		return ErrConflict
	}
	if err := b.validateColdQuotaWorkerHistoryState(state, origin); err != nil {
		return err
	}

	state.history.release = release
	state.releaseKV, state.snapshot = nil, nil
	state.phase = coldQuotaWorkerHistoryDone
	return nil
}
