package mounter

import (
	"fmt"
	"os"
	"path/filepath"
)

var requiredImageBinaries = []string{"/usr/bin/s3fs", "/usr/bin/fusermount3", "/bin/ls"}

func CheckFuseDevice() error {
	info, err := os.Stat("/dev/fuse")
	if err != nil || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("FUSE character device is unavailable")
	}
	return nil
}

func CheckWorkspaceAnchor(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o555 {
		return fmt.Errorf("workspace anchor must be a mode-0555 directory")
	}
	if !ownedByRoot(info) {
		return fmt.Errorf("workspace anchor must be owned by root")
	}
	return nil
}

func PrepareWorkspaceAnchor(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByRoot(info) {
		return fmt.Errorf("workspace anchor must be a root-owned directory")
	}
	if info.Mode().Perm() == 0o555 {
		return CheckWorkspaceAnchor(path)
	}
	if err := os.Chmod(path, 0o555); err != nil {
		return fmt.Errorf("secure workspace anchor: %w", err)
	}
	return CheckWorkspaceAnchor(path)
}

func CheckImage(runDir, cacheRoot string) error {
	for _, binary := range requiredImageBinaries {
		info, err := os.Stat(binary)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || !ownedByRoot(info) {
			return fmt.Errorf("required image binary is unavailable")
		}
	}
	if err := secureDirectory(runDir, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(cacheRoot); err != nil || !info.IsDir() || !ownedByRoot(info) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required cache root is unavailable")
	}
	if info, err := os.Stat(filepath.Join(cacheRoot, "tmp")); err != nil || !info.IsDir() || !ownedByRoot(info) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required cache directory is unavailable")
	}
	return nil
}
