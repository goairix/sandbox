//go:build linux || darwin

package transferspool

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type fileIdentity struct{ dev, ino uint64 }

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	err = unix.Fstat(fd, &stat)
	if err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&07777 != 0700 || stat.Uid != uint32(os.Geteuid())) {
		err = ErrUnavailable
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func identity(stat unix.Stat_t) (fileIdentity, error) {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fileIdentity{}, ErrUnavailable
	}
	return fileIdentity{uint64(stat.Dev), stat.Ino}, nil
}

func fdIdentity(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	return identity(stat)
}

func checkEntry(root *os.File, name string, expected fileIdentity) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(root.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	got, err := identity(stat)
	if err != nil || got != expected {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}

func checkFile(root *os.File, name string, file *os.File, expected fileIdentity) error {
	got, err := fdIdentity(file)
	if err != nil || got != expected {
		return errors.Join(ErrUnavailable, err)
	}
	return checkEntry(root, name, expected)
}

func createFile(root *os.File, name string) (*os.File, fileIdentity, error) {
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	id, err := fdIdentity(f)
	return f, id, err
}

func removeFile(root *os.File, name string, file *os.File, expected fileIdentity) error {
	if file == nil {
		return nil
	}
	if err := checkFile(root, name, file, expected); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := checkEntry(root, name, expected); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(root.Fd()), name, 0); err != nil {
		return err
	}
	return root.Sync()
}

func validName(name string) bool {
	if !strings.HasPrefix(name, "http-download-") || len(name) != len("http-download-")+32 {
		return false
	}
	for _, c := range name[len("http-download-"):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func recoverStale(root *os.File) (err error) {
	names, err := root.Readdirnames(9)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) > 8 {
		return ErrUnavailable
	}
	if err == nil {
		extra, readErr := root.Readdirnames(1)
		if len(extra) != 0 || readErr != io.EOF {
			return errors.Join(ErrUnavailable, readErr)
		}
	}
	type stale struct {
		name     string
		file     *os.File
		identity fileIdentity
	}
	files := make([]stale, 0, len(names))
	defer func() {
		for _, old := range files {
			if old.file != nil {
				err = errors.Join(err, old.file.Close())
			}
		}
	}()
	// Validate the complete finite inventory before deleting the first entry.
	for _, name := range names {
		if !validName(name) {
			return ErrUnavailable
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(root.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		id, err := identity(stat)
		if err != nil {
			return err
		}
		fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDWR|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), name)
		files = append(files, stale{name, f, id})
		if err := checkFile(root, name, f, id); err != nil {
			return err
		}
	}
	for i := range files {
		old := &files[i]
		err := removeFile(root, old.name, old.file, old.identity)
		old.file = nil
		if err != nil {
			return err
		}
	}
	return nil
}
