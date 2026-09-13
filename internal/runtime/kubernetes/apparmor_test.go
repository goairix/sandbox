package kubernetes

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var appArmorReadForTest = []string{"/bin/cat", "/proc/1/attr/current"}

func TestAppArmorPrivateReadGrammar(t *testing.T) {
	require.True(t, allowedMounterCommand(appArmorReadForTest))
	require.False(t, fuseprotocol.AllowedMounterCommand(appArmorReadForTest))
	for _, argv := range [][]string{{"cat", "/proc/1/attr/current"}, {"/bin/cat", "/proc/self/attr/current"}, {"/bin/cat", "/proc/1/attr/current", "/etc/passwd"}, {"/bin/sh", "-c", "cat /proc/1/attr/current"}} {
		require.False(t, allowedMounterCommand(argv))
	}
}

func TestAppArmorPrivateReadRejectsInput(t *testing.T) {
	rt, _ := newFakeKubernetesRuntime(t, preparedScript())
	_, err := rt.execControl(context.Background(), "pod", workspaceMounterContainer, appArmorReadForTest, []byte("secret"))
	require.ErrorIs(t, err, ErrInvalidControlCommand)
}

func TestAppArmorBoundary(t *testing.T) {
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name    string
		output  string
		readErr error
		mutate  string
		ok      bool
	}{
		{name: "enforce", output: profile + " (enforce)\n", ok: true},
		{name: "unconfined", output: "unconfined\n"},
		{name: "complain", output: profile + " (complain)\n"},
		{name: "different", output: "sandbox-fuse-other (enforce)"},
		{name: "oversized", output: strings.Repeat(" ", 513) + profile + " (enforce)"},
		{name: "read failure", readErr: errors.New("private read failed")},
		{name: "replacement", output: profile + " (enforce)", mutate: "uid"},
		{name: "restart", output: profile + " (enforce)", mutate: "restart"},
		{name: "security drift", output: profile + " (enforce)", mutate: "security"},
	} {
		for _, authorize := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/health", true: "/authorize"}[authorize], func(t *testing.T) {
				rt, client := newFakeKubernetesRuntime(t, preparedScript())
				info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
				require.NoError(t, err)
				ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
				pod, err := client.CoreV1().Pods(rt.namespace).Get(context.Background(), ref.ID, metav1.GetOptions{})
				require.NoError(t, err)
				pod.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+workspaceMounterContainer] = "localhost/" + profile
				pod.Spec.InitContainers[0].SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeLocalhost, LocalhostProfile: &profile}
				_, err = client.CoreV1().Pods(rt.namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				WithAppArmorEnforcement(profile)(rt)
				require.Contains(t, rt.WarmPoolContract(), ":apparmor="+profile)
				base := preparedScript()
				authCalls := 0
				readCalls := 0
				rt.controlExecutor = podExecutorFunc(func(ctx context.Context, id, container string, argv []string, stdin []byte) ([]byte, error) {
					if reflect.DeepEqual(argv, appArmorReadForTest) {
						readCalls++
						require.Empty(t, stdin)
						require.Equal(t, workspaceMounterContainer, container)
						require.NotNil(t, ctx.Done())
						if tc.mutate != "" {
							current, getErr := client.CoreV1().Pods(rt.namespace).Get(ctx, id, metav1.GetOptions{})
							require.NoError(t, getErr)
							switch tc.mutate {
							case "uid":
								current.UID = types.UID("replacement")
							case "restart":
								current.Status.InitContainerStatuses[0].RestartCount++
							case "security":
								*current.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem = false
							}
							_, updateErr := client.CoreV1().Pods(rt.namespace).Update(ctx, current, metav1.UpdateOptions{})
							require.NoError(t, updateErr)
						}
						return []byte(tc.output), tc.readErr
					}
					if reflect.DeepEqual(argv, []string{mounterBinary, "authorize"}) {
						authCalls++
					}
					return base.handler(recordedPodCommand{pod: id, container: container, argv: argv, stdin: stdin})
				})
				if authorize {
					err = rt.AuthorizeWorkspaceMount(context.Background(), ref, sandboxruntime.WorkspaceMountAuthorization{RuntimeUID: ref.UID, PoolKey: preparedFUSESpecForTest().WorkspaceFUSE.PoolKey, WorkspaceHash: "hash", Prefix: "workspace/", LeaseGeneration: 7, MountAttempt: 1})
				} else {
					err = rt.PreparedSandboxHealth(context.Background(), ref, preparedFUSESpecForTest().WorkspaceFUSE.PoolKey)
				}
				if tc.ok {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.Equal(t, 1, readCalls)
				if authorize && tc.ok {
					require.Equal(t, 1, authCalls)
				} else {
					require.Zero(t, authCalls)
				}
			})
		}
	}
}

func TestAppArmorSecurityTemplate(t *testing.T) {
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"workspace host root", func(p *corev1.Pod) {
			p.Spec.Volumes[0].EmptyDir = nil
			p.Spec.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/"}
		}},
		{"extra host mount", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}},
		{"tenant privilege", func(p *corev1.Pod) { yes := true; p.Spec.Containers[0].SecurityContext.Privileged = &yes }},
		{"tenant root", func(p *corev1.Pod) { *p.Spec.Containers[0].SecurityContext.RunAsUser = 0 }},
		{"pod seccomp", func(p *corev1.Pod) { p.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined }},
		{"device path", func(p *corev1.Pod) { p.Spec.Volumes[2].HostPath.Path = "/dev/mem" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, client := newFakeKubernetesRuntime(t, preparedScript())
			info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			require.NoError(t, err)
			ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
			pod, err := client.CoreV1().Pods(rt.namespace).Get(context.Background(), ref.ID, metav1.GetOptions{})
			require.NoError(t, err)
			pod.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+workspaceMounterContainer] = "localhost/" + profile
			tc.mutate(pod)
			_, err = client.CoreV1().Pods(rt.namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			WithAppArmorEnforcement(profile)(rt)
			rt.controlExecutor = podExecutorFunc(func(context.Context, string, string, []string, []byte) ([]byte, error) {
				return []byte(profile + " (enforce)"), nil
			})
			require.Error(t, rt.verifyMounterAppArmor(context.Background(), ref))
		})
	}
}

func TestAppArmorUnconfinedPreparationIsNeverPublished(t *testing.T) {
	base := preparedScript()
	script := &commandScript{handler: func(command recordedPodCommand) ([]byte, error) {
		if reflect.DeepEqual(command.argv, appArmorReadForTest) {
			return []byte("unconfined\n"), nil
		}
		return base.handler(command)
	}}
	rt, client := newFakeKubernetesRuntime(t, script)
	installFinalizerAwarePodDeletion(t, client)
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	WithAppArmorEnforcement(profile)(rt)
	spec := preparedFUSESpecForTest()
	spec.WorkspaceFUSE.LSMProfile = profile
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.ErrorContains(t, err, "enforce AppArmor profile")
	require.Nil(t, info)
	_, err = client.CoreV1().Pods(rt.namespace).Get(context.Background(), spec.ID, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "failed preparation must clean only its exact Pod")
	for _, command := range script.commands {
		require.False(t, reflect.DeepEqual(command.argv, []string{mounterBinary, "authorize"}))
	}
}

func TestAppArmorReadHonorsCallerDeadline(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	profile := "sandbox-fuse-" + strings.Repeat("a", 64)
	pod, err := client.CoreV1().Pods(rt.namespace).Get(context.Background(), ref.ID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Annotations[corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix+workspaceMounterContainer] = "localhost/" + profile
	_, err = client.CoreV1().Pods(rt.namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	WithAppArmorEnforcement(profile)(rt)
	rt.controlExecutor = podExecutorFunc(func(ctx context.Context, _, _ string, argv []string, _ []byte) ([]byte, error) {
		require.Equal(t, appArmorReadForTest, argv)
		deadline, exists := ctx.Deadline()
		require.True(t, exists)
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	require.Error(t, rt.verifyMounterAppArmor(ctx, ref))
	require.Less(t, time.Since(started), time.Second)
}
