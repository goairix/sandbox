package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redisclient "github.com/redis/go-redis/v9"
)

const (
	fusePoolRecordPrefix = "fusepool:record:"
	fusePoolIndexPrefix  = "fusepool:index:"
	fusePoolLockPrefix   = "fusepool:lock:"
	fusePoolRecordPools  = "fusepool:record-pools"
	fusePoolDeadlines    = "fusepool:reservation-deadlines"
	fusePoolRecordUIDs   = "fusepool:record-uids"
	fusePoolPoolValues   = "fusepool:record-pool-values"
)

const (
	poolResultNone     int64 = 0
	poolResultOK       int64 = 1
	poolResultNotFound int64 = -1
	poolResultCAS      int64 = -2
	poolResultConflict int64 = -3
	poolResultCorrupt  int64 = -4
	poolResultInvalid  int64 = -5
	poolResultToken    int64 = -6
)

var createPreparingScript = redisclient.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
    return -3
end
if redis.call('HEXISTS', KEYS[3], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[4], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[5], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[6], ARGV[1]) == 1 then
    return -4
end
redis.call('SET', KEYS[1], ARGV[3])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[4], ARGV[1], ARGV[4])
redis.call('HSET', KEYS[5], ARGV[1], ARGV[5])
redis.call('HSET', KEYS[6], ARGV[1], ARGV[6])
redis.call('SADD', KEYS[2], ARGV[1])
return 1
`)

var reservePreparedScript = redisclient.NewScript(`
local members = redis.call('SMEMBERS', KEYS[1])
table.sort(members)
local candidateMember = nil
local candidateKey = nil
local candidate = nil
for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[2], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[4], member)
    local mappedPoolValue = redis.call('HGET', KEYS[5], member)
    local recordKey = ARGV[5] .. member
    local raw = redis.call('GET', recordKey)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or type(record) ~= 'table'
        or type(record.runtime_uid) ~= 'string'
        or record.runtime_uid == ''
        or not mappedUID
        or record.runtime_uid ~= mappedUID
        or type(record.runtime_id) ~= 'string'
        or record.runtime_id == ''
        or type(record.pool_key) ~= 'string'
        or record.pool_key ~= ARGV[2]
        or not mappedPoolValue
        or record.pool_key ~= mappedPoolValue
        or type(record.state) ~= 'string'
        or (record.state ~= 'preparing' and record.state ~= 'prepared'
            and record.state ~= 'reserved' and record.state ~= 'binding'
            and record.state ~= 'consumed')
        or type(record.maintainer_token) ~= 'string'
        or (record.reservation_token ~= nil and type(record.reservation_token) ~= 'string')
        or type(record.revision) ~= 'number'
        or record.revision < 1
        or record.revision ~= math.floor(record.revision) then
        return {-4}
    end
    if record.state == 'prepared' and not candidate then
        if (record.reservation_token or '') ~= '' then
            return {-4}
        end
        candidateMember = member
        candidateKey = recordKey
        candidate = record
    end
end
if candidate then
    candidate.state = 'reserved'
    candidate.reservation_token = ARGV[3]
    candidate.reserved_until = ARGV[4]
    candidate.updated_at = ARGV[6]
    candidate.revision = candidate.revision + 1
    local updated = cjson.encode(candidate)
    redis.call('SET', candidateKey, updated)
    redis.call('HSET', KEYS[3], candidateMember, ARGV[7])
    return {1, updated}
end
return {0}
`)

var transitionScript = redisclient.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then
    return {-1}
end
local decoded, record = pcall(cjson.decode, raw)
if not decoded or type(record) ~= 'table'
    or type(record.runtime_uid) ~= 'string'
    or record.runtime_uid ~= ARGV[1]
    or type(record.runtime_id) ~= 'string'
    or record.runtime_id == ''
    or type(record.pool_key) ~= 'string'
    or record.pool_key == ''
    or type(record.state) ~= 'string'
    or type(record.maintainer_token) ~= 'string'
    or (record.reservation_token ~= nil and type(record.reservation_token) ~= 'string')
    or type(record.revision) ~= 'number'
    or record.revision < 1
    or record.revision ~= math.floor(record.revision) then
    return {-4}
end
local mappedPoolDigest = redis.call('HGET', KEYS[3], ARGV[8])
local mappedUID = redis.call('HGET', KEYS[4], ARGV[8])
local mappedPoolValue = redis.call('HGET', KEYS[5], ARGV[8])
if not mappedPoolDigest or string.len(mappedPoolDigest) ~= 64
    or not string.match(mappedPoolDigest, '^[0-9a-f]+$')
    or mappedUID ~= record.runtime_uid
    or mappedPoolValue ~= record.pool_key
    or redis.call('SISMEMBER', ARGV[9] .. mappedPoolDigest, ARGV[8]) ~= 1 then
    return {-4}
end
if record.revision ~= tonumber(ARGV[5]) then
    return {-2}
end
if record.state ~= ARGV[2] then
    return {-3}
end

local from = ARGV[2]
local to = ARGV[3]
local token = ARGV[4]
local reservation = record.reservation_token or ''
local function reservationIsLive()
    local deadline = tonumber(redis.call('HGET', KEYS[2], ARGV[8]))
    local redisTime = redis.call('TIME')
    local nowMillis = tonumber(redisTime[1]) * 1000 + math.floor(tonumber(redisTime[2]) / 1000)
    return deadline and deadline > nowMillis
end
if from == 'preparing' and to == 'prepared' then
    if reservation ~= '' then return {-5} end
    if token ~= (record.maintainer_token or '') then return {-6} end
elseif from == 'preparing' and to == 'reserved' then
    if reservation == '' then return {-5} end
    if token ~= reservation then return {-6} end
    if not reservationIsLive() then return {-5} end
elseif from == 'reserved' and to == 'binding' then
    if reservation == '' then return {-4} end
    if token ~= reservation then return {-6} end
    if not reservationIsLive() then return {-5} end
elseif from == 'binding' and to == 'consumed' then
    if reservation == '' then return {-4} end
    if token ~= reservation then return {-6} end
elseif from == 'reserved' and to == 'prepared' then
    if reservation == '' then return {-4} end
    if token ~= reservation then return {-6} end
    if not reservationIsLive() then return {-5} end
    record.reservation_token = nil
    record.reserved_until = ARGV[7]
    redis.call('HSET', KEYS[2], ARGV[8], '0')
else
    return {-5}
end

record.state = to
record.updated_at = ARGV[6]
record.revision = record.revision + 1
local updated = cjson.encode(record)
redis.call('SET', KEYS[1], updated)
return {1, updated}
`)

var listByPoolKeyScript = redisclient.NewScript(`
local members = redis.call('SMEMBERS', KEYS[1])
table.sort(members)
local result = {1}
for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[2], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[3], member)
    local mappedPoolValue = redis.call('HGET', KEYS[4], member)
    local raw = redis.call('GET', ARGV[3] .. member)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or type(record) ~= 'table'
        or type(record.runtime_uid) ~= 'string'
        or record.runtime_uid ~= mappedUID
        or type(record.runtime_id) ~= 'string'
        or type(record.pool_key) ~= 'string'
        or record.pool_key ~= ARGV[2]
        or record.pool_key ~= mappedPoolValue
        or type(record.state) ~= 'string'
        or type(record.revision) ~= 'number'
        or record.revision < 1
        or record.revision ~= math.floor(record.revision) then
        return {-4}
    end
    table.insert(result, member)
    table.insert(result, raw)
end
return result
`)

var countPreparingAndPreparedScript = redisclient.NewScript(`
local members = redis.call('SMEMBERS', KEYS[1])
local result = {1}
for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[2], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[3], member)
    local mappedPoolValue = redis.call('HGET', KEYS[4], member)
    local raw = redis.call('GET', ARGV[3] .. member)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or type(record) ~= 'table'
        or type(record.runtime_uid) ~= 'string'
        or record.runtime_uid ~= mappedUID
        or type(record.pool_key) ~= 'string'
        or record.pool_key ~= ARGV[2]
        or record.pool_key ~= mappedPoolValue
        or type(record.state) ~= 'string'
        or type(record.revision) ~= 'number'
        or record.revision < 1
        or record.revision ~= math.floor(record.revision) then
        return {-4}
    end
    table.insert(result, member)
    table.insert(result, raw)
end
return result
`)

var conditionalDeleteScript = redisclient.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then
    return -1
end
local decoded, record = pcall(cjson.decode, raw)
if not decoded or type(record) ~= 'table'
    or type(record.runtime_uid) ~= 'string'
    or record.runtime_uid ~= ARGV[1]
    or type(record.state) ~= 'string'
    or type(record.revision) ~= 'number'
    or record.revision < 1
    or record.revision ~= math.floor(record.revision) then
    return -4
end
if record.revision ~= tonumber(ARGV[5]) then return -2 end
if record.state ~= ARGV[2] then return -3 end
if (record.maintainer_token or '') ~= ARGV[3] then return -6 end
if (record.reservation_token or '') ~= ARGV[4] then return -6 end
local poolDigest = redis.call('HGET', KEYS[2], ARGV[6])
if not poolDigest or string.len(poolDigest) ~= 64 or not string.match(poolDigest, '^[0-9a-f]+$') then
    return -4
end
local mappedUID = redis.call('HGET', KEYS[4], ARGV[6])
local mappedPoolValue = redis.call('HGET', KEYS[5], ARGV[6])
if mappedUID ~= record.runtime_uid
    or mappedPoolValue ~= record.pool_key
    or redis.call('SISMEMBER', ARGV[7] .. poolDigest, ARGV[6]) ~= 1 then
    return -4
end
redis.call('DEL', KEYS[1])
redis.call('SREM', ARGV[7] .. poolDigest, ARGV[6])
redis.call('HDEL', KEYS[2], ARGV[6])
redis.call('HDEL', KEYS[3], ARGV[6])
redis.call('HDEL', KEYS[4], ARGV[6])
redis.call('HDEL', KEYS[5], ARGV[6])
return 1
`)

var tryRefillLockScript = redisclient.NewScript(`
local result = redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2])
if result then return 1 end
return 0
`)

var unlockRefillScript = redisclient.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return -1 end
if current ~= ARGV[1] then return -6 end
redis.call('DEL', KEYS[1])
return 1
`)

type FUSEPoolRepository struct {
	store *Store
}

func NewFUSEPoolRepository(store *Store) *FUSEPoolRepository {
	return &FUSEPoolRepository{store: store}
}

func (r *FUSEPoolRepository) CreatePreparing(ctx context.Context, record state.FUSEPoolRecord) error {
	if r == nil || r.store == nil {
		return errors.New("fuse pool repository: nil store")
	}
	if err := validatePreparingRecord(record); err != nil {
		return err
	}
	record.UpdatedAt = time.Now().UTC()
	deadlineMillis := int64(0)
	if !record.ReservedUntil.IsZero() {
		record.ReservedUntil = record.ReservedUntil.UTC()
		deadlineMillis = record.ReservedUntil.UnixMilli()
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode fuse pool record: %w", state.ErrFUSEPoolInvalidRecord)
	}
	uidDigest := fusePoolDigest(record.RuntimeUID)
	poolDigest := fusePoolDigest(record.PoolKey)
	result, err := createPreparingScript.Run(ctx, r.store.client,
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues},
		uidDigest, poolDigest, raw, deadlineMillis, record.RuntimeUID, record.PoolKey,
	).Int64()
	if err != nil {
		return err
	}
	return poolMutationError("create preparing record", result)
}

func (r *FUSEPoolRepository) ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if poolKey == "" || token == "" || ttl <= 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	poolDigest := fusePoolDigest(poolKey)
	now := time.Now().UTC()
	ttlMillis, err := redisTTLMilliseconds(ttl)
	if err != nil {
		return nil, err
	}
	deadlineMillis := now.UnixMilli() + ttlMillis
	reservedUntil := time.UnixMilli(deadlineMillis).UTC()
	result, err := reservePreparedScript.Run(ctx, r.store.client,
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues},
		poolDigest, poolKey, token, reservedUntil.Format(time.RFC3339Nano), fusePoolRecordPrefix, now.Format(time.RFC3339Nano), deadlineMillis,
	).Slice()
	if err != nil {
		return nil, err
	}
	code, payload, err := poolScriptResult(result)
	if err != nil {
		return nil, err
	}
	if code == poolResultNone {
		return nil, nil
	}
	if code != poolResultOK {
		return nil, poolMutationError("reserve prepared record", code)
	}
	return decodePoolRecord(payload)
}

func (r *FUSEPoolRepository) Transition(ctx context.Context, runtimeUID string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if runtimeUID == "" || expectedRevision == 0 || !allowedFUSEPoolTransition(from, to) {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	uidDigest := fusePoolDigest(runtimeUID)
	result, err := transitionScript.Run(ctx, r.store.client,
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolDeadlines, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues},
		runtimeUID, string(from), string(to), token, strconv.FormatUint(expectedRevision, 10), now, time.Time{}.Format(time.RFC3339Nano), uidDigest, fusePoolIndexPrefix,
	).Slice()
	if err != nil {
		return nil, err
	}
	code, payload, err := poolScriptResult(result)
	if err != nil {
		return nil, err
	}
	if code != poolResultOK {
		return nil, poolMutationError("transition record", code)
	}
	return decodePoolRecord(payload)
}

func (r *FUSEPoolRepository) ListByPoolKey(ctx context.Context, poolKey string) ([]state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if poolKey == "" {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	poolDigest := fusePoolDigest(poolKey)
	result, err := listByPoolKeyScript.Run(ctx, r.store.client,
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues},
		poolDigest, poolKey, fusePoolRecordPrefix,
	).Slice()
	if err != nil {
		return nil, err
	}
	code, payloads, err := poolScriptResults(result)
	if err != nil {
		return nil, err
	}
	if code != poolResultOK {
		return nil, poolMutationError("list records", code)
	}
	if len(payloads)%2 != 0 {
		return nil, state.ErrFUSEPoolCorrupt
	}
	records := make([]state.FUSEPoolRecord, 0, len(payloads)/2)
	for i := 0; i < len(payloads); i += 2 {
		member, ok := payloads[i].(string)
		if !ok {
			return nil, state.ErrFUSEPoolCorrupt
		}
		record, decodeErr := decodePoolRecord(payloads[i+1])
		if decodeErr != nil {
			return nil, decodeErr
		}
		if record.PoolKey != poolKey || fusePoolDigest(record.RuntimeUID) != member {
			return nil, state.ErrFUSEPoolCorrupt
		}
		records = append(records, *record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].RuntimeUID == records[j].RuntimeUID {
			return records[i].RuntimeID < records[j].RuntimeID
		}
		return records[i].RuntimeUID < records[j].RuntimeUID
	})
	return records, nil
}

func (r *FUSEPoolRepository) CountPreparingAndPrepared(ctx context.Context, poolKey string) (int, error) {
	if r == nil || r.store == nil {
		return 0, errors.New("fuse pool repository: nil store")
	}
	if poolKey == "" {
		return 0, state.ErrFUSEPoolInvalidRecord
	}
	poolDigest := fusePoolDigest(poolKey)
	result, err := countPreparingAndPreparedScript.Run(ctx, r.store.client,
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues},
		poolDigest, poolKey, fusePoolRecordPrefix,
	).Slice()
	if err != nil {
		return 0, err
	}
	code, payloads, err := poolScriptResults(result)
	if err != nil {
		return 0, err
	}
	if code != poolResultOK {
		return 0, poolMutationError("count records", code)
	}
	if len(payloads)%2 != 0 {
		return 0, state.ErrFUSEPoolCorrupt
	}
	count := 0
	for i := 0; i < len(payloads); i += 2 {
		member, ok := payloads[i].(string)
		if !ok {
			return 0, state.ErrFUSEPoolCorrupt
		}
		record, decodeErr := decodePoolRecord(payloads[i+1])
		if decodeErr != nil || record.PoolKey != poolKey || fusePoolDigest(record.RuntimeUID) != member {
			return 0, state.ErrFUSEPoolCorrupt
		}
		if record.State == state.FUSEPoolPreparing || record.State == state.FUSEPoolPrepared {
			count++
		}
	}
	return count, nil
}

func (r *FUSEPoolRepository) ConditionalDelete(ctx context.Context, runtimeUID string, expectedState state.FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("fuse pool repository: nil store")
	}
	if runtimeUID == "" || !validFUSEPoolState(expectedState) || expectedRevision == 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	uidDigest := fusePoolDigest(runtimeUID)
	result, err := conditionalDeleteScript.Run(ctx, r.store.client,
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues},
		runtimeUID, string(expectedState), maintainerToken, reservationToken, strconv.FormatUint(expectedRevision, 10), uidDigest, fusePoolIndexPrefix,
	).Int64()
	if err != nil {
		return false, err
	}
	if result != poolResultOK {
		return false, poolMutationError("conditionally delete record", result)
	}
	return true, nil
}

func (r *FUSEPoolRepository) TryRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("fuse pool repository: nil store")
	}
	if poolKey == "" || token == "" || ttl <= 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	ttlMillis, err := redisTTLMilliseconds(ttl)
	if err != nil {
		return false, err
	}
	result, err := tryRefillLockScript.Run(ctx, r.store.client,
		[]string{fusePoolLockPrefix + fusePoolDigest(poolKey)}, token, ttlMillis,
	).Int64()
	if err != nil {
		return false, err
	}
	return result == poolResultOK, nil
}

func (r *FUSEPoolRepository) UnlockRefill(ctx context.Context, poolKey, token string) error {
	if r == nil || r.store == nil {
		return errors.New("fuse pool repository: nil store")
	}
	if poolKey == "" || token == "" {
		return state.ErrFUSEPoolInvalidRecord
	}
	result, err := unlockRefillScript.Run(ctx, r.store.client,
		[]string{fusePoolLockPrefix + fusePoolDigest(poolKey)}, token,
	).Int64()
	if err != nil {
		return err
	}
	return poolMutationError("unlock refill", result)
}

func fusePoolDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validatePreparingRecord(record state.FUSEPoolRecord) error {
	if record.RuntimeID == "" || record.RuntimeUID == "" || record.PoolKey == "" || record.State != state.FUSEPoolPreparing || record.Revision != 1 {
		return state.ErrFUSEPoolInvalidRecord
	}
	if (record.ReservationToken == "") != record.ReservedUntil.IsZero() {
		return state.ErrFUSEPoolInvalidRecord
	}
	return nil
}

func validFUSEPoolState(value state.FUSEPoolState) bool {
	switch value {
	case state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed:
		return true
	default:
		return false
	}
}

func allowedFUSEPoolTransition(from, to state.FUSEPoolState) bool {
	switch {
	case from == state.FUSEPoolPreparing && to == state.FUSEPoolPrepared:
		return true
	case from == state.FUSEPoolPreparing && to == state.FUSEPoolReserved:
		return true
	case from == state.FUSEPoolReserved && to == state.FUSEPoolBinding:
		return true
	case from == state.FUSEPoolBinding && to == state.FUSEPoolConsumed:
		return true
	case from == state.FUSEPoolReserved && to == state.FUSEPoolPrepared:
		return true
	default:
		return false
	}
}

func poolMutationError(operation string, code int64) error {
	var err error
	switch code {
	case poolResultOK:
		return nil
	case poolResultNotFound:
		err = state.ErrFUSEPoolNotFound
	case poolResultCAS:
		err = state.ErrFUSEPoolCASMismatch
	case poolResultConflict:
		err = state.ErrFUSEPoolConflict
	case poolResultCorrupt:
		err = state.ErrFUSEPoolCorrupt
	case poolResultInvalid:
		err = state.ErrFUSEPoolInvalidTransition
	case poolResultToken:
		err = state.ErrFUSEPoolTokenMismatch
	default:
		err = state.ErrFUSEPoolCorrupt
	}
	return fmt.Errorf("fuse pool %s: %w", operation, err)
}

func poolScriptResult(result []any) (int64, any, error) {
	code, payloads, err := poolScriptResults(result)
	if err != nil {
		return 0, nil, err
	}
	if len(payloads) == 0 {
		return code, nil, nil
	}
	return code, payloads[0], nil
}

func poolScriptResults(result []any) (int64, []any, error) {
	if len(result) == 0 {
		return 0, nil, state.ErrFUSEPoolCorrupt
	}
	code, ok := result[0].(int64)
	if !ok {
		return 0, nil, state.ErrFUSEPoolCorrupt
	}
	return code, result[1:], nil
}

func decodePoolRecord(value any) (*state.FUSEPoolRecord, error) {
	var raw []byte
	switch typed := value.(type) {
	case string:
		raw = []byte(typed)
	case []byte:
		raw = typed
	default:
		return nil, state.ErrFUSEPoolCorrupt
	}
	var record state.FUSEPoolRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, state.ErrFUSEPoolCorrupt
	}
	if record.RuntimeID == "" || record.RuntimeUID == "" || record.PoolKey == "" || !validFUSEPoolState(record.State) || record.Revision == 0 || record.UpdatedAt.IsZero() {
		return nil, state.ErrFUSEPoolCorrupt
	}
	return &record, nil
}
