//go:build linux || darwin

package etcd

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type taskArchiveIdentity struct{ dev, ino uint64 }

func taskArchiveOpenRoot(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err == nil && (st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700) {
		err = errTaskArchive
	}
	if err == nil {
		// One original store owns this directory even across concurrent starts.
		// Nonblocking admission never waits on another store's physical work.
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err == nil {
		names, readErr := f.Readdirnames(1)
		if len(names) != 0 || readErr != io.EOF {
			err = errTaskArchive
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func taskArchiveFileIdentity(f *os.File) (taskArchiveIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return taskArchiveIdentity{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return taskArchiveIdentity{}, errTaskArchive
	}
	return taskArchiveIdentity{uint64(st.Dev), st.Ino}, nil
}

func taskArchiveCreate(root *os.File, name string) (*os.File, taskArchiveIdentity, error) {
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_EXCL|unix.O_CREAT|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, taskArchiveIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	id, err := taskArchiveFileIdentity(f)
	// Preserve unexpected identities; no speculative unlink or quota refund.
	return f, id, err
}

func taskArchiveCheck(root *os.File, name string, f *os.File, expected taskArchiveIdentity) error {
	id, err := taskArchiveFileIdentity(f)
	if err != nil || id != expected {
		return errors.Join(errTaskArchive, err)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(root.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Mode&07777 != 0600 || uint64(st.Dev) != expected.dev || st.Ino != expected.ino {
		return errTaskArchive
	}
	return nil
}

func taskArchiveRemove(root *os.File, name string, f *os.File, expected taskArchiveIdentity) error {
	if f == nil {
		return nil
	}
	if err := taskArchiveCheck(root, name, f, expected); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(root.Fd()), name, 0)
}
