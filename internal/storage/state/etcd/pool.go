package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// FUSEPoolRepository stores one pool's capacity in a fixed-size metadata key.
// State transitions must update that key and the affected record/index in one
// transaction; no request may recompute admission by scanning resident N.
type FUSEPoolRepository struct {
	store *Store
}

type poolMetadata struct {
	PoolKey    string `json:"pool_key"`
	Generation uint64 `json:"generation"`
	Total      uint64 `json:"total"`
	Preparing  uint64 `json:"preparing"`
	Prepared   uint64 `json:"prepared"`
}

// NewFUSEPoolRepository binds pool state to the Store's isolated namespace.
func NewFUSEPoolRepository(store *Store) *FUSEPoolRepository {
	return &FUSEPoolRepository{store: store}
}

func validPoolID(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value)
}

func poolDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (r *FUSEPoolRepository) poolBase(poolKey string) string {
	return r.store.prefix + "fusepool/v1/pool/" + poolDigest(poolKey) + "/"
}

func (r *FUSEPoolRepository) metadataKey(poolKey string) string {
	return r.poolBase(poolKey) + "meta"
}

func (r *FUSEPoolRepository) refillKey(poolKey string) string {
	return r.poolBase(poolKey) + "refill"
}

func (r *FUSEPoolRepository) preparingKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/preparing/" + poolDigest(id)
}

func (r *FUSEPoolRepository) preparedKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/prepared/" + poolDigest(id)
}

func (r *FUSEPoolRepository) recordKey(id string) string {
	return r.store.prefix + "fusepool/v1/record/" + poolDigest(id)
}

func (r *FUSEPoolRepository) uidOwnerKey(uid string) string {
	return r.store.prefix + "fusepool/v1/uid-owner/" + poolDigest(uid)
}

func (r *FUSEPoolRepository) prepareDeadlineKey(id string) string {
	return r.store.prefix + "fusepool/v1/deadline/prepare/" + poolDigest(id)
}

func (r *FUSEPoolRepository) reservationDeadlineKey(id string) string {
	return r.store.prefix + "fusepool/v1/deadline/reservation/" + poolDigest(id)
}

func (r *FUSEPoolRepository) reservedKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/reserved/" + poolDigest(id)
}

func (r *FUSEPoolRepository) bindingKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/binding/" + poolDigest(id)
}

func (r *FUSEPoolRepository) consumedKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/consumed/" + poolDigest(id)
}

func (r *FUSEPoolRepository) cleanupKey(poolKey, id string) string {
	return r.poolBase(poolKey) + "state/cleanup/" + poolDigest(id)
}

func (r *FUSEPoolRepository) cleanupDeadlineKey(id string) string {
	return r.store.prefix + "fusepool/v1/deadline/cleanup/" + poolDigest(id)
}

func (r *FUSEPoolRepository) stateKey(poolKey, id string, poolState state.FUSEPoolState) string {
	switch poolState {
	case state.FUSEPoolPreparing:
		return r.preparingKey(poolKey, id)
	case state.FUSEPoolPrepared:
		return r.preparedKey(poolKey, id)
	case state.FUSEPoolReserved:
		return r.reservedKey(poolKey, id)
	case state.FUSEPoolBinding:
		return r.bindingKey(poolKey, id)
	case state.FUSEPoolConsumed:
		return r.consumedKey(poolKey, id)
	case state.FUSEPoolCleanup:
		return r.cleanupKey(poolKey, id)
	default:
		return ""
	}
}

func validPoolState(poolState state.FUSEPoolState) bool {
	switch poolState {
	case state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved,
		state.FUSEPoolBinding, state.FUSEPoolConsumed, state.FUSEPoolCleanup:
		return true
	default:
		return false
	}
}

func decodePoolMetadata(raw []byte, poolKey string) (poolMetadata, error) {
	var metadata poolMetadata
	if json.Unmarshal(raw, &metadata) != nil || metadata.PoolKey != poolKey || metadata.Generation == 0 ||
		metadata.Total < metadata.Preparing || metadata.Total < metadata.Prepared ||
		metadata.Preparing+metadata.Prepared < metadata.Preparing ||
		metadata.Preparing+metadata.Prepared > metadata.Total {
		return poolMetadata{}, state.ErrFUSEPoolCorrupt
	}
	return metadata, nil
}

// TryRefillLock grants the exact pool maintainer a server-expiring lease.
// A lost write reply is unknown and never retried under a new lease here.
func (r *FUSEPoolRepository) TryRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(poolKey) || !validPoolID(token) || ttl <= 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	lease, err := r.store.leaseFor(requestCtx, ttl)
	if err != nil {
		return false, err
	}
	key := r.refillKey(poolKey)
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.Version(key), "=", 0)).
		Then(clientv3.OpPut(key, token, clientv3.WithLease(lease))).Commit()
	if err != nil {
		return false, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(requestCtx, lease)
		return false, nil
	}
	return true, nil
}

// RenewRefillLock moves the exact holder to a fresh server lease. The old
// lease is revoked only after an acknowledged replacement, so a lost reply
// cannot cancel a lock that may already have committed.
func (r *FUSEPoolRepository) RenewRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(poolKey) || !validPoolID(token) || ttl <= 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.refillKey(poolKey)
	read, err := r.store.client.Get(requestCtx, key)
	if err != nil {
		return false, err
	}
	if len(read.Kvs) == 0 || string(read.Kvs[0].Value) != token {
		return false, nil
	}
	lease, err := r.store.leaseFor(requestCtx, ttl)
	if err != nil {
		return false, err
	}
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)).
		Then(clientv3.OpPut(key, token, clientv3.WithLease(lease))).Commit()
	if err != nil {
		return false, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(requestCtx, lease)
		return false, nil
	}
	if oldLease := clientv3.LeaseID(read.Kvs[0].Lease); oldLease != 0 {
		_, _ = r.store.client.Revoke(requestCtx, oldLease)
	}
	return true, nil
}

// UnlockRefill deletes only the live matching refill lease. Absence is not a
// successful release because the caller may need to distinguish expiry.
func (r *FUSEPoolRepository) UnlockRefill(ctx context.Context, poolKey, token string) error {
	if r == nil || r.store == nil {
		return errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(poolKey) || !validPoolID(token) {
		return state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.refillKey(poolKey)
	read, err := r.store.client.Get(requestCtx, key)
	if err != nil {
		return err
	}
	if len(read.Kvs) == 0 {
		return state.ErrFUSEPoolNotFound
	}
	if string(read.Kvs[0].Value) != token {
		return state.ErrFUSEPoolTokenMismatch
	}
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)).
		Then(clientv3.OpDelete(key)).Commit()
	if err != nil {
		return err
	}
	if !written.Succeeded {
		return state.ErrFUSEPoolTokenMismatch
	}
	return nil
}

// CountPreparingAndPrepared reads the same aggregate used by admission. It
// does not traverse consumed records or derive capacity from local time.
func (r *FUSEPoolRepository) CountPreparingAndPrepared(ctx context.Context, poolKey string) (int, error) {
	if r == nil || r.store == nil || !validPoolID(poolKey) {
		return 0, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	read, err := r.store.client.Get(requestCtx, r.metadataKey(poolKey))
	if err != nil || len(read.Kvs) == 0 {
		return 0, err
	}
	metadata, err := decodePoolMetadata(read.Kvs[0].Value, poolKey)
	if err != nil {
		return 0, err
	}
	count := metadata.Preparing + metadata.Prepared
	if count > uint64(^uint(0)>>1) {
		return 0, state.ErrFUSEPoolCorrupt
	}
	return int(count), nil
}

// ReadByPreparationID is an exact lookup; absence returns nil. The record ID
// is checked after decoding because its key is a digest of caller input.
func (r *FUSEPoolRepository) ReadByPreparationID(ctx context.Context, id string) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil || !validPoolID(id) {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	read, err := r.store.client.Get(requestCtx, r.recordKey(id))
	if err != nil || len(read.Kvs) == 0 {
		return nil, err
	}
	record, err := decodePoolRecord(read.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// CreatePreparingWithAdmission reserves one preparing slot under a live
// refill lock. This first production path accepts the unbound, unreserved
// intent emitted by FUSEPool.prepareOne. Other input shapes fail explicitly
// until their lease semantics are implemented.
func (r *FUSEPoolRepository) CreatePreparingWithAdmission(ctx context.Context, record state.FUSEPoolRecord, refillToken string, maxSize int, prepareTTL time.Duration) error {
	if r == nil || r.store == nil {
		return errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(record.PreparationID) || !validPoolID(record.PoolKey) || !validPoolID(record.MaintainerToken) ||
		!validPoolID(refillToken) || record.State != state.FUSEPoolPreparing || record.Revision != 1 ||
		record.RuntimeID != "" || record.RuntimeUID != "" || record.ReservationToken != "" ||
		!record.ReservedUntil.IsZero() || !record.PrepareUntil.IsZero() ||
		record.CleanupToken != "" || !record.CleanupUntil.IsZero() || record.TerminationEvidence != nil ||
		maxSize <= 0 || prepareTTL <= 0 {
		return state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	metaKey := r.metadataKey(record.PoolKey)
	lockKey := r.refillKey(record.PoolKey)
	read, err := r.store.client.Txn(requestCtx).
		Then(clientv3.OpGet(metaKey), clientv3.OpGet(lockKey)).Commit()
	if err != nil {
		return err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil || read.Responses[1].GetResponseRange() == nil {
		return state.ErrFUSEPoolCorrupt
	}
	locks := read.Responses[1].GetResponseRange().Kvs
	if len(locks) == 0 || string(locks[0].Value) != refillToken {
		return state.ErrFUSEPoolTokenMismatch
	}
	metaRead := read.Responses[0].GetResponseRange()
	metadata := poolMetadata{PoolKey: record.PoolKey}
	metaCompare := clientv3.Compare(clientv3.Version(metaKey), "=", 0)
	if len(metaRead.Kvs) != 0 {
		metadata, err = decodePoolMetadata(metaRead.Kvs[0].Value, record.PoolKey)
		if err != nil {
			return err
		}
		metaCompare = clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision)
	}
	if metadata.Preparing+metadata.Prepared >= uint64(maxSize) {
		return state.ErrFUSEPoolConflict
	}
	if metadata.Generation == ^uint64(0) || metadata.Total == ^uint64(0) || metadata.Preparing == ^uint64(0) {
		return state.ErrFUSEPoolCorrupt
	}
	metadata.Generation++
	metadata.Total++
	metadata.Preparing++
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	// PrepareUntil is a scheduling hint. The leased deadline key is the
	// server-side expiry fact that later cleanup transitions must compare.
	now := time.Now().UTC()
	record.UpdatedAt = now
	record.PrepareUntil = now.Add(prepareTTL)
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		return err
	}
	lease, err := r.store.leaseFor(requestCtx, prepareTTL)
	if err != nil {
		return err
	}
	recordKey := r.recordKey(record.PreparationID)
	indexKey := r.preparingKey(record.PoolKey, record.PreparationID)
	deadlineKey := r.prepareDeadlineKey(record.PreparationID)
	written, err := r.store.client.Txn(requestCtx).
		If(metaCompare,
			clientv3.Compare(clientv3.Value(lockKey), "=", refillToken),
			clientv3.Compare(clientv3.Version(recordKey), "=", 0),
			clientv3.Compare(clientv3.Version(indexKey), "=", 0),
			clientv3.Compare(clientv3.Version(deadlineKey), "=", 0)).
		Then(clientv3.OpPut(metaKey, string(encodedMetadata)),
			clientv3.OpPut(recordKey, string(encodedRecord)),
			clientv3.OpPut(indexKey, record.PreparationID),
			clientv3.OpPut(deadlineKey, record.PreparationID, clientv3.WithLease(lease))).
		Else(clientv3.OpGet(lockKey), clientv3.OpGet(metaKey), clientv3.OpGet(recordKey)).Commit()
	if err != nil {
		return err
	}
	if written.Succeeded {
		return nil
	}
	_, _ = r.store.client.Revoke(requestCtx, lease)
	if len(written.Responses) != 3 || written.Responses[0].GetResponseRange() == nil ||
		written.Responses[1].GetResponseRange() == nil || written.Responses[2].GetResponseRange() == nil {
		return state.ErrFUSEPoolCorrupt
	}
	currentLocks := written.Responses[0].GetResponseRange().Kvs
	if len(currentLocks) == 0 || string(currentLocks[0].Value) != refillToken {
		return state.ErrFUSEPoolTokenMismatch
	}
	if len(written.Responses[2].GetResponseRange().Kvs) != 0 {
		return state.ErrFUSEPoolConflict
	}
	return state.ErrFUSEPoolConflict
}
