package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	redislib "github.com/redis/go-redis/v9"

	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	activeSandboxKeyPrefix = "sandbox:active:v1:"
	activeSandboxBuckets   = 16
)

var activeSandboxIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

var publishActiveSandboxScript = redislib.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SADD', KEYS[2], ARGV[2])
return 1
`)

var changeActiveRecordScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if tonumber(record.revision) ~= tonumber(ARGV[1]) then return {2, raw} end
if record.phase ~= ARGV[2] then return {3, raw} end
record.phase = ARGV[3]
record.revision = tonumber(record.revision) + 1
record.snapshot = cjson.decode(ARGV[4])
record.updated_at = ARGV[5]
local updated = cjson.encode(record)
redis.call('SET', KEYS[1], updated)
return {1, updated}
`)

var beginActiveOperationScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if record.phase ~= 'active' then return {2, raw} end
if ARGV[3] == 'mutation' then
  local holder = redis.call('GET', KEYS[3])
  if holder then return {3, raw} end
  redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[2])
end
local expires = nowms + tonumber(ARGV[2])
redis.call('ZADD', KEYS[2], expires, ARGV[1])
return {1, raw, tostring(expires)}
`)

var renewActiveOperationScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if tonumber(record.generation) ~= tonumber(ARGV[2]) then return {2} end
local score = redis.call('ZSCORE', KEYS[2], ARGV[1])
if not score then return {2} end
if ARGV[4] == 'mutation' then
  local holder = redis.call('GET', KEYS[3])
  if holder ~= ARGV[1] then return {2} end
end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
if tonumber(score) <= nowms then
  redis.call('ZREM', KEYS[2], ARGV[1])
  if ARGV[4] == 'mutation' and redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
  return {2}
end
local expires = nowms + tonumber(ARGV[3])
redis.call('ZADD', KEYS[2], expires, ARGV[1])
if ARGV[4] == 'mutation' then redis.call('PEXPIRE', KEYS[3], ARGV[3]) end
return {1, tostring(expires)}
`)

var endActiveOperationScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local record = cjson.decode(raw)
if tonumber(record.generation) ~= tonumber(ARGV[2]) then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
if ARGV[3] == 'mutation' and redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
return 1
`)

var beginActiveDestroyScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
local won = 0
if record.phase == 'active' or record.phase == 'publishing' then
  record.phase = 'destroying'
  record.revision = tonumber(record.revision) + 1
  record.updated_at = ARGV[1]
  raw = cjson.encode(record)
  redis.call('SET', KEYS[1], raw)
  won = 1
elseif record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending' then
  return {2, raw}
end
return {1, raw, tostring(redis.call('ZCARD', KEYS[2])), tostring(won)}
`)

var liveActiveOperationsScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', nowms)
return redis.call('ZCARD', KEYS[1])
`)

var checkpointActiveSandboxScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if tonumber(record.revision) ~= tonumber(ARGV[1]) then return {2, raw} end
if record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending' then return {3, raw} end
record.phase = 'cleanup_pending'
record.revision = tonumber(record.revision) + 1
record.cleanup_checkpoint = ARGV[2]
record.updated_at = ARGV[3]
local updated = cjson.encode(record)
redis.call('SET', KEYS[1], updated)
return {1, updated}
`)

var deleteActiveSandboxScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return 1 end
local record = cjson.decode(raw)
if tonumber(record.revision) ~= tonumber(ARGV[1]) or tonumber(record.generation) ~= tonumber(ARGV[2]) then return 0 end
if record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending' then return 0 end
if redis.call('ZCARD', KEYS[2]) ~= 0 then return 0 end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
redis.call('SREM', KEYS[5], ARGV[3])
return 1
`)

var acquireActiveControllerScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if (record.phase ~= 'publishing' and record.phase ~= 'active' and record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending') or tonumber(record.generation) ~= tonumber(ARGV[2]) then return {2} end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local lease = cjson.decode(ARGV[1])
lease.expires_at_unix_ms = nowms + tonumber(ARGV[3])
local encoded = cjson.encode(lease)
if not redis.call('SET', KEYS[2], encoded, 'PX', ARGV[3], 'NX') then return {3} end
return {1, tostring(lease.expires_at_unix_ms)}
`)

var renewActiveControllerScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if (record.phase ~= 'publishing' and record.phase ~= 'active' and record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending') or tonumber(record.generation) ~= tonumber(ARGV[2]) then return {2} end
local current = redis.call('GET', KEYS[2])
if not current then return {2} end
local lease = cjson.decode(current)
if lease.token ~= ARGV[1] or tonumber(lease.generation) ~= tonumber(ARGV[2]) then return {2} end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
lease.expires_at_unix_ms = nowms + tonumber(ARGV[3])
local encoded = cjson.encode(lease)
redis.call('SET', KEYS[2], encoded, 'PX', ARGV[3])
return {1, tostring(lease.expires_at_unix_ms)}
`)

var releaseActiveControllerScript = redislib.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 1 end
local lease = cjson.decode(current)
if lease.token ~= ARGV[1] or tonumber(lease.generation) ~= tonumber(ARGV[2]) then return 0 end
redis.call('DEL', KEYS[1])
return 1
`)

type activeSandboxKeys struct {
	record, operations, mutation, controller, index string
}

type ActiveSandboxRepository struct {
	store       *Store
	scopeDigest string
}

func NewActiveSandboxRepository(store *Store, scope string) (*ActiveSandboxRepository, error) {
	if store == nil || strings.TrimSpace(scope) == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	sum := sha256.Sum256([]byte(scope))
	return &ActiveSandboxRepository{store: store, scopeDigest: hex.EncodeToString(sum[:16])}, nil
}

func (r *ActiveSandboxRepository) keys(id string) activeSandboxKeys {
	bucket := activeSandboxBucket(id)
	tag := fmt.Sprintf("{%s:%02x}", r.scopeDigest, bucket)
	base := activeSandboxKeyPrefix + tag + ":" + id
	return activeSandboxKeys{
		record: base + ":record", operations: base + ":operations",
		mutation: base + ":mutation", controller: base + ":controller",
		index: activeSandboxKeyPrefix + tag + ":index",
	}
}

func activeSandboxBucket(id string) uint8 {
	sum := sha256.Sum256([]byte(id))
	return sum[0] % activeSandboxBuckets
}

func (r *ActiveSandboxRepository) validateID(id string) error {
	if !activeSandboxIDPattern.MatchString(id) {
		return state.ErrActiveSandboxCorrupt
	}
	return nil
}

func (r *ActiveSandboxRepository) Publish(ctx context.Context, record state.ActiveSandboxRecord) error {
	if err := record.Validate(); err != nil || record.Phase != state.ActiveSandboxPublishing || r.validateID(record.SandboxID) != nil {
		return state.ErrActiveSandboxCorrupt
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	k := r.keys(record.SandboxID)
	ok, err := publishActiveSandboxScript.Run(ctx, r.store.client, []string{k.record, k.index}, raw, record.SandboxID).Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxConflict
	}
	return r.store.acknowledgeSafetyWrite(ctx)
}

func (r *ActiveSandboxRepository) Load(ctx context.Context, id string) (*state.ActiveSandboxRecord, error) {
	if err := r.validateID(id); err != nil {
		return nil, err
	}
	raw, err := r.store.Get(ctx, r.keys(id).record)
	if err != nil || raw == nil {
		return nil, err
	}
	return decodeActiveRecord(raw, id)
}

func decodeActiveRecord(raw []byte, id string) (*state.ActiveSandboxRecord, error) {
	var record state.ActiveSandboxRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.SandboxID != id {
		return nil, state.ErrActiveSandboxCorrupt
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *ActiveSandboxRepository) Activate(ctx context.Context, id string, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	return r.changeRecord(ctx, id, revision, state.ActiveSandboxPublishing, state.ActiveSandboxActive, snapshot)
}

func (r *ActiveSandboxRepository) Update(ctx context.Context, id string, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	return r.changeRecord(ctx, id, revision, state.ActiveSandboxActive, state.ActiveSandboxActive, snapshot)
}

func (r *ActiveSandboxRepository) changeRecord(ctx context.Context, id string, revision uint64, from, to state.ActiveSandboxPhase, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	if r.validateID(id) != nil || revision == 0 || !json.Valid(snapshot) {
		return nil, state.ErrActiveSandboxCorrupt
	}
	result, err := changeActiveRecordScript.Run(ctx, r.store.client, []string{r.keys(id).record}, revision, string(from), string(to), string(snapshot), time.Now().UTC().Format(time.RFC3339Nano)).Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status != 1 {
		if status == 0 {
			return nil, nil
		}
		return nil, state.ErrActiveSandboxConflict
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), id)
	if err != nil {
		return nil, err
	}
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return record, err
	}
	return record, nil
}

func (r *ActiveSandboxRepository) BeginOperation(ctx context.Context, id, token string, kind state.ActiveOperationKind, ttl time.Duration) (*state.ActiveSandboxRecord, *state.ActiveSandboxOperation, error) {
	if r.validateID(id) != nil || token == "" || (kind != state.ActiveOperationData && kind != state.ActiveOperationMutation) || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	result, err := beginActiveOperationScript.Run(ctx, r.store.client, []string{k.record, k.operations, k.mutation}, token, ttl.Milliseconds(), string(kind)).Slice()
	if err != nil {
		return nil, nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, nil, err
	}
	switch status {
	case 0:
		return nil, nil, nil
	case 2:
		return nil, nil, state.ErrActiveSandboxAdmissionClosed
	case 3:
		return nil, nil, state.ErrActiveSandboxConflict
	case 1:
	default:
		return nil, nil, state.ErrActiveSandboxCorrupt
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), id)
	if err != nil {
		return nil, nil, err
	}
	expires, err := scriptInt(result, 2)
	if err != nil {
		return nil, nil, err
	}
	op := &state.ActiveSandboxOperation{SandboxID: id, Token: token, Generation: record.Generation, Kind: kind, ExpiresAt: time.UnixMilli(expires)}
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return record, op, err
	}
	return record, op, nil
}

func (r *ActiveSandboxRepository) RenewOperation(ctx context.Context, op state.ActiveSandboxOperation, ttl time.Duration) (*state.ActiveSandboxOperation, error) {
	if r.validateID(op.SandboxID) != nil || op.Token == "" || op.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(op.SandboxID)
	result, err := renewActiveOperationScript.Run(ctx, r.store.client, []string{k.record, k.operations, k.mutation}, op.Token, op.Generation, ttl.Milliseconds(), string(op.Kind)).Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status != 1 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	expires, err := scriptInt(result, 1)
	if err != nil {
		return nil, err
	}
	op.ExpiresAt = time.UnixMilli(expires)
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return &op, err
	}
	return &op, nil
}

func (r *ActiveSandboxRepository) EndOperation(ctx context.Context, op state.ActiveSandboxOperation) error {
	if r.validateID(op.SandboxID) != nil || op.Token == "" || op.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	k := r.keys(op.SandboxID)
	ok, err := endActiveOperationScript.Run(ctx, r.store.client, []string{k.record, k.operations, k.mutation}, op.Token, op.Generation, string(op.Kind)).Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxStaleToken
	}
	return nil
}

func (r *ActiveSandboxRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	if r.validateID(id) != nil {
		return nil, 0, false, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	result, err := beginActiveDestroyScript.Run(ctx, r.store.client, []string{k.record, k.operations}, time.Now().UTC().Format(time.RFC3339Nano)).Slice()
	if err != nil {
		return nil, 0, false, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, 0, false, err
	}
	if status == 0 {
		return nil, 0, false, nil
	}
	if status != 1 {
		return nil, 0, false, state.ErrActiveSandboxConflict
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), id)
	if err != nil {
		return nil, 0, false, err
	}
	live, err := scriptInt(result, 2)
	if err != nil {
		return nil, 0, false, err
	}
	won, err := scriptInt(result, 3)
	if err != nil {
		return nil, 0, false, err
	}
	if won == 1 {
		if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
			return record, live, true, err
		}
	}
	return record, live, won == 1, nil
}

func (r *ActiveSandboxRepository) LiveOperations(ctx context.Context, id string) (int64, error) {
	if r.validateID(id) != nil {
		return 0, state.ErrActiveSandboxCorrupt
	}
	return liveActiveOperationsScript.Run(ctx, r.store.client, []string{r.keys(id).operations}).Int64()
}

func (r *ActiveSandboxRepository) Checkpoint(ctx context.Context, id string, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	if r.validateID(id) != nil || revision == 0 || checkpoint == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	result, err := checkpointActiveSandboxScript.Run(ctx, r.store.client, []string{r.keys(id).record}, revision, checkpoint, time.Now().UTC().Format(time.RFC3339Nano)).Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status == 0 {
		return nil, nil
	}
	if status != 1 {
		return nil, state.ErrActiveSandboxConflict
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), id)
	if err != nil {
		return nil, err
	}
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return record, err
	}
	return record, nil
}

func (r *ActiveSandboxRepository) Delete(ctx context.Context, id string, revision uint64, generation int64) error {
	if r.validateID(id) != nil || revision == 0 || generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	ok, err := deleteActiveSandboxScript.Run(ctx, r.store.client, []string{k.record, k.operations, k.mutation, k.controller, k.index}, revision, generation, id).Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxConflict
	}
	return nil
}

type redisControllerLease struct {
	SandboxID       string `json:"sandbox_id"`
	Token           string `json:"token"`
	InstanceID      string `json:"instance_id"`
	PodUID          string `json:"pod_uid,omitempty"`
	Generation      int64  `json:"generation"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
}

func (r *ActiveSandboxRepository) AcquireController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, false, state.ErrActiveSandboxCorrupt
	}
	payload, _ := json.Marshal(redisControllerLease{SandboxID: lease.SandboxID, Token: lease.Token, InstanceID: lease.InstanceID, PodUID: lease.PodUID, Generation: lease.Generation})
	k := r.keys(lease.SandboxID)
	result, err := acquireActiveControllerScript.Run(ctx, r.store.client, []string{k.record, k.controller}, string(payload), lease.Generation, ttl.Milliseconds()).Slice()
	if err != nil {
		return nil, false, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, false, err
	}
	if status == 3 {
		return nil, false, nil
	}
	if status != 1 {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	expires, err := scriptInt(result, 1)
	if err != nil {
		return nil, false, err
	}
	lease.ExpiresAt = time.UnixMilli(expires)
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return &lease, true, err
	}
	return &lease, true, nil
}

func (r *ActiveSandboxRepository) RenewController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(lease.SandboxID)
	result, err := renewActiveControllerScript.Run(ctx, r.store.client, []string{k.record, k.controller}, lease.Token, lease.Generation, ttl.Milliseconds()).Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status != 1 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	expires, err := scriptInt(result, 1)
	if err != nil {
		return nil, err
	}
	lease.ExpiresAt = time.UnixMilli(expires)
	if err := r.store.acknowledgeSafetyWrite(ctx); err != nil {
		return &lease, err
	}
	return &lease, nil
}

func (r *ActiveSandboxRepository) ReleaseController(ctx context.Context, lease state.ActiveSandboxControllerLease) error {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	ok, err := releaseActiveControllerScript.Run(ctx, r.store.client, []string{r.keys(lease.SandboxID).controller}, lease.Token, lease.Generation).Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxStaleToken
	}
	return nil
}

func (r *ActiveSandboxRepository) Scan(ctx context.Context, cursor uint64, count int64) (state.ActiveSandboxPage, error) {
	if count <= 0 || count > 1000 {
		return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
	}
	bucket, innerCursor, err := decodeActiveScanCursor(cursor)
	if err != nil {
		return state.ActiveSandboxPage{}, err
	}
	indexKey := activeSandboxKeyPrefix + fmt.Sprintf("{%s:%02x}:index", r.scopeDigest, bucket)
	ids, next, err := r.store.client.SScan(ctx, indexKey, innerCursor, "*", count).Result()
	if err != nil {
		return state.ActiveSandboxPage{}, err
	}
	page := state.ActiveSandboxPage{Records: make([]state.ActiveSandboxRecord, 0, len(ids))}
	if next != 0 {
		page.Cursor = encodeActiveScanCursor(bucket, next)
	} else if bucket+1 < activeSandboxBuckets {
		page.Cursor = encodeActiveScanCursor(bucket+1, 0)
	}
	for _, id := range ids {
		if r.validateID(id) != nil {
			return state.ActiveSandboxPage{}, state.ErrActiveSandboxCorrupt
		}
		raw, getErr := r.store.client.Get(ctx, r.keys(id).record).Bytes()
		if errors.Is(getErr, redislib.Nil) {
			_ = r.store.client.SRem(ctx, indexKey, id).Err()
			continue
		}
		if getErr != nil {
			return state.ActiveSandboxPage{}, getErr
		}
		record, decodeErr := decodeActiveRecord(raw, id)
		if decodeErr != nil {
			return state.ActiveSandboxPage{}, decodeErr
		}
		page.Records = append(page.Records, *record)
	}
	return page, nil
}

func encodeActiveScanCursor(bucket uint8, cursor uint64) uint64 {
	return cursor<<8 | uint64(bucket+1)
}

func decodeActiveScanCursor(encoded uint64) (uint8, uint64, error) {
	if encoded == 0 {
		return 0, 0, nil
	}
	bucket := uint8(encoded&0xff) - 1
	if bucket >= activeSandboxBuckets {
		return 0, 0, state.ErrActiveSandboxCorrupt
	}
	return bucket, encoded >> 8, nil
}

func (r *ActiveSandboxRepository) Ping(ctx context.Context) error {
	return r.store.client.Ping(ctx).Err()
}

func (r *ActiveSandboxRepository) forceDelete(ctx context.Context, id string) error {
	k := r.keys(id)
	_, err := r.store.client.TxPipelined(ctx, func(pipe redislib.Pipeliner) error {
		pipe.Del(ctx, k.record, k.operations, k.mutation, k.controller)
		pipe.SRem(ctx, k.index, id)
		return nil
	})
	return err
}

func scriptInt(result []any, index int) (int64, error) {
	if index >= len(result) {
		return 0, state.ErrActiveSandboxCorrupt
	}
	switch value := result[index].(type) {
	case int64:
		return value, nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: invalid script integer", state.ErrActiveSandboxCorrupt)
		}
		return parsed, nil
	default:
		return 0, state.ErrActiveSandboxCorrupt
	}
}
