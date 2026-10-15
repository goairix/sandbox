//go:build linux

package etcd

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/sys/unix"
)

type taskWorkspaceSyncLocalWalkFrame struct {
	fd, anchor int
	end        int
	cookie     int64
	remove     bool
	ledger     bool
	original   uint32
}

func (f *taskWorkspaceSyncLocalFinalizer) protectedPath(path string) bool {
	for _, excluded := range f.source.plan.exclude {
		if workspaceSyncExcluded(path, excluded) || workspaceSyncExcluded(excluded, path) {
			return true
		}
	}
	return false
}

type taskWorkspaceSyncLocalDirPage struct {
	scratch         [32768]byte
	fd, offset, end int
	cookie          int64
}

// Siblings consume one shared page. A depth switch or mutation invalidates it;
// only the native cookie survives in the bounded ancestor frame.
func (p *taskWorkspaceSyncLocalDirPage) next(fd int, cookie int64) (string, int64, error) {
	if p.fd != fd || p.cookie != cookie || p.offset == p.end {
		if position, err := unix.Seek(fd, cookie, io.SeekStart); err != nil || position != cookie {
			return "", 0, errors.Join(ErrIdentityMismatch, err)
		}
		n, err := unix.Getdents(fd, p.scratch[:])
		if err != nil {
			return "", 0, err
		}
		p.fd, p.cookie, p.offset, p.end = fd, cookie, 0, n
		if n == 0 {
			return "", cookie, io.EOF
		}
	}
	for p.offset < p.end {
		if p.end-p.offset < 20 {
			return "", 0, ErrIdentityMismatch
		}
		d := p.scratch[p.offset:p.end]
		size := int(binary.NativeEndian.Uint16(d[16:18]))
		next := int64(binary.NativeEndian.Uint64(d[8:16]))
		// Cookies are opaque: tmpfs legitimately returns descending values.
		// Require native seek round-trip and actual advancement, never numeric
		// ordering or a cookie persisted across a crash.
		if size < 20 || size > len(d) || next < 0 || next == cookie {
			return "", 0, fmt.Errorf("%w: getdents cookie %d after %d (record %d)", ErrIdentityMismatch, next, cookie, size)
		}
		end := 19
		for end < size && d[end] != 0 {
			end++
		}
		if end == size || end-19 > 255 {
			return "", 0, ErrIdentityMismatch
		}
		name := string(d[19:end])
		cookie, p.cookie, p.offset = next, next, p.offset+size
		if name == "." || name == ".." || binary.NativeEndian.Uint64(d[:8]) == 0 {
			continue
		}
		if !validWorkspaceSyncPath(name) || strings.Contains(name, "/") {
			return "", 0, ErrIdentityMismatch
		}
		return name, cookie, nil
	}
	// A page containing only dot entries has a valid advancing cookie.
	return "", cookie, nil
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataUnlink(parent int, name, path string, anchor int, directory bool, guard func() error) error {
	if f.protectedPath(path) {
		return ErrConflict
	}
	if err := f.metadataStage(parent, name, path, anchor, guard); err != nil {
		return err
	}
	st, err := f.metadataStat(anchor)
	if err != nil {
		return err
	}
	flags, stage := 0, "unlink"
	if directory {
		flags, stage = unix.AT_REMOVEDIR, "rmdir"
	}
	if err := unix.Unlinkat(parent, name, flags); err != nil {
		return err
	}
	if err := f.transcriptEffect(stage, path, st, 0); err != nil {
		return err
	}
	if f.result.RemovedEntries == ^uint64(0) {
		return ErrInvalidRecord
	}
	f.result.RemovedEntries++
	if err := guard(); err != nil {
		return err
	}
	if err := unix.Fsync(parent); err != nil {
		return err
	}
	var absent unix.Stat_t
	if err := unix.Fstatat(parent, name, &absent, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return errors.Join(ErrIdentityMismatch, err)
	}
	return guard()
}

// removeTree is confined to the exact intended regular-file path. Its
// exclusion precheck includes descendants before any destructive syscall.
func (f *taskWorkspaceSyncLocalFinalizer) removeTree(ctx context.Context, path string, guard func() error) (resultErr error) {
	if f.protectedPath(path) {
		return ErrConflict
	}
	root := f.source.target.fd
	parentPath, name := ".", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		parentPath, name = path[:i], path[i+1:]
	}
	parent, err := workspaceSyncLocalScopedOpen(root, parentPath, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(parent)) }()
	return f.walkMetadata(ctx, parent, name, path, true, guard)
}

func (f *taskWorkspaceSyncLocalFinalizer) walkMetadata(ctx context.Context, parent int, name, initial string, all bool, guard func() error) (resultErr error) {
	page := taskWorkspaceSyncLocalDirPage{fd: -1}
	// The fixed root frame is additional to at most 128 relative components.
	var frames [129]taskWorkspaceSyncLocalWalkFrame
	path := make([]byte, 0, 4096)
	path = append(path, initial...)
	depth := 0
	defer func() {
		for i := depth - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unix.Close(frames[i].fd), unix.Close(frames[i].anchor))
		}
	}()
	open := func(base int, child, full string, remove bool) error {
		if depth == len(frames) || len(path) > 4096 {
			return ErrInvalidRecord
		}
		anchor, err := workspaceSyncLocalScopedOpen(base, child, unix.O_PATH, 0)
		if err != nil {
			return err
		}
		st, err := f.metadataStat(anchor)
		if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.Join(ErrIdentityMismatch, err, unix.Close(anchor))
		}
		if st.Mode&0777 != 0700 {
			if !remove && f.protectedPath(full) {
				next := f.progress
				next.Ledger.Path = base64.StdEncoding.EncodeToString([]byte(full))
				next.Ledger.Frames = append(append([][3]uint64(nil), next.Ledger.Frames...), [3]uint64{uint64(len(full)), st.Ino, uint64(st.Mode & 0777)})
				if err := f.saveProgress(ctx, next); err != nil {
					return errors.Join(err, unix.Close(anchor))
				}
			}
			if err := f.metadataChmod(ctx, base, child, full, anchor, 0700, guard); err != nil {
				return errors.Join(err, unix.Close(anchor))
			}
		}
		fd, err := workspaceSyncLocalScopedOpen(base, child, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return errors.Join(err, unix.Close(anchor))
		}
		frames[depth] = taskWorkspaceSyncLocalWalkFrame{fd: fd, anchor: anchor, end: len(path), remove: remove, ledger: !remove && f.protectedPath(full) && st.Mode&0777 != 0700, original: st.Mode & 0777}
		depth++
		return nil
	}
	if err := open(parent, name, initial, all); err != nil {
		return err
	}
	for depth > 0 {
		frame := &frames[depth-1]
		if err := guard(); err != nil {
			return err
		}
		child, next, err := page.next(frame.fd, frame.cookie)
		frame.cookie = next
		if err == io.EOF {
			page.fd = -1
			base, leaf := parent, name
			if depth > 1 {
				start := frames[depth-2].end
				if start > 0 {
					start++
				}
				base, leaf = frames[depth-2].fd, string(path[start:])
			}
			if err := guard(); err != nil {
				return err
			}
			if err := unix.Fsync(frame.fd); err != nil {
				return err
			}
			if frame.remove {
				if err := f.metadataUnlink(base, leaf, string(path), frame.anchor, true, guard); err != nil {
					return err
				}
			}
			if frame.ledger {
				if err := f.metadataChmod(ctx, base, leaf, string(path), frame.anchor, frame.original, guard); err != nil {
					return err
				}
				if err := guard(); err != nil {
					return err
				}
				if err := unix.Fsync(frame.fd); err != nil {
					return err
				}
			}
			pop := frame.ledger
			closeErr := errors.Join(unix.Close(frame.fd), unix.Close(frame.anchor))
			depth--
			if closeErr != nil {
				return closeErr
			}
			if f.source.localIOComplete != nil {
				f.source.localIOComplete("metadata-close")
			}
			if err := guard(); err != nil {
				return err
			}
			if pop {
				next := f.progress
				next.Ledger.Frames = append([][3]uint64(nil), next.Ledger.Frames[:len(next.Ledger.Frames)-1]...)
				next.Ledger.Path = ""
				if len(next.Ledger.Frames) > 0 {
					next.Ledger.Path = base64.StdEncoding.EncodeToString(path[:next.Ledger.Frames[len(next.Ledger.Frames)-1][0]])
				}
				if err := f.saveProgress(ctx, next); err != nil {
					return err
				}
			}
			if depth > 0 {
				path = path[:frames[depth-1].end]
			}
			continue
		}
		if err != nil {
			return err
		}
		if child == "" {
			continue
		}
		length := len(path)
		if length > 0 {
			path = append(path, '/')
		}
		path = append(path, child...)
		if len(path) > 4096 || strings.Count(string(path), "/") >= 128 {
			return ErrInvalidRecord
		}
		full := string(path)
		excluded, err := f.source.plan.Excludes(full)
		if err != nil {
			return err
		}
		if excluded {
			path = path[:length]
			continue
		}
		kind, err := taskArchiveIndex(ctx, f.source.index, "workspace/"+full, 0, false)
		if err != nil && !errors.Is(err, errTaskArchive) {
			return err
		}
		remove := all || kind == 0 && !f.protectedPath(full)
		anchor, err := workspaceSyncLocalScopedOpen(frame.fd, child, unix.O_PATH, 0)
		if err != nil {
			return err
		}
		st, statErr := f.metadataStat(anchor)
		if statErr != nil {
			return errors.Join(statErr, unix.Close(anchor))
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			page.fd = -1
			if err := unix.Close(anchor); err != nil {
				return err
			}
			if err := open(frame.fd, child, full, remove); err != nil {
				return err
			}
			continue
		}
		if remove {
			page.fd = -1
			err = f.metadataUnlink(frame.fd, child, full, anchor, false, guard)
		}
		if err := errors.Join(err, unix.Close(anchor)); err != nil {
			return err
		}
		path = path[:length]
	}
	return guard()
}
