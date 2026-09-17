package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestFUSEUnstartedPodNormalDeletion(t *testing.T) {
	rt, client, pod, script := unstartedFUSEFixture(t)
	installFinalizerAwarePodDeletion(t, client)
	deletes := 0
	client.PrependReactor("delete", "pods", func(a kt.Action) (bool, k8sruntime.Object, error) {
		deletes++
		opts := a.(kt.DeleteAction).GetDeleteOptions()
		require.NotNil(t, opts.Preconditions)
		require.Equal(t, pod.UID, *opts.Preconditions.UID)
		require.Equal(t, pod.Spec.TerminationGracePeriodSeconds, opts.GracePeriodSeconds)
		return false, nil, nil
	})
	evidence, err := rt.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
	require.NoError(t, err)
	require.True(t, evidence.ProcessExited)
	require.False(t, evidence.GracefulUnmount)
	require.Equal(t, "node-a", evidence.NodeName)
	require.Equal(t, 1, deletes)
	require.Empty(t, script.commands)
	retained, err := client.CoreV1().Pods("runtime").Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, retained.Finalizers, fuseRuntimeCleanupFinalizer)
	policies, err := client.NetworkingV1().NetworkPolicies("runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, policies.Items, "policies must remain until evidence is persisted and finalized")
	require.NoError(t, rt.FinalizePreparedSandboxRemoval(context.Background(), pod.Name, string(pod.UID), evidence))
}

func unstartedFUSEFixture(t *testing.T) (*Runtime, *kubefake.Clientset, *corev1.Pod, *commandScript) {
	t.Helper()
	script := preparedScript()
	rt, client := newFakeKubernetesRuntime(t, script)
	info, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	pod, err := client.CoreV1().Pods("runtime").Get(context.Background(), info.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	pod.Spec.NodeName = "node-a"
	pod.Status.Phase = corev1.PodPending
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: workspaceMounterContainer,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError"}}}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: sandboxContainer,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
	require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
	script.commands = nil
	script.handler = func(recordedPodCommand) ([]byte, error) { return nil, errors.New("container has not started") }
	client.ClearActions()
	return rt, client, pod, script
}

func terminalUnstartedFUSE(pod *corev1.Pod) {
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Status.Phase = corev1.PodFailed
	if len(pod.Status.InitContainerStatuses) > 0 {
		pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	}
}

func assertFUSECleanupProtected(t *testing.T, client *kubefake.Clientset) {
	t.Helper()
	policies, err := client.NetworkingV1().NetworkPolicies("runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, policies.Items)
	for _, a := range client.Actions() {
		require.False(t, a.GetVerb() == "update" && a.GetResource().Resource == "pods", "no finalizer release before proof")
		require.False(t, a.GetVerb() == "delete" && a.GetResource().Resource != "pods", "no policy release before proof")
	}
}

func TestFUSEUnstartedPodWaitsForKubeletEvidence(t *testing.T) {
	for _, scenario := range []string{"never started", "started during deletion", "lost DELETE response", "already deleting"} {
		t.Run(scenario, func(t *testing.T) {
			rt, client, pod, script := unstartedFUSEFixture(t)
			policies, err := client.NetworkingV1().NetworkPolicies("runtime").List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, policies.Items)
			deleting := scenario == "already deleting"
			if deleting {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
			}
			deletes, observations := 0, 0
			client.PrependReactor("delete", "pods", func(a kt.Action) (bool, k8sruntime.Object, error) {
				deletes++
				opts := a.(kt.DeleteAction).GetDeleteOptions()
				require.NotNil(t, opts.Preconditions)
				require.Equal(t, pod.UID, *opts.Preconditions.UID)
				require.Equal(t, pod.Spec.TerminationGracePeriodSeconds, opts.GracePeriodSeconds)
				deleting = true
				if scenario == "lost DELETE response" {
					return true, nil, errors.New("lost API response")
				}
				return true, nil, nil
			})
			client.PrependReactor("get", "pods", func(kt.Action) (bool, k8sruntime.Object, error) {
				if !deleting {
					return false, nil, nil
				}
				observations++
				current := pod.DeepCopy()
				now := metav1.Now()
				current.DeletionTimestamp = &now
				if observations < 3 {
					require.Contains(t, current.Finalizers, fuseRuntimeCleanupFinalizer)
					for _, policy := range policies.Items {
						_, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", policy.Name)
						require.NoError(t, err, "policies must remain before terminal proof")
					}
					if scenario == "started during deletion" {
						started := true
						current.Status.InitContainerStatuses[0].Started = &started
						current.Status.InitContainerStatuses[0].ContainerID = "containerd://started"
						current.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
					}
				} else {
					terminalUnstartedFUSE(current)
				}
				require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), current, "runtime"))
				return true, current, nil
			})
			evidence, err := rt.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
			require.NoError(t, err)
			require.True(t, evidence.ProcessExited)
			require.False(t, evidence.GracefulUnmount)
			require.Equal(t, "node-a", evidence.NodeName)
			require.GreaterOrEqual(t, observations, 3)
			wantDeletes := 1
			if scenario == "already deleting" {
				wantDeletes = 0
			}
			require.Equal(t, wantDeletes, deletes)
			require.Empty(t, script.commands, "never exec into an unstarted container")
			assertFUSECleanupProtected(t, client)
		})
	}
}

func TestFUSEUnstartedPodCannotInferExitFromDeletion(t *testing.T) {
	for _, scenario := range []string{"gone", "replacement", "finalizer lost", "identity drift", "attempt drift", "node drift", "namespace drift", "incomplete", "mixed terminated", "no deletion", "timeout", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			rt, client, pod, script := unstartedFUSEFixture(t)
			rt.terminationTimeout = 5 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deleting, deletes := false, 0
			client.PrependReactor("delete", "pods", func(kt.Action) (bool, k8sruntime.Object, error) {
				deleting = true
				deletes++
				if scenario == "cancelled" {
					cancel()
				}
				return true, nil, nil
			})
			client.PrependReactor("get", "pods", func(kt.Action) (bool, k8sruntime.Object, error) {
				if !deleting {
					return false, nil, nil
				}
				current := pod.DeepCopy()
				terminalUnstartedFUSE(current)
				switch scenario {
				case "gone":
					require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", pod.Name))
					return false, nil, nil
				case "replacement":
					current.UID = "pod-replacement"
				case "finalizer lost":
					current.Finalizers = nil
				case "identity drift":
					current.Labels["sandbox.managed"] = "false"
				case "attempt drift":
					current.Annotations[fusePrepareAttemptAnnotation] = strings.Repeat("f", 64)
				case "node drift":
					current.Spec.NodeName = "node-b"
				case "namespace drift":
					current.Namespace = "other"
					// The object store correctly refuses cross-namespace writes;
					// simulate a malformed GET response without mutating its scope.
					return true, current, nil
				case "incomplete":
					current.Status.ContainerStatuses = nil
				case "mixed terminated":
					current.Status.InitContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
				case "no deletion":
					current.DeletionTimestamp = nil
				case "timeout", "cancelled":
					current.Status.Phase = corev1.PodPending
					current.Status.InitContainerStatuses = pod.Status.InitContainerStatuses
				}
				require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), current, "runtime"))
				return true, current, nil
			})
			evidence, err := rt.ConfirmPreparedSandboxTermination(ctx, pod.Name, string(pod.UID))
			require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
			require.False(t, evidence.ProcessExited)
			require.Equal(t, 1, deletes, "uncertain DELETE must not be replayed")
			require.Empty(t, script.commands)
			assertFUSECleanupProtected(t, client)
		})
	}
}

func TestFUSEUnstartedPodFallbackRequiresExactInfrastructureFence(t *testing.T) {
	for _, scenario := range []string{"matching", "wrong UID", "wrong node", "no proof"} {
		t.Run(scenario, func(t *testing.T) {
			rt, client, pod, _ := unstartedFUSEFixture(t)
			evidence := sandboxruntime.TerminationEvidence{RuntimeUID: string(pod.UID), NodeName: pod.Spec.NodeName, InfrastructureFenced: true}
			switch scenario {
			case "wrong UID":
				evidence.RuntimeUID = "other"
			case "wrong node":
				evidence.NodeName = "other"
			case "no proof":
				evidence.InfrastructureFenced = false
			}
			fencer := &fakeInfrastructureFencer{evidence: evidence}
			rt.infraFencer = fencer
			client.PrependReactor("delete", "pods", func(kt.Action) (bool, k8sruntime.Object, error) {
				require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "runtime", pod.Name))
				return true, nil, nil
			})
			proof, err := rt.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
			if scenario == "matching" {
				require.NoError(t, err)
				require.True(t, proof.InfrastructureFenced)
			} else {
				require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
			}
			require.Equal(t, []string{pod.Spec.NodeName}, fencer.nodes)
			assertFUSECleanupProtected(t, client)
		})
	}
}

func TestFUSEUnstartedPodCancellationBeforeDeleteIsReadOnly(t *testing.T) {
	rt, client, pod, _ := unstartedFUSEFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	client.PrependReactor("get", "pods", func(kt.Action) (bool, k8sruntime.Object, error) {
		reads++
		if reads == 2 {
			cancel()
		}
		return false, nil, nil
	})
	_, err := rt.ConfirmPreparedSandboxTermination(ctx, pod.Name, string(pod.UID))
	require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
	for _, a := range client.Actions() {
		require.Equal(t, "get", a.GetVerb())
	}
}

func TestFUSEUnstartedPodRecoversWithoutInMemoryProof(t *testing.T) {
	rt, client, pod, _ := unstartedFUSEFixture(t)
	terminalUnstartedFUSE(pod)
	require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
	restarted := &Runtime{client: client, dynClient: rt.dynClient, namespace: rt.namespace,
		pollInterval: rt.pollInterval, terminationTimeout: rt.terminationTimeout}
	evidence, err := restarted.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
	require.NoError(t, err)
	require.True(t, evidence.ProcessExited)
	require.Equal(t, pod.Spec.NodeName, evidence.NodeName)
	assertFUSECleanupProtected(t, client)
	installFinalizerAwarePodDeletion(t, client)
	require.NoError(t, restarted.FinalizePreparedSandboxRemoval(context.Background(), pod.Name, string(pod.UID), evidence))
	policies, err := client.NetworkingV1().NetworkPolicies("runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, policies.Items)
}

func TestFUSEUnstartedPodCandidateRejectsUnsafeStatuses(t *testing.T) {
	_, _, original, _ := unstartedFUSEFixture(t)
	mutations := map[string]func(*corev1.Pod){
		"running": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		},
		"mixed waiting": func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{} },
		"mixed terminated": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{}
		},
		"container ID": func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].ContainerID = "containerd://prior" },
		"restarts":     func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].RestartCount = 1 },
		"started":      func(p *corev1.Pod) { started := true; p.Status.InitContainerStatuses[0].Started = &started },
		"last terminated": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{}
		},
		"last running": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].LastTerminationState.Running = &corev1.ContainerStateRunning{}
		},
		"last waiting": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].LastTerminationState.Waiting = &corev1.ContainerStateWaiting{}
		},
		"unknown":          func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State = corev1.ContainerState{} },
		"missing init":     func(p *corev1.Pod) { p.Status.InitContainerStatuses = nil },
		"missing ordinary": func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"duplicate status": func(p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		},
		"undeclared status":     func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "extra" },
		"duplicate declaration": func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, p.Spec.Containers[0]) },
		"empty declaration":     func(p *corev1.Pod) { p.Spec.Containers[0].Name = ""; p.Status.ContainerStatuses[0].Name = "" },
		"unbound":               func(p *corev1.Pod) { p.Spec.NodeName = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pod := original.DeepCopy()
			mutate(pod)
			require.False(t, preparedPodCanRequestNormalDeletion(pod))
			if pod.Spec.NodeName != "" {
				terminalUnstartedFUSE(pod)
				// Mutations on the ordinary container remain unsafe even in a
				// deleting/Failed Pod, when never-started Waiting is permitted.
				if strings.Contains(name, "ordinary") || strings.Contains(name, "duplicate") || strings.Contains(name, "declaration") || name == "undeclared status" {
					require.False(t, preparedPodContainersTerminated(pod))
				}
			}
		})
	}
}

func TestFUSEUnstartedPodCoversEphemeralContainers(t *testing.T) {
	_, _, pod, _ := unstartedFUSEFixture(t)
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}
	require.False(t, preparedPodCanRequestNormalDeletion(pod))
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}}}
	require.True(t, preparedPodCanRequestNormalDeletion(pod))
	require.False(t, preparedPodContainersTerminated(pod), "a deletion candidate is not exit proof")
	terminalUnstartedFUSE(pod)
	require.True(t, preparedPodContainersTerminated(pod))
	pod.Status.EphemeralContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
	require.False(t, preparedPodContainersTerminated(pod))
	pod.Status.EphemeralContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	require.True(t, preparedPodContainersTerminated(pod))
	pod.Status.EphemeralContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{}
	require.False(t, preparedPodContainersTerminated(pod))
}

func TestFUSEUnstartedPodKeepsLegacyPathForHistory(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			rt, client, pod, script := unstartedFUSEFixture(t)
			if running {
				pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			} else {
				pod.Status.InitContainerStatuses[0].RestartCount = 1
			}
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, "runtime"))
			_, err := rt.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
			require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
			require.NotEmpty(t, script.commands, "running/history status must retain shutdown path")
			for _, a := range client.Actions() {
				require.NotEqual(t, "delete", a.GetVerb())
			}
			assertFUSECleanupProtected(t, client)
		})
	}
}

func BenchmarkFUSEDeletionCandidateRunning(b *testing.B) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{Name: workspaceMounterContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}}
	b.ReportAllocs()
	for b.Loop() {
		if preparedPodCanRequestNormalDeletion(pod) {
			b.Fatal("running Pod must not select new deletion path")
		}
	}
}
