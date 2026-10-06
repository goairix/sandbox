//go:build linux && (amd64 || arm64)

package launcher

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// monitorPolicy builds native little-endian seccomp_data filters. Architecture
// is checked before nr; no compat ABI can reach the native allow path.
func monitorPolicy(arch string) ([]unix.SockFilter, error) {
	var audit, clone uint32
	var denied []uint32
	switch arch {
	case "amd64":
		audit = unix.AUDIT_ARCH_X86_64
		clone = 56
		denied = []uint32{272, 308, 101, 310, 311}
	case "arm64":
		audit = unix.AUDIT_ARCH_AARCH64
		clone = 220
		denied = []uint32{97, 268, 117, 270, 271}
	default:
		return nil, ErrUnsupported
	}
	load := func(offset uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset}
	}
	eq := func(n uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: n, Jt: jt, Jf: jf}
	}
	ret := func(action uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: action}
	}
	enosys := ret(unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS))
	eacces := ret(unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES))
	p := []unix.SockFilter{load(4), eq(audit, 1, 0), ret(unix.SECCOMP_RET_KILL_PROCESS), load(0)}
	if arch == "amd64" {
		p = append(p, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1}, enosys,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 512, Jf: 2},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 548, Jt: 1}, enosys)
	}
	p = append(p, eq(435, 0, 1), enosys) // clone3 has pointed-to flags, inaccessible to cBPF.
	for _, nr := range denied {
		p = append(p, eq(nr, 0, 1), eacces)
	}
	// Both native architectures place legacy clone flags in args[0]. Only its low
	// 32 bits contain the namespace flags; CLONE_NEWTIME also overlaps CSIGNAL.
	const namespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWTIME
	p = append(p, eq(clone, 0, 3), load(16), unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: namespaces, Jf: 1}, eacces, ret(unix.SECCOMP_RET_ALLOW))
	return p, nil
}

func installMonitorPolicy() error {
	filter, err := monitorPolicy(runtime.GOARCH)
	if err != nil {
		return err
	}
	program := &unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// AllThreadsSyscall6 is uintptrescapes: the program and backing instructions
	// stay at stable addresses through STW dispatch. A divergent thread result is
	// runtime-fatal; an ordinary failure is returned without a usable boundary.
	_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(program)), 0, 0, 0)
	runtime.KeepAlive(program)
	runtime.KeepAlive(filter)
	return setterError("all-thread monitor seccomp", errno)
}
