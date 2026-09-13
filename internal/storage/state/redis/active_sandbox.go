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
for i = 3, 4 do
  local holder = redis.call('GET', KEYS[i])
  if holder and not redis.call('ZSCORE', KEYS[2], holder) then redis.call('DEL', KEYS[i]) end
end
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if record.phase == 'workspace_exclusive' and redis.call('EXISTS', KEYS[4]) == 0 then
  if record.snapshot.workspace_transition and record.snapshot.workspace_transition ~= '' then
    record.phase = 'cleanup_pending'
    record.cleanup_checkpoint = 'workspace_transition_abandoned'
  else record.phase = 'active' end
  record.revision = tonumber(record.revision) + 1
  raw = cjson.encode(record)
  redis.call('SET', KEYS[1], raw)
end
if record.phase ~= 'active' then return {2, raw} end
if redis.call('EXISTS', KEYS[4]) == 1 then return {3, raw} end
if ARGV[3] == 'mutation' or ARGV[3] == 'exclusive' then
  local holder = redis.call('GET', KEYS[3])
  if holder then return {3, raw} end
  redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[2])
end
if ARGV[3] == 'exclusive' then
  redis.call('SET', KEYS[4], ARGV[1], 'PX', ARGV[2])
  record.phase = 'workspace_exclusive'
  record.revision = tonumber(record.revision) + 1
  raw = cjson.encode(record)
  redis.call('SET', KEYS[1], raw)
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
if ARGV[4] == 'mutation' or ARGV[4] == 'exclusive' then
  local holder = redis.call('GET', KEYS[3])
  if holder ~= ARGV[1] then return {2} end
end
if ARGV[4] == 'exclusive' and redis.call('GET', KEYS[4]) ~= ARGV[1] then return {2} end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
if tonumber(score) <= nowms then
  redis.call('ZREM', KEYS[2], ARGV[1])
  if redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
  if redis.call('GET', KEYS[4]) == ARGV[1] then redis.call('DEL', KEYS[4]) end
  return {2}
end
local expires = nowms + tonumber(ARGV[3])
redis.call('ZADD', KEYS[2], expires, ARGV[1])
if ARGV[4] == 'mutation' or ARGV[4] == 'exclusive' then redis.call('PEXPIRE', KEYS[3], ARGV[3]) end
if ARGV[4] == 'exclusive' then redis.call('PEXPIRE', KEYS[4], ARGV[3]) end
return {1, tostring(expires)}
`)

var endActiveOperationScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local record = cjson.decode(raw)
if tonumber(record.generation) ~= tonumber(ARGV[2]) then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
if ARGV[3] == 'exclusive' and redis.call('GET', KEYS[4]) == ARGV[1] and record.phase == 'workspace_exclusive' then
  if record.snapshot.workspace_transition and record.snapshot.workspace_transition ~= '' then
    record.phase = 'cleanup_pending'; record.cleanup_checkpoint = 'workspace_transition_incomplete'
  else record.phase = 'active' end
  record.revision = tonumber(record.revision) + 1
  redis.call('SET', KEYS[1], cjson.encode(record))
end
if redis.call('GET', KEYS[3]) == ARGV[1] then redis.call('DEL', KEYS[3]) end
if redis.call('GET', KEYS[4]) == ARGV[1] then redis.call('DEL', KEYS[4]) end
return 1
`)

var updateActiveOperationScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
local score = redis.call('ZSCORE', KEYS[2], ARGV[1])
if not score or tonumber(score) <= nowms or tonumber(record.generation) ~= tonumber(ARGV[2]) then return {4} end
if ARGV[3] ~= 'mutation' and ARGV[3] ~= 'exclusive' then return {4} end
if redis.call('GET', KEYS[3]) ~= ARGV[1] then return {4} end
if ARGV[3] == 'exclusive' then
  if redis.call('GET', KEYS[4]) ~= ARGV[1] then return {4} end
  if redis.call('ZCARD', KEYS[2]) ~= 1 then return {2} end
end
local expectedPhase = 'active'
if ARGV[3] == 'exclusive' then expectedPhase = 'workspace_exclusive' end
if record.phase ~= expectedPhase or tonumber(record.revision) ~= tonumber(ARGV[4]) then return {2} end
record.revision = tonumber(record.revision) + 1
record.snapshot = cjson.decode(ARGV[5])
-- Detaching the workspace revokes the previous background controller in the
-- same atomic write. It may not keep fencing side effects against old local
-- metadata or block a subsequent ordinary cleanup/remount.
if record.snapshot.workspace == nil or record.snapshot.workspace == cjson.null or record.snapshot.workspace_transition == 'unmount_synced' or record.snapshot.workspace_transition == 'unmount_releasing' then
  redis.call('DEL', KEYS[5])
end
record.updated_at = ARGV[6]
local updated = cjson.encode(record)
redis.call('SET', KEYS[1], updated)
return {1, updated}
`)

var beginActiveDestroyScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
local won = 0
if record.phase == 'active' or record.phase == 'publishing' or record.phase == 'workspace_exclusive' then
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

var checkpointActiveControllerScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
local current = redis.call('GET', KEYS[2])
if not current then return {4} end
local lease = cjson.decode(current)
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
if lease.token ~= ARGV[1] or tonumber(lease.generation) ~= tonumber(ARGV[2]) or tonumber(record.generation) ~= tonumber(ARGV[2]) or tonumber(lease.expires_at_unix_ms) <= nowms then return {4} end
if tonumber(record.revision) ~= tonumber(ARGV[3]) then return {2} end
if record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending' then return {3} end
record.phase = 'cleanup_pending'
record.revision = tonumber(record.revision) + 1
record.cleanup_checkpoint = ARGV[4]
record.updated_at = ARGV[5]
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
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[6])
redis.call('SREM', KEYS[5], ARGV[3])
return 1
`)

var deleteActiveControllerScript = redislib.NewScript(`
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowms)
local raw = redis.call('GET', KEYS[1])
if not raw then return 1 end
local record = cjson.decode(raw)
local current = redis.call('GET', KEYS[4])
if not current then return 4 end
local lease = cjson.decode(current)
if lease.token ~= ARGV[4] or tonumber(lease.generation) ~= tonumber(ARGV[2]) or tonumber(lease.expires_at_unix_ms) <= nowms then return 4 end
if tonumber(record.revision) ~= tonumber(ARGV[1]) or tonumber(record.generation) ~= tonumber(ARGV[2]) then return 0 end
if record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending' then return 0 end
if redis.call('ZCARD', KEYS[2]) ~= 0 then return 0 end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[6])
redis.call('SREM', KEYS[5], ARGV[3])
return 1
`)

var acquireActiveControllerScript = redislib.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return {0} end
local record = cjson.decode(raw)
if (record.phase ~= 'publishing' and record.phase ~= 'active' and record.phase ~= 'workspace_exclusive' and record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending') or tonumber(record.generation) ~= tonumber(ARGV[2]) then return {2} end
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
if (record.phase ~= 'publishing' and record.phase ~= 'active' and record.phase ~= 'workspace_exclusive' and record.phase ~= 'destroying' and record.phase ~= 'cleanup_pending') or tonumber(record.generation) ~= tonumber(ARGV[2]) then return {2} end
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
	record, operations, mutation, controller, index, exclusive string
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
		exclusive: base + ":exclusive",
		index:     activeSandboxKeyPrefix + tag + ":index",
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
	cmd, durabilityErr := r.store.runSafetyScript(ctx, publishActiveSandboxScript, []string{k.record, k.index}, raw, record.SandboxID)
	ok, err := cmd.Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxConflict
	}
	return durabilityErr
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
	cmd, durabilityErr := r.store.runSafetyScript(ctx, changeActiveRecordScript, []string{r.keys(id).record}, revision, string(from), string(to), string(snapshot), time.Now().UTC().Format(time.RFC3339Nano))
	result, err := cmd.Slice()
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
	return record, durabilityErr
}

func (r *ActiveSandboxRepository) BeginOperation(ctx context.Context, id, token string, kind state.ActiveOperationKind, ttl time.Duration) (*state.ActiveSandboxRecord, *state.ActiveSandboxOperation, error) {
	if r.validateID(id) != nil || token == "" || (kind != state.ActiveOperationData && kind != state.ActiveOperationMutation && kind != state.ActiveOperationExclusive) || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, beginActiveOperationScript, []string{k.record, k.operations, k.mutation, k.exclusive}, token, ttl.Milliseconds(), string(kind))
	result, err := cmd.Slice()
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
	return record, op, durabilityErr
}

// UpdateOperation publishes a snapshot only while the exact mutation is live.
func (r *ActiveSandboxRepository) UpdateOperation(ctx context.Context, op state.ActiveSandboxOperation, revision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	if r.validateID(op.SandboxID) != nil || op.Token == "" || op.Generation <= 0 || revision == 0 || !json.Valid(snapshot) || len(snapshot) > 1<<20 {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(op.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, updateActiveOperationScript, []string{k.record, k.operations, k.mutation, k.exclusive, k.controller}, op.Token, op.Generation, string(op.Kind), revision, string(snapshot), time.Now().UTC().Format(time.RFC3339Nano))
	result, err := cmd.Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status == 4 || status == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if status != 1 {
		return nil, state.ErrActiveSandboxConflict
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), op.SandboxID)
	if err != nil {
		return nil, err
	}
	return record, durabilityErr
}

func (r *ActiveSandboxRepository) RenewOperation(ctx context.Context, op state.ActiveSandboxOperation, ttl time.Duration) (*state.ActiveSandboxOperation, error) {
	if r.validateID(op.SandboxID) != nil || op.Token == "" || op.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(op.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, renewActiveOperationScript, []string{k.record, k.operations, k.mutation, k.exclusive}, op.Token, op.Generation, ttl.Milliseconds(), string(op.Kind))
	result, err := cmd.Slice()
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
	return &op, durabilityErr
}

func (r *ActiveSandboxRepository) EndOperation(ctx context.Context, op state.ActiveSandboxOperation) error {
	if r.validateID(op.SandboxID) != nil || op.Token == "" || op.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	k := r.keys(op.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, endActiveOperationScript, []string{k.record, k.operations, k.mutation, k.exclusive}, op.Token, op.Generation, string(op.Kind))
	ok, err := cmd.Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxStaleToken
	}
	return durabilityErr
}

func (r *ActiveSandboxRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	if r.validateID(id) != nil {
		return nil, 0, false, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, beginActiveDestroyScript, []string{k.record, k.operations}, time.Now().UTC().Format(time.RFC3339Nano))
	result, err := cmd.Slice()
	if err != nil {
		return nil, 0, false, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, 0, false, err
	}
	if status == 0 {
		return nil, 0, false, durabilityErr
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
	return record, live, won == 1, durabilityErr
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
	cmd, durabilityErr := r.store.runSafetyScript(ctx, checkpointActiveSandboxScript, []string{r.keys(id).record}, revision, checkpoint, time.Now().UTC().Format(time.RFC3339Nano))
	result, err := cmd.Slice()
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
	return record, durabilityErr
}

func (r *ActiveSandboxRepository) CheckpointController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 || revision == 0 || checkpoint == "" || len(checkpoint) > 1<<20 {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(lease.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, checkpointActiveControllerScript, []string{k.record, k.controller}, lease.Token, lease.Generation, revision, checkpoint, time.Now().UTC().Format(time.RFC3339Nano))
	result, err := cmd.Slice()
	if err != nil {
		return nil, err
	}
	status, err := scriptInt(result, 0)
	if err != nil {
		return nil, err
	}
	if status == 4 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if status != 1 {
		return nil, state.ErrActiveSandboxConflict
	}
	record, err := decodeActiveRecord([]byte(result[1].(string)), lease.SandboxID)
	if err != nil {
		return nil, err
	}
	return record, durabilityErr
}

func (r *ActiveSandboxRepository) Delete(ctx context.Context, id string, revision uint64, generation int64) error {
	if r.validateID(id) != nil || revision == 0 || generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	k := r.keys(id)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, deleteActiveSandboxScript, []string{k.record, k.operations, k.mutation, k.controller, k.index, k.exclusive}, revision, generation, id)
	ok, err := cmd.Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxConflict
	}
	return durabilityErr
}

func (r *ActiveSandboxRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) error {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 || revision == 0 {
		return state.ErrActiveSandboxCorrupt
	}
	k := r.keys(lease.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, deleteActiveControllerScript, []string{k.record, k.operations, k.mutation, k.controller, k.index, k.exclusive}, revision, lease.Generation, lease.SandboxID, lease.Token)
	status, err := cmd.Int64()
	if err != nil {
		return err
	}
	if status == 4 {
		return state.ErrActiveSandboxStaleToken
	}
	if status != 1 {
		return state.ErrActiveSandboxConflict
	}
	return durabilityErr
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
	cmd, durabilityErr := r.store.runSafetyScript(ctx, acquireActiveControllerScript, []string{k.record, k.controller}, string(payload), lease.Generation, ttl.Milliseconds())
	result, err := cmd.Slice()
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
	return &lease, true, durabilityErr
}

func (r *ActiveSandboxRepository) RenewController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	k := r.keys(lease.SandboxID)
	cmd, durabilityErr := r.store.runSafetyScript(ctx, renewActiveControllerScript, []string{k.record, k.controller}, lease.Token, lease.Generation, ttl.Milliseconds())
	result, err := cmd.Slice()
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
	return &lease, durabilityErr
}

func (r *ActiveSandboxRepository) ReleaseController(ctx context.Context, lease state.ActiveSandboxControllerLease) error {
	if r.validateID(lease.SandboxID) != nil || lease.Token == "" || lease.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	cmd, durabilityErr := r.store.runSafetyScript(ctx, releaseActiveControllerScript, []string{r.keys(lease.SandboxID).controller}, lease.Token, lease.Generation)
	ok, err := cmd.Int64()
	if err != nil {
		return err
	}
	if ok != 1 {
		return state.ErrActiveSandboxStaleToken
	}
	return durabilityErr
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
		pipe.Del(ctx, k.record, k.operations, k.mutation, k.controller, k.exclusive)
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
