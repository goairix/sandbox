//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type cgroupFacts struct {
	Identity descriptorIdentity `json:"identity"`
	MountID  uint64             `json:"mount_id"`
	Path     string             `json:"path"`
}

type cgroupAnchor struct {
	facts      cgroupFacts
	fd, events int
	membership string
}

func cgroupMembership(wire []byte) (string, error) {
	if len(wire) == 0 || len(wire) > 4096 || !strings.HasPrefix(string(wire), "0::/") || !strings.HasSuffix(string(wire), "\n") || strings.Count(string(wire), "\n") != 1 {
		return "", fmt.Errorf("ambiguous cgroup-v2 membership")
	}
	path := strings.TrimSuffix(string(wire[3:]), "\n")
	if path == "/" || strings.ContainsAny(path, "\x00\r\\") || strings.HasSuffix(path, "/") {
		return "", fmt.Errorf("shared/root cgroup refused")
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid kernel cgroup membership")
		}
	}
	return path, nil
}

func mountID(fd int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil || stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("kernel mount identity unavailable")
	}
	return stat.Mnt_id, nil
}

func openCgroup(membership []byte, namespace descriptorIdentity) (_ *cgroupAnchor, result error) {
	path, err := cgroupMembership(membership)
	if err != nil {
		return nil, err
	}
	root, err := unix.Open("/sys/fs/cgroup", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(root)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(root, &fs); err != nil || fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, fmt.Errorf("observer requires fixed cgroup-v2 mount")
	}
	fd, err := unix.Openat2(root, path[1:], &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, err
	}
	a := &cgroupAnchor{fd: fd, events: -1, membership: string(membership)}
	defer func() {
		if result != nil {
			a.close()
		}
	}()
	a.facts.Identity, err = descriptorStat(fd)
	if err != nil {
		return nil, err
	}
	a.facts.MountID, err = mountID(fd)
	if err != nil {
		return nil, err
	}
	a.facts.Path = path
	a.events, err = unix.Openat(fd, "cgroup.events", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := a.revalidate(namespace); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *cgroupAnchor) revalidate(namespace descriptorIdentity) error {
	if a == nil || a.fd < 0 || a.events < 0 {
		return fmt.Errorf("cgroup anchors unavailable")
	}
	identity, err := descriptorStat(a.fd)
	if err != nil || identity != a.facts.Identity {
		return fmt.Errorf("original cgroup identity changed")
	}
	mount, err := mountID(a.fd)
	if err != nil || mount != a.facts.MountID {
		return fmt.Errorf("original cgroup mount changed")
	}
	root, err := unix.Open("/sys/fs/cgroup", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	current, openErr := unix.Openat2(root, a.facts.Path[1:], &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	rootCloseErr := unix.Close(root)
	if openErr != nil || rootCloseErr != nil {
		return errors.Join(openErr, rootCloseErr)
	}
	currentIdentity, statErr := descriptorStat(current)
	currentMount, mountErr := mountID(current)
	closeErr := unix.Close(current)
	if statErr != nil || mountErr != nil || closeErr != nil || currentIdentity != a.facts.Identity || currentMount != a.facts.MountID {
		return fmt.Errorf("original cgroup path/mount replaced")
	}
	// Enrollment accepts a leaf with all kernel-attributed tasks in exactly the
	// held private PID namespace. Child cgroups or foreign tasks are refused;
	// approved runtime confinement prevents creating or escaping cgroups later.
	dup, err := unix.Openat(a.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "enrolled-cgroup")
	entries, readErr := dir.ReadDir(1025)
	closeErr = dir.Close()
	if err = errors.Join(readErr, closeErr); err != nil || len(entries) > 1024 {
		return fmt.Errorf("cgroup topology unavailable or exceeds bound")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("nested/shared cgroup topology refused")
		}
	}
	procs, err := readProcAt(a.fd, "cgroup.procs", 65536)
	if err != nil {
		return err
	}
	ids := strings.Fields(string(procs))
	if len(ids) == 0 || len(ids) > 4096 {
		return fmt.Errorf("enrolled cgroup task budget unavailable")
	}
	for _, id := range ids {
		pid, err := strconv.Atoi(id)
		if err != nil || pid <= 1 {
			return fmt.Errorf("invalid cgroup kernel PID")
		}
		var stat unix.Stat_t
		if err := unix.Stat("/proc/"+id+"/ns/pid", &stat); err != nil || (descriptorIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}) != namespace {
			return fmt.Errorf("foreign/escaped cgroup task refused")
		}
	}
	return nil
}

func (a *cgroupAnchor) empty() (bool, error) {
	identity, err := descriptorStat(a.fd)
	if err != nil || identity != a.facts.Identity {
		return false, fmt.Errorf("original cgroup anchor lost")
	}
	var buffer [4097]byte
	n, err := unix.Pread(a.events, buffer[:], 0)
	if err != nil || n == 0 || n > 4096 {
		return false, fmt.Errorf("original cgroup events unavailable")
	}
	seen, empty := false, false
	for _, line := range strings.Split(string(buffer[:n]), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "populated" {
			if seen || len(fields) != 2 || (fields[1] != "0" && fields[1] != "1") {
				return false, fmt.Errorf("invalid original cgroup populated fact")
			}
			seen, empty = true, fields[1] == "0"
		}
	}
	if !seen {
		return false, fmt.Errorf("missing original cgroup populated fact")
	}
	return empty, nil
}

func (a *cgroupAnchor) close() error {
	if a == nil {
		return nil
	}
	var result error
	for _, fd := range []*int{&a.fd, &a.events} {
		if *fd >= 0 {
			result = errors.Join(result, unix.Close(*fd))
			*fd = -1
		}
	}
	return result
}
