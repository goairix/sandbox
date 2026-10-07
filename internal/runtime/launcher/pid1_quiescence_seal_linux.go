//go:build linux

package launcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func procOpenAt(dir int, name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(dir, name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func procReadAt(ctx context.Context, dir int, name string, limit int64) (string, error) {
	if ctx == nil {
		return "", ErrKernelUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := procOpenAt(dir, name, false)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", ErrKernelUnavailable
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return string(b), nil
}
func sealStat(f *os.File, typ int64) (uint64, uint64, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0, err
	}
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return 0, 0, err
	}
	if int64(fs.Type) != typ {
		return 0, 0, ErrUnsafeKernel
	}
	return uint64(st.Dev), st.Ino, nil
}
func mountIdentity(raw, mountpoint, kind string) (string, error) {
	var found string
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			if line == "" {
				continue
			}
			return "", ErrKernelUnavailable
		}
		sep := -1
		for i, v := range fields {
			if v == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) != sep+4 {
			return "", ErrKernelUnavailable
		}
		if fields[4] != mountpoint {
			continue
		}
		if fields[sep+1] != kind || found != "" {
			return "", ErrUnsafeKernel
		}
		// IDs, root, device, mountpoint and options pin the same original mount.
		found = line
	}
	if found == "" {
		return "", ErrKernelUnavailable
	}
	return found, nil
}
func canonicalCgroup(raw string) (string, error) {
	if !strings.HasSuffix(raw, "\n") || strings.Count(raw, "\n") != 1 || !strings.HasPrefix(raw, "0::/") {
		return "", ErrUnsafeKernel
	}
	name := strings.TrimSuffix(strings.TrimPrefix(raw, "0::"), "\n")
	if path.Clean(name) != name || strings.ContainsAny(name, "\x00\r\t\\") {
		return "", ErrUnsafeKernel
	}
	return raw, nil
}

// capturePID1Seal returns an operation-owned pinned proc directory. No fd escapes
// the observation. Fixed namespace magic links are opened only beneath PID 1.
func capturePID1Seal(ctx context.Context) (pid1Seal, *os.File, error) {
	var seal pid1Seal
	proc, err := procOpenAt(unix.AT_FDCWD, "/proc", true)
	if err != nil {
		return seal, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			proc.Close()
		}
	}()
	seal.procDev, seal.procIno, err = sealStat(proc, unix.PROC_SUPER_MAGIC)
	if err != nil {
		return seal, nil, err
	}
	pid, err := procOpenAt(int(proc.Fd()), "1", true)
	if err != nil {
		return seal, nil, err
	}
	defer pid.Close()
	membership, err := procReadAt(ctx, int(pid.Fd()), "cgroup", 65536)
	if err != nil {
		return seal, nil, err
	}
	seal.membership, err = canonicalCgroup(membership)
	if err != nil {
		return seal, nil, err
	}
	mounts, err := procReadAt(ctx, int(pid.Fd()), "mountinfo", 1<<20)
	if err != nil {
		return seal, nil, err
	}
	seal.procMount, err = mountIdentity(mounts, "/proc", "proc")
	if err != nil {
		return seal, nil, err
	}
	seal.cgroupMount, err = mountIdentity(mounts, "/sys/fs/cgroup", "cgroup2")
	if err != nil {
		return seal, nil, err
	}
	// fdinfo's mount ID binds our opened proc fd to the matched mount entry.
	self, err := procOpenAt(int(proc.Fd()), strconv.Itoa(os.Getpid()), true)
	if err != nil {
		return seal, nil, err
	}
	defer self.Close()
	fdinfo, err := procOpenAt(int(self.Fd()), "fdinfo", true)
	if err != nil {
		return seal, nil, err
	}
	defer fdinfo.Close()
	info, err := procReadAt(ctx, int(fdinfo.Fd()), strconv.Itoa(int(proc.Fd())), 4096)
	if err != nil {
		return seal, nil, err
	}
	mountID := strings.Fields(seal.procMount)[0]
	if !strings.Contains(info, "mnt_id:\t"+mountID+"\n") {
		return seal, nil, ErrUnsafeKernel
	}
	nsdir, err := procOpenAt(int(pid.Fd()), "ns", true)
	if err != nil {
		return seal, nil, err
	}
	defer nsdir.Close()
	fd, err := unix.Openat(int(nsdir.Fd()), "pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return seal, nil, err
	}
	ns := os.NewFile(uintptr(fd), "PID1 namespace")
	defer ns.Close()
	typ, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil || typ != unix.CLONE_NEWPID {
		return seal, nil, fmt.Errorf("%w: namespace type: %v", ErrUnsafeKernel, err)
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return seal, nil, err
	}
	seal.namespaceDev = uint64(stat.Dev)
	seal.namespaceIno = stat.Ino
	cg, err := procOpenAt(unix.AT_FDCWD, "/sys/fs/cgroup", true)
	if err != nil {
		return seal, nil, err
	}
	defer cg.Close()
	seal.cgroupDev, seal.cgroupIno, err = sealStat(cg, unix.CGROUP2_SUPER_MAGIC)
	if err != nil {
		return seal, nil, err
	}
	info, err = procReadAt(ctx, int(fdinfo.Fd()), strconv.Itoa(int(cg.Fd())), 4096)
	if err != nil {
		return seal, nil, err
	}
	if !strings.Contains(info, "mnt_id:\t"+strings.Fields(seal.cgroupMount)[0]+"\n") {
		return seal, nil, ErrUnsafeKernel
	}
	if err = ctx.Err(); err != nil {
		return seal, nil, err
	}
	ok = true
	return seal, proc, nil
}
