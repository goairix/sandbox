package etcd

import "time"

// workerBucket retains whole tokens and fractional nanosecond credit. Only
// the fixed 1/s, capacity 1 and 1MiB/s, capacity 256KiB profiles construct it.
// Go's monotonic time component is retained by production time.Now values.
type workerBucket struct {
	last              time.Time
	rate, capacity    uint64
	tokens, remainder uint64
}

func newWorkerBucket(now time.Time, rate, capacity uint64) workerBucket {
	return workerBucket{last: now, rate: rate, capacity: capacity}
}

func (b *workerBucket) take(now time.Time, amount uint32) bool {
	if amount == 0 || uint64(amount) > b.capacity || b.rate == 0 {
		return false
	}
	if now.After(b.last) {
		elapsed := uint64(now.Sub(b.last))
		b.last = now
		missing := b.capacity - b.tokens
		if missing == 0 {
			b.remainder = 0
		} else {
			// With the fixed profiles missing*1e9 <= 262144e9. Compare
			// against saturation BEFORE multiplying an arbitrarily long
			// elapsed duration by rate, so even Duration's maximum is safe.
			needed := missing*uint64(time.Second) - b.remainder
			fillNanos := (needed + b.rate - 1) / b.rate
			if elapsed >= fillNanos {
				b.tokens, b.remainder = b.capacity, 0
			} else {
				credit := elapsed*b.rate + b.remainder
				b.tokens += credit / uint64(time.Second)
				b.remainder = credit % uint64(time.Second)
			}
		}
	}
	if b.tokens < uint64(amount) {
		return false
	}
	b.tokens -= uint64(amount)
	return true
}
