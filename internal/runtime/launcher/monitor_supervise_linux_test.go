//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The regression is attributing any adopted status to the user root, returning
// before real ECHILD, or using an already-canceled command budget for cleanup.
func TestMonitorSuperviseRootModel(t *testing.T) {
	for _, scenario := range []string{"root_then_descendant", "adopted_then_root", "missing_root", "canceled_before_entry", "wait_error", "combined_limit", "final_seal_error"} {
		t.Run(scenario, func(t *testing.T) {
			m := drainTestHandle()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if scenario == "canceled_before_entry" {
				cancel()
			}
			waits, scans, checks := 0, 0, 0
			calls := rootCalls{root: func(int) error { return nil }, drainCalls: drainCalls{
				validate: func() error {
					checks++
					if scenario == "final_seal_error" && checks > 1 {
						return unix.EIO
					}
					return nil
				},
				scan: func(clean context.Context) error {
					scans++
					if clean.Err() != nil {
						t.Fatal("cleanup inherited command cancellation")
					}
					d, ok := clean.Deadline()
					if !ok || time.Until(d) > 30*time.Second {
						t.Fatal("cleanup budget missing")
					}
					return nil
				},
				wait: func(s *unix.WaitStatus) (int, error) {
					waits++
					if scenario == "wait_error" {
						return -1, unix.EIO
					}
					if scenario == "canceled_before_entry" && waits == 1 {
						return 0, nil
					}
					if scenario == "missing_root" {
						if waits == 1 {
							*s = unix.WaitStatus(3 << 8)
							return 99, nil
						}
						return -1, unix.ECHILD
					}
					if scenario == "combined_limit" {
						if waits <= 4097 {
							*s = unix.WaitStatus(0)
							return 100 + waits, nil
						}
						return -1, unix.ECHILD
					}
					if scenario == "canceled_before_entry" {
						if waits == 2 {
							*s = unix.WaitStatus(9)
							return 42, nil
						}
						return -1, unix.ECHILD
					}
					if waits == 1 {
						if scenario == "adopted_then_root" {
							*s = unix.WaitStatus(9)
							return 99, nil
						}
						*s = unix.WaitStatus(7 << 8)
						return 42, nil
					}
					if waits == 2 {
						if scenario == "adopted_then_root" {
							*s = unix.WaitStatus(7 << 8)
							return 42, nil
						}
						*s = unix.WaitStatus(9)
						return 99, nil
					}
					return -1, unix.ECHILD
				},
			}}
			o, err := m.superviseRootWith(ctx, 42, nil, calls)
			good := scenario == "root_then_descendant" || scenario == "adopted_then_root" || scenario == "canceled_before_entry"
			if !good {
				if err == nil || o.RootPID != 0 || !m.poisoned {
					t.Fatalf("unsafe result %+v %v poisoned=%t", o, err, m.poisoned)
				}
				return
			}
			want := uint32(7 << 8)
			if scenario == "canceled_before_entry" {
				want = 9
				if scans == 0 {
					t.Fatal("canceled command never drained")
				}
			}
			if err != nil || o.RootPID != 42 || o.RootWaitStatus != want || o.Drain.MonitorPID != m.pid {
				t.Fatalf("result %+v %v", o, err)
			}
			if _, err = m.superviseRootWith(ctx, 42, nil, calls); err == nil {
				t.Fatal("same root supervised twice")
			}
		})
	}
}

func TestMonitorSuperviseInvalidContexts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3601*time.Second)
	defer cancel()
	for _, c := range []context.Context{nil, context.Background(), ctx} {
		m := drainTestHandle()
		_, err := m.superviseRootWith(c, 42, nil, rootCalls{})
		if err == nil || m.drainStarted {
			t.Fatal("invalid context reached owner")
		}
	}
	m := drainTestHandle()
	valid, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	m.mu.Lock()
	_, err := m.superviseRootWith(valid, 42, nil, rootCalls{})
	m.mu.Unlock()
	if !errors.Is(err, ErrKernelUnavailable) {
		t.Fatal("competing owner accepted", err)
	}
}
