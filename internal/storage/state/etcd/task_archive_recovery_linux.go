//go:build linux

package etcd

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func taskArchiveRecoverySupported() error { return nil }

func taskArchiveRecoveryRootIdentity(f *os.File) (taskArchiveRecoveryRoot, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	var sx unix.Statx_t
	fd := int(f.Fd())
	if err := errors.Join(unix.Fstat(fd, &st), unix.Fstatfs(fd, &fs), unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &sx)); err != nil {
		return taskArchiveRecoveryRoot{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) || sx.Mask&unix.STATX_MNT_ID == 0 || sx.Mnt_id == 0 {
		return taskArchiveRecoveryRoot{}, errTaskArchive
	}
	fboot, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return taskArchiveRecoveryRoot{}, err
	}
	var boot [64]byte
	n, readErr := io.ReadFull(fboot, boot[:])
	closeErr := fboot.Close()
	if readErr != io.ErrUnexpectedEOF || closeErr != nil || n != 37 || boot[36] != '\n' {
		return taskArchiveRecoveryRoot{}, errors.Join(errTaskArchive, closeErr)
	}
	var ns unix.Stat_t
	if err := unix.Stat("/proc/self/ns/mnt", &ns); err != nil {
		return taskArchiveRecoveryRoot{}, err
	}
	r := taskArchiveRecoveryRoot{uint64(st.Dev), st.Ino, sx.Mnt_id, uint64(fs.Type), st.Uid, st.Gid, string(boot[:36]), ns.Ino}
	if !validPreparationUUID(r.BootID) || r.Dev == 0 || r.Ino == 0 || r.MountNamespace == 0 {
		return taskArchiveRecoveryRoot{}, errTaskArchive
	}
	return r, nil
}

func taskArchiveRecoveryCheckRoot(f *os.File, directory string, expected taskArchiveRecoveryRoot) error {
	actual, err := taskArchiveRecoveryRootIdentity(f)
	if err != nil || actual != expected {
		return errors.Join(errTaskArchive, err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(directory, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&07777 != 0700 || st.Uid != expected.UID || st.Gid != expected.GID || uint64(st.Dev) != expected.Dev || st.Ino != expected.Ino {
		return errTaskArchive
	}
	return nil
}

func taskArchiveRecoveryOpenRoot(directory string) (*os.File, taskArchiveRecoveryRoot, error) {
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, taskArchiveRecoveryRoot{}, err
	}
	f := os.NewFile(uintptr(fd), directory)
	r, err := taskArchiveRecoveryRootIdentity(f)
	if err == nil {
		err = taskArchiveRecoveryCheckRoot(f, directory, r)
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return nil, taskArchiveRecoveryRoot{}, errors.Join(err, f.Close())
	}
	return f, r, nil
}

func taskArchiveRecoveryOpen(root *os.File, name string) (*os.File, taskArchiveIdentity, error) {
	if strings.Contains(name, "/") {
		return nil, taskArchiveIdentity{}, errTaskArchive
	}
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, taskArchiveIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	id, err := taskArchiveFileIdentity(f)
	if err == nil {
		err = taskArchiveCheck(root, name, f, id)
	}
	if err != nil {
		return nil, taskArchiveIdentity{}, errors.Join(err, f.Close())
	}
	return f, id, nil
}

// Cold quarantine cannot exempt file contents from the precharged disk cap.
// Never open or follow an untrusted name merely to measure its content.
func taskArchiveRecoveryContentSize(root *os.File, name string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(root.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	if st.Size < 0 || st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return 0, errTaskArchive
	}
	return uint64(st.Size), nil
}

// No failure path unlinks or truncates an existing generation. An uncertain
// next generation remains charged and will be quarantined by a cold open.
func taskArchiveRecoveryCommit(root *os.File, base string, wire []byte) (taskArchiveIdentity, error) {
	f, id, err := taskArchiveCreate(root, base+".next")
	if err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return taskArchiveIdentity{}, err
	}
	n, err := f.Write(wire)
	if err == nil && n != len(wire) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = taskArchiveCheck(root, base+".next", f, id)
	}
	if err == nil {
		err = unix.Renameat(int(root.Fd()), base+".next", int(root.Fd()), base+".json")
	}
	if err == nil {
		err = root.Sync()
	}
	if err == nil {
		err = taskArchiveCheck(root, base+".json", f, id)
	}
	return id, errors.Join(err, f.Close())
}
