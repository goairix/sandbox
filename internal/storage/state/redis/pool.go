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
	fusePoolVersions     = "fusepool:pool-versions"
	fusePoolStatePrefix  = "fusepool:state:"
	fusePoolStateCounts  = "fusepool:state-counts:"
)

var fusePoolStates = [...]state.FUSEPoolState{
	state.FUSEPoolPreparing,
	state.FUSEPoolPrepared,
	state.FUSEPoolReserved,
	state.FUSEPoolBinding,
	state.FUSEPoolConsumed,
}

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

local poolStates = {'preparing', 'prepared', 'reserved', 'binding', 'consumed'}

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
        or type(record.maintainer_token) ~= 'string' or record.maintainer_token == ''
        or (record.reservation_token ~= nil and type(record.reservation_token) ~= 'string')
        or type(record.revision) ~= 'number' or record.revision < 1
        or record.revision ~= math.floor(record.revision)
        or record.revision > 99999999999999 then
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

local function validateMutableRecord(record, deadlineValue)
    -- Redis cjson preserves at most 14 significant digits. Keep the result of
    -- revision+1 within that exact, non-exponent integer range for Go uint64.
    return validateRecord(record, deadlineValue) and record.revision <= 99999999999998
end

local function validatePoolCardinality(indexKey, countsKey, poolDigest)
    local cardinality = redis.call('SCARD', indexKey)
    local expectedRaw = redis.call('HGET', countsKey, poolDigest)
    if cardinality == 0 then return not expectedRaw end
    if not expectedRaw or not string.match(expectedRaw, '^%d+$') then return false end
    local expected = tonumber(expectedRaw)
    return expected and expected == math.floor(expected) and expected == cardinality
end

local function validatePoolInventory(poolDigest, indexPrefix, countsKey, statePrefix, stateCountsPrefix)
    local totalKey = indexPrefix .. poolDigest
    if not validatePoolCardinality(totalKey, countsKey, poolDigest) then return false end
    local total = redis.call('SCARD', totalKey)
    local stateTotal = 0
    for _, stateName in ipairs(poolStates) do
        local stateKey = statePrefix .. stateName .. ':' .. poolDigest
        if not validatePoolCardinality(stateKey, stateCountsPrefix .. stateName, poolDigest) then
            return false
        end
        stateTotal = stateTotal + redis.call('SCARD', stateKey)
    end
    return stateTotal == total
end

local function validatePoolVersion(versionsKey, poolDigest, total)
    local raw = redis.call('HGET', versionsKey, poolDigest)
    if not raw then return total == 0, 0 end
    if not raw or not string.match(raw, '^[1-9]%d*$') then return false, 0 end
    local version = tonumber(raw)
    -- Keep version comparisons exact in Redis Lua doubles.
    return version and version == math.floor(version) and version <= 9007199254740991, version
end

local function validateMutablePoolVersion(versionsKey, poolDigest, total)
    local valid, version = validatePoolVersion(versionsKey, poolDigest, total)
    -- Leave room for the mutation's final HINCRBY. Every mutation calls this
    -- before any write, while reads accept the last exact integer.
    return valid and version <= 9007199254740990
end

local function recordIsInExactlyState(member, poolDigest, expectedState, statePrefix)
    for _, stateName in ipairs(poolStates) do
        local present = redis.call('SISMEMBER', statePrefix .. stateName .. ':' .. poolDigest, member)
        if present ~= (stateName == expectedState and 1 or 0) then return false end
    end
    return true
end

local function setCount(countsKey, poolDigest, delta)
    local value = redis.call('HINCRBY', countsKey, poolDigest, delta)
    if value == 0 then redis.call('HDEL', countsKey, poolDigest) end
end
`

var createPreparingScript = redisclient.NewScript(fusePoolLuaHelpers + `
local decoded, record = pcall(cjson.decode, ARGV[3])
if not decoded or type(record) ~= 'table'
    or type(record.runtime_id) ~= 'string' or record.runtime_id == ''
    or type(record.runtime_uid) ~= 'string' or record.runtime_uid ~= ARGV[5]
    or type(record.pool_key) ~= 'string' or record.pool_key ~= ARGV[6]
    or record.state ~= 'preparing'
    or type(record.maintainer_token) ~= 'string' or record.maintainer_token == ''
    or (record.reservation_token ~= nil and type(record.reservation_token) ~= 'string')
    or type(record.revision) ~= 'number' or record.revision ~= 1
    or not parseRFC3339Millis(record.updated_at)
    or parseRFC3339Millis(record.updated_at) == -62135596800000 then
    return -4
end
local redisTime = redis.call('TIME')
local nowMillis = tonumber(redisTime[1]) * 1000 + math.floor(tonumber(redisTime[2]) / 1000)
local ttlMillis = tonumber(ARGV[4])
local reservation = record.reservation_token or ''
local zeroReserved = record.reserved_until == '0001-01-01T00:00:00Z'
local deadlineMillis = 0
if reservation == '' then
    if not zeroReserved or ttlMillis ~= 0 then return -4 end
else
    local suppliedDeadline = parseRFC3339Millis(record.reserved_until)
    if not ttlMillis or ttlMillis <= 0 or ttlMillis ~= math.floor(ttlMillis)
        or not suppliedDeadline or suppliedDeadline <= 0 then
        return -4
    end
    -- The positive TTL proves the deadline was live at API admission. The
    -- absolute JSON value is never compared with Redis time because queueing
    -- or host clock skew must not shorten/reject the reservation.
    deadlineMillis = nowMillis + ttlMillis
    record.reserved_until = formatRFC3339Millis(deadlineMillis)
end
record.updated_at = formatRFC3339Millis(nowMillis)
local encoded = cjson.encode(record)
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
    or redis.call('SISMEMBER', KEYS[8], ARGV[1]) == 1
    or not validatePoolInventory(ARGV[2], 'fusepool:index:', KEYS[7], 'fusepool:state:', 'fusepool:state-counts:') then
    return -4
end
local versionOK = validateMutablePoolVersion(KEYS[10], ARGV[2], redis.call('SCARD', KEYS[2]))
if not versionOK then return -4 end
redis.call('SET', KEYS[1], encoded)
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[4], ARGV[1], deadlineMillis)
redis.call('HSET', KEYS[5], ARGV[1], ARGV[5])
redis.call('HSET', KEYS[6], ARGV[1], ARGV[6])
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[8], ARGV[1])
setCount(KEYS[7], ARGV[2], 1)
setCount(KEYS[9], ARGV[2], 1)
redis.call('HINCRBY', KEYS[10], ARGV[2], 1)
return 1
`)

var reservePreparedScript = redisclient.NewScript(fusePoolLuaHelpers + `
local reserveTTL = tonumber(ARGV[4])
if not reserveTTL or reserveTTL <= 0 or reserveTTL ~= math.floor(reserveTTL) then return {-5} end
if not validatePoolInventory(ARGV[1], ARGV[6], KEYS[2], ARGV[7], ARGV[8]) then
    return {-4}
end
local versionOK = validateMutablePoolVersion(KEYS[17], ARGV[1], redis.call('SCARD', KEYS[1]))
if not versionOK then return {-4} end
local members = redis.call('SMEMBERS', KEYS[5])
table.sort(members)
local candidateMember = nil
local candidateKey = nil
local candidate = nil
for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[13], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[15], member)
    local mappedPoolValue = redis.call('HGET', KEYS[16], member)
    local deadline = redis.call('HGET', KEYS[14], member)
    local recordKey = ARGV[5] .. member
    local raw = redis.call('GET', recordKey)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or not validateMutableRecord(record, deadline) or record.state ~= 'prepared'
        or not mappedUID
        or record.runtime_uid ~= mappedUID
        or record.pool_key ~= ARGV[2]
        or not mappedPoolValue
        or record.pool_key ~= mappedPoolValue
        or redis.call('SISMEMBER', KEYS[1], member) ~= 1
        or not recordIsInExactlyState(member, ARGV[1], 'prepared', ARGV[7])
        then
        return {-4}
    end
    if not candidate then
        candidateMember = member
        candidateKey = recordKey
        candidate = record
    end
end
if candidate then
    local redisTime = redis.call('TIME')
    local nowMillis = tonumber(redisTime[1]) * 1000 + math.floor(tonumber(redisTime[2]) / 1000)
    local deadlineMillis = nowMillis + reserveTTL
    candidate.state = 'reserved'
    candidate.reservation_token = ARGV[3]
    candidate.reserved_until = formatRFC3339Millis(deadlineMillis)
    candidate.updated_at = formatRFC3339Millis(nowMillis)
    candidate.revision = candidate.revision + 1
    local updated = cjson.encode(candidate)
    redis.call('SET', candidateKey, updated)
    redis.call('HSET', KEYS[14], candidateMember, deadlineMillis)
    redis.call('SREM', KEYS[5], candidateMember)
    redis.call('SADD', KEYS[7], candidateMember)
    setCount(KEYS[6], ARGV[1], -1)
    setCount(KEYS[8], ARGV[1], 1)
    redis.call('HINCRBY', KEYS[17], ARGV[1], 1)
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
if not decoded or not validateMutableRecord(record, deadline) or record.runtime_uid ~= ARGV[1] then
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
    or not validatePoolInventory(mappedPoolDigest, ARGV[9], KEYS[6], ARGV[10], ARGV[11])
    or not recordIsInExactlyState(ARGV[8], mappedPoolDigest, record.state, ARGV[10]) then
    return {-4}
end
local versionOK = validateMutablePoolVersion(KEYS[7], mappedPoolDigest, redis.call('SCARD', ARGV[9] .. mappedPoolDigest))
if not versionOK then return {-4} end
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
local redisTime = redis.call('TIME')
local nowMillis = tonumber(redisTime[1]) * 1000 + math.floor(tonumber(redisTime[2]) / 1000)
local function reservationIsLive()
    local deadline = tonumber(redis.call('HGET', KEYS[2], ARGV[8]))
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
    record.reserved_until = '0001-01-01T00:00:00Z'
    clearDeadline = true
else
    return {-5}
end

record.state = to
record.updated_at = formatRFC3339Millis(nowMillis)
record.revision = record.revision + 1
local updated = cjson.encode(record)
if clearDeadline then redis.call('HSET', KEYS[2], ARGV[8], '0') end
redis.call('SET', KEYS[1], updated)
redis.call('SREM', ARGV[10] .. from .. ':' .. mappedPoolDigest, ARGV[8])
redis.call('SADD', ARGV[10] .. to .. ':' .. mappedPoolDigest, ARGV[8])
setCount(ARGV[11] .. from, mappedPoolDigest, -1)
setCount(ARGV[11] .. to, mappedPoolDigest, 1)
redis.call('HINCRBY', KEYS[7], mappedPoolDigest, 1)
return {1, updated}
`)

var listSnapshotScript = redisclient.NewScript(fusePoolLuaHelpers + `
if not validatePoolInventory(ARGV[1], ARGV[2], KEYS[2], ARGV[3], ARGV[4]) then
    return {-4}
end
local total = redis.call('SCARD', KEYS[1])
local ok, version = validatePoolVersion(KEYS[3], ARGV[1], total)
if not ok then return {-4} end
return {1, version, total}
`)

var listBatchScript = redisclient.NewScript(fusePoolLuaHelpers + `
local result = {1}
for i = 5, #ARGV do
    local member = ARGV[i]
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
        or redis.call('SISMEMBER', KEYS[1], member) ~= 1
        or not recordIsInExactlyState(member, ARGV[1], record.state, ARGV[4])
        then
        return {-4}
    end
    table.insert(result, member)
    table.insert(result, raw)
end
return result
`)

var countPreparingAndPreparedScript = redisclient.NewScript(fusePoolLuaHelpers + `
if not validatePoolInventory(ARGV[1], ARGV[4], KEYS[2], ARGV[5], ARGV[6]) then
    return {-4}
end
local versionOK = validatePoolVersion(KEYS[17], ARGV[1], redis.call('SCARD', KEYS[1]))
if not versionOK then return {-4} end
local count = 0
for _, expectedState in ipairs({'preparing', 'prepared'}) do
    local stateKey
    if expectedState == 'preparing' then stateKey = KEYS[3] else stateKey = KEYS[5] end
    local members = redis.call('SMEMBERS', stateKey)
    count = count + #members
    for _, member in ipairs(members) do
    local mapped = redis.call('HGET', KEYS[13], member)
    if not mapped or mapped ~= ARGV[1] then
        return {-4}
    end
    local mappedUID = redis.call('HGET', KEYS[15], member)
    local mappedPoolValue = redis.call('HGET', KEYS[16], member)
    local deadline = redis.call('HGET', KEYS[14], member)
    local raw = redis.call('GET', ARGV[3] .. member)
    if not raw then
        return {-4}
    end
    local decoded, record = pcall(cjson.decode, raw)
    if not decoded or not validateRecord(record, deadline) or record.state ~= expectedState
        or record.runtime_uid ~= mappedUID
        or record.pool_key ~= ARGV[2]
        or record.pool_key ~= mappedPoolValue
        or redis.call('SISMEMBER', KEYS[1], member) ~= 1
        or not recordIsInExactlyState(member, ARGV[1], expectedState, ARGV[5])
        then
        return {-4}
    end
    end
end
return {1, count}
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
    or not validatePoolInventory(poolDigest, ARGV[7], KEYS[6], ARGV[8], ARGV[9])
    or not recordIsInExactlyState(ARGV[6], poolDigest, record.state, ARGV[8]) then
    return -4
end
local versionOK = validateMutablePoolVersion(KEYS[7], poolDigest, redis.call('SCARD', ARGV[7] .. poolDigest))
if not versionOK then return -4 end
redis.call('DEL', KEYS[1])
redis.call('SREM', ARGV[7] .. poolDigest, ARGV[6])
redis.call('SREM', ARGV[8] .. record.state .. ':' .. poolDigest, ARGV[6])
redis.call('HDEL', KEYS[2], ARGV[6])
redis.call('HDEL', KEYS[3], ARGV[6])
redis.call('HDEL', KEYS[4], ARGV[6])
redis.call('HDEL', KEYS[5], ARGV[6])
setCount(KEYS[6], poolDigest, -1)
setCount(ARGV[9] .. record.state, poolDigest, -1)
redis.call('HINCRBY', KEYS[7], poolDigest, 1)
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

// FUSEPoolRepository uses atomic multi-key Lua transactions. The configured
// deployment is standalone Redis; its keys are intentionally not cluster-slot
// tagged and this repository must not be used against Redis Cluster.
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
	ttlMillis := int64(0)
	if record.ReservationToken != "" {
		// ReservedUntil is an API-supplied duration carrier only. Redis TIME is
		// authoritative: Lua replaces the absolute value with server-now + TTL.
		ttl := time.Until(record.ReservedUntil)
		if ttl <= 0 {
			return state.ErrFUSEPoolInvalidRecord
		}
		var err error
		ttlMillis, err = redisTTLMilliseconds(ttl)
		if err != nil || ttlMillis <= 0 {
			return state.ErrFUSEPoolInvalidRecord
		}
		record.ReservedUntil = record.ReservedUntil.UTC()
	}
	// Lua validates this placeholder and replaces it with Redis server time.
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode fuse pool record: %w", state.ErrFUSEPoolInvalidRecord)
	}
	uidDigest := fusePoolDigest(record.RuntimeUID)
	poolDigest := fusePoolDigest(record.PoolKey)
	result, err := createPreparingScript.Run(ctx, r.store.client,
		[]string{
			fusePoolRecordPrefix + uidDigest,
			fusePoolIndexPrefix + poolDigest,
			fusePoolRecordPools,
			fusePoolDeadlines,
			fusePoolRecordUIDs,
			fusePoolPoolValues,
			fusePoolCounts,
			fusePoolStateIndexKey(poolDigest, state.FUSEPoolPreparing),
			fusePoolStateCountsKey(state.FUSEPoolPreparing),
			fusePoolVersions,
		},
		uidDigest, poolDigest, raw, ttlMillis, record.RuntimeUID, record.PoolKey,
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
		fusePoolInventoryKeys(poolDigest),
		poolDigest, poolKey, token, ttlMillis, fusePoolRecordPrefix, fusePoolIndexPrefix, fusePoolStatePrefix, fusePoolStateCounts,
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
	uidDigest := fusePoolDigest(runtimeUID)
	result, err := transitionScript.Run(ctx, r.store.client,
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolDeadlines, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts, fusePoolVersions},
		runtimeUID, string(from), string(to), token, strconv.FormatUint(expectedRevision, 10), "", "", uidDigest, fusePoolIndexPrefix, fusePoolStatePrefix, fusePoolStateCounts,
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
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		beforeVersion, beforeTotal, err := r.poolListSnapshot(ctx, poolDigest)
		if err != nil {
			return nil, err
		}
		rawByMember := make(map[string]any, beforeTotal)
		cursor := uint64(0)
		retry := false
		for {
			members, next, scanErr := r.store.client.SScan(ctx, fusePoolIndexPrefix+poolDigest, cursor, "", 64).Result()
			if scanErr != nil {
				return nil, scanErr
			}
			if len(members) > 0 {
				args := make([]any, 0, len(members)+4)
				args = append(args, poolDigest, poolKey, fusePoolRecordPrefix, fusePoolStatePrefix)
				for _, member := range members {
					args = append(args, member)
				}
				result, batchErr := listBatchScript.Run(ctx, r.store.client,
					[]string{fusePoolIndexPrefix + poolDigest, fusePoolRecordPools, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolDeadlines}, args...).Slice()
				if batchErr != nil {
					return nil, batchErr
				}
				code, payloads, parseErr := poolScriptResults(result)
				if parseErr != nil {
					return nil, parseErr
				}
				if code != poolResultOK {
					afterVersion, _, snapshotErr := r.poolListSnapshot(ctx, poolDigest)
					if snapshotErr == nil && afterVersion != beforeVersion {
						retry = true
						break
					}
					return nil, poolMutationError("list records", code)
				}
				if len(payloads)%2 != 0 {
					return nil, state.ErrFUSEPoolCorrupt
				}
				for i := 0; i < len(payloads); i += 2 {
					member, ok := payloads[i].(string)
					if !ok {
						return nil, state.ErrFUSEPoolCorrupt
					}
					rawByMember[member] = payloads[i+1]
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if retry {
			continue
		}
		afterVersion, afterTotal, err := r.poolListSnapshot(ctx, poolDigest)
		if err != nil {
			return nil, err
		}
		if beforeVersion != afterVersion || beforeTotal != afterTotal {
			continue
		}
		if len(rawByMember) != beforeTotal {
			return nil, state.ErrFUSEPoolCorrupt
		}
		records := make([]state.FUSEPoolRecord, 0, len(rawByMember))
		for member, raw := range rawByMember {
			record, decodeErr := decodePoolRecord(raw)
			if decodeErr != nil || record.PoolKey != poolKey || fusePoolDigest(record.RuntimeUID) != member {
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
	return nil, state.ErrFUSEPoolConflict
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
		fusePoolInventoryKeys(poolDigest),
		poolDigest, poolKey, fusePoolRecordPrefix, fusePoolIndexPrefix, fusePoolStatePrefix, fusePoolStateCounts,
	).Slice()
	if err != nil {
		return 0, err
	}
	code, payload, err := poolScriptResult(result)
	if err != nil {
		return 0, err
	}
	if code != poolResultOK {
		return 0, poolMutationError("count records", code)
	}
	count, ok := payload.(int64)
	if !ok || count < 0 {
		return 0, state.ErrFUSEPoolCorrupt
	}
	return int(count), nil
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
		[]string{fusePoolRecordPrefix + uidDigest, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolCounts, fusePoolVersions},
		runtimeUID, string(expectedState), maintainerToken, reservationToken, strconv.FormatUint(expectedRevision, 10), uidDigest, fusePoolIndexPrefix, fusePoolStatePrefix, fusePoolStateCounts,
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

func fusePoolStateIndexKey(poolDigest string, poolState state.FUSEPoolState) string {
	return fusePoolStatePrefix + string(poolState) + ":" + poolDigest
}

func fusePoolStateCountsKey(poolState state.FUSEPoolState) string {
	return fusePoolStateCounts + string(poolState)
}

func fusePoolInventoryKeys(poolDigest string) []string {
	keys := []string{fusePoolIndexPrefix + poolDigest, fusePoolCounts}
	for _, poolState := range fusePoolStates {
		keys = append(keys, fusePoolStateIndexKey(poolDigest, poolState), fusePoolStateCountsKey(poolState))
	}
	return append(keys, fusePoolRecordPools, fusePoolDeadlines, fusePoolRecordUIDs, fusePoolPoolValues, fusePoolVersions)
}

func (r *FUSEPoolRepository) poolListSnapshot(ctx context.Context, poolDigest string) (int64, int, error) {
	result, err := listSnapshotScript.Run(ctx, r.store.client,
		[]string{fusePoolIndexPrefix + poolDigest, fusePoolCounts, fusePoolVersions},
		poolDigest, fusePoolIndexPrefix, fusePoolStatePrefix, fusePoolStateCounts,
	).Slice()
	if err != nil {
		return 0, 0, err
	}
	code, payloads, err := poolScriptResults(result)
	if err != nil {
		return 0, 0, err
	}
	if code != poolResultOK {
		return 0, 0, poolMutationError("snapshot records", code)
	}
	if len(payloads) != 2 {
		return 0, 0, state.ErrFUSEPoolCorrupt
	}
	version, versionOK := payloads[0].(int64)
	total, totalOK := payloads[1].(int64)
	if !versionOK || !totalOK || version < 0 || total < 0 {
		return 0, 0, state.ErrFUSEPoolCorrupt
	}
	return version, int(total), nil
}

func validatePreparingRecord(record state.FUSEPoolRecord) error {
	if record.RuntimeID == "" || record.RuntimeUID == "" || record.PoolKey == "" || record.MaintainerToken == "" || record.State != state.FUSEPoolPreparing || record.Revision != 1 {
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
	if record.RuntimeID == "" || record.RuntimeUID == "" || record.PoolKey == "" || record.MaintainerToken == "" || !validFUSEPoolState(record.State) || record.Revision == 0 || record.Revision > 99999999999999 || record.UpdatedAt.IsZero() {
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
