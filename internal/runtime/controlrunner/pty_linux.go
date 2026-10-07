//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// TTY input has canonical terminal semantics, not binary pipe semantics. Keep
// the master open through root/descendant drain so final output is never lost.
func openPTY() (*os.File, *os.File, byte, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, 0, err
	}
	master := os.NewFile(uintptr(fd), "pty-master")
	fail := func(e error) (*os.File, *os.File, byte, error) { master.Close(); return nil, nil, 0, e }
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	term, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		slave.Close()
		return fail(err)
	}
	term.Lflag |= unix.ICANON
	term.Lflag &^= unix.ECHO | unix.ECHONL
	term.Oflag &^= unix.OPOST
	if err = unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, term); err != nil {
		slave.Close()
		return fail(err)
	}
	return master, slave, term.Cc[unix.VEOF], nil
}
