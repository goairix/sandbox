package launcher

import (
	"context"
	"testing"
	"time"
)

func TestRootLifecycleReadiness(t *testing.T) {
	t.Run("registration-before-publication", func(t *testing.T) {
		l := newRootLifecycle(1)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- l.WaitRegistered(ctx) }()
		select {
		case err := <-done:
			t.Fatalf("unregistered returned %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		if err := l.bind(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		l.stop()
	})
	t.Run("closed-wins-ready", func(t *testing.T) {
		l := newRootLifecycle(1)
		if err := l.bind(); err != nil {
			t.Fatal(err)
		}
		l.stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for range 100 {
			if err := l.WaitRegistered(ctx); err == nil {
				t.Fatal("closed readiness succeeded")
			}
		}
	})
	t.Run("aborted-before-registration", func(t *testing.T) {
		l := newRootLifecycle(1)
		l.stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := l.WaitRegistered(ctx); err == nil {
			t.Fatal("aborted readiness succeeded")
		}
	})
	t.Run("timeout-and-cancel", func(t *testing.T) {
		l := newRootLifecycle(1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := l.WaitRegistered(ctx); err != context.DeadlineExceeded {
			t.Fatalf("timeout %v", err)
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
		cancel2()
		if err := l.WaitRegistered(ctx2); err != context.Canceled {
			t.Fatalf("canceled %v", err)
		}
	})
	t.Run("origin-before-lock-and-context-bound", func(t *testing.T) {
		l := newRootLifecycle(1)
		l.mu.Lock()
		copy := *l
		l.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- copy.WaitRegistered(ctx) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("copy accepted")
			}
		case <-ctx.Done():
			t.Fatal("copy used copied lock")
		}
		for _, v := range []*RootLifecycle{nil, {}, l} {
			if err := v.WaitRegistered(context.Background()); err == nil {
				t.Fatal("unbounded accepted")
			}
		}
		if err := l.WaitRegistered(nil); err == nil {
			t.Fatal("nil context accepted")
		}
	})
}
