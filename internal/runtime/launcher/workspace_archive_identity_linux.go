//go:build linux

package launcher

import (
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func archiveIdentityRuntimeSupported() bool {
	return runtime.Version() == "go1.25.6" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
}

func (s *WorkspaceArchiveSource) openRoot(ctx context.Context) (*os.File, unix.Stat_t, error) {
	if s.readIdentity == nil {
		return openArchiveRoot()
	}
	var stat unix.Stat_t
	fd, err := s.userReadOpen(ctx, -1, "/workspace")
	if err != nil {
		return nil, stat, err
	}
	file := os.NewFile(uintptr(fd), "/workspace")
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, stat, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		file.Close()
		return nil, stat, ErrWorkspaceArchive
	}
	return file, stat, nil
}

func (s *WorkspaceArchiveSource) openChild(ctx context.Context, root int, path string) (*os.File, error) {
	if s.readIdentity == nil {
		return openArchiveChild(root, path)
	}
	fd, err := s.userReadOpen(ctx, root, path)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// All fields are read with raw current-thread syscalls. Fixed arrays bound
// storage before mutation; no proc parsing or general callback runs in scope.
type archiveThreadIdentity struct {
	uids, gids   [3]uint32
	fsuid, fsgid uint32
	groups       [64]uint32
	groupCount   uintptr
	caps         [2]unix.CapUserData
}

func rawArchiveIdentity(state *archiveThreadIdentity, capLast uint32) bool {
	*state = archiveThreadIdentity{}
	_, _, e := syscall.RawSyscall(unix.SYS_GETRESUID, uintptr(unsafe.Pointer(&state.uids[0])), uintptr(unsafe.Pointer(&state.uids[1])), uintptr(unsafe.Pointer(&state.uids[2])))
	if e != 0 {
		return false
	}
	_, _, e = syscall.RawSyscall(unix.SYS_GETRESGID, uintptr(unsafe.Pointer(&state.gids[0])), uintptr(unsafe.Pointer(&state.gids[1])), uintptr(unsafe.Pointer(&state.gids[2])))
	if e != 0 {
		return false
	}
	u, _, e := syscall.RawSyscall(unix.SYS_SETFSUID, ^uintptr(0), 0, 0)
	if e != 0 {
		return false
	}
	g, _, e := syscall.RawSyscall(unix.SYS_SETFSGID, ^uintptr(0), 0, 0)
	if e != 0 {
		return false
	}
	state.fsuid, state.fsgid = uint32(u), uint32(g)
	n, _, e := syscall.RawSyscall(unix.SYS_GETGROUPS, 64, uintptr(unsafe.Pointer(&state.groups[0])), 0)
	if e != 0 || n > 64 {
		return false
	}
	state.groupCount = n
	h := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	_, _, e = syscall.RawSyscall(unix.SYS_CAPGET, uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&state.caps[0])), 0)
	if e != 0 || state.uids != [3]uint32{} || state.gids != [3]uint32{} || state.caps != [2]unix.CapUserData{{Effective: 0xe0, Permitted: 0xe0, Inheritable: 0xe0}, {}} {
		return false
	}
	for _, pair := range [][2]uintptr{{unix.PR_GET_DUMPABLE, 0}, {unix.PR_GET_NO_NEW_PRIVS, 1}, {unix.PR_GET_SECUREBITS, 0}} {
		v, _, e := syscall.RawSyscall6(unix.SYS_PRCTL, pair[0], 0, 0, 0, 0, 0)
		if e != 0 || v != pair[1] {
			return false
		}
	}
	if capLast < 8 || capLast > 63 {
		return false
	}
	for cap := uint32(0); cap <= capLast; cap++ {
		v, _, e := syscall.RawSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_READ, uintptr(cap), 0, 0, 0, 0)
		if e != 0 || v != 0 {
			return false
		}
		v, _, e = syscall.RawSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, uintptr(cap), 0, 0, 0)
		if e != 0 || v != 0 {
			return false
		}
	}
	return true
}

// Unverified restoration must never release the dirty thread or deliver an FD.
// exit_group is process-wide and nonblocking; this has no injectable callback.
func archiveIdentityFatal() {
	for {
		syscall.RawSyscall(unix.SYS_EXIT_GROUP, 70, 0, 0)
	}
}

// userReadOpen only permits the two fixed readonly operations used by this
// source. Path/how and all credential storage exist before entering scope.
func (s *WorkspaceArchiveSource) userReadOpen(ctx context.Context, root int, path string) (fd int, err error) {
	b := s.origin
	identity := s.readIdentity
	if ctx == nil || b == nil || b.self != b || b.role != rolePID1 || b.pid != 1 || os.Getpid() != 1 || b.seal == nil || identity == nil || !archiveReadIdentityValid(identity.uid, identity.gid) || (root < 0 && path != "/workspace") {
		return -1, ErrWorkspaceArchive
	}
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return -1, err
	}
	how := unix.OpenHow{Flags: unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV}
	dirfd := unix.AT_FDCWD
	var before, entered, after archiveThreadIdentity
	fd = -1
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	// Changing filesystem IDs resets dumpability to suid_dumpable. It must
	// already be controlled at zero; a different value fails before mutation.
	dump, err := os.ReadFile("/proc/sys/fs/suid_dumpable")
	if err != nil || strings.TrimSpace(string(dump)) != "0" || !rawArchiveIdentity(&before, b.verified.CapLast) || before.fsuid != 0 || before.fsgid != 0 {
		return -1, ErrUnsafeKernel
	}
	defer func() {
		// Attempt every restoration even if an earlier setter is refused.
		_, _, uidErr := syscall.RawSyscall(unix.SYS_SETFSUID, 0, 0, 0)
		_, _, gidErr := syscall.RawSyscall(unix.SYS_SETFSGID, 0, 0, 0)
		_, _, groupsErr := syscall.RawSyscall(unix.SYS_SETGROUPS, before.groupCount, uintptr(unsafe.Pointer(&before.groups[0])), 0)
		verified := rawArchiveIdentity(&after, b.verified.CapLast)
		runtime.KeepAlive(&before)
		runtime.KeepAlive(name)
		runtime.KeepAlive(&how)
		if uidErr != 0 || gidErr != 0 || groupsErr != 0 || !verified || after != before {
			archiveIdentityFatal()
		}
	}()
	_, _, groupsErr := syscall.RawSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	_, _, gidErr := syscall.RawSyscall(unix.SYS_SETFSGID, uintptr(identity.gid), 0, 0)
	_, _, uidErr := syscall.RawSyscall(unix.SYS_SETFSUID, uintptr(identity.uid), 0, 0)
	// setfsuid/setfsgid can silently refuse. Full readback, including groups
	// and unchanged management capabilities, is required before the raw open.
	if groupsErr != 0 || gidErr != 0 || uidErr != 0 || !rawArchiveIdentity(&entered, b.verified.CapLast) || entered.fsuid != identity.uid || entered.fsgid != identity.gid || entered.groupCount != 0 {
		return -1, ErrUnsafeKernel
	}
	var opened uintptr
	var openErr syscall.Errno
	if root < 0 {
		opened, _, openErr = syscall.RawSyscall6(unix.SYS_OPENAT, uintptr(dirfd), uintptr(unsafe.Pointer(name)), uintptr(how.Flags)|unix.O_DIRECTORY, 0, 0, 0)
	} else {
		opened, _, openErr = syscall.RawSyscall6(unix.SYS_OPENAT2, uintptr(root), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
	}
	if openErr != 0 {
		return -1, openErr
	}
	// Deferred verified restoration happens before callers can receive this FD.
	return int(opened), nil
}
