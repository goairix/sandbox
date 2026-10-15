//go:build linux

package etcd

import (
	"context"
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// This private handle only exists inside a registered fresh charged repair.
// Its O_PATH inode stays open through the common executor, so permission repair
// and every subsequent path observation refer to the same owned regular file.
type taskWorkspaceSyncRecoveryPermission struct {
	source           *taskWorkspaceSyncRecoverySource
	anchor           workspaceSyncLocalHandle
	procRoot, procFD int
}

func (p *taskWorkspaceSyncRecoveryPermission) close() error {
	var err error
	for _, fd := range []*int{&p.procFD, &p.procRoot, &p.anchor.fd} {
		if *fd >= 0 {
			owned := *fd
			*fd = -1 // even an indeterminate Close is never retried on a reused FD
			err = errors.Join(err, unix.Close(owned))
		}
	}
	return err
}

func (p *taskWorkspaceSyncRecoveryPermission) check(ctx context.Context) error {
	if err := p.source.local(ctx); err != nil {
		return err
	}
	for _, fd := range []int{p.procRoot, p.procFD} {
		var fs unix.Statfs_t
		var st unix.Stat_t
		if err := errors.Join(unix.Fstatfs(fd, &fs), unix.Fstat(fd, &st)); err != nil {
			return err
		}
		if fs.Type != unix.PROC_SUPER_MAGIC || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrIdentityMismatch
		}
	}
	var root, current, directory, self, linked unix.Stat_t
	if err := errors.Join(unix.Fstat(p.procRoot, &root), unix.Fstatat(unix.AT_FDCWD, "/proc", &current, unix.AT_SYMLINK_NOFOLLOW), unix.Fstat(p.procFD, &directory), unix.Fstatat(p.procFD, strconv.Itoa(p.procFD), &self, 0), unix.Fstatat(p.procFD, strconv.Itoa(p.anchor.fd), &linked, 0)); err != nil {
		return err
	}
	if root.Dev != current.Dev || root.Ino != current.Ino || current.Mode&unix.S_IFMT != unix.S_IFDIR || directory.Dev != self.Dev || directory.Ino != self.Ino || self.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrIdentityMismatch
	}
	st := p.anchor.stat
	if linked.Dev != st.Dev || linked.Ino != st.Ino || linked.Uid != st.Uid || linked.Gid != st.Gid || linked.Mode&unix.S_IFMT != unix.S_IFREG || linked.Nlink != 1 {
		return ErrIdentityMismatch
	}
	if err := p.source.target.checkLocalHandle(p.anchor, false); err != nil {
		return err
	}
	return p.source.local(ctx)
}

func (p *taskWorkspaceSyncRecoveryPermission) guard(ctx context.Context) error {
	if err := p.source.guard(ctx); err != nil {
		return err
	}
	return p.check(ctx)
}

func (s *taskWorkspaceSyncRecoverySource) prepareLocalPermission(ctx context.Context, body *taskWorkspaceSyncRecoveryBody) (permission *taskWorkspaceSyncRecoveryPermission, resultErr error) {
	if err := s.validRecoveryBody(body, body.charge); err != nil {
		return nil, err
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	fd, err := workspaceSyncLocalScopedOpen(s.target.fd, body.entry.path, unix.O_PATH, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &taskWorkspaceSyncRecoveryPermission{source: s, anchor: workspaceSyncLocalHandle{fd: fd, path: body.entry.path}, procRoot: -1, procFD: -1}
	defer func() {
		if permission == nil {
			resultErr = errors.Join(resultErr, p.close())
		}
	}()
	p.anchor.stat, err = s.target.localStat(fd, false)
	if err != nil {
		return nil, err
	}
	if p.anchor.stat.Mode&0200 != 0 {
		return nil, nil
	}
	if s.target.pin.Worker.PID != os.Getpid() {
		return nil, ErrIdentityMismatch
	}
	p.procRoot, err = unix.Open("/proc", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	// Fixed numeric self components, never a caller-controlled symlink or a
	// substituted /proc/self path. O_PATH also works with dumpable=0 proc dirs.
	p.procFD, err = unix.Openat2(p.procRoot, strconv.Itoa(os.Getpid())+"/fd", &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, err
	}
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	// The only followed magic link is this private, still-held fd-number inode
	// anchor on verified procfs. flags=0 uses longstanding fchmodat, not 6.6's
	// fchmodat2/AT_EMPTY_PATH. No mutable workspace pathname is passed to chmod.
	temporary := uint32(body.entry.mode) | 0200
	if err := unix.Fchmodat(p.procFD, strconv.Itoa(p.anchor.fd), temporary, 0); err != nil {
		return nil, err
	}
	if s.localIOComplete != nil {
		s.localIOComplete("permission-chmod")
	}
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	actual, err := s.target.localStat(p.anchor.fd, false)
	if err != nil || actual.Mode&07777 != temporary {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	return p, nil
}
