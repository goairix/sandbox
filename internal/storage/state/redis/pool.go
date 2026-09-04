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
	fusePoolCounts       = "fusepool:pool-counts"
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

const fusePoolLuaHelpers = `
local function isValidState(value)
    return value == 'preparing' or value == 'prepared' or value == 'reserved'
        or value == 'binding' or value == 'consumed'
end

local function parseRFC3339Millis(value)
    if type(value) ~= 'string' then return nil end
    local length = string.len(value)
    if length < 20 or length > 30
        or string.sub(value, 5, 5) ~= '-'
        or string.sub(value, 8, 8) ~= '-'
        or string.sub(value, 11, 11) ~= 'T'
        or string.sub(value, 14, 14) ~= ':'
        or string.sub(value, 17, 17) ~= ':'
        or string.sub(value, length, length) ~= 'Z' then
        return nil
    end
    if not string.match(string.sub(value, 1, 19),
        '^%d%d%d%d%-%d%d%-%d%dT%d%d:%d%d:%d%d$') then
        return nil
    end
    local fraction = ''
    if length > 20 then
        if string.sub(value, 20, 20) ~= '.' then return nil end
        fraction = string.sub(value, 21, length - 1)
        if fraction == '' or string.len(fraction) > 9 or not string.match(fraction, '^%d+$') then
            return nil
        end
    elseif string.sub(value, 20, 20) ~= 'Z' then
        return nil
    end
    local year = tonumber(string.sub(value, 1, 4))
    local month = tonumber(string.sub(value, 6, 7))
    local day = tonumber(string.sub(value, 9, 10))
    local hour = tonumber(string.sub(value, 12, 13))
    local minute = tonumber(string.sub(value, 15, 16))
    local second = tonumber(string.sub(value, 18, 19))
    if not year or year < 1 or not month or month < 1 or month > 12
        or not day or day < 1 or not hour or hour < 0 or hour > 23
        or not minute or minute < 0 or minute > 59
        or not second or second < 0 or second > 59 then
        return nil
    end
    local monthDays = {31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
    if month == 2 and (year % 4 == 0 and (year % 100 ~= 0 or year % 400 == 0)) then
        monthDays[2] = 29
    end
    if day > monthDays[month] then return nil end
    local adjustedYear = year
    if month <= 2 then adjustedYear = adjustedYear - 1 end
    local era = math.floor(adjustedYear / 400)
    local yearOfEra = adjustedYear - era * 400
    local adjustedMonth
    if month > 2 then adjustedMonth = month - 3 else adjustedMonth = month + 9 end
    local dayOfYear = math.floor((153 * adjustedMonth + 2) / 5) + day - 1
    local dayOfEra = yearOfEra * 365 + math.floor(yearOfEra / 4) - math.floor(yearOfEra / 100) + dayOfYear
    local days = era * 146097 + dayOfEra - 719468
    local millisText = string.sub(fraction .. '000', 1, 3)
    local millis = tonumber(millisText) or 0
    return days * 86400000 + hour * 3600000 + minute * 60000 + second * 1000 + millis
end

local function formatRFC3339Millis(epochMillis)
    local epochSeconds = math.floor(epochMillis / 1000)
    local millis = epochMillis - epochSeconds * 1000
    local days = math.floor(epochSeconds / 86400)
    local secondsOfDay = epochSeconds - days * 86400
    local z = days + 719468
    local era = math.floor(z / 146097)
    local dayOfEra = z - era * 146097
    local yearOfEra = math.floor((dayOfEra - math.floor(dayOfEra / 1460)
        + math.floor(dayOfEra / 36524) - math.floor(dayOfEra / 146096)) / 365)
    local year = yearOfEra + era * 400
    local dayOfYear = dayOfEra - (365 * yearOfEra + math.floor(yearOfEra / 4) - math.floor(yearOfEra / 100))
    local monthPart = math.floor((5 * dayOfYear + 2) / 153)
    local day = dayOfYear - math.floor((153 * monthPart + 2) / 5) + 1
    local month
    if monthPart < 10 then month = monthPart + 3 else month = monthPart - 9 end
    if month <= 2 then year = year + 1 end
    local hour = math.floor(secondsOfDay / 3600)
    local minute = math.floor((secondsOfDay % 3600) / 60)
    local second = secondsOfDay % 60
    return string.format('%04d-%02d-%02dT%02d:%02d:%02d.%03dZ',
        year, month, day, hour, minute, second, millis)
end

local function validateRecord(record, deadlineValue)
    if type(record) ~= 'table'
        or type(record.runtime_id) ~= 'string' or record.runtime_id == ''
        or type(record.runtime_uid) ~= 'string' or record.runtime_uid == ''
        or type(record.pool_key) ~= 'string' or record.pool_key == ''
        or not isValidState(record.state)
        or type(record.maintainer_token) ~= 'string'
        or (record.reservation_token ~= nil and type(record.reservation_token) ~= 'string')
        or type(record.revision) ~= 'number' or record.revision < 1
        or record.revision ~= math.floor(record.revision) then
        return false
    end
    local updatedMillis = parseRFC3339Millis(record.updated_at)
    if not updatedMillis or updatedMillis == -62135596800000 then return false end
    local deadline = tonumber(deadlineValue)
    if not deadline or deadline ~= math.floor(deadline) then return false end
    local reservation = record.reservation_token or ''
    local reservedMillis = parseRFC3339Millis(record.reserved_until)
    local zeroReserved = record.reserved_until == '0001-01-01T00:00:00Z'
    if record.state == 'prepared' then
        return reservation == '' and zeroReserved and deadline == 0
    end
    if record.state == 'preparing' then
        if reservation == '' then return zeroReserved and deadline == 0 end
        return not zeroReserved and reservedMillis ~= nil and reservedMillis == deadline and deadline > 0
    end
    return reservation ~= '' and not zeroReserved and reservedMillis ~= nil
        and reservedMillis == deadline and deadline > 0
end

local function validatePoolCardinality(indexKey, countsKey, poolDigest)
    local cardinality = redis.call('SCARD', indexKey)
    local expectedRaw = redis.call('HGET', countsKey, poolDigest)
    if cardinality == 0 then return not expectedRaw end
    if not expectedRaw or not string.match(expectedRaw, '^%d+$') then return false end
    local expected = tonumber(expectedRaw)
    return expected and expected == math.floor(expected) and expected == cardinality
end
`

var createPreparingScript = redisclient.NewScript(fusePoolLuaHelpers + `
if redis.call('EXISTS', KEYS[1]) == 1 then
    return -3
end
if redis.call('HEXISTS', KEYS[3], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[4], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[5], ARGV[1]) == 1
    or redis.call('HEXISTS', KEYS[6], ARGV[1]) == 1 then
    return -4
end
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) == 1
    or not validatePoolCardinality(KEYS[2], KEYS[7], ARGV[2]) then
    return -4
end
redis.call('SET', KEYS[1], ARGV[3])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[4], ARGV[1], ARGV[4])
redis.call('HSET', KEYS[5], ARGV[1], ARGV[5])
redis.call('HSET', KEYS[6], ARGV[1], ARGV[6])
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('HINCRBY', KEYS[7], ARGV[2], 1)
return 1
`)

var reservePreparedScript = redisclient.NewScript(fusePoolLuaHelpers + `
if not validatePoolCardinality(KEYS[1], KEYS[6], ARGV[1]) then
    return {-4}
end
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
    local deadline = redis.call('HGET', KEYS[3], member)
    local recordKey = ARGV[5] .. member
    local raw = redis.call('GET', recordKey)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or not validateRecord(record, deadline)
        or not mappedUID
        or record.runtime_uid ~= mappedUID
        or record.pool_key ~= ARGV[2]
        or not mappedPoolValue
        or record.pool_key ~= mappedPoolValue
        then
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
    local redisTime = redis.call('TIME')
    local nowMillis = tonumber(redisTime[1]) * 1000 + math.floor(tonumber(redisTime[2]) / 1000)
    local deadlineMillis = nowMillis + tonumber(ARGV[4])
    candidate.state = 'reserved'
    candidate.reservation_token = ARGV[3]
    candidate.reserved_until = formatRFC3339Millis(deadlineMillis)
    candidate.updated_at = formatRFC3339Millis(nowMillis)
    candidate.revision = candidate.revision + 1
    local updated = cjson.encode(candidate)
    redis.call('SET', candidateKey, updated)
    redis.call('HSET', KEYS[3], candidateMember, deadlineMillis)
    return {1, updated}
end
return {0}
`)

var transitionScript = redisclient.NewScript(fusePoolLuaHelpers + `
local raw = redis.call('GET', KEYS[1])
if not raw then
    return {-1}
end
local decoded, record = pcall(cjson.decode, raw)
local deadline = redis.call('HGET', KEYS[2], ARGV[8])
if not decoded or not validateRecord(record, deadline) or record.runtime_uid ~= ARGV[1] then
    return {-4}
end
local mappedPoolDigest = redis.call('HGET', KEYS[3], ARGV[8])
local mappedUID = redis.call('HGET', KEYS[4], ARGV[8])
local mappedPoolValue = redis.call('HGET', KEYS[5], ARGV[8])
if not mappedPoolDigest or string.len(mappedPoolDigest) ~= 64
    or not string.match(mappedPoolDigest, '^[0-9a-f]+$')
    or mappedUID ~= record.runtime_uid
    or mappedPoolValue ~= record.pool_key
    or redis.call('SISMEMBER', ARGV[9] .. mappedPoolDigest, ARGV[8]) ~= 1
    or not validatePoolCardinality(ARGV[9] .. mappedPoolDigest, KEYS[6], mappedPoolDigest) then
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
local clearDeadline = false
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
    clearDeadline = true
else
    return {-5}
end

record.state = to
record.updated_at = ARGV[6]
record.revision = record.revision + 1
local updated = cjson.encode(record)
if clearDeadline then redis.call('HSET', KEYS[2], ARGV[8], '0') end
redis.call('SET', KEYS[1], updated)
return {1, updated}
`)

var listByPoolKeyScript = redisclient.NewScript(fusePoolLuaHelpers + `
if not validatePoolCardinality(KEYS[1], KEYS[6], ARGV[1]) then
    return {-4}
end
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
    local deadline = redis.call('HGET', KEYS[5], member)
    local raw = redis.call('GET', ARGV[3] .. member)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or not validateRecord(record, deadline)
        or record.runtime_uid ~= mappedUID
        or record.pool_key ~= ARGV[2]
        or record.pool_key ~= mappedPoolValue
        then
        return {-4}
    end
    table.insert(result, member)
    table.insert(result, raw)
end
return result
`)

var countPreparingAndPreparedScript = redisclient.NewScript(fusePoolLuaHelpers + `
if not validatePoolCardinality(KEYS[1], KEYS[6], ARGV[1]) then
    return {-4}
end
local members = redis.call('SMEMBERS', KEYS[1])
local result = {1}
for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[2], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[3], member)
    local mappedPoolValue = redis.call('HGET', KEYS[4], member)
    local deadline = redis.call('HGET', KEYS[5], member)
    local raw = redis.call('GET', ARGV[3] .. member)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or not validateRecord(record, deadline)
        or record.runtime_uid ~= mappedUID
        or record.pool_key ~= ARGV[2]
        or record.pool_key ~= mappedPoolValue
        then
        return {-4}
    end
    table.insert(result, member)
    table.insert(result, raw)
end
return result
`)

var conditionalDeleteScript = redisclient.NewScript(fusePoolLuaHelpers + `
local raw = redis.call('GET', KEYS[1])
if not raw then
    return -1
end
local decoded, record = pcall(cjson.decode, raw)
local deadline = redis.call('HGET', KEYS[3], ARGV[6])
if not decoded or not validateRecord(record, deadline) or record.runtime_uid ~= ARGV[1] then
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
    or redis.call('SISMEMBER', ARGV[7] .. poolDigest, ARGV[6]) ~= 1
    or not validatePoolCardinality(ARGV[7] .. poolDigest, KEYS[6], poolDigest) then
    return -4
end
redis.call('DEL', KEYS[1])
redis.call('SREM', ARGV[7] .. poolDigest, ARGV[6])
redis.call('HDEL', KEYS[2], ARGV[6])
redis.call('HDEL', KEYS[3], ARGV[6])
redis.call('HDEL', KEYS[4], ARGV[6])
redis.call('HDEL', KEYS[5], ARGV[6])
local remaining = redis.call('HINCRBY', KEYS[6], poolDigest, -1)
if remaining == 0 then redis.call('HDEL', KEYS[6], poolDigest) end
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
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts},
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
	ttlMillis, err := redisTTLMilliseconds(ttl)
	if err != nil {
		return nil, err
	}
	result, err := reservePreparedScript.Run(ctx, r.store.client,
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts},
		poolDigest, poolKey, token, ttlMillis, fusePoolRecordPrefix,
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
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolDeadlines, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts},
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
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolDeadlines, fusePoolCounts},
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
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolDeadlines, fusePoolCounts},
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
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts},
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
	switch record.State {
	case state.FUSEPoolPreparing:
		if (record.ReservationToken == "") != record.ReservedUntil.IsZero() {
			return nil, state.ErrFUSEPoolCorrupt
		}
	case state.FUSEPoolPrepared:
		if record.ReservationToken != "" || !record.ReservedUntil.IsZero() {
			return nil, state.ErrFUSEPoolCorrupt
		}
	case state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed:
		if record.ReservationToken == "" || record.ReservedUntil.IsZero() {
			return nil, state.ErrFUSEPoolCorrupt
		}
	default:
		return nil, state.ErrFUSEPoolCorrupt
	}
	return &record, nil
}
