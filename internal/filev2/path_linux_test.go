//go:build linux

package filev2

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func rootDescriptor(t *testing.T, root string) int {
	t.Helper()
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func TestOpenBeneathRejectsTraversalSymlinkAndSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	fd := rootDescriptor(t, root)
	for _, path := range []string{"../secret", "escape/secret", "dangling", "/etc/passwd"} {
		opened, err := openBeneath(fd, path, unix.O_RDONLY, 0)
		if err == nil {
			_ = unix.Close(opened)
			t.Errorf("opened forbidden path %q", path)
		}
	}

	// Replace a safe entry with an escaping symlink. Resolution must use the
	// pinned root descriptor rather than a checked path string.
	if err := os.Mkdir(filepath.Join(root, "changing"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "changing")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "changing")); err != nil {
		t.Fatal(err)
	}
	opened, err := openBeneath(fd, "changing/secret", unix.O_RDONLY, 0)
	if err == nil {
		_ = unix.Close(opened)
		t.Fatal("opened a swapped escaping symlink")
	}
	if !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.EINVAL) {
		t.Logf("swapped symlink rejected with %v", err)
	}
}

func TestOpenBeneathAllowsWorkspaceAndTmp(t *testing.T) {
	for _, name := range []string{"workspace", "tmp"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "data"), []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("data", filepath.Join(root, "inside")); err != nil {
				t.Fatal(err)
			}
			fd := rootDescriptor(t, root)
			for _, path := range []string{"data", "inside"} {
				opened, err := openBeneath(fd, path, unix.O_RDONLY, 0)
				if err != nil {
					t.Fatalf("open %q: %v", path, err)
				}
				buf := make([]byte, len(name))
				_, err = unix.Read(opened, buf)
				_ = unix.Close(opened)
				if err != nil || string(buf) != name {
					t.Fatalf("read %q: %q, %v", path, buf, err)
				}
			}
		})
	}
}
