//go:build linux

package etcd

import (
	"context"
	"errors"
	"io"
	"strings"

	"golang.org/x/sys/unix"
)

type workspaceSyncLocalHandle struct {
	fd   int
	path string
	stat unix.Stat_t
}

func workspaceSyncLocalScopedOpen(root int, path string, flags int, mode uint32) (int, error) {
	return unix.Openat2(root, path, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NONBLOCK), Mode: uint64(mode), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
}

func (t *taskWorkspaceSyncLocalTarget) localStat(fd int, directory bool) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if uint64(st.Dev) != t.pin.Dev || st.Uid != t.pin.UID || st.Gid != t.pin.GID || directory && st.Mode != unix.S_IFDIR|0700 || !directory && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1) {
		return st, ErrIdentityMismatch
	}
	return st, nil
}

func (t *taskWorkspaceSyncLocalTarget) checkLocalHandle(h workspaceSyncLocalHandle, directory bool) error {
	st, err := t.localStat(h.fd, directory)
	if err != nil || st.Dev != h.stat.Dev || st.Ino != h.stat.Ino {
		return errors.Join(ErrIdentityMismatch, err)
	}
	flags := unix.O_RDONLY
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := workspaceSyncLocalScopedOpen(t.fd, h.path, flags, 0)
	if err != nil {
		return err
	}
	current, statErr := t.localStat(fd, directory)
	closeErr := unix.Close(fd)
	if statErr != nil || closeErr != nil || current.Dev != st.Dev || current.Ino != st.Ino {
		return errors.Join(ErrIdentityMismatch, statErr, closeErr)
	}
	return nil
}

func (p *taskWorkspaceSyncLocalPut) localStage(parents []workspaceSyncLocalHandle, file *workspaceSyncLocalHandle) error {
	if err := p.guard(); err != nil {
		return err
	}
	for _, parent := range parents {
		if err := p.target.checkLocalHandle(parent, true); err != nil {
			return err
		}
	}
	if file != nil {
		return p.target.checkLocalHandle(*file, false)
	}
	return nil
}

func (s *PreparedTaskWorkspaceSyncSource) localCallComplete(stage string) {
	s.mu.Lock()
	completion := s.localIOComplete
	s.mu.Unlock()
	if completion != nil {
		completion(stage)
	}
}

func (s *PreparedTaskWorkspaceSyncSource) putLocal(ctx context.Context, body *taskWorkspaceSyncBody, target *taskWorkspaceSyncLocalTarget) (witness *taskWorkspaceSyncLocalWitness, resultErr error) {
	p, err := s.prepareLocalPut(ctx, body, target)
	if err != nil {
		return nil, err
	}
	if err := p.beginIO(); err != nil {
		p.release()
		return nil, err
	}
	// Cleanup completes before finishIO releases any source/body/target wait.
	defer p.finishIO()
	parents := make([]workspaceSyncLocalHandle, 0, 128)
	fd := -1
	defer func() {
		if fd >= 0 {
			resultErr = errors.Join(resultErr, unix.Close(fd))
		}
		for i := len(parents) - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unix.Close(parents[i].fd))
		}
		if resultErr != nil {
			witness = nil
		}
	}()
	e, d := body.entry, p.draft.record
	if !validWorkspaceSyncPath(e.path) || e.path != d.RelativePath || e.mode != d.Mode || e.size != d.Size || e.offset != d.Offset || e.sha256 != d.BodySHA256 {
		return nil, ErrInvalidRecord
	}
	components := strings.Split(e.path, "/")
	if len(components) > 128 {
		return nil, ErrInvalidRecord
	}
	for i := 0; i < len(components)-1; i++ {
		path := strings.Join(components[:i+1], "/")
		if err := p.localStage(parents, nil); err != nil {
			return nil, err
		}
		parentFD, err := workspaceSyncLocalScopedOpen(target.fd, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) {
			base := target.fd
			if len(parents) != 0 {
				base = parents[len(parents)-1].fd
			}
			if err := unix.Mkdirat(base, components[i], 0700); err != nil {
				return nil, err
			}
			s.localCallComplete("mkdir")
			if err := p.localStage(parents, nil); err != nil {
				return nil, err
			}
			if err := unix.Fsync(base); err != nil {
				return nil, err
			}
			parentFD, err = workspaceSyncLocalScopedOpen(target.fd, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		}
		if err != nil {
			return nil, err
		}
		st, err := target.localStat(parentFD, true)
		parents = append(parents, workspaceSyncLocalHandle{parentFD, path, st})
		if err != nil {
			return nil, err
		}
	}
	if err := p.localStage(parents, nil); err != nil {
		return nil, err
	}
	fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_WRONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, uint32(e.mode))
		s.localCallComplete("create")
	}
	if err != nil {
		return nil, err
	}
	st, err := target.localStat(fd, false)
	if err != nil {
		return nil, err
	}
	file := workspaceSyncLocalHandle{fd, e.path, st}
	if err := p.localStage(parents, &file); err != nil {
		return nil, err
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return nil, err
	}
	s.localCallComplete("truncate")
	var scratch [32768]byte
	for {
		n, readErr := body.readOwned(p.ctx, scratch[:])
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		for written := 0; written < n; {
			if err := p.localStage(parents, &file); err != nil {
				return nil, err
			}
			count, err := unix.Write(fd, scratch[written:n])
			s.localCallComplete("write")
			if err != nil || count == 0 {
				return nil, errors.Join(io.ErrShortWrite, err)
			}
			written += count
		}
		if readErr == io.EOF {
			break
		}
	}
	if err := p.localStage(parents, &file); err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, uint32(e.mode)); err != nil {
		return nil, err
	}
	if err := p.localStage(parents, &file); err != nil {
		return nil, err
	}
	if err := unix.Fsync(fd); err != nil {
		return nil, err
	}
	s.localCallComplete("fsync")
	if err := p.localStage(parents, &file); err != nil {
		return nil, err
	}
	final, err := target.localStat(fd, false)
	if err != nil || final.Size != e.size || final.Mode&07777 != uint32(e.mode) {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	closeErr := unix.Close(fd)
	fd = -1 // close failure is indeterminate: never retry a possibly recycled FD
	s.localCallComplete("close")
	if closeErr != nil {
		return nil, closeErr
	}
	for i := len(parents) - 1; i >= 0; i-- {
		if err := p.localStage(parents, nil); err != nil {
			return nil, err
		}
		if err := unix.Fsync(parents[i].fd); err != nil {
			return nil, err
		}
	}
	if err := p.localStage(parents, nil); err != nil {
		return nil, err
	}
	if err := unix.Fsync(target.fd); err != nil {
		return nil, err
	}
	fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	file.fd = fd
	if err := p.localStage(parents, &file); err != nil {
		return nil, err
	}
	current, err := target.localStat(fd, false)
	if err != nil || current.Size != final.Size || current.Mode != final.Mode {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	closeErr = unix.Close(fd)
	fd = -1
	if closeErr != nil {
		return nil, closeErr
	}
	for i := len(parents) - 1; i >= 0; i-- {
		err := unix.Close(parents[i].fd)
		parents = parents[:i]
		if err != nil {
			return nil, err
		}
	}
	if err := p.guard(); err != nil {
		return nil, err
	}
	w := &taskWorkspaceSyncLocalWitness{origin: p, bytes: e.size, sha256: e.sha256, dev: uint64(final.Dev), ino: final.Ino}
	w.self = w
	p.witness = w
	return w, nil
}
