package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

// This private control double proves command exit ordering only. It supplies
// no KernelBoundary, credentials, etcd session or physical cleanup evidence.
type cleanupWorkerControlFixture struct {
	done         chan struct{}
	closeStarted chan struct{}
	closeRelease chan struct{}
	terminalErr  error
	closeErr     error
	closes       int
}

func (f *cleanupWorkerControlFixture) Done() <-chan struct{} {
	return f.done
}

func (f *cleanupWorkerControlFixture) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return f.terminalErr
	}
}

func (f *cleanupWorkerControlFixture) Close() error {
	f.closes++
	if f.closeStarted != nil {
		close(f.closeStarted)
	}
	if f.closeRelease != nil {
		<-f.closeRelease
	}
	return f.closeErr
}

func TestCleanupWorkerSignalJoinsClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &cleanupWorkerControlFixture{
		done:         make(chan struct{}),
		closeStarted: make(chan struct{}),
		closeRelease: make(chan struct{}),
	}
	var release sync.Once
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		result <- awaitCleanupWorker(ctx, f)
		close(finished)
	}()
	defer func() {
		cancel()
		release.Do(func() { close(f.closeRelease) })
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("signal control goroutine did not join during test cleanup")
		}
	}()
	cancel()
	select {
	case <-f.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("signal did not start owned cleanup")
	}
	select {
	case err := <-result:
		t.Fatalf("command returned before Close joined: %v", err)
	default:
	}
	release.Do(func() { close(f.closeRelease) })
	select {
	case err := <-result:
		if err != nil || f.closes != 1 {
			t.Fatalf("joined shutdown: closes=%d, error=%v", f.closes, err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not return after Close joined")
	}
}

func TestCleanupWorkerUnexpectedExitFailsClosed(t *testing.T) {
	fatal := errors.New("original worker fatal")
	for name, terminal := range map[string]error{"fatal": fatal, "unexpected-clean": nil} {
		t.Run(name, func(t *testing.T) {
			f := &cleanupWorkerControlFixture{done: make(chan struct{}), terminalErr: terminal}
			close(f.done)
			err := awaitCleanupWorker(context.Background(), f)
			if err == nil || f.closes != 1 {
				t.Fatalf("unexpected worker exit accepted: closes=%d error=%v", f.closes, err)
			}
			if terminal != nil && !errors.Is(err, terminal) {
				t.Fatalf("original terminal error lost: %v", err)
			}
		})
	}
}

func TestCleanupWorkerCloseFailureIsFatal(t *testing.T) {
	closeErr := errors.New("owned dependency failed close")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &cleanupWorkerControlFixture{done: make(chan struct{}), closeErr: closeErr}
	if err := awaitCleanupWorker(ctx, f); !errors.Is(err, closeErr) || f.closes != 1 {
		t.Fatalf("shutdown error lost: closes=%d error=%v", f.closes, err)
	}
}

func TestCleanupWorkerShutdownCancellationLeaves(t *testing.T) {
	other := errors.New("provider close failed")
	for name, test := range map[string]struct {
		closeErr error
		wantErr  error
	}{
		"canceled":                      {closeErr: context.Canceled},
		"wrapped-canceled":              {closeErr: fmt.Errorf("worker shutdown: %w", context.Canceled)},
		"joined-canceled-only":          {closeErr: errors.Join(context.Canceled, fmt.Errorf("dependency stopped: %w", context.Canceled))},
		"canceled-and-unknown":          {closeErr: errors.Join(context.Canceled, state.ErrOutcomeUnknown), wantErr: state.ErrOutcomeUnknown},
		"wrapped-mixed":                 {closeErr: fmt.Errorf("close: %w", errors.Join(context.Canceled, other)), wantErr: other},
		"deadline-is-not-signal-cancel": {closeErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			f := &cleanupWorkerControlFixture{done: make(chan struct{}), closeErr: test.closeErr}
			err := awaitCleanupWorker(ctx, f)
			if test.wantErr == nil && err != nil {
				t.Fatalf("joined signal cancellation failed: %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("non-cancellation shutdown error discarded: %v", err)
			}
			if f.closes != 1 {
				t.Fatalf("shutdown did not join once: %d", f.closes)
			}
		})
	}
}
