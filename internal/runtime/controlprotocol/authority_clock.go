package controlprotocol

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// ClockObservation is a call-time controlled UTC estimate with verified uncertainty.
type ClockObservation struct {
	UTC         time.Time
	Uncertainty time.Duration
}

// AuthorityClock is an explicit operator-trusted source, immutable and concurrency
// safe. Observe must honor context. This interface cannot certify source honesty.
// Callers borrow it; they never close it. There is no implicit local-clock source.
type AuthorityClock interface {
	Observe(context.Context) (ClockObservation, error)
}

func nilDependency(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}
func accountClockElapsed(o ClockObservation, elapsed time.Duration) (ClockObservation, error) {
	if !validUTC(o.UTC) || o.Uncertainty < 0 || o.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-o.Uncertainty {
		return ClockObservation{}, fmt.Errorf("invalid or late controlled clock observation")
	}
	o.UTC = o.UTC.Add(elapsed)
	o.Uncertainty += elapsed
	if !validUTC(o.UTC) {
		return ClockObservation{}, fmt.Errorf("clock outside UTC range")
	}
	return o, nil
}

// observeAuthorityClock bounds a trusted source call and conservatively accounts
// for its complete monotonic elapsed time. time.Now measures duration only.
func observeAuthorityClock(ctx context.Context, source AuthorityClock) (ClockObservation, error) {
	if nilDependency(ctx) || nilDependency(source) {
		return ClockObservation{}, fmt.Errorf("missing controlled clock or context")
	}
	if err := ctx.Err(); err != nil {
		return ClockObservation{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	o, err := source.Observe(bounded)
	if err != nil {
		return ClockObservation{}, err
	}
	if err := bounded.Err(); err != nil {
		return ClockObservation{}, err
	}
	return accountClockElapsed(o, time.Since(start))
}
