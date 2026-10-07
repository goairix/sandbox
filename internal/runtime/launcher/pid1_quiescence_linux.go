//go:build linux

package launcher

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func (b *KernelBoundary) ObserveNoUserDescendants(ctx context.Context) (*UserNamespaceQuiescenceObservation, error) {
	if ctx == nil || b == nil || b.self != b || b.role != rolePID1 || b.pid != 1 || os.Getpid() != 1 || b.seal == nil {
		return nil, ErrKernelUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.ValidateCurrent(); err != nil {
		return nil, err
	}
	seal, proc, err := capturePID1Seal(ctx)
	if err != nil {
		return nil, err
	}
	defer proc.Close()
	if seal != *b.seal {
		return nil, ErrUnsafeKernel
	}
	count := 0
	seenSelf := false
	for {
		names, readErr := proc.Readdirnames(128)
		for _, name := range names {
			n, err := strconv.ParseUint(name, 10, 32)
			if err != nil {
				continue
			}
			if strconv.FormatUint(n, 10) != name || n == 0 {
				return nil, ErrUnsafeKernel
			}
			count++
			if count > 4096 {
				return nil, ErrKernelUnavailable
			}
			entry, err := procOpenAt(int(proc.Fd()), name, true)
			if err != nil {
				return nil, err
			}
			entry.Close()
			if n != 1 {
				return nil, ErrUnsafeKernel
			}
			seenSelf = true
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if !seenSelf {
		return nil, ErrKernelUnavailable
	}
	// Non-reaping: zero/no-event is NOT absence. ECHILD is required, including
	// clone children (__WALL); no cmd.Wait status is consumed by this observer.
	var info unix.Siginfo
	err = unix.Waitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT|unix.WALL, nil)
	if !errors.Is(err, unix.ECHILD) {
		return nil, ErrKernelUnavailable
	}
	if err = b.ValidateCurrent(); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	o := &UserNamespaceQuiescenceObservation{origin: b, pid: b.pid, seal: seal}
	o.self = o
	return o, nil
}
