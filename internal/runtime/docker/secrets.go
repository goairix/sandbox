package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/goairix/sandbox/internal/runtime"
)

const maxWorkspaceSecretBytes = 64 << 10

type FUSESecretMaterializer interface {
	Materialize(context.Context, runtime.SandboxSpec, string) error
	RemoveSecret(context.Context, string) error
	ListSecretPreparations(context.Context) ([]string, error)
}

func (m *FileSecretMaterializer) ListSecretPreparations(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || !filepath.IsAbs(m.Root) {
		return nil, fmt.Errorf("workspace secret root is invalid")
	}
	if err := validateRootDirectory(m.Root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace secret root is not trusted: %w", err)
	}
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return nil, err
	}
	preparations := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !validDockerPreparationID(entry.Name()) {
			return nil, fmt.Errorf("workspace secret root contains an untrusted entry")
		}
		if err := validateRootDirectory(filepath.Join(m.Root, entry.Name()), 0o700); err != nil {
			return nil, fmt.Errorf("workspace secret preparation is not trusted: %w", err)
		}
		preparations = append(preparations, entry.Name())
	}
	return preparations, nil
}

// FileSecretMaterializer copies operator-selected files; request data never
// selects a source path.
type FileSecretMaterializer struct {
	Root, CAFile string
}

func (m *FileSecretMaterializer) Materialize(ctx context.Context, spec runtime.SandboxSpec, target string) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || target != filepath.Join(m.Root, spec.ID) || filepath.Dir(target) != filepath.Clean(m.Root) {
		return fmt.Errorf("workspace secret target is invalid")
	}
	if err := validateRootDirectory(m.Root, 0o700); err != nil {
		return fmt.Errorf("workspace secret root is not trusted: %w", err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create workspace secret directory: %w", err)
	}
	defer func() {
		if result != nil {
			_ = removeExactSecretDirectory(target)
		}
	}()
	if spec.WorkspaceFUSE == nil || spec.WorkspaceFUSE.CASecretKey == "" || filepath.Base(spec.WorkspaceFUSE.CASecretKey) != spec.WorkspaceFUSE.CASecretKey || m.CAFile == "" {
		return fmt.Errorf("workspace CA secret source is invalid")
	}
	if err := copySecretFile(m.CAFile, filepath.Join(target, spec.WorkspaceFUSE.CASecretKey)); err != nil {
		return err
	}
	return validateRootSecretDirectory(target, caSecretName(spec))
}

func (m *FileSecretMaterializer) RemoveSecret(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || target == "" || target != filepath.Join(m.Root, filepath.Base(target)) || filepath.Dir(target) != filepath.Clean(m.Root) {
		return fmt.Errorf("workspace secret cleanup target is invalid")
	}
	return removeExactSecretDirectory(target)
}

func copySecretFile(source, target string) error {
	if source == "" || !filepath.IsAbs(source) {
		return fmt.Errorf("workspace secret source is invalid")
	}
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || !rootOwned(before) {
		return fmt.Errorf("workspace secret source is not a root-only regular file")
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open workspace secret source: %w", err)
	}
	defer in.Close()
	after, err := in.Stat()
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("workspace secret source changed")
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create workspace secret file: %w", err)
	}
	written, copyErr := io.Copy(out, io.LimitReader(in, maxWorkspaceSecretBytes+1))
	syncErr, closeErr := out.Sync(), out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written <= 0 || written > maxWorkspaceSecretBytes {
		_ = os.Remove(target)
		return fmt.Errorf("copy workspace secret file")
	}
	return nil
}

func validateRootSecretDirectory(target, caName string) error {
	if err := validateRootDirectory(target, 0o700); err != nil {
		return err
	}
	expected := make(map[string]struct{}, 1)
	if caName != "" {
		expected[caName] = struct{}{}
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != len(expected) {
		return fmt.Errorf("workspace secret directory contents are invalid")
	}
	for _, entry := range entries {
		if _, ok := expected[entry.Name()]; !ok || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace secret directory contains an unexpected entry")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !rootOwned(info) || info.Size() <= 0 || info.Size() > maxWorkspaceSecretBytes {
			return fmt.Errorf("workspace secret file is not trusted")
		}
	}
	return nil
}

func validateRootDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !rootOwned(info) {
		return fmt.Errorf("root-only directory check failed")
	}
	return nil
}

func rootOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && stat.Gid == 0
}

func removeExactSecretDirectory(target string) error {
	if _, err := os.Lstat(target); os.IsNotExist(err) {
		return nil
	}
	if err := validateRootDirectory(target, 0o700); err != nil {
		return fmt.Errorf("workspace secret cleanup target is not trusted: %w", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return fmt.Errorf("workspace secret cleanup found unexpected entry")
		}
		if err := os.Remove(filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(target)
}

func caSecretName(spec runtime.SandboxSpec) string {
	if spec.WorkspaceFUSE == nil {
		return ""
	}
	return spec.WorkspaceFUSE.CASecretKey
}
