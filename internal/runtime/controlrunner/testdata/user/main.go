//go:build linux

// Fixed native test helper; never linked into sandbox-launcher.
package main

import (
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "flood":
		fmt.Println("USER_READY")
		time.Sleep(500 * time.Millisecond)
		b := make([]byte, 32768)
		for {
			if _, err := os.Stdout.Write(b); err != nil {
				return
			}
		}
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
		fdInfo := []map[string]any{}
		for _, f := range fds {
			p, e := os.Readlink("/proc/self/fd/" + f.Name())
			if e == nil {
				names = append(names, p)
				fd, _ := strconv.Atoi(f.Name())
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
				if err != nil {
					panic(err)
				}
				fdInfo = append(fdInfo, map[string]any{"fd": fd, "target": p, "flags": flags})
			}
		}
		tty := [3]bool{}
		for i := 0; i < 3; i++ {
			_, e := unix.IoctlGetTermios(i, unix.TCGETS)
			tty[i] = e == nil
		}
		sid, _ := unix.Getsid(0)
		result := map[string]any{"pid": os.Getpid(), "ppid": os.Getppid(), "uid": os.Getuid(), "gid": os.Getgid(), "groups": groups, "argv": os.Args, "env": os.Environ(), "cwd": cwd, "stdin": input, "tty": tty, "sid": sid, "fds": names, "fd_info": fdInfo}
		if os.Getenv("PROBE_PROTECTED") == "1" {
			denied := map[string]string{}
			for _, path := range []string{"/journal/protected", "/journal/protected/key-marker", "/proc/1/mem", "/proc/1/fd", "/proc/" + strconv.Itoa(os.Getppid()) + "/mem"} {
				f, err := os.Open(path)
				if err == nil {
					f.Close()
					denied[path] = "ACCESS_GRANTED"
				} else {
					denied[path] = err.Error()
				}
			}
			c, err := net.DialTimeout("unix", "/journal/protected/control.sock", 250*time.Millisecond)
			if err == nil {
				c.Close()
				denied["socket"] = "ACCESS_GRANTED"
			} else {
				denied["socket"] = err.Error()
			}
			result["denied"] = denied
		}
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		fmt.Fprintln(os.Stderr, "STDERR_MARKER")
		time.Sleep(500 * time.Millisecond)
	}
}
