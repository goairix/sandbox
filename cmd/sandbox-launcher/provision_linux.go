//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"golang.org/x/sys/unix"
)

// provisionProtectedConfig only narrows a fresh fixed control EmptyDir. Every
// existing birth artifact is retained on failure; no retry adopts old state.
func provisionProtectedConfig(wire []byte) error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return fmt.Errorf("provisioning requires root PID1")
	}
	if _, err := launcherconfig.Decode(wire); err != nil {
		return err
	}
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	if err := provisionParent(root); err != nil {
		return err
	}
	run, err := unix.Openat(root, "run", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(run)
	if err := provisionParent(run); err != nil {
		return err
	}
	fd, err := unix.Openat(run, "sandbox-control", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), "enrolled-control-directory")
	defer directory.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Gid != 0 || !freshControlMode(st.Mode) {
		return fmt.Errorf("invalid fresh control directory")
	}
	// Kubelet's fresh disk EmptyDir and memory tmpfs start writable. Narrow
	// the held original directory before trusting any child; failures retain
	// all birth data and may only leave this permission tightening behind.
	if err := unix.Fchmod(fd, 0700); err != nil {
		return err
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Gid != 0 || st.Mode != unix.S_IFDIR|0700 {
		return fmt.Errorf("control directory did not become protected")
	}
	entries, err := directory.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "enrollment.sock" {
		return fmt.Errorf("control directory contains missing socket or prior birth data")
	}
	if err := unix.Fstatat(fd, "enrollment.sock", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Uid != 0 || st.Gid != 0 || st.Mode != unix.S_IFSOCK|0600 {
		return fmt.Errorf("invalid enrolled observer socket")
	}
	configFD, err := unix.Openat(fd, "config.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(configFD), "enrolled-public-configuration")
	defer file.Close()
	if err := unix.Fchmod(configFD, 0600); err != nil {
		return err
	}
	if err := unix.Fstat(configFD, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Gid != 0 || st.Nlink != 1 || st.Mode != unix.S_IFREG|0600 {
		return fmt.Errorf("invalid new public configuration file")
	}
	n, err := file.Write(wire)
	if err != nil {
		return err
	}
	if n != len(wire) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return directory.Sync()
}

func provisionParent(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != 0 || st.Gid != 0 || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 {
		return fmt.Errorf("unprotected provisioning ancestor")
	}
	return nil
}
