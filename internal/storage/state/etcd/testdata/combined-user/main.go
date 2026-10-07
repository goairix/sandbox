//go:build linux

// Fixed, independently bounded fixture user; never linked into the launcher.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	// Even orphaned descendants have their own absolute bound.
	timer := time.AfterFunc(60*time.Second, func() { os.Exit(0) })
	defer timer.Stop()
	if len(os.Args) != 2 || os.Getuid() != 1000 || os.Getgid() != 1000 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hold":
		fmt.Printf("USER_READY pid=%d uid=%d gid=%d\n", os.Getpid(), os.Getuid(), os.Getgid())
		time.Sleep(59 * time.Second)
	case "double":
		// The middle process creates a leaf and exits. The original registered
		// root stays alive after readiness, so both root and orphan can cross
		// DATA/USERS closure under their actual monitor's sole Wait ownership.
		c := exec.Command("/native/combined-user", "middle")
		c.Env, c.Stdout, c.Stderr = os.Environ(), os.Stdout, os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Printf("DOUBLE_READY pid=%d uid=%d gid=%d\n", os.Getpid(), os.Getuid(), os.Getgid())
		time.Sleep(59 * time.Second)
	case "middle":
		c := exec.Command("/native/combined-user", "leaf")
		c.Env, c.Stdout, c.Stderr = os.Environ(), os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Printf("LEAF_STARTED pid=%d parent=%d\n", c.Process.Pid, os.Getpid())
		if err := c.Process.Release(); err != nil {
			os.Exit(3)
		}
	case "leaf":
		fmt.Printf("LEAF_READY pid=%d parent=%d uid=%d gid=%d\n", os.Getpid(), os.Getppid(), os.Getuid(), os.Getgid())
		time.Sleep(59 * time.Second)
	case "probe":
		result := map[string]string{}
		for _, path := range []string{"/run/sandbox-control/control.sock", "/run/sandbox-clock/clock.sock"} {
			c, err := net.DialTimeout("unix", path, 250*time.Millisecond)
			if err == nil {
				c.Close()
				result[path] = "ACCESS_GRANTED"
			} else if errors.Is(err, os.ErrPermission) {
				result[path] = "DAC_DENIED"
			} else {
				result[path] = err.Error()
			}
		}
		for _, path := range []string{"/run/sandbox-metadata", "/run/sandbox-fixture/private"} {
			_, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				result[path] = "ABSENT"
			} else if errors.Is(err, os.ErrPermission) {
				result[path] = "DAC_DENIED"
			} else if err == nil {
				result[path] = "PRESENT"
			} else {
				result[path] = err.Error()
			}
		}
		wire, err := json.Marshal(result)
		if err != nil {
			os.Exit(3)
		}
		fmt.Println(string(wire))
	default:
		os.Exit(2)
	}
}
