//go:build linux

package launcher

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// OpenWorkspaceArchiveAsUser selects readonly filesystem access policy. The
// caller must obtain uid/gid from its authenticated activated user identity;
// these parameters and the resulting summary grant no external authority.
func (b *KernelBoundary) OpenWorkspaceArchiveAsUser(ctx context.Context, observation *UserNamespaceQuiescenceObservation, uid, gid uint32) (*WorkspaceArchiveSource, error) {
	if !archiveReadIdentityValid(uid, gid) {
		return nil, ErrWorkspaceArchive
	}
	return b.openWorkspaceArchive(ctx, observation, &archiveReadIdentity{uid: uid, gid: gid})
}

func (b *KernelBoundary) OpenWorkspaceArchive(ctx context.Context, observation *UserNamespaceQuiescenceObservation) (*WorkspaceArchiveSource, error) {
	return b.openWorkspaceArchive(ctx, observation, nil)
}

func (b *KernelBoundary) openWorkspaceArchive(ctx context.Context, observation *UserNamespaceQuiescenceObservation, identity *archiveReadIdentity) (*WorkspaceArchiveSource, error) {
	if ctx == nil || b == nil || b.self != b || b.role != rolePID1 || b.pid != 1 || os.Getpid() != 1 || b.seal == nil || observation == nil || observation.self != observation || observation.origin != b || observation.pid != 1 || observation.seal != *b.seal {
		return nil, ErrWorkspaceArchive
	}
	if err := observation.RevalidateCurrent(ctx); err != nil {
		return nil, err
	}
	// Original revalidation already rejects CGO via AllThreadsSyscall. Check
	// the reviewed runtime template and syscall ABI once, before any mutation.
	if identity != nil && !archiveIdentityRuntimeSupported() {
		return nil, ErrUnsupported
	}
	s := &WorkspaceArchiveSource{origin: b, observation: observation, readIdentity: identity, limits: productionArchiveLimits()}
	root, stat, err := s.openRoot(ctx)
	if err != nil {
		return nil, err
	}
	// There is deliberately no fallback when native confinement is unavailable.
	probe, err := s.openChild(ctx, int(root.Fd()), ".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if err := probe.Close(); err != nil {
		root.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		root.Close()
		return nil, err
	}
	s.root, s.dev, s.ino = root, uint64(stat.Dev), stat.Ino
	s.self = s
	return s, nil
}

func (s *WorkspaceArchiveSource) writeArchive(ctx context.Context, dst io.Writer) (WorkspaceArchiveSummary, error) {
	zero := WorkspaceArchiveSummary{}
	if ctx == nil || dst == nil || s.observation == nil || s.observation.origin != s.origin {
		return zero, ErrWorkspaceArchive
	}
	if err := s.observation.RevalidateCurrent(ctx); err != nil {
		return zero, err
	}
	if err := s.validateRootContext(ctx); err != nil {
		return zero, err
	}
	output := &archiveOutput{ctx: ctx, dst: dst, hash: sha256.New(), limit: s.limits.bytes}
	tw := tar.NewWriter(output)
	walk := archiveWalk{source: s, ctx: ctx, tar: tw, buffer: make([]byte, 32*1024)}
	var stat unix.Stat_t
	if err := unix.Fstat(int(s.root.Fd()), &stat); err != nil {
		return zero, err
	}
	if err := walk.directory(s.root, "", "workspace/", 0, &stat); err != nil {
		return zero, err
	}
	if err := tw.Close(); err != nil {
		return zero, err
	}
	if err := s.observation.RevalidateCurrent(ctx); err != nil {
		return zero, err
	}
	if err := s.validateRootContext(ctx); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	var digest [32]byte
	copy(digest[:], output.hash.Sum(nil))
	return WorkspaceArchiveSummary{ArchiveBytes: output.bytes, EntryCount: walk.entries, SHA256: digest}, nil
}

func openArchiveRoot() (*os.File, unix.Stat_t, error) {
	var stat unix.Stat_t
	fd, err := unix.Open("/workspace", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, stat, err
	}
	file := os.NewFile(uintptr(fd), "/workspace")
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, stat, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		file.Close()
		return nil, stat, ErrWorkspaceArchive
	}
	return file, stat, nil
}

func openArchiveChild(root int, path string) (*os.File, error) {
	fd, err := unix.Openat2(root, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (s *WorkspaceArchiveSource) validateRoot() error {
	return s.validateRootContext(context.Background())
}

func (s *WorkspaceArchiveSource) validateRootContext(ctx context.Context) error {
	current, stat, err := s.openRoot(ctx)
	if err != nil {
		return err
	}
	closeErr := current.Close()
	if uint64(stat.Dev) != s.dev || stat.Ino != s.ino {
		return fmt.Errorf("%w: fixed root identity changed", ErrWorkspaceArchive)
	}
	return closeErr
}

type archiveWalk struct {
	source  *WorkspaceArchiveSource
	ctx     context.Context
	tar     *tar.Writer
	buffer  []byte
	entries uint64
}

func (w *archiveWalk) header(name string, depth int, stat *unix.Stat_t, kind byte) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := w.source.limits.checkEntry(depth, len(name), w.entries); err != nil {
		return err
	}
	w.entries++
	size := int64(0)
	if kind == tar.TypeReg {
		size = stat.Size
		if size < 0 {
			return ErrWorkspaceArchive
		}
	}
	return w.tar.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Size: size, Mode: int64(stat.Mode & 0777), ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC(), Format: tar.FormatPAX})
}

// Only a bounded page of names and a bounded stack of directory FDs is held.
// Each new child resolves from the pinned root, so an externally renamed
// ancestor cannot become a new traversal root outside /workspace.
func (w *archiveWalk) directory(dir *os.File, relative, name string, depth int, stat *unix.Stat_t) error {
	if err := w.header(name, depth, stat, tar.TypeDir); err != nil {
		return err
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		names, readErr := dir.Readdirnames(128)
		for _, child := range names {
			if !archiveComponentValid(child) {
				return fmt.Errorf("%w: invalid component", ErrWorkspaceArchive)
			}
			path := child
			if relative != "" {
				path = relative + "/" + child
			}
			archiveName := "workspace/" + path
			// Check before open; directories add one slash, checked by header.
			if err := w.source.limits.checkEntry(depth+1, len(archiveName), w.entries); err != nil {
				return err
			}
			if err := w.ctx.Err(); err != nil {
				return err
			}
			file, err := w.source.openChild(w.ctx, int(w.source.root.Fd()), path)
			if err != nil {
				return err
			}
			err = w.child(file, path, archiveName, depth+1)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (w *archiveWalk) child(file *os.File, path, name string, depth int) error {
	var before unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &before); err != nil {
		return err
	}
	switch before.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return w.directory(file, path, name+"/", depth, &before)
	case unix.S_IFREG:
		if before.Nlink != 1 {
			return fmt.Errorf("%w: hardlinked regular file", ErrWorkspaceArchive)
		}
	default:
		return fmt.Errorf("%w: object kind", ErrWorkspaceArchive)
	}
	if err := w.header(name, depth, &before, tar.TypeReg); err != nil {
		return err
	}
	remaining := before.Size
	for remaining > 0 {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		next := int64(len(w.buffer))
		if remaining < next {
			next = remaining
		}
		n, err := io.ReadFull(file, w.buffer[:int(next)])
		if err != nil {
			return err
		}
		if _, err := w.tar.Write(w.buffer[:n]); err != nil {
			return err
		}
		remaining -= int64(n)
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Nlink != after.Nlink || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return fmt.Errorf("%w: file changed during read", ErrWorkspaceArchive)
	}
	return nil
}
