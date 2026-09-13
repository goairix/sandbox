package redis

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/goairix/sandbox/internal/storage/state"
	redislib "github.com/redis/go-redis/v9"
)

var readReturnTerminalScript = redislib.NewScript(`return {redis.call('GET', KEYS[1])}`)

// ConfirmReturnPreparedTerminal confirms only a previously validated terminal
// snapshot. Ordinary inventory reads remain read-only and retain their fast path.
func (r *FUSEPoolRepository) ConfirmReturnPreparedTerminal(ctx context.Context, id string, expected *state.FUSEPoolRecord) error {
	if r.store.durability != DurabilityReplicaAck {
		return nil
	}
	cmd, ackErr := r.store.runSafetyScript(ctx, readReturnTerminalScript, []string{fusePoolRecordPrefix + fusePoolDigest(id)})
	values, err := cmd.Slice()
	if err != nil {
		return err
	}
	if len(values) != 1 {
		return state.ErrFUSEPoolCorrupt
	}
	if values[0] == nil {
		if expected != nil {
			return state.ErrFUSEPoolConflict
		}
		return ackErr
	}
	if expected == nil {
		return state.ErrFUSEPoolConflict
	}
	var current state.FUSEPoolRecord
	raw, ok := values[0].(string)
	if !ok || json.Unmarshal([]byte(raw), &current) != nil || !reflect.DeepEqual(current, *expected) {
		return state.ErrFUSEPoolConflict
	}
	return ackErr
}
