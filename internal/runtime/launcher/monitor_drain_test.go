package launcher

import (
	"context"
	"testing"
	"time"
)

// Invalid budgets must never reach actual validation, wait, or signals.
func TestMonitorDrainContext(t *testing.T) {
	expired, cancel1 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel1()
	long, cancel2 := context.WithTimeout(context.Background(), 31*time.Second)
	defer cancel2()
	canceled, cancel3 := context.WithTimeout(context.Background(), time.Second)
	cancel3()
	for _, ctx := range []context.Context{nil, context.Background(), expired, long, canceled} {
		if err := validateDrainContext(ctx); err == nil {
			t.Fatal("invalid context accepted")
		}
	}
	live, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := validateDrainContext(live); err != nil {
		t.Fatal(err)
	}
}
