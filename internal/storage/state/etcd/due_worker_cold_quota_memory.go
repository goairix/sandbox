package etcd

import (
	"context"
	"reflect"
)

const (
	// This reservation is additional to automatic discovery's original 4MiB.
	// It includes one workspace and every compact addition in the 32 registry.
	cleanupColdQuotaBytes    = 4 << 20
	cleanupColdQuotaJobBytes = 16 << 10
	// Thirteen is the largest indivisible paid source group. Reserve it even
	// between calls, in addition to retained history/slot/journal/frozen keys.
	cleanupColdQuotaGroupKeys = 13
	cleanupColdQuotaKeys      = 64
)

type cleanupColdQuotaPointer struct {
	typeOf  reflect.Type
	address uintptr
}

// The walker starts only at the explicitly owned workspace and compact job
// additions. It never traverses a Backend, session, budget, job, worker or
// context root. A bounded node count and early byte/capacity checks make even
// malformed internal inputs finite; unknown maps are refused rather than
// traversing an unbounded registry. Aliased pointers deduplicate, while strings
// may be conservatively overcounted across the authenticated decoded records.
type cleanupColdQuotaCounter struct {
	bytes    uint64
	nodes    int
	pointers map[cleanupColdQuotaPointer]bool
	keys     map[string]bool
}

func (w *DueWorker) cleanupColdQuotaOwnedBytes() (uint64, int, error) {
	c := &cleanupColdQuotaCounter{pointers: make(map[cleanupColdQuotaPointer]bool), keys: make(map[string]bool)}
	// Enqueue and physical completion can publish/remove registry pointers.
	// Copy the fixed array under its original lock; compact cold fields remain
	// actor-owned and physical completion never touches the cold workspace.
	w.backend.archiveLife.mu.Lock()
	jobs := w.cleanupJobs
	w.backend.archiveLife.mu.Unlock()
	for _, j := range jobs {
		if j == nil {
			continue
		}
		before := c.bytes
		if err := c.value(reflect.ValueOf(&j.coldQuota), false); err != nil {
			return c.bytes, len(c.keys) + cleanupColdQuotaGroupKeys, err
		}
		if c.bytes-before > cleanupColdQuotaJobBytes {
			return c.bytes, len(c.keys) + cleanupColdQuotaGroupKeys, ErrInvalidRecord
		}
	}
	if w.coldQuota != nil {
		if err := c.value(reflect.ValueOf(w.coldQuota), false); err != nil {
			return c.bytes, len(c.keys) + cleanupColdQuotaGroupKeys, err
		}
	}
	keys := len(c.keys) + cleanupColdQuotaGroupKeys
	if c.bytes > cleanupColdQuotaBytes || keys > cleanupColdQuotaKeys {
		return c.bytes, keys, ErrInvalidRecord
	}
	return c.bytes, keys, nil
}

func (c *cleanupColdQuotaCounter) charge(bytes uint64) error {
	if bytes > cleanupColdQuotaBytes || c.bytes > cleanupColdQuotaBytes-bytes {
		return ErrInvalidRecord
	}
	c.bytes += bytes
	return nil
}

func cleanupColdQuotaSharedRoot(t reflect.Type) bool {
	for _, shared := range []reflect.Type{
		reflect.TypeOf((*Backend)(nil)), reflect.TypeOf((*DueWorker)(nil)),
		reflect.TypeOf((*cleanupLocalJob)(nil)), reflect.TypeOf((*WorkerSession)(nil)),
		reflect.TypeOf((*WorkerPartitionClaim)(nil)), reflect.TypeOf((*WorkerBudgetShare)(nil)),
		reflect.TypeOf((*context.Context)(nil)).Elem(),
	} {
		if t == shared {
			return true
		}
	}
	return false
}

func (c *cleanupColdQuotaCounter) key(v reflect.Value, name string) error {
	if name != "key" && name != "Key" && name != "returnKey" && name != "journalKey" {
		return nil
	}
	var key string
	if v.Kind() == reflect.String {
		key = v.String()
	} else if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		key = string(v.Bytes())
	}
	if key != "" {
		c.keys[key] = true
		if len(c.keys)+cleanupColdQuotaGroupKeys > cleanupColdQuotaKeys {
			return ErrInvalidRecord
		}
	}
	return nil
}

func (c *cleanupColdQuotaCounter) value(v reflect.Value, inline bool) error {
	if !v.IsValid() || cleanupColdQuotaSharedRoot(v.Type()) {
		return nil
	}
	c.nodes++
	if c.nodes > 65536 {
		return ErrInvalidRecord
	}
	if inline {
		if err := c.charge(uint64(v.Type().Size())); err != nil {
			return err
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		pointer := cleanupColdQuotaPointer{typeOf: v.Type(), address: v.Pointer()}
		if c.pointers[pointer] {
			return nil
		}
		c.pointers[pointer] = true
		return c.value(v.Elem(), true)
	case reflect.Interface:
		if !v.IsNil() {
			return c.value(v.Elem(), true)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if err := c.key(field, v.Type().Field(i).Name); err != nil {
				return err
			}
			if err := c.value(field, false); err != nil {
				return err
			}
		}
	case reflect.String:
		return c.charge(uint64(v.Len()))
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		// Capacity, rather than length, accounts retained backing allocations.
		if err := c.charge(uint64(v.Cap()) * uint64(v.Type().Elem().Size())); err != nil {
			return err
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		full := v.Slice(0, v.Cap())
		for i := 0; i < full.Len(); i++ {
			if err := c.value(full.Index(i), false); err != nil {
				return err
			}
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := c.value(v.Index(i), false); err != nil {
				return err
			}
		}
	case reflect.Chan:
		return c.charge(128 + uint64(v.Cap())*uint64(v.Type().Elem().Size()))
	case reflect.Map, reflect.UnsafePointer:
		return ErrInvalidRecord
	}
	return nil
}
