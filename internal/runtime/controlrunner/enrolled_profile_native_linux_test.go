//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/goairix/sandbox/internal/kubecontract"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// The digest is fixture Pod data. The ELF is a verified readonly bind, so this
// checks the production profile without claiming OCI packaging or kubelet use.
const enrolledProfileFixtureImage = "fixture.invalid/sandbox@sha256:684aa844d79d5b300db6586a25503607838055db7e7f480a50abb7c7ee7b4e32"

func TestEnrolledProfileNative(t *testing.T) {
	runEnrolledLauncherNativeWithProfile(t, []string{"profile"}, true)
}

func profileFixtureContract(t *testing.T) string {
	t.Helper()
	digest, err := kubecontract.EnrolledPlainContractDigest(enrolledProfileFixtureImage)
	require.NoError(t, err)
	return digest
}

func profileFixturePod(t *testing.T, binding p.TrustBinding, root, clock ed25519.PublicKey, nodeUID, boot, intent, name, uid, mode string) *v1.Pod {
	t.Helper()
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root}, UID: 1000, GID: 1000, ContractDigest: profileFixtureContract(t), ClockSocket: ct.ClockSocket, ClockPublicKey: clock, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: nodeUID, NodeBootID: boot}}
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	pod, err := kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: name, Image: enrolledProfileFixtureImage, CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi", ReadOnlyRootFS: true, RunAsUser: 1000, Labels: map[string]string{"sandbox.id": "sandbox-" + mode}}, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, CreationIntentID: intent, Generation: 1, Config: wire})
	require.NoError(t, err)
	pod.UID, pod.ResourceVersion = types.UID(uid), "1"
	return pod
}

// This helper only installs the fixed mounts and approved private boundary,
// then execs the formal binary; it never constructs a supervisor or kernel.
func TestEnrolledProfileNativeExec(t *testing.T) {
	mode := os.Getenv("ENROLLED_PROFILE_EXEC_MODE")
	if mode == "" {
		return
	}
	require.Contains(t, []string{"active", "stale", "foreign", "nonroot"}, mode)
	require.Equal(t, 1, os.Getpid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	for _, mount := range []struct{ source, target string }{{"/run/sandbox-clock", "/run/sandbox-clock"}, {kubecontract.EnrolledEnrollmentSource, ct.EnrollmentSocket}} {
		require.NoError(t, unix.Mount(mount.source, mount.target, "", unix.MS_BIND, ""))
		require.NoError(t, unix.Mount("", mount.target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""))
	}
	require.NoError(t, unix.Unmount(os.Getenv("ENROLLED_LAUNCHER_EXEC_CGROUP_MOUNT"), 0))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	for _, capability := range []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_PTRACE} {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1e0}}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	require.Zero(t, errno)
	if mode == "nonroot" {
		_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETGID, 1000, 0, 0)
		require.Zero(t, errno)
		_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETUID, 1000, 0, 0)
		require.Zero(t, errno)
	}
	t.Logf("profile helper actual fixed exec mode=%s path=/usr/local/bin/sandbox-launcher", mode)
	require.NoError(t, syscall.Exec(kubecontract.EnrolledLauncherPath, []string{kubecontract.EnrolledLauncherPath, "serve-provisioned"}, os.Environ()))
}

func profileNativeRefusals(t *testing.T, mount string, config []byte) {
	t.Helper()
	initial, err := os.Stat(ct.ControlDirectory)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(os.ModeDir|0755), initial.Mode())
	for _, mode := range []string{"stale", "foreign", "nonroot"} {
		t.Run("provision-"+mode, func(t *testing.T) {
			var artifact string
			if mode == "stale" {
				artifact = ct.ControlDirectory + "/config.json"
			}
			if mode == "foreign" {
				artifact = ct.ControlDirectory + "/foreign"
			}
			if artifact != "" {
				require.NoError(t, os.WriteFile(artifact, []byte("retained prior birth"), 0600))
				t.Cleanup(func() { require.NoError(t, os.Remove(artifact)) })
			}
			owned := filepath.Join(mount, "profile-negative-"+uuid.NewString())
			require.NoError(t, os.Mkdir(owned, 0700))
			t.Cleanup(func() {
				wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
				require.NoError(t, err)
				require.Contains(t, string(wire), "populated 0\n")
				require.NoError(t, os.Remove(owned))
			})
			fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			child := exec.Command(os.Args[0], "-test.run=^TestEnrolledProfileNativeExec$", "-test.v", "-test.timeout=10s")
			child.Env = append(os.Environ(), "ENROLLED_LAUNCHER_EXEC_CGROUP_MOUNT="+mount, launcherconfig.Environment+"="+string(config), "ENROLLED_PROFILE_EXEC_MODE="+mode)
			child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			startErr := child.Start()
			closeErr := unix.Close(fd)
			require.NoError(t, startErr)
			wait := make(chan error, 1)
			go func() { wait <- child.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = child.Process.Kill()
					t.Logf("profile refusal cleanup original Cmd.Wait=%v", <-wait)
				}
			})
			require.NoError(t, closeErr)
			select {
			case err = <-wait:
				waited = true
			case <-time.After(5 * time.Second):
				t.Fatal("formal CLI refusal timed out")
			}
			var exited *exec.ExitError
			require.ErrorAs(t, err, &exited)
			require.Equal(t, 1, exited.ExitCode())
			message := "sandbox-launcher: control directory contains missing socket or prior birth data\n"
			if mode == "nonroot" {
				message = "sandbox-launcher: supervisor unavailable\n"
			}
			require.Contains(t, output.String(), "profile helper actual fixed exec mode="+mode)
			require.Contains(t, output.String(), message, "a helper failure is not formal CLI refusal")
			require.NotContains(t, output.String(), "--- FAIL:")
			if artifact != "" {
				wire, err := os.ReadFile(artifact)
				require.NoError(t, err)
				require.Equal(t, "retained prior birth", string(wire))
			}
			if mode != "stale" {
				_, err := os.Lstat(ct.ControlDirectory + "/config.json")
				require.True(t, os.IsNotExist(err))
			}
			t.Logf("formal serve-provisioned %s refused exact CLI error; original Cmd.Wait=exit1; birth artifacts retained", mode)
		})
	}
}

func profileNativeProvisioned(t *testing.T, pid int, config []byte) {
	t.Helper()
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{ct.ControlDirectory, os.ModeDir | 0700}, {ct.ControlDirectory + "/config.json", 0600}} {
		info, err := os.Stat(item.path)
		require.NoError(t, err)
		require.Equal(t, item.mode, info.Mode())
		st, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		require.Zero(t, st.Uid)
		require.Zero(t, st.Gid)
	}
	wire, err := os.ReadFile(ct.ControlDirectory + "/config.json")
	require.NoError(t, err)
	require.Equal(t, config, wire)
	mounts, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	require.NoError(t, err)
	for _, target := range []string{"/run/sandbox-clock", ct.EnrollmentSocket} {
		found := false
		for _, line := range strings.Split(string(mounts), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 5 && fields[4] == target {
				require.Contains(t, strings.Split(fields[5], ","), "ro")
				found = true
			}
		}
		require.True(t, found, "readonly mounted %s", target)
	}
	hash := func(path string) []byte {
		file, err := os.Open(path)
		require.NoError(t, err)
		defer file.Close()
		h := sha256.New()
		_, err = io.Copy(h, file)
		require.NoError(t, err)
		return h.Sum(nil)
	}
	require.Equal(t, hash(kubecontract.EnrolledLauncherPath), hash(fmt.Sprintf("/proc/%d/exe", pid)))
	t.Log("formal PID1 provisioned original0755 to0700/config0600; actual readonly socket/clock mounts and exact executable ELF verified")
}

func profileNativeCleanupPrepare(t *testing.T, endpoint string, authority p.TrustBinding, identity p.RuntimeIdentityContext, intent, nodeUID, boot, contract, enrollment string) {
	t.Helper()
	client, err := kube.NewTaskRuntimeCleanupClient(kube.TaskRuntimeCleanupOptions{Config: &rest.Config{Host: endpoint}, Namespace: "runtimes", RequestTimeout: time.Second, MaxActive: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handle, err := client.Prepare(ctx, kube.TaskRuntimeCleanupBinding{Authority: authority, Runtime: identity.Runtime, Generation: identity.Generation, DataGateEpoch: 1, TaskID: uuid.NewString(), LogicalID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, CreationIntentID: intent, NodeUID: nodeUID, NodeBootID: boot, EnrollmentDigest: enrollment, ContractDigest: contract}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, handle.Descriptor())
	t.Log("original cleanup Prepare accepted same production-built Pod served to real observer GET; no cleanup mutation or death claim")
}
