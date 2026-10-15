//go:build linux

package etcd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func (f *taskWorkspaceSyncLocalFinalizer) metadataStat(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	var sx unix.Statx_t
	t := f.source.target
	if err := errors.Join(unix.Fstat(fd, &st), unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &sx)); err != nil {
		return st, err
	}
	kind := st.Mode & unix.S_IFMT
	if uint64(st.Dev) != t.pin.Dev || st.Uid != t.pin.UID || st.Gid != t.pin.GID || sx.Mask&unix.STATX_MNT_ID == 0 || sx.Mnt_id != t.pin.MountID || st.Mode&07000 != 0 || kind != unix.S_IFDIR && kind != unix.S_IFREG || kind == unix.S_IFREG && st.Nlink != 1 || kind == unix.S_IFDIR && st.Nlink < 2 {
		return st, ErrIdentityMismatch
	}
	return st, nil
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataStage(parent int, name, path string, anchor int, guard func() error) error {
	if err := guard(); err != nil {
		return err
	}
	var linked unix.Stat_t
	st, err := f.metadataStat(anchor)
	if err != nil {
		return err
	}
	if err := unix.Fstatat(parent, name, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if linked.Dev != st.Dev || linked.Ino != st.Ino || linked.Mode&unix.S_IFMT != st.Mode&unix.S_IFMT {
		return ErrIdentityMismatch
	}
	fd, err := workspaceSyncLocalScopedOpen(f.source.target.fd, path, unix.O_PATH, 0)
	if err != nil {
		return err
	}
	current, statErr := f.metadataStat(fd)
	closeErr := unix.Close(fd)
	if statErr != nil || closeErr != nil || current.Dev != st.Dev || current.Ino != st.Ino {
		return errors.Join(ErrIdentityMismatch, statErr, closeErr)
	}
	return nil
}

func (f *taskWorkspaceSyncLocalFinalizer) transcriptEffect(stage, path string, st unix.Stat_t, mode uint32) error {
	if f.transcript == nil {
		return ErrInvalidRecord
	}
	_, err := fmt.Fprintf(f.transcript, "%d:%s%d:%s:%d:%d:%d:%d;", len(stage), stage, len(path), path, st.Dev, st.Ino, st.Mode&unix.S_IFMT, mode)
	if f.source.localIOComplete != nil {
		f.source.localIOComplete(stage)
	}
	return err
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataChmod(ctx context.Context, parent int, name, path string, anchor int, mode uint32, guard func() error) (resultErr error) {
	if mode > 0777 || f.source.target.pin.Worker.PID != os.Getpid() {
		return ErrIdentityMismatch
	}
	st, err := f.metadataStat(anchor)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.Join(ErrIdentityMismatch, err)
	}
	proc, err := unix.Open("/proc", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(proc)) }()
	fd, err := unix.Openat2(proc, strconv.Itoa(os.Getpid())+"/fd", &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(fd)) }()
	var fs unix.Statfs_t
	var linked, root, current unix.Stat_t
	if err := errors.Join(unix.Fstatfs(proc, &fs), unix.Fstat(proc, &root), unix.Fstatat(unix.AT_FDCWD, "/proc", &current, unix.AT_SYMLINK_NOFOLLOW), unix.Fstatat(fd, strconv.Itoa(anchor), &linked, 0)); err != nil {
		return err
	}
	if fs.Type != unix.PROC_SUPER_MAGIC || root.Dev != current.Dev || root.Ino != current.Ino || linked.Dev != st.Dev || linked.Ino != st.Ino || linked.Mode != st.Mode {
		return ErrIdentityMismatch
	}
	if err := f.metadataStage(parent, name, path, anchor, guard); err != nil {
		return err
	}
	if err := unix.Fchmodat(fd, strconv.Itoa(anchor), mode, 0); err != nil {
		return err
	}
	if err := f.transcriptEffect("directory-chmod", path, st, mode); err != nil {
		return err
	}
	if err := f.metadataStage(parent, name, path, anchor, guard); err != nil {
		return err
	}
	after, err := f.metadataStat(anchor)
	if err != nil || after.Mode&07777 != mode {
		return errors.Join(ErrIdentityMismatch, err)
	}
	return ctx.Err()
}
