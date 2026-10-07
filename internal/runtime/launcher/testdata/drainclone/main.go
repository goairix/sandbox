//go:build linux && (amd64 || arm64)

// Fixed test-only clone adversary. This binary is never shipped by production.
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// rawClone returns only in the original parent. The child blocks entirely in
// assembly, with all catchable signals masked; it cannot enter Go signal code.
func rawClone(flags uintptr) int64

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 || os.Getuid() != 1000 || os.Getgid() != 1000 {
		return fmt.Errorf("fixed helper identity/argv mismatch")
	}
	var flags uintptr
	switch os.Args[1] {
	case "clone-zero":
		flags = 0
	case "clone-parent":
		flags = unix.CLONE_PARENT | uintptr(unix.SIGCHLD)
	default:
		return fmt.Errorf("unknown helper mode")
	}
	pid := rawClone(flags)
	if pid <= 0 {
		return fmt.Errorf("raw clone failed: %d", pid)
	}
	fmt.Printf("drain-helper parent=%d child=%d mode=%s\n", os.Getpid(), pid, os.Args[1])
	if flags == 0 {
		var info unix.Siginfo
		plain := unix.Waitid(unix.P_PID, int(pid), &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		wall := unix.Waitid(unix.P_PID, int(pid), &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT|unix.WALL, nil)
		fmt.Printf("clone waitid plain=%v WALL=%v signo=%d (both WNOWAIT)\n", plain, wall, info.Signo)
		if !errors.Is(plain, unix.ECHILD) || wall != nil || info.Signo != 0 {
			return fmt.Errorf("non-SIGCHLD wait contrast failed")
		}
	}
	fmt.Println("phase drain ready")
	for {
		time.Sleep(time.Hour)
	}
}
