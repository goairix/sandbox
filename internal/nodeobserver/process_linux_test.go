//go:build linux && (amd64 || arm64)

package nodeobserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNodeObserverEnrollmentKernelParsers(t *testing.T) {
	for _, text := range []string{"0::/\n", "0::/../x\n", "0::/x\n0::/y\n", "1:cpu:/x\n"} {
		if _, err := cgroupMembership([]byte(text)); err == nil {
			t.Fatalf("accepted invalid membership %q", text)
		}
	}
	for _, text := range []string{"NSpid:\t1\n", "NSpid:\t2\t3\n", "NSpid:\t2\t1\nNSpid:\t2\t1\n"} {
		if privatePID1([]byte(text)) == nil {
			t.Fatalf("accepted invalid private PID1 %q", text)
		}
	}
	if err := privatePID1([]byte("NSpid:\t7\t1\n")); err != nil {
		t.Fatal(err)
	}
}

func TestNodeObserverOptionsRejectMissingAuthority(t *testing.T) {
	if observer, err := New(Options{}); err == nil || observer != nil {
		t.Fatal("observer accepted missing concrete authority")
	}
	var empty Observer
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeObserverEnrollmentBoundary(t *testing.T) {
	actual := []byte("NoNewPrivs:\t1\nSeccomp:\t2\nCapEff:\t00000000000000e0\nCapPrm:\t00000000000000e0\nCapBnd:\t0000000000000000\nCapAmb:\t0000000000000000\nCapInh:\t00000000000000e0\n")
	if err := validateProcessStatus(actual); err != nil {
		t.Fatalf("genuine Supervisor fixed inheritable set rejected: %v", err)
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(actual), "CapInh:\t00000000000000e0", "CapInh:\t00000000002000e0", 1)),
		[]byte(strings.Replace(string(actual), "CapInh:\t00000000000000e0", "CapInh:\t00000000000001e0", 1)),
		[]byte(strings.Replace(string(actual), "CapEff:\t00000000000000e0", "CapEff:\t00000000000800e0", 1)),
		[]byte(strings.Replace(string(actual), "CapBnd:\t0000000000000000", "CapBnd:\t00000000000000e0", 1)),
		[]byte(strings.Replace(string(actual), "CapAmb:\t0000000000000000", "CapAmb:\t00000000000000e0", 1)),
		[]byte(strings.Replace(string(actual), "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1)),
	} {
		if err := validateProcessStatus(invalid); err == nil {
			t.Fatal("escape capability or missing NNP accepted")
		}
	}
}

func TestNodeObserverPhysicalKernelChild(t *testing.T) {
	if os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_CHILD") != "1" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatal("kernel child must be genuine namespace PID1")
	}
	child := exec.Command("/bin/sleep", "600")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_SOCKET"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("descendant-ready")); err != nil {
		t.Fatal(err)
	}
	// No success callback or voluntary exit: the parent uses its held pidfd.
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("descendant exited before observer signal")
}

func TestNodeObserverPhysicalKernel(t *testing.T) {
	if os.Getenv("SANDBOX_NODE_OBSERVER_KERNEL_NATIVE") != "1" {
		t.Skip("requires exact owned native kernel fixture")
	}
	if os.Getuid() != 0 {
		t.Fatal("kernel fixture requires root")
	}
	group, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || string(group) != "0::/\n" {
		t.Fatal("fixture must have private delegated cgroup namespace")
	}
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(dir, "cgroup")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("none", mount, "cgroup2", 0, ""); err != nil {
		t.Fatalf("private cgroup delegation unavailable: %v", err)
	}
	groupName := fmt.Sprintf("observer-owned-%d", os.Getpid())
	owned := filepath.Join(mount, groupName)
	if err := os.Mkdir(owned, 0700); err != nil {
		unix.Unmount(mount, 0)
		t.Fatal(err)
	}
	cgroupFD, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cgroupFD >= 0 {
			_ = unix.Close(cgroupFD)
		}
	}()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "enroll.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestNodeObserverPhysicalKernelChild$", "-test.v", "-test.timeout=35s")
	child.Env = append(os.Environ(), "SANDBOX_NODE_OBSERVER_KERNEL_CHILD=1", "SANDBOX_NODE_OBSERVER_KERNEL_SOCKET="+listener.Addr().String())
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: cgroupFD}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(cgroupFD); err != nil {
		t.Fatal(err)
	}
	cgroupFD = -1
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			err := child.Wait()
			t.Logf("original child Cmd.Wait cleanup: %v", err)
		}
		events, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		if err != nil || !strings.Contains(string(events), "populated 0\n") {
			t.Errorf("owned cgroup cleanup not empty: %q %v", events, err)
			return
		}
		if err := os.Remove(owned); err != nil {
			t.Error(err)
			return
		}
		if err := unix.Unmount(mount, 0); err != nil {
			t.Error(err)
			return
		}
		t.Log("owned child cgroup empty→rmdir→private unmount joined")
	}()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready [16]byte
	n, err := conn.Read(ready[:])
	if err != nil || string(ready[:n]) != "descendant-ready" {
		t.Fatalf("actual descendant readiness: %q %v", ready[:n], err)
	}
	pid, err := kernelPeer(conn)
	if err != nil || pid != child.Process.Pid {
		t.Fatalf("actual kernel peer pid=%d child=%d: %v", pid, child.Process.Pid, err)
	}
	anchor, err := openProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.close()
	t.Logf("actual enrollment anchors: %+v", anchor.facts)
	if err := anchor.revalidate(); err != nil {
		t.Fatal(err)
	}
	if err := anchor.kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := anchor.waitDeathAndEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("actual original child Cmd.Wait expected SIGKILL: %v", err)
	}
	t.Logf("original child Cmd.Wait=%v; pidfd death + original cgroup populated0 observed", err)
}
