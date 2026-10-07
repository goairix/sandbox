//go:build linux

// Fixed native test helper; never linked into sandbox-launcher.
package main

import (
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "quick":
		return
	case "hold":
		fmt.Println("USER_READY")
		time.Sleep(time.Minute)
	case "leaf":
		time.Sleep(time.Minute)
	case "middle":
		c := exec.Command(os.Args[0], "leaf")
		c.Env = os.Environ()
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if e := c.Start(); e != nil {
			panic(e)
		}
		c.Process.Release()
	case "double":
		c := exec.Command(os.Args[0], "middle")
		c.Env = os.Environ()
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if e := c.Run(); e != nil {
			panic(e)
		}
		fmt.Println("DOUBLE_READY")
	default:
		input, err := io.ReadAll(io.LimitReader(os.Stdin, 1048577))
		if err != nil {
			panic(err)
		}
		cwd, _ := os.Getwd()
		groups, _ := os.Getgroups()
		fds, _ := os.ReadDir("/proc/self/fd")
		names := []string{}
		for _, f := range fds {
			p, e := os.Readlink("/proc/self/fd/" + f.Name())
			if e == nil {
				names = append(names, p)
			}
		}
		tty := [3]bool{}
		for i := 0; i < 3; i++ {
			_, e := unix.IoctlGetTermios(i, unix.TCGETS)
			tty[i] = e == nil
		}
		sid, _ := unix.Getsid(0)
		result := map[string]any{"pid": os.Getpid(), "ppid": os.Getppid(), "uid": os.Getuid(), "gid": os.Getgid(), "groups": groups, "argv": os.Args, "env": os.Environ(), "cwd": cwd, "stdin": input, "tty": tty, "sid": sid, "fds": names}
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		fmt.Fprintln(os.Stderr, "STDERR_MARKER")
		time.Sleep(500 * time.Millisecond)
	}
}
