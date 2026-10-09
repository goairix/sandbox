package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func TestTaskWorkspaceSyncRecyclingOptionsContract(t *testing.T) {
	field, ok := reflect.TypeOf(Options{}).FieldByName("TaskWorkspaceSyncRecyclingBudget")
	require.True(t, ok, "recycling must have an independent opt-in")
	require.Equal(t, reflect.TypeOf((*TaskWorkspaceSyncBudgetOptions)(nil)), field.Type)
	require.Equal(t, 3, reflect.TypeOf(TaskWorkspaceSyncBudgetOptions{}).NumField(), "legacy options remain three fields")
}

func TestTaskWorkspaceSyncRecyclingUnknownBudgetPin(t *testing.T) {
	kv := &mvccpb.KeyValue{Key: []byte("config"), Value: []byte("value"), CreateRevision: 1, ModRevision: 1, Version: 1}
	for _, profile := range []uint8{0, 1, 2, 3, 255} {
		b := &Backend{workspaceSyncBudget: &taskWorkspaceSyncBudgetPin{profile: profile, key: "config", value: "value", create: 1, mod: 1}}
		if profile <= 2 {
			require.True(t, b.workspaceSyncBudgetMatches(kv))
		} else {
			require.False(t, b.workspaceSyncBudgetMatches(kv), "unknown profile cannot match a native pin")
		}
	}
}

func TestTaskWorkspaceSyncRecyclingConfiguration(t *testing.T) {
	o := validOptions(t)
	budget := TaskWorkspaceSyncBudgetOptions{uuid.NewString(), 1, 4096}
	o.TaskWorkspaceSyncRecyclingBudget = &budget
	require.NoError(t, validateOptions(o))
	o.TaskWorkspaceSyncBudget = &budget
	require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
	o.TaskWorkspaceSyncBudget = nil
	for _, mutation := range []func(*TaskWorkspaceSyncBudgetOptions){
		func(b *TaskWorkspaceSyncBudgetOptions) { b.BudgetID = uuid.Nil.String() },
		func(b *TaskWorkspaceSyncBudgetOptions) { b.SlotsPerPartition = 0 },
		func(b *TaskWorkspaceSyncBudgetOptions) { b.SlotsPerPartition = 65 },
		func(b *TaskWorkspaceSyncBudgetOptions) { b.MaxIntentBytes = 4095 },
		func(b *TaskWorkspaceSyncBudgetOptions) { b.MaxIntentBytes = 16385 },
	} {
		bad := budget
		mutation(&bad)
		o.TaskWorkspaceSyncRecyclingBudget = &bad
		require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
	}
	legacy, err := encodeOperationMetadata(taskWorkspaceSyncBudgetRecord{1, budget}, 1024)
	require.NoError(t, err)
	require.Equal(t, `{"version":1,"budget_id":"`+budget.BudgetID+`","slots_per_partition":1,"max_intent_bytes":4096}`, legacy)
	recycling, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, budget}, 1024)
	require.NoError(t, err)
	require.Equal(t, strings.Replace(legacy, `"version":1`, `"version":2`, 1), recycling)
	require.ErrorIs(t, (taskWorkspaceSyncRecyclingBudgetRecord{1, budget}).Validate(), ErrInvalidRecord)
	require.ErrorIs(t, (taskWorkspaceSyncBudgetRecord{2, budget}).Validate(), ErrInvalidRecord)
	require.EqualValues(t, 256*(4096+512+2*1024), budget.logicalByteBound())
}

func recyclingSlotFixture() taskWorkspaceSyncRecyclingSlotRecord {
	return taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: "11111111-1111-4111-8111-111111111111", Partition: 255, Slot: 63, State: "free"}
}

func TestTaskWorkspaceSyncRecyclingStrictSlots(t *testing.T) {
	for _, state := range []string{"free", "occupied", "terminal"} {
		t.Run(state, func(t *testing.T) {
			r := recyclingSlotFixture()
			r.State = state
			if state != "free" {
				r.IntentID, r.IntentDigest = uuid.NewString(), strings.Repeat("a", 64)
			}
			if state == "terminal" {
				r.IntentBirth, r.EvidenceDigest = 3, strings.Repeat("b", 64)
			}
			value, err := encodeWorkspaceSyncRecyclingSlot(r)
			require.NoError(t, err)
			require.LessOrEqual(t, len(value), 512)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(value), &fields))
			require.Len(t, fields, 9)
			kv := &mvccpb.KeyValue{Value: []byte(value), CreateRevision: 2, ModRevision: 9, Version: 4}
			decoded, err := decodeWorkspaceSyncRecyclingSlot(kv)
			require.NoError(t, err)
			require.Equal(t, r, decoded)
			if state == "free" {
				initial := *kv
				initial.ModRevision, initial.Version = initial.CreateRevision, 1
				decoded, err = decodeWorkspaceSyncRecyclingSlot(&initial)
				require.NoError(t, err)
				require.Equal(t, r, decoded, "initial and recycled free bytes may match while native generations differ")
			}
			for name, raw := range fields {
				t.Run("missing-"+name, func(t *testing.T) {
					bad := *kv
					field := `"` + name + `":` + string(raw)
					missing := strings.Replace(value, field+",", "", 1)
					if missing == value {
						missing = strings.Replace(value, ","+field, "", 1)
					}
					bad.Value = []byte(missing)
					_, err := decodeWorkspaceSyncRecyclingSlot(&bad)
					require.ErrorIs(t, err, ErrCorruptRecord)
				})
				t.Run("null-"+name, func(t *testing.T) {
					bad := *kv
					bad.Value = []byte(strings.Replace(value, `"`+name+`":`+string(raw), `"`+name+`":null`, 1))
					_, err := decodeWorkspaceSyncRecyclingSlot(&bad)
					require.ErrorIs(t, err, ErrCorruptRecord)
				})
			}
			for _, fault := range []string{"lease", "create", "mod", "version", "version-overflow", "revision-overflow", "duplicate", "unknown", "space", "oversize", "partition-overflow", "slot-overflow", "trailing"} {
				t.Run(fault, func(t *testing.T) {
					bad := *kv
					switch fault {
					case "lease":
						bad.Lease = 1
					case "create":
						bad.CreateRevision = 0
					case "mod":
						bad.ModRevision = 1
					case "version":
						bad.Version = 0
					case "version-overflow":
						bad.Version = math.MaxInt64
					case "revision-overflow":
						bad.ModRevision = math.MaxInt64
					case "duplicate":
						bad.Value = []byte(strings.Replace(value, `"version":2`, `"version":2,"version":2`, 1))
					case "unknown":
						bad.Value = []byte(strings.Replace(value, `"version":2`, `"version":2,"extra":0`, 1))
					case "space":
						bad.Value = []byte(" " + value)
					case "oversize":
						bad.Value = []byte(strings.Repeat("x", 513))
					case "partition-overflow":
						bad.Value = []byte(strings.Replace(value, `"partition":255`, `"partition":256`, 1))
					case "slot-overflow":
						bad.Value = []byte(strings.Replace(value, `"slot":63`, `"slot":4294967296`, 1))
					case "trailing":
						bad.Value = []byte(value + "{}")
					}
					_, err := decodeWorkspaceSyncRecyclingSlot(&bad)
					require.ErrorIs(t, err, ErrCorruptRecord)
				})
			}
		})
	}
	_, err := decodeWorkspaceSyncRecyclingSlot(nil)
	require.ErrorIs(t, err, ErrCorruptRecord)
}

func TestTaskWorkspaceSyncRecyclingSlotStateRules(t *testing.T) {
	for name, mutation := range map[string]func(*taskWorkspaceSyncRecyclingSlotRecord){
		"version":       func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.Version = 1 },
		"budget":        func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.BudgetID = uuid.Nil.String() },
		"slot":          func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.Slot = 64 },
		"unknown-state": func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.State = "ready" },
		"free-id":       func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.IntentID = uuid.NewString() },
		"free-digest":   func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.IntentDigest = strings.Repeat("a", 64) },
		"free-birth":    func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.IntentBirth = 1 },
		"free-evidence": func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.EvidenceDigest = strings.Repeat("a", 64) },
		"occupied-missing-id": func(r *taskWorkspaceSyncRecyclingSlotRecord) {
			r.State = "occupied"
			r.IntentDigest = strings.Repeat("a", 64)
		},
		"occupied-missing-digest": func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.State = "occupied"; r.IntentID = uuid.NewString() },
		"occupied-birth": func(r *taskWorkspaceSyncRecyclingSlotRecord) {
			r.State = "occupied"
			r.IntentID = uuid.NewString()
			r.IntentDigest = strings.Repeat("a", 64)
			r.IntentBirth = 1
		},
		"occupied-evidence": func(r *taskWorkspaceSyncRecyclingSlotRecord) {
			r.State = "occupied"
			r.IntentID = uuid.NewString()
			r.IntentDigest = strings.Repeat("a", 64)
			r.EvidenceDigest = strings.Repeat("a", 64)
		},
		"terminal-missing-birth": func(r *taskWorkspaceSyncRecyclingSlotRecord) {
			r.State = "terminal"
			r.IntentID = uuid.NewString()
			r.IntentDigest = strings.Repeat("a", 64)
			r.EvidenceDigest = strings.Repeat("b", 64)
		},
		"terminal-missing-evidence": func(r *taskWorkspaceSyncRecyclingSlotRecord) {
			r.State = "terminal"
			r.IntentID = uuid.NewString()
			r.IntentDigest = strings.Repeat("a", 64)
			r.IntentBirth = 1
		},
		"negative-birth": func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.IntentBirth = -1 },
		"long-id":        func(r *taskWorkspaceSyncRecyclingSlotRecord) { r.IntentID = strings.Repeat("x", 513) },
	} {
		t.Run(name, func(t *testing.T) {
			r := recyclingSlotFixture()
			mutation(&r)
			_, err := encodeWorkspaceSyncRecyclingSlot(r)
			require.ErrorIs(t, err, ErrInvalidRecord)
		})
	}
	r := recyclingSlotFixture()
	r.Slot = 0
	require.NoError(t, r.Validate())
}

func TestTaskWorkspaceSyncRecyclingKeysAndLegacyFailClosed(t *testing.T) {
	n, err := NewNamespace("/test", "scope", "cell")
	require.NoError(t, err)
	b := &Backend{namespace: n, restoreEpoch: "epoch"}
	ref := workspaceSyncIntentRecordFixture(t).Reference
	i, s, err := b.workspaceSyncRecyclingKeys(ref)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-intents/%s/%s", n.Root(), ref.Task.Partition, ref.BudgetID, ref.IntentID), i)
	require.Equal(t, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-slots/%s/00", n.Root(), ref.Task.Partition, ref.BudgetID), s)
	legacyIntent, legacySlot, err := b.workspaceSyncIntentKeys(ref)
	require.NoError(t, err)
	require.NotEqual(t, legacyIntent, i)
	require.NotEqual(t, legacySlot, s)
	ref.BudgetID = uuid.NewString()
	i2, s2, err := b.workspaceSyncRecyclingKeys(ref)
	require.NoError(t, err)
	require.NotEqual(t, i, i2)
	require.NotEqual(t, s, s2)
	ref.Task.Partition++
	i3, s3, err := b.workspaceSyncRecyclingKeys(ref)
	require.NoError(t, err)
	require.NotEqual(t, i2, i3)
	require.NotEqual(t, s2, s3)
	ref.Task.Namespace = "/other/scope/cell/"
	_, _, err = b.workspaceSyncRecyclingKeys(ref)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	for _, profile := range []uint8{0, 1} {
		require.True(t, (&taskWorkspaceSyncBudgetPin{profile: profile}).legacyProfile())
	}
	for _, profile := range []uint8{2, 3, 255} {
		b.workspaceSyncBudget = &taskWorkspaceSyncBudgetPin{profile: profile}
		_, err = b.readWorkspaceSyncIntent(context.Background(), TaskWorkspaceSyncIntentReference{})
		require.ErrorIs(t, err, ErrInvalidConfiguration, "must refuse before nil client RPC")
		_, err = b.LoadTaskWorkspaceSyncIntent(context.Background(), TaskWorkspaceSyncIntentReference{})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		backend, source := syncSourceHostIO(t, nil)
		backend.workspaceSyncBudget = b.workspaceSyncBudget
		entry := &taskWorkspaceSyncEntry{origin: source, kind: '0'}
		entry.self = entry
		body := &taskWorkspaceSyncBody{source: source, entry: entry, ctx: context.Background()}
		body.self = body
		source.current, source.body = entry, body
		_, err = source.reservePutIntent(context.Background(), body)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		require.Nil(t, body.intent)
	}
}
