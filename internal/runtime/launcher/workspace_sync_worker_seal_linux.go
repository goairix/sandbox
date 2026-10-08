//go:build linux

package launcher

import (
	"context"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The final ')' terminates comm, whose contents may include spaces and ')'.
// Fields after it begin with state (field 3); starttime is field 22.
func parseWorkspaceSyncWorkerStat(raw string, self int) (uint64, error) {
	if len(raw) == 0 || len(raw) > 16384 || self < 1 || strings.ContainsAny(raw, "\x00\r") || !strings.HasSuffix(raw, "\n") {
		return 0, ErrUnsafeKernel
	}
	open := strings.Index(raw, " (")
	close := strings.LastIndex(raw, ") ")
	if open < 1 || close < open+2 {
		return 0, ErrUnsafeKernel
	}
	pid, err := parseUnsigned(raw[:open], 10, 31)
	if err != nil || pid == 0 || pid != uint64(self) {
		return 0, ErrUnsafeKernel
	}
	fields := strings.Fields(raw[close+2:])
	if len(fields) < 20 || len(fields[0]) != 1 || !strings.Contains("RSDZTWtXxKPI", fields[0]) {
		return 0, ErrUnsafeKernel
	}
	start, err := parseUnsigned(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, ErrUnsafeKernel
	}
	return start, nil
}

// All paths are fixed relative to an observed procfs and our numeric self PID.
// Only the fixed namespace/exe magic links are followed. Every FD is CLOEXEC
// and closed inside this observation; no caller path or FD escapes.
func captureWorkspaceSyncWorkerSeal(s KernelSnapshot) (workspaceSyncWorkerSeal, error) {
	var seal workspaceSyncWorkerSeal
	if s.PID != os.Getpid() || validateKernelSnapshot(s, roleWorkspaceSyncWorker, false) != nil {
		return seal, ErrUnsafeKernel
	}
	proc, err := procOpenAt(unix.AT_FDCWD, "/proc", true)
	if err != nil {
		return seal, err
	}
	defer proc.Close()
	seal.procDev, seal.procIno, err = sealStat(proc, unix.PROC_SUPER_MAGIC)
	if err != nil {
		return seal, err
	}
	self, err := procOpenAt(int(proc.Fd()), strconv.Itoa(os.Getpid()), true)
	if err != nil {
		return seal, err
	}
	defer self.Close()
	if _, _, err := sealStat(self, unix.PROC_SUPER_MAGIC); err != nil {
		return seal, err
	}
	r := &seal.reference
	r.PID, r.UID, r.GID = s.PID, s.UIDs[0], s.GIDs[0]
	raw, err := procReadAt(context.Background(), int(self.Fd()), "stat", 16384)
	if err != nil {
		return seal, err
	}
	r.StartTicks, err = parseWorkspaceSyncWorkerStat(raw, s.PID)
	if err != nil {
		return seal, err
	}
	// Open each fixed directory component without following caller-controlled links.
	sys, err := procOpenAt(int(proc.Fd()), "sys", true)
	if err != nil {
		return seal, err
	}
	defer sys.Close()
	kernel, err := procOpenAt(int(sys.Fd()), "kernel", true)
	if err != nil {
		return seal, err
	}
	defer kernel.Close()
	boot, err := procOpenAt(int(kernel.Fd()), "random", true)
	if err != nil {
		return seal, err
	}
	defer boot.Close()
	raw, err = procReadAt(context.Background(), int(boot.Fd()), "boot_id", 64)
	if err != nil || len(raw) != 37 || raw[36] != '\n' || !workerUUID(raw[:36], false) {
		return seal, ErrUnsafeKernel
	}
	r.BootID = raw[:36]
	// dumpable=0 makes this proc directory root-owned/read-restricted. O_PATH
	// pins the fixed directory without requiring directory read permission;
	// relative namespace opens still enforce actual self magic-link checks.
	nsfd, err := unix.Openat(int(self.Fd()), "ns", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return seal, unavailable("worker self namespace directory", err)
	}
	nsdir := os.NewFile(uintptr(nsfd), "worker self namespace directory")
	defer nsdir.Close()
	if _, _, err := sealStat(nsdir, unix.PROC_SUPER_MAGIC); err != nil {
		return seal, err
	}
	for _, n := range []struct {
		name     string
		typ      int
		dev, ino *uint64
	}{
		{"pid", unix.CLONE_NEWPID, &r.PIDNamespaceDev, &r.PIDNamespaceIno},
		{"mnt", unix.CLONE_NEWNS, &r.MountNamespaceDev, &r.MountNamespaceIno},
	} {
		fd, err := unix.Openat(int(nsdir.Fd()), n.name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return seal, unavailable("worker self namespace "+n.name, err)
		}
		f := os.NewFile(uintptr(fd), n.name)
		dev, ino, statErr := sealStat(f, unix.NSFS_MAGIC)
		typ, typeErr := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
		closeErr := f.Close()
		if statErr != nil || typeErr != nil || closeErr != nil || typ != n.typ {
			return seal, ErrUnsafeKernel
		}
		*n.dev, *n.ino = dev, ino
	}
	fd, err := unix.Openat(int(self.Fd()), "exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return seal, unavailable("worker self executable", err)
	}
	exe := os.NewFile(uintptr(fd), "worker executable")
	defer exe.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return seal, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return seal, ErrUnsafeKernel
	}
	r.ExecutableDev, r.ExecutableIno = uint64(st.Dev), st.Ino
	return seal, nil
}
