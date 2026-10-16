package etcd

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkerBucket(t *testing.T) {
	start := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	t.Run("range-empty-refill-and-idle-burst", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1048576, 262144)
		require.False(t, bucket.take(start, 1), "a newly borrowed share receives no free tokens")
		require.True(t, bucket.take(start.Add(250*time.Millisecond), 262144))
		require.False(t, bucket.take(start.Add(250*time.Millisecond), 1))
		require.True(t, bucket.take(start.Add(time.Hour), 262144))
		require.False(t, bucket.take(start.Add(time.Hour), 1), "idle refill cannot exceed one bounded chunk")
	})
	t.Run("claim-fraction-is-retained", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1, 1)
		for i := 1; i < 10; i++ {
			require.False(t, bucket.take(start.Add(time.Duration(i)*100*time.Millisecond), 1))
		}
		require.True(t, bucket.take(start.Add(time.Second), 1), "failed small probes must preserve fractional earned credit")
		require.False(t, bucket.take(start.Add(time.Second), 1))
		require.False(t, bucket.take(start.Add(1999*time.Millisecond), 1))
		require.True(t, bucket.take(start.Add(2*time.Second), 1))
	})
	t.Run("range-fraction-below-one-byte", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1048576, 262144)
		require.False(t, bucket.take(start.Add(953*time.Nanosecond), 1))
		require.True(t, bucket.take(start.Add(954*time.Nanosecond), 1))
		require.False(t, bucket.take(start.Add(1907*time.Nanosecond), 1))
		require.True(t, bucket.take(start.Add(1908*time.Nanosecond), 1))
	})
	t.Run("backward-time-never-reearns-credit", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1, 1)
		require.True(t, bucket.take(start.Add(time.Second), 1))
		require.False(t, bucket.take(start.Add(900*time.Millisecond), 1))
		require.False(t, bucket.take(start.Add(1999*time.Millisecond), 1), "a backward probe cannot move the earning origin backward")
		require.True(t, bucket.take(start.Add(2*time.Second), 1))
	})
	t.Run("long-jump-saturates-without-overflow", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1048576, 262144)
		later := start.Add(time.Duration(math.MaxInt64))
		require.True(t, bucket.take(later, 262144))
		require.False(t, bucket.take(later, 1))
		require.False(t, bucket.take(later.Add(953*time.Nanosecond), 1))
		require.True(t, bucket.take(later.Add(954*time.Nanosecond), 1))
	})
	t.Run("invalid-charge-does-not-spend", func(t *testing.T) {
		bucket := newWorkerBucket(start, 1, 1)
		require.False(t, bucket.take(start.Add(time.Second), 0))
		require.False(t, bucket.take(start.Add(time.Second), 2))
		require.True(t, bucket.take(start.Add(time.Second), 1))
		require.False(t, bucket.take(start.Add(time.Second), 0))
		require.False(t, bucket.take(start.Add(time.Second), 1))
	})
}
