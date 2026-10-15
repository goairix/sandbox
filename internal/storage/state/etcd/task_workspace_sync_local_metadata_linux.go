//go:build linux

package etcd

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"golang.org/x/sys/unix"
)

func (f *taskWorkspaceSyncLocalFinalizer) restoreLedger(ctx context.Context) (resultErr error) {
	if len(f.progress.Ledger.Frames) == 0 {
		return nil
	}
	pathBytes, err := base64.StdEncoding.Strict().DecodeString(f.progress.Ledger.Path)
	if err != nil {
		return err
	}
	path := string(pathBytes)
	var fds, anchors [128]int
	var ends [128]int
	depth, parent, end, pinned := 0, f.source.target.fd, 0, 0
	guard := func() error { return f.metadataGuard(ctx) }
	defer func() {
		for i := depth - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unix.Close(fds[i]), unix.Close(anchors[i]))
		}
	}()
	for name := range strings.SplitSeq(path, "/") {
		end += len(name)
		anchor, err := workspaceSyncLocalScopedOpen(parent, name, unix.O_PATH, 0)
		if err != nil {
			return err
		}
		st, err := f.metadataStat(anchor)
		if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.Join(ErrIdentityMismatch, err, unix.Close(anchor))
		}
		ledger := pinned < len(f.progress.Ledger.Frames) && f.progress.Ledger.Frames[pinned][0] == uint64(end)
		if ledger {
			if st.Ino != f.progress.Ledger.Frames[pinned][1] {
				return errors.Join(ErrIdentityMismatch, unix.Close(anchor))
			}
			pinned++
		}
		if st.Mode&0777 != 0700 {
			kind, _ := taskArchiveIndex(ctx, f.source.index, "workspace/"+path[:end], 0, false)
			if !ledger && kind != tar.TypeDir {
				return errors.Join(ErrConflict, unix.Close(anchor))
			}
			if err := f.metadataChmod(ctx, parent, name, path[:end], anchor, 0700, guard); err != nil {
				return errors.Join(err, unix.Close(anchor))
			}
		}
		fd, err := workspaceSyncLocalScopedOpen(parent, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return errors.Join(err, unix.Close(anchor))
		}
		if depth == len(fds) {
			return errors.Join(ErrInvalidRecord, unix.Close(fd), unix.Close(anchor))
		}
		fds[depth], anchors[depth], ends[depth] = fd, anchor, end
		depth, parent, end = depth+1, fd, end+1
	}
	if pinned != len(f.progress.Ledger.Frames) {
		return ErrCorruptRecord
	}
	for depth > 0 {
		i := depth - 1
		base, start := f.source.target.fd, 0
		if i > 0 {
			base, start = fds[i-1], ends[i-1]+1
		}
		if pinned > 0 && f.progress.Ledger.Frames[pinned-1][0] == uint64(ends[i]) {
			mode := uint32(f.progress.Ledger.Frames[pinned-1][2])
			if err := f.metadataChmod(ctx, base, path[start:ends[i]], path[:ends[i]], anchors[i], mode, guard); err != nil {
				return err
			}
			if err := guard(); err != nil {
				return err
			}
			if err := unix.Fsync(fds[i]); err != nil {
				return err
			}
			pinned--
		}
		closeErr := errors.Join(unix.Close(fds[i]), unix.Close(anchors[i]))
		depth--
		if closeErr != nil {
			return closeErr
		}
	}
	next := f.progress
	next.Ledger = taskWorkspaceSyncLocalLedger{}
	return f.saveProgress(ctx, next)
}

func (f *taskWorkspaceSyncLocalFinalizer) metadataPass(ctx context.Context, modes bool) (resultErr error) {
	s := f.source
	cursor := newWorkspaceSyncArchiveCursor(s.manifest.Received.Bytes, s.manifest.Received.Entries, s.index, s.plan, s.readAt)
	guard := func() error { return f.metadataGuard(ctx) }
	type frame struct {
		fd, anchor, end int
		mode            uint32
	}
	var frames [128]frame
	path := make([]byte, 0, 4096)
	depth := 0
	defer func() {
		for i := depth - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unix.Close(frames[i].fd), unix.Close(frames[i].anchor))
		}
	}()
	finish := func() error {
		i := depth - 1
		base, start := s.target.fd, 0
		if i > 0 {
			base, start = frames[i-1].fd, frames[i-1].end+1
		}
		q := frames[i]
		if err := f.metadataChmod(ctx, base, string(path[start:q.end]), string(path[:q.end]), q.anchor, q.mode, guard); err != nil {
			return err
		}
		if f.result.UpdatedDirectoryModes == ^uint64(0) {
			return ErrInvalidRecord
		}
		f.result.UpdatedDirectoryModes++
		if err := guard(); err != nil {
			return err
		}
		if err := unix.Fsync(q.fd); err != nil {
			return err
		}
		if err := guard(); err != nil {
			return err
		}
		if err := unix.Fsync(base); err != nil {
			return err
		}
		closeErr := errors.Join(unix.Close(q.fd), unix.Close(q.anchor))
		depth--
		if depth > 0 {
			path = path[:frames[depth-1].end]
		} else {
			path = path[:0]
		}
		return closeErr
	}
	for {
		e, err := cursor.next(ctx)
		if err != nil && err != io.EOF {
			return err
		}
		if modes {
			for depth > 0 && (err == io.EOF || !strings.HasPrefix(e.path, string(path)+"/")) {
				if err := finish(); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			if err := guard(); err != nil {
				return err
			}
			return errors.Join(unix.Fsync(s.target.fd), guard())
		}
		if e.kind != tar.TypeDir {
			continue
		}
		if !modes {
			if err := f.ensureArchiveParents(ctx, e.path, true, guard); err != nil {
				return err
			}
			continue
		}
		if depth == len(frames) {
			return ErrInvalidRecord
		}
		base, name := s.target.fd, e.path
		if depth > 0 {
			base, name = frames[depth-1].fd, e.path[frames[depth-1].end+1:]
		}
		anchor, err := workspaceSyncLocalScopedOpen(base, name, unix.O_PATH, 0)
		if err != nil {
			return err
		}
		st, err := f.metadataStat(anchor)
		if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0777 != 0700 {
			return errors.Join(ErrIdentityMismatch, err, unix.Close(anchor))
		}
		fd, err := workspaceSyncLocalScopedOpen(base, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return errors.Join(err, unix.Close(anchor))
		}
		path = append(path[:0], e.path...)
		frames[depth] = frame{fd, anchor, len(path), uint32(e.mode)}
		depth++
	}
}

func (f *taskWorkspaceSyncLocalFinalizer) executeMetadata(ctx context.Context) error {
	s := f.source
	if err := s.target.begin(ctx); err != nil {
		return err
	}
	defer s.target.ops.Done()
	if err := f.restoreLedger(ctx); err != nil {
		return err
	}
	if err := f.metadataPass(ctx, false); err != nil {
		return err
	}
	if s.plan.deleteExtraneous {
		if err := f.walkMetadata(ctx, s.target.fd, ".", "", false, func() error { return f.metadataGuard(ctx) }); err != nil {
			return err
		}
	}
	if err := f.metadataPass(ctx, true); err != nil {
		return err
	}
	if len(f.progress.Ledger.Frames) != 0 {
		return ErrConflict
	}
	return f.metadataGuard(ctx)
}

// Full-only repair is authorized by the exact charged regular entry. It never
// allocates a metadata slot, so a legacy put occupying S=1 remains recoverable.
func (f *taskWorkspaceSyncLocalFinalizer) repairRegular(ctx context.Context, body *taskWorkspaceSyncRecoveryBody) (resultErr error) {
	s := f.source
	if !s.fullLocal || s.effectsSealed || s.validRecoveryBody(body, body.charge) != nil {
		return ErrInvalidRecord
	}
	guard := func() error { return s.guard(ctx) }
	if err := f.ensureArchiveParents(ctx, body.entry.path, false, guard); err != nil {
		return err
	}
	fd, err := workspaceSyncLocalScopedOpen(s.target.fd, body.entry.path, unix.O_PATH, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	st, statErr := f.metadataStat(fd)
	closeErr := unix.Close(fd)
	if statErr != nil || closeErr != nil {
		return errors.Join(statErr, closeErr)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return f.removeTree(ctx, body.entry.path, guard)
	}
	return guard()
}

func (f *taskWorkspaceSyncLocalFinalizer) ensureArchiveParents(ctx context.Context, path string, includeLeaf bool, guard func() error) (resultErr error) {
	var fds [128]int
	depth, parent := 0, f.source.target.fd
	defer func() {
		for i := depth - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unix.Close(fds[i]))
		}
	}()
	end := 0
	for name := range strings.SplitSeq(path, "/") {
		end += len(name)
		if end == len(path) && !includeLeaf {
			break
		}
		full := path[:end]
		kind, err := taskArchiveIndex(ctx, f.source.index, "workspace/"+full, 0, false)
		if err != nil || kind != '5' {
			return errors.Join(ErrCorruptRecord, err)
		}
		anchor, err := workspaceSyncLocalScopedOpen(parent, name, unix.O_PATH, 0)
		if err == nil {
			st, e := f.metadataStat(anchor)
			if e != nil {
				return errors.Join(e, unix.Close(anchor))
			}
			if st.Mode&unix.S_IFMT == unix.S_IFREG {
				err = f.metadataUnlink(parent, name, full, anchor, false, guard)
				if closeErr := unix.Close(anchor); err != nil || closeErr != nil {
					return errors.Join(err, closeErr)
				}
				anchor, err = -1, unix.ENOENT
			} else if st.Mode&0777 != 0700 {
				err = f.metadataChmod(ctx, parent, name, full, anchor, 0700, guard)
			}
			if anchor >= 0 {
				if closeErr := unix.Close(anchor); err != nil || closeErr != nil {
					return errors.Join(err, closeErr)
				}
			}
		}
		if errors.Is(err, unix.ENOENT) {
			if err := guard(); err != nil {
				return err
			}
			if err := unix.Mkdirat(parent, name, 0700); err != nil {
				return err
			}
			if f.result.CreatedDirectories == ^uint64(0) {
				return ErrInvalidRecord
			}
			f.result.CreatedDirectories++
			if err := f.transcriptEffect("mkdir", full, unix.Stat_t{}, 0700); err != nil {
				return err
			}
			if err := guard(); err != nil {
				return err
			}
			if err := unix.Fsync(parent); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		fd, err := workspaceSyncLocalScopedOpen(parent, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		if depth == len(fds) {
			return errors.Join(ErrInvalidRecord, unix.Close(fd))
		}
		fds[depth], depth, parent = fd, depth+1, fd
		if _, err := f.metadataStat(fd); err != nil {
			return err
		}
		end++
	}
	return guard()
}

// Readonly post-seal attribution never reopens a mutable target owner.
func (s *taskWorkspaceSyncRecoverySource) checkFinalizationRoot(ctx context.Context) error {
	if err := s.local(ctx); err != nil {
		return err
	}
	t := s.target
	if err := t.worker.ValidateCurrent(); err != nil {
		return err
	}
	w, err := t.worker.WorkspaceSyncWorkerReference()
	if err != nil || w != t.pin.Worker {
		return errors.Join(ErrIdentityMismatch, err)
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, t.pin.Root, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return err
	}
	pin, pinErr := workspaceSyncLocalRootPin(fd, t.pin.Root, w)
	closeErr := unix.Close(fd)
	if pinErr != nil || closeErr != nil || pin != t.pin {
		return errors.Join(ErrIdentityMismatch, pinErr, closeErr)
	}
	return s.local(ctx)
}
