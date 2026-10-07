//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRootLifecycleOrdering(t *testing.T) {
	for _, scenario := range []string{"renew_before_exit", "expired_before_renew", "root_before_renew", "canceled_request", "backward_deadline", "late_clock"} {
		t.Run(scenario, func(t *testing.T) {
			m := drainTestHandle()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			now := int64(time.Second)
			life := newRootLifecycle(now + int64(time.Second))
			requestCtx, requestCancel := context.WithTimeout(context.Background(), time.Second)
			defer requestCancel()
			r := rootRenewRequest{ctx: requestCtx, deadline: now + int64(2*time.Second), result: make(chan error, 1)}
			if scenario == "canceled_request" {
				requestCancel()
			}
			if scenario == "backward_deadline" {
				r.deadline = now
			}
			life.requests <- r
			waits := 0
			calls := rootCalls{now: func() (int64, error) {
				if scenario == "late_clock" && waits > 0 {
					return 3 * int64(time.Second), nil
				}
				return now, nil
			}, root: func(int) error { return nil }, drainCalls: drainCalls{validate: func() error { return nil }, scan: func(context.Context) error { return nil }, wait: func(s *unix.WaitStatus) (int, error) {
				waits++
				if scenario == "root_before_renew" && waits == 1 {
					*s = 0
					return 42, nil
				}
				if waits == 1 {
					if scenario == "expired_before_renew" {
						now = 3 * int64(time.Second)
					}
					return 0, nil
				}
				if scenario == "renew_before_exit" && waits == 2 {
					select {
					case e := <-r.result:
						if e != nil {
							t.Fatal(e)
						}
					default:
						t.Fatal("renewal did not linearize before subsequent root wait")
					}
				}
				if scenario == "root_before_renew" {
					return -1, unix.ECHILD
				}
				if waits == 2 {
					*s = 0
					return 42, nil
				}
				return -1, unix.ECHILD
			}}}
			o, err := m.superviseRootWith(ctx, 42, life, calls)
			if err != nil || o.RootPID != 42 {
				t.Fatalf("observation %+v %v", o, err)
			}
			if scenario != "renew_before_exit" {
				select {
				case e := <-r.result:
					if e == nil {
						t.Fatal("renewal resurrected expired/terminal or malformed command")
					}
				default:
					select {
					case <-life.done:
					default:
						t.Fatal("request neither failed nor closed")
					}
				}
			}
			if life.running || !life.closed {
				t.Fatal("lifecycle left live after drain")
			}
			if e := life.Renew(requestCtx, 4*int64(time.Second)); e == nil {
				t.Fatal("closed lifecycle renewed")
			}
		})
	}
}

func TestRootLifecycleOriginAndBoundedCaller(t *testing.T) {
	now, err := monotonicNS()
	if err != nil {
		t.Fatal(err)
	}
	life, err := NewRootLifecycle(now + int64(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = life.Renew(ctx, now+int64(2*time.Second)); err == nil {
		t.Fatal("unregistered renewal accepted")
	}
	copy := &RootLifecycle{self: life, pid: os.Getpid()}
	copy.mu.Lock()
	defer copy.mu.Unlock()
	if err = copy.Renew(ctx, now+int64(2*time.Second)); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatal("copy reached lock", err)
	}
	long, cancelLong := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLong()
	if err = life.Renew(long, now+int64(2*time.Second)); err == nil {
		t.Fatal("unbounded caller accepted")
	}
	for _, n := range []int64{0, -1, now + int64(31*time.Second)} {
		if _, err = NewRootLifecycle(n); err == nil {
			t.Fatal("invalid deadline accepted", n)
		}
	}
}
