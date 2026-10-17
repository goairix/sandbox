//go:build linux

package mounter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	// proc stat includes the kernel's task name and fixed numeric fields.
	// 16 KiB bounds parsing well above that kernel record's normal size.
	maxNativeStatBytes = 16 << 10
	// Linux boot_id is one canonical 36-byte UUID followed by a newline.
	maxNativeBootIDBytes = 37
)

// captureNativeOwner accepts no serialized identity or substitute syscall.
// Every acquisition is CLOEXEC and tied to the actual process's procfs entry.
// Partial acquisition always releases its own files before returning nil.
func captureNativeOwner() (_ *nativeOwner, result error) {
	if os.Getpid() != 1 {
		return nil, fmt.Errorf("native helper supervisor must be PID1")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	owner := &nativeOwner{}
	owner.self = owner
	defer func() {
		if result != nil {
			result = errors.Join(result, owner.close())
		}
	}()

	var err error
	owner.proc, err = openNativeFileAt(unix.AT_FDCWD, "/proc/self", unix.O_RDONLY|unix.O_DIRECTORY, unix.PROC_SUPER_MAGIC)
	if err != nil {
		return nil, fmt.Errorf("capture helper proc directory: %w", err)
	}
	owner.pidNamespace, owner.mountNamespace, owner.executable, err = openNativeProcessFiles(owner.proc)
	if err != nil {
		return nil, err
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return nil, fmt.Errorf("capture helper pidfd: %w", err)
	}
	owner.pidfd = os.NewFile(uintptr(fd), "native-helper-pidfd")
	owner.identity, err = readNativeIdentity(owner.proc, owner.pidNamespace, owner.mountNamespace, owner.executable)
	if err != nil {
		return nil, err
	}
	if err := owner.validateCurrent(); err != nil {
		return nil, err
	}
	return owner, nil
}

// validateCurrent compares both the retained objects and freshly opened current
// objects. Reading only retained namespace/executable FDs would miss a switch.
// Failure is sticky but never closes handles or changes Supervisor cleanup state.
func (owner *nativeOwner) validateCurrent() (result error) {
	if owner == nil {
		return fmt.Errorf("native helper owner is unavailable")
	}
	if owner.self != owner {
		return fmt.Errorf("native helper owner was copied")
	}
	if owner.closed || owner.revoked {
		return fmt.Errorf("native helper admission is sealed")
	}
	defer func() {
		if result != nil {
			owner.revoked = true
		}
	}()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getpid() != 1 || owner.identity.pid != os.Getpid() {
		return fmt.Errorf("native helper process identity changed")
	}
	if err := checkNativePIDFD(owner.pidfd); err != nil {
		return err
	}
	pinned, err := readNativeIdentity(owner.proc, owner.pidNamespace, owner.mountNamespace, owner.executable)
	if err != nil {
		return err
	}
	if pinned != owner.identity {
		return fmt.Errorf("native helper retained identity changed")
	}
	current, err := readCurrentNativeIdentity()
	if err != nil {
		return err
	}
	if current != owner.identity {
		return fmt.Errorf("native helper current incarnation changed")
	}
	return checkNativeThreadNamespaces(owner.identity)
}

// openNativeProcessFiles follows only the kernel's procfs namespace/exe links.
// On partial failure it closes its acquisitions and returns no file ownership.
func openNativeProcessFiles(proc *os.File) (pidNamespace, mountNamespace, executable *os.File, result error) {
	defer func() {
		if result != nil {
			for _, file := range []*os.File{executable, mountNamespace, pidNamespace} {
				if file != nil {
					result = errors.Join(result, file.Close())
				}
			}
			pidNamespace, mountNamespace, executable = nil, nil, nil
		}
	}()

	pidNamespace, result = openNativeFileAt(int(proc.Fd()), "ns/pid", unix.O_RDONLY, unix.NSFS_MAGIC)
	if result != nil {
		return nil, nil, nil, fmt.Errorf("capture helper PID namespace: %w", result)
	}
	mountNamespace, result = openNativeFileAt(int(proc.Fd()), "ns/mnt", unix.O_RDONLY, unix.NSFS_MAGIC)
	if result != nil {
		return pidNamespace, nil, nil, fmt.Errorf("capture helper mount namespace: %w", result)
	}
	executable, result = openNativeFileAt(int(proc.Fd()), "exe", unix.O_PATH, 0)
	if result != nil {
		return pidNamespace, mountNamespace, nil, fmt.Errorf("capture helper executable: %w", result)
	}
	return pidNamespace, mountNamespace, executable, nil
}

func readCurrentNativeIdentity() (nativeIdentity, error) {
	proc, err := openNativeFileAt(unix.AT_FDCWD, "/proc/self", unix.O_RDONLY|unix.O_DIRECTORY, unix.PROC_SUPER_MAGIC)
	if err != nil {
		return nativeIdentity{}, fmt.Errorf("inspect current helper proc directory: %w", err)
	}
	defer proc.Close()
	pidNamespace, mountNamespace, executable, err := openNativeProcessFiles(proc)
	if err != nil {
		return nativeIdentity{}, err
	}
	defer pidNamespace.Close()
	defer mountNamespace.Close()
	defer executable.Close()
	return readNativeIdentity(proc, pidNamespace, mountNamespace, executable)
}

func readNativeIdentity(proc, pidNamespace, mountNamespace, executable *os.File) (nativeIdentity, error) {
	var identity nativeIdentity
	var err error
	identity.proc, err = nativeFileID(proc, unix.PROC_SUPER_MAGIC, unix.S_IFDIR)
	if err != nil {
		return identity, fmt.Errorf("inspect retained helper proc: %w", err)
	}
	identity.pidNamespace, err = nativeFileID(pidNamespace, unix.NSFS_MAGIC, 0)
	if err != nil {
		return identity, fmt.Errorf("inspect retained PID namespace: %w", err)
	}
	identity.mountNamespace, err = nativeFileID(mountNamespace, unix.NSFS_MAGIC, 0)
	if err != nil {
		return identity, fmt.Errorf("inspect retained mount namespace: %w", err)
	}
	identity.executable, err = nativeFileID(executable, 0, unix.S_IFREG)
	if err != nil {
		return identity, fmt.Errorf("inspect retained executable: %w", err)
	}

	stat, err := readNativeFileAt(int(proc.Fd()), "stat", maxNativeStatBytes)
	if err != nil {
		return identity, fmt.Errorf("read retained helper stat: %w", err)
	}
	identity.pid, identity.startTime, err = parseNativeStat(stat)
	if err != nil {
		return identity, err
	}
	bootID, err := readNativeFileAt(unix.AT_FDCWD, "/proc/sys/kernel/random/boot_id", maxNativeBootIDBytes)
	if err != nil {
		return identity, fmt.Errorf("read helper boot ID: %w", err)
	}
	identity.bootID = strings.TrimSuffix(string(bootID), "\n")
	parsed, err := uuid.Parse(identity.bootID)
	if err != nil || parsed == uuid.Nil || parsed.String() != identity.bootID {
		return identity, fmt.Errorf("kernel boot ID is invalid")
	}
	return identity, nil
}

func parseNativeStat(raw []byte) (int, uint64, error) {
	text := string(raw)
	left, right := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if left < 0 || right <= left {
		return 0, 0, fmt.Errorf("kernel process stat is invalid")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:left]))
	if err != nil || pid != 1 {
		return 0, 0, fmt.Errorf("kernel process stat does not identify PID1")
	}
	// comm can itself contain spaces or parentheses. Fields after its final
	// ')' begin at stat field 3; starttime is field 22, hence index 19.
	fields := strings.Fields(text[right+1:])
	if len(fields) < 20 {
		return 0, 0, fmt.Errorf("kernel process stat lacks starttime")
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTime == 0 {
		return 0, 0, fmt.Errorf("kernel process starttime is invalid")
	}
	return pid, startTime, nil
}

func checkNativeThreadNamespaces(identity nativeIdentity) error {
	for _, namespace := range []struct {
		path     string
		expected nativeFileIdentity
	}{
		{"/proc/thread-self/ns/pid", identity.pidNamespace},
		{"/proc/thread-self/ns/mnt", identity.mountNamespace},
	} {
		file, err := openNativeFileAt(unix.AT_FDCWD, namespace.path, unix.O_RDONLY, unix.NSFS_MAGIC)
		if err != nil {
			return fmt.Errorf("inspect helper calling thread namespace: %w", err)
		}
		actual, inspectErr := nativeFileID(file, unix.NSFS_MAGIC, 0)
		closeErr := file.Close()
		if inspectErr != nil || closeErr != nil {
			return fmt.Errorf("inspect helper calling thread namespace: %w", errors.Join(inspectErr, closeErr))
		}
		if actual != namespace.expected {
			return fmt.Errorf("helper calling thread namespace changed")
		}
	}
	return nil
}

func checkNativePIDFD(file *os.File) error {
	if _, err := nativeFileID(file, 0, 0); err != nil {
		return fmt.Errorf("inspect helper pidfd: %w", err)
	}
	poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	if _, err := unix.Poll(poll, 0); err != nil {
		return fmt.Errorf("poll helper pidfd: %w", err)
	}
	if poll[0].Revents != 0 {
		return fmt.Errorf("original helper pidfd is no longer live")
	}
	return nil
}

func openNativeFileAt(directory int, path string, flags int, filesystem int64) (*os.File, error) {
	fd, err := unix.Openat(directory, path, flags|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "native-helper-kernel-file")
	if _, err := nativeFileID(file, filesystem, 0); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func nativeFileID(file *os.File, filesystem int64, kind uint32) (nativeFileIdentity, error) {
	if file == nil {
		return nativeFileIdentity{}, fmt.Errorf("original kernel handle is unavailable")
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nativeFileIdentity{}, err
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		return nativeFileIdentity{}, fmt.Errorf("kernel handle is not close-on-exec")
	}
	if kind != 0 && stat.Mode&unix.S_IFMT != kind {
		return nativeFileIdentity{}, fmt.Errorf("kernel handle has an unexpected file type")
	}
	if filesystem != 0 {
		var statfs unix.Statfs_t
		if err := unix.Fstatfs(fd, &statfs); err != nil {
			return nativeFileIdentity{}, err
		}
		if int64(statfs.Type) != filesystem {
			return nativeFileIdentity{}, fmt.Errorf("kernel handle has an unexpected filesystem type")
		}
	}
	return nativeFileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func readNativeFileAt(directory int, path string, limit int64) ([]byte, error) {
	file, err := openNativeFileAt(directory, path, unix.O_RDONLY|unix.O_NOFOLLOW, unix.PROC_SUPER_MAGIC)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || int64(len(raw)) > limit {
		return nil, fmt.Errorf("kernel identity record exceeds its bound or is empty")
	}
	return raw, nil
}
