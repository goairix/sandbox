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
	flags |= unix.O_CLOEXEC
	if flags&unix.O_PATH == 0 {
		flags |= unix.O_NONBLOCK // opening a hostile FIFO must never block
	}
	return unix.Openat2(root, path, &unix.OpenHow{Flags: uint64(flags), Mode: uint64(mode), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
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
	flags := unix.O_PATH
	if directory {
		flags = unix.O_RDONLY | unix.O_DIRECTORY
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

// executeWorkspaceSyncLocalPut owns only mechanical IO. Its caller must hold
// source/body/target registrations through this call and its final guard.
func executeWorkspaceSyncLocalPut(ctx context.Context, target *taskWorkspaceSyncLocalTarget, e workspaceSyncLocalFile, read func(context.Context, []byte) (int, error), guard func() error, completed func(string)) (result workspaceSyncLocalIOResult, resultErr error) {
	var err error
	stage := func(parents []workspaceSyncLocalHandle, file *workspaceSyncLocalHandle) error {
		if err := guard(); err != nil {
			return err
		}
		for _, parent := range parents {
			if err := target.checkLocalHandle(parent, true); err != nil {
				return err
			}
		}
		if file != nil {
			if err := target.checkLocalHandle(*file, false); err != nil {
				return err
			}
		}
		return errors.Join(ctx.Err(), target.ctx.Err())
	}
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
			result = workspaceSyncLocalIOResult{}
		}
	}()
	if !validWorkspaceSyncPath(e.path) || e.mode < 0 || e.mode > 0777 || e.size < 0 {
		return result, ErrInvalidRecord
	}
	components := strings.Split(e.path, "/")
	if len(components) > 128 {
		return result, ErrInvalidRecord
	}
	prefixEnd := 0
	for i := 0; i < len(components)-1; i++ {
		prefixEnd += len(components[i])
		path := e.path[:prefixEnd]
		prefixEnd++
		if err := stage(parents, nil); err != nil {
			return result, err
		}
		parentFD, err := workspaceSyncLocalScopedOpen(target.fd, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) {
			base := target.fd
			if len(parents) != 0 {
				base = parents[len(parents)-1].fd
			}
			if err := unix.Mkdirat(base, components[i], 0700); err != nil {
				return result, err
			}
			completed("mkdir")
			if err := stage(parents, nil); err != nil {
				return result, err
			}
			if err := unix.Fsync(base); err != nil {
				return result, err
			}
			parentFD, err = workspaceSyncLocalScopedOpen(target.fd, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		}
		if err != nil {
			return result, err
		}
		st, err := target.localStat(parentFD, true)
		parents = append(parents, workspaceSyncLocalHandle{parentFD, path, st})
		if err != nil {
			return result, err
		}
	}
	if err := stage(parents, nil); err != nil {
		return result, err
	}
	fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_WRONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, uint32(e.mode))
		completed("create")
	}
	if err != nil {
		return result, err
	}
	st, err := target.localStat(fd, false)
	if err != nil {
		return result, err
	}
	file := workspaceSyncLocalHandle{fd, e.path, st}
	if err := stage(parents, &file); err != nil {
		return result, err
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return result, err
	}
	completed("truncate")
	var scratch [32768]byte
	for {
		n, readErr := read(ctx, scratch[:])
		if readErr != nil && readErr != io.EOF {
			return result, readErr
		}
		for written := 0; written < n; {
			if err := stage(parents, &file); err != nil {
				return result, err
			}
			count, err := unix.Write(fd, scratch[written:n])
			completed("write")
			if err != nil || count == 0 {
				return result, errors.Join(io.ErrShortWrite, err)
			}
			written += count
		}
		if readErr == io.EOF {
			break
		}
	}
	if err := stage(parents, &file); err != nil {
		return result, err
	}
	if err := unix.Fchmod(fd, uint32(e.mode)); err != nil {
		return result, err
	}
	if err := stage(parents, &file); err != nil {
		return result, err
	}
	if err := unix.Fsync(fd); err != nil {
		return result, err
	}
	completed("fsync")
	if err := stage(parents, &file); err != nil {
		return result, err
	}
	final, err := target.localStat(fd, false)
	if err != nil || final.Size != e.size || final.Mode&07777 != uint32(e.mode) {
		return result, errors.Join(ErrIdentityMismatch, err)
	}
	closeErr := unix.Close(fd)
	fd = -1 // close failure is indeterminate: never retry a possibly recycled FD
	completed("close")
	if closeErr != nil {
		return result, closeErr
	}
	for i := len(parents) - 1; i >= 0; i-- {
		if err := stage(parents, nil); err != nil {
			return result, err
		}
		if err := unix.Fsync(parents[i].fd); err != nil {
			return result, err
		}
	}
	if err := stage(parents, nil); err != nil {
		return result, err
	}
	if err := unix.Fsync(target.fd); err != nil {
		return result, err
	}
	fd, err = workspaceSyncLocalScopedOpen(target.fd, e.path, unix.O_PATH, 0)
	if err != nil {
		return result, err
	}
	file.fd = fd
	if err := stage(parents, &file); err != nil {
		return result, err
	}
	current, err := target.localStat(fd, false)
	if err != nil || current.Size != final.Size || current.Mode != final.Mode {
		return result, errors.Join(ErrIdentityMismatch, err)
	}
	closeErr = unix.Close(fd)
	fd = -1
	if closeErr != nil {
		return result, closeErr
	}
	for i := len(parents) - 1; i >= 0; i-- {
		err := unix.Close(parents[i].fd)
		parents = parents[:i]
		if err != nil {
			return result, err
		}
	}
	if err := guard(); err != nil {
		return result, err
	}
	if err := errors.Join(ctx.Err(), target.ctx.Err()); err != nil {
		return result, err
	}
	return workspaceSyncLocalIOResult{bytes: e.size, sha256: e.sha256, dev: uint64(final.Dev), ino: final.Ino}, nil
}
