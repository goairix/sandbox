//go:build linux

package etcd

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"golang.org/x/sys/unix"
)

func openTaskWorkspaceSyncLocalTarget(ctx context.Context, worker *launcher.KernelBoundary, plan TaskWorkspaceSyncPlan, root string) (*taskWorkspaceSyncLocalTarget, error) {
	if ctx == nil || ctx.Err() != nil || worker == nil || plan.validate() != nil || plan.workspace.provider != "local" || !validWorkspaceSyncLocalRoot(root) {
		return nil, ErrInvalidRecord
	}
	if err := worker.ValidateCurrent(); err != nil {
		return nil, err
	}
	w, err := worker.WorkspaceSyncWorkerReference()
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, errors.Join(ErrInvalidRecord, err)
	}
	pin, err := workspaceSyncLocalRootPin(fd, root, w)
	if err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	owned, cancel := context.WithCancel(ctx)
	t := &taskWorkspaceSyncLocalTarget{worker: worker, plan: plan, pin: pin, fd: fd, ctx: owned, cancel: cancel, closed: make(chan struct{})}
	t.self = t
	if err := t.check(ctx); err != nil {
		return nil, errors.Join(err, t.Close())
	}
	return t, nil
}

func workspaceSyncLocalRootPin(fd int, root string, w launcher.WorkspaceSyncWorkerReference) (taskWorkspaceSyncLocalAttribution, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	var sx unix.Statx_t
	p := taskWorkspaceSyncLocalAttribution{Worker: w, Root: root}
	if err := unix.Fstat(fd, &st); err != nil {
		return p, err
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return p, err
	}
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &sx); err != nil {
		return p, err
	}
	p.Dev, p.Ino, p.MountID, p.Filesystem, p.UID, p.GID = uint64(st.Dev), st.Ino, sx.Mnt_id, uint64(fs.Type), st.Uid, st.Gid
	var err error
	p.FilesystemName, err = workspaceSyncLocalMountFilesystem(p.MountID)
	if err != nil {
		return p, err
	}
	if st.Mode != unix.S_IFDIR|0700 || sx.Mask&unix.STATX_MNT_ID == 0 || p.Validate() != nil {
		return p, ErrInvalidRecord
	}
	return p, nil
}

// Magic 0xef53 is shared with ext2/ext3. The exact statx mount ID in this
// original worker namespace must also name an allowlisted filesystem.
func workspaceSyncLocalMountFilesystem(id uint64) (name string, resultErr error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 32768), 32768)
	for count := 0; scan.Scan() && count < 4096; count++ {
		fields := strings.Fields(scan.Text())
		if len(fields) < 10 {
			return "", ErrInvalidRecord
		}
		mountID, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return "", ErrInvalidRecord
		}
		if mountID != id {
			continue
		}
		for i := 6; i+3 < len(fields); i++ {
			if fields[i] == "-" {
				return fields[i+1], nil
			}
		}
		return "", ErrInvalidRecord
	}
	return "", errors.Join(ErrInvalidRecord, scan.Err())
}

// begin must be held, so Close cannot recycle fd during either observation.
func (t *taskWorkspaceSyncLocalTarget) check(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || t.ctx.Err() != nil {
		return ErrGuardExpired
	}
	if err := t.worker.ValidateCurrent(); err != nil {
		return err
	}
	w, err := t.worker.WorkspaceSyncWorkerReference()
	if err != nil || w != t.pin.Worker {
		return errors.Join(ErrIdentityMismatch, err)
	}
	pin, err := workspaceSyncLocalRootPin(t.fd, t.pin.Root, w)
	if err != nil || pin != t.pin {
		return errors.Join(ErrIdentityMismatch, err)
	}
	// Reopen the configured path readonly to detect rename/replacement as well
	// as changes to the original directory. Child IO uses the original fd.
	fd, err := unix.Openat2(unix.AT_FDCWD, t.pin.Root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return errors.Join(ErrIdentityMismatch, err)
	}
	current, pinErr := workspaceSyncLocalRootPin(fd, t.pin.Root, w)
	closeErr := unix.Close(fd)
	if pinErr != nil || closeErr != nil || current != t.pin {
		return errors.Join(ErrIdentityMismatch, pinErr, closeErr)
	}
	return errors.Join(ctx.Err(), t.ctx.Err())
}

func closeWorkspaceSyncLocalFD(fd int) error { return unix.Close(fd) }
