//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

func TestDetachedBusinessServiceNative(t *testing.T) {
	if os.Getenv("SANDBOX_DETACHED_BUSINESS_NATIVE") != "1" {
		t.Skip("requires exact owned detached business Linux fixture")
	}
	for _, variant := range []struct {
		name            string
		ack, concurrent bool
	}{{"key-replay-default-drift-conflict", false, false}, {"two-first-absent", false, true}, {"activation-ack-loss-key-recovery", true, false}} {
		t.Run(variant.name, func(t *testing.T) {
			runColdCreateServiceVariantNative(t, variant.ack, true, true, variant.concurrent)
		})
	}
}

// Both calls reach the final pre-acquisition clock only after the business
// request lookup. Holding the first there forces both original lookups absent,
// without intercepting etcd, granting permits or modifying production code.
type detachedNativeAcquisitionClock struct {
	armed    atomic.Bool
	arrivals atomic.Int32
	ready    chan struct{}
}

func (c *detachedNativeAcquisitionClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	if c.armed.Load() {
		arrival := c.arrivals.Add(1)
		if arrival == 2 {
			close(c.ready)
		}
		if arrival <= 2 {
			select {
			case <-c.ready:
			case <-ctx.Done():
				return p.ClockObservation{}, ctx.Err()
			}
		}
	}
	return nativeFixtureClock{}.Observe(ctx)
}
