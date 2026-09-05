package mounter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const packagedProfileManifest = "/etc/workspace-fuse/profile.json"

const packagedFuseConfig = "/etc/fuse.conf"

const (
	defaultS3FSVersionTimeout = 5 * time.Second
	maxS3FSVersionTimeout     = 10 * time.Second
	maxS3FSVersionOutput      = 8 << 10
)

var s3fsVersionLine = regexp.MustCompile(`^Amazon Simple Storage Service File System V[0-9]+\.[0-9]+(?:\.[0-9]+)?(?: ?\(commit:[0-9A-Za-z._-]+\))? with (?:OpenSSL|GnuTLS(?:\([0-9A-Za-z._-]+\))?)$`)

var requiredImageBinaries = []string{"/usr/bin/s3fs", "/usr/bin/fusermount3", "/bin/ls"}

type ImageCheckConfig struct {
	RunDir           string
	CacheRoot        string
	ManifestPath     string
	S3FSPath         string
	WorkspacePath    string
	FuseConfigPath   string
	RequiredBinaries []string
	BoundProfileID   string
	ExpectedOwnerUID int
	VersionTimeout   time.Duration
}

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

func CheckImage(runDir, cacheRoot, boundProfileID string) error {
	return CheckImageWithConfig(packagedImageCheckConfig(runDir, cacheRoot, boundProfileID))
}

func CheckImageRelease(runDir, cacheRoot, boundProfileID string) error {
	return CheckImageReleaseWithConfig(packagedImageCheckConfig(runDir, cacheRoot, boundProfileID))
}

func packagedImageCheckConfig(runDir, cacheRoot, boundProfileID string) ImageCheckConfig {
	return ImageCheckConfig{
		RunDir: runDir, CacheRoot: cacheRoot, ManifestPath: packagedProfileManifest,
		S3FSPath: "/usr/bin/s3fs", WorkspacePath: "/workspace", FuseConfigPath: packagedFuseConfig,
		RequiredBinaries: requiredImageBinaries,
		BoundProfileID:   boundProfileID, ExpectedOwnerUID: 0,
	}
}

func CheckImageWithConfig(config ImageCheckConfig) error {
	if config.ExpectedOwnerUID < 0 || config.ManifestPath == "" || config.S3FSPath == "" || config.WorkspacePath == "" || config.FuseConfigPath == "" || config.BoundProfileID == "" || len(config.RequiredBinaries) == 0 {
		return fmt.Errorf("image contract is incomplete")
	}
	for _, binary := range config.RequiredBinaries {
		info, err := os.Lstat(binary)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || !ownedByUID(info, config.ExpectedOwnerUID) {
			return fmt.Errorf("required image binary is unavailable")
		}
	}
	if err := checkOwnedDirectory(config.RunDir, 0o700, config.ExpectedOwnerUID); err != nil {
		return fmt.Errorf("required run directory is unavailable")
	}
	if info, err := os.Lstat(config.CacheRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByUID(info, config.ExpectedOwnerUID) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required cache root is unavailable")
	}
	if info, err := os.Lstat(filepath.Join(config.CacheRoot, "tmp")); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByUID(info, config.ExpectedOwnerUID) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("required cache directory is unavailable")
	}
	if info, err := os.Lstat(config.WorkspacePath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o555 || !ownedByUID(info, config.ExpectedOwnerUID) {
		return fmt.Errorf("workspace anchor must be a trusted mode-0555 directory")
	}
	fuseInfo, err := os.Lstat(config.FuseConfigPath)
	if err != nil || !fuseInfo.Mode().IsRegular() || fuseInfo.Mode()&os.ModeSymlink != 0 || fuseInfo.Mode().Perm()&0o022 != 0 || !ownedByUID(fuseInfo, config.ExpectedOwnerUID) {
		return fmt.Errorf("fuse.conf must be a trusted regular non-symlink file")
	}
	fuseConfig, err := os.ReadFile(config.FuseConfigPath)
	if err != nil || string(fuseConfig) != "user_allow_other\n" {
		return fmt.Errorf("fuse.conf must contain exactly one user_allow_other directive")
	}
	manifestInfo, err := os.Lstat(config.ManifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 || manifestInfo.Mode().Perm()&0o022 != 0 || !ownedByUID(manifestInfo, config.ExpectedOwnerUID) {
		return fmt.Errorf("profile manifest must be a trusted regular non-symlink file")
	}
	raw, err := os.ReadFile(config.ManifestPath)
	if err != nil || len(raw) > 64<<10 {
		return fmt.Errorf("read profile manifest")
	}
	digest, err := sha256File(config.S3FSPath)
	if err != nil {
		return fmt.Errorf("hash packaged s3fs")
	}
	if err := validateProfileManifest(raw, config.BoundProfileID, digest); err != nil {
		return err
	}
	if err := checkS3FSVersion(config.S3FSPath, config.VersionTimeout); err != nil {
		return err
	}
	return nil
}

func checkS3FSVersion(path string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = defaultS3FSVersionTimeout
	}
	if timeout < 0 || timeout > maxS3FSVersionTimeout {
		return fmt.Errorf("s3fs version probe timeout is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir = "/"
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.WaitDelay = 100 * time.Millisecond
	output := &boundedVersionOutput{remaining: maxS3FSVersionOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("s3fs version probe timed out")
		}
		return fmt.Errorf("s3fs version probe failed")
	}
	if output.exceeded {
		return fmt.Errorf("s3fs version probe output exceeds limit")
	}
	firstLine, _, _ := strings.Cut(strings.TrimSuffix(output.String(), "\n"), "\n")
	if !s3fsVersionLine.MatchString(firstLine) {
		return fmt.Errorf("s3fs version probe output is invalid")
	}
	return nil
}

type boundedVersionOutput struct {
	bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *boundedVersionOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.exceeded = true
		return 0, fmt.Errorf("version output exceeds limit")
	}
	w.remaining -= len(p)
	return w.Buffer.Write(p)
}

// CheckImageReleaseWithConfig layers production eligibility on top of the
// packaging/integrity check. Candidate images can pass CheckImageWithConfig so
// they can be built and tested without accidentally becoming selectable.
func CheckImageReleaseWithConfig(config ImageCheckConfig) error {
	if err := CheckImageWithConfig(config); err != nil {
		return err
	}
	profile, ok := InspectCompiledProfile(config.BoundProfileID)
	if !ok {
		return fmt.Errorf("image profile binding is not compiled")
	}
	return CheckProductionProfile(profile.Provider, config.BoundProfileID)
}

func checkOwnedDirectory(path string, exactMode os.FileMode, uid int) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != exactMode || !ownedByUID(info, uid) {
		return fmt.Errorf("untrusted directory")
	}
	return nil
}
