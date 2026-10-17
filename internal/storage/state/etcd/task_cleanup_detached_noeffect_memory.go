package etcd

import (
	"context"
	"reflect"
	"runtime"

	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// This is a per-job retained-ownership ceiling, not a reservation from the
// worker's global 64MiB. Consumer admission must reserve that separately.
const cleanupDetachedNoEffectJobBytes = 128 << 10

type cleanupDetachedNoEffectMemoryPointer struct {
	typeOf  reflect.Type
	address uintptr
}

type cleanupDetachedNoEffectMemoryString struct {
	address uintptr
	length  int
}

type cleanupDetachedNoEffectMemoryCounter struct {
	bytes        uint64
	nodes        uint32
	seen         map[cleanupDetachedNoEffectMemoryPointer]bool
	seenStrings  map[cleanupDetachedNoEffectMemoryString]bool
	shared       map[cleanupDetachedNoEffectMemoryPointer]bool
	zeroContexts map[reflect.Type]bool
}

// Addition checks the finite ceiling before arithmetic, including malicious
// widths near MaxUint64. Wrapping cannot turn an over-cap graph into zero.
func cleanupDetachedNoEffectAddBytes(total, amount uint64) (uint64, error) {
	if amount > cleanupDetachedNoEffectJobBytes || total > cleanupDetachedNoEffectJobBytes-amount {
		return total, ErrInvalidRecord
	}
	return total + amount, nil
}

func cleanupDetachedNoEffectSharedType(t reflect.Type) bool {
	for _, shared := range []reflect.Type{
		reflect.TypeOf((*Backend)(nil)), reflect.TypeOf((*DueWorker)(nil)),
		reflect.TypeOf((*cleanupLocalJob)(nil)), reflect.TypeOf((*WorkerSession)(nil)),
		reflect.TypeOf((*WorkerPartitionClaim)(nil)), reflect.TypeOf((*WorkerBudgetShare)(nil)),
		reflect.TypeOf((*launcher.KernelBoundary)(nil)),
	} {
		if t == shared {
			return true
		}
	}
	return t.Implements(reflect.TypeOf((*context.Context)(nil)).Elem())
}

// Only infrastructure explicitly supplied as top-level arguments is shared.
// A different nested Backend/job/Kernel of the same type is never silently
// excluded. Claim, prepared drain, old E/S and all wire backings remain owned.
// This pure byte walker grants no KV, Run or effect permission.
// String sizes rely on controlled compact constructor/native-codec boundaries:
// Claim clones external WorkerID before retention. This is not an allocation
// oracle for arbitrary caller substrings that bypass those original builders.
// Only exact string address/length aliases share a charge. Different substring
// lengths or overlapping regions are conservatively charged separately; slice
// capacities also retain their existing independent charges.
func cleanupDetachedNoEffectGraphBytes(roots ...any) (uint64, error) {
	// Address keys do not retain their allocations. Keep all original roots
	// alive through the complete scan, including rejection and early returns.
	defer func() { runtime.KeepAlive(roots) }()

	c := &cleanupDetachedNoEffectMemoryCounter{
		seen:         make(map[cleanupDetachedNoEffectMemoryPointer]bool),
		seenStrings:  make(map[cleanupDetachedNoEffectMemoryString]bool),
		shared:       make(map[cleanupDetachedNoEffectMemoryPointer]bool),
		zeroContexts: make(map[reflect.Type]bool),
	}
	for _, root := range roots {
		v := reflect.ValueOf(root)
		if !v.IsValid() || !cleanupDetachedNoEffectSharedType(v.Type()) {
			continue
		}
		if v.Kind() == reflect.Pointer {
			if !v.IsNil() {
				c.shared[cleanupDetachedNoEffectMemoryPointer{v.Type(), v.Pointer()}] = true
			}
		} else if v.Type().Size() == 0 {
			// Background/TODO are immutable zero-sized context values. Other
			// nonpointer context graphs cannot be hidden through this exception.
			c.zeroContexts[v.Type()] = true
		} else {
			return 0, ErrInvalidRecord
		}
	}
	for _, root := range roots {
		if err := c.value(reflect.ValueOf(root), false); err != nil {
			return c.bytes, err
		}
	}
	return c.bytes, nil
}

func (c *cleanupDetachedNoEffectMemoryCounter) charge(n uint64) error {
	var err error
	c.bytes, err = cleanupDetachedNoEffectAddBytes(c.bytes, n)
	return err
}

func (c *cleanupDetachedNoEffectMemoryCounter) value(v reflect.Value, inline bool) error {
	if !v.IsValid() || c.zeroContexts[v.Type()] {
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
		p := cleanupDetachedNoEffectMemoryPointer{v.Type(), v.Pointer()}
		if c.shared[p] || c.seen[p] {
			return nil
		}
		c.seen[p] = true
		return c.value(v.Elem(), true)
	case reflect.Interface:
		if !v.IsNil() {
			return c.value(v.Elem(), true)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := c.value(v.Field(i), false); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() == 0 {
			return nil
		}
		backing := cleanupDetachedNoEffectMemoryString{address: v.Pointer(), length: v.Len()}
		if c.seenStrings[backing] {
			return nil
		}
		c.seenStrings[backing] = true
		return c.charge(uint64(v.Len()))
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		size := uint64(v.Type().Elem().Size())
		if size > 0 && uint64(v.Cap()) > cleanupDetachedNoEffectJobBytes/size {
			return ErrInvalidRecord
		}
		if err := c.charge(uint64(v.Cap()) * size); err != nil {
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
		if !v.IsNil() {
			// Opaque queued pointer graphs cannot be inspected without taking
			// ownership from their receiver. Refuse them rather than undercount.
			if v.Len() != 0 {
				return ErrInvalidRecord
			}
			size := uint64(v.Type().Elem().Size())
			if size > 0 && uint64(v.Cap()) > cleanupDetachedNoEffectJobBytes/size {
				return ErrInvalidRecord
			}
			return c.charge(128 + uint64(v.Cap())*size)
		}
	case reflect.Map, reflect.Func:
		if !v.IsNil() {
			return ErrInvalidRecord
		}
	case reflect.UnsafePointer:
		return ErrInvalidRecord
	}
	return nil
}

// Only the original physical owner calls this between actor continuations.
// It counts both additions together, retaining the old Claim/prepared graph
// during uncertain recovery. Finish returns zero once both roots are released.
func (b *Backend) cleanupDetachedNoEffectOwnedBytes(j *cleanupLocalJob) (uint64, error) {
	if b == nil || j == nil || j.worker == nil || b.localCleanup == nil {
		return 0, ErrInvalidRecord
	}
	return cleanupDetachedNoEffectGraphBytes(b, j, j.worker, j.ctx, b.localCleanup.Worker, j.detachedNoEffect, j.detachedRecovery)
}
