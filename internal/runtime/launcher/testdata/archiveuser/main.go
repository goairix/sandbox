//go:build linux

// archiveuser is a fixed-path test fixture, never a production archive worker.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func run() error {
	groups, err := unix.Getgroups()
	if err != nil || len(groups) != 0 || os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return fmt.Errorf("fixture requires actual user1000 without groups")
	}
	h := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var caps [2]unix.CapUserData
	// A non-root exec can retain inheritable bits; clear them before fixture IO.
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&caps[0])), 0)
	runtime.KeepAlive(&h)
	runtime.KeepAlive(&caps)
	if errno != 0 {
		return errno
	}
	if err := unix.Capget(&h, &caps[0]); err != nil || caps != [2]unix.CapUserData{} {
		return fmt.Errorf("fixture requires no capabilities")
	}
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil || len(threads) == 0 || len(threads) > 64 {
		return fmt.Errorf("fixture thread census unavailable")
	}
	for _, thread := range threads {
		status, err := os.ReadFile(filepath.Join("/proc/self/task", thread.Name(), "status"))
		if err != nil {
			return err
		}
		for _, field := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
			if !strings.Contains(string(status), field+":\t0000000000000000\n") {
				return fmt.Errorf("fixture thread capability is not zero")
			}
		}
	}
	if len(os.Args) != 2 {
		return fmt.Errorf("fixed prepare/clean action required")
	}
	switch os.Args[1] {
	case "prepare":
		if err := os.Mkdir("/workspace/private", 0700); err != nil {
			return err
		}
		if err := os.WriteFile("/workspace/private/content", []byte("user private physical content\n"), 0600); err != nil {
			return err
		}
		for _, path := range []string{"/workspace/private", "/workspace/private/content"} {
			var st unix.Stat_t
			if err := unix.Stat(path, &st); err != nil {
				return err
			}
			want := uint32(0600)
			if path == "/workspace/private" {
				want = 0700
			}
			if st.Uid != 1000 || st.Gid != 1000 || st.Mode&0777 != want {
				return fmt.Errorf("incorrect fixture metadata")
			}
			fmt.Printf("actual fixture path=%s uid=%d gid=%d mode=%o groups=empty capabilities=zero\n", path, st.Uid, st.Gid, st.Mode&0777)
		}
		return nil
	case "clean":
		return os.RemoveAll("/workspace/private")
	default:
		return fmt.Errorf("unknown fixed action")
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
