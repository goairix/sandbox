package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestDNSAdmissionDefaultPodBaseline(t *testing.T) {
	ordinary, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-baseline", Image: "example.test/sandbox:v1"})
	require.NoError(t, err)
	fuse, err := buildPreparedFUSEPod("runtime", preparedFUSESpecForTest())
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"ordinary", ordinary, "a8e4406774995e4bb2c4fac136d9a0fc142c82ac8d0e6aa5a9a15f2bbf2ae57e"},
		{"fuse", fuse, "44530bbabacf0b481ad7d1317afe5637a712d2add0b15e4bc0b99554b48989cf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.pod)
			require.NoError(t, err)
			require.Equal(t, tc.want, fmt.Sprintf("%x", sha256.Sum256(encoded)))
		})
	}
}

func TestPodIntentMismatchReasonReportsDNSNotDefaultPriority(t *testing.T) {
	desired, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-diagnostic", Image: "example.test/sandbox:v1"})
	require.NoError(t, err)
	current := desired.DeepCopy()
	current.UID = "fixed-uid"
	current.Spec.Priority = new(int32)
	two := "2"
	current.Spec.DNSConfig.Options = []corev1.PodDNSConfigOption{{Name: "timeout", Value: &two}}
	require.False(t, preparedPodIntentMatches(current, desired, false))
	require.Equal(t, "spec.DNSConfig", preparedPodIntentMismatchReason(current, desired))
}

func configuredDNSOptions() []kubecontract.DNSOption {
	return []kubecontract.DNSOption{{Name: "single-request-reopen"}, {Name: "timeout", Value: "2"}}
}

func TestDNSAdmissionOwnedContractAndDefault(t *testing.T) {
	input := configuredDNSOptions()
	option := WithDNSAdmission(true, input)
	input[0].Value = "mutated"
	a, b := &Runtime{namespace: "runtime"}, &Runtime{namespace: "runtime"}
	option(a)
	option(b)
	require.Equal(t, "", a.dnsOptions[0].Value)
	require.Contains(t, a.WarmPoolContract(), ":dns-admission=")
	reversed := configuredDNSOptions()
	reversed[0], reversed[1] = reversed[1], reversed[0]
	c := &Runtime{namespace: "runtime"}
	WithDNSAdmission(true, reversed)(c)
	require.Equal(t, a.WarmPoolContract(), c.WarmPoolContract())
	a.dnsOptions[0].Value = "mutated"
	require.Equal(t, "", b.dnsOptions[0].Value)
	empty := &Runtime{namespace: "runtime"}
	WithDNSAdmission(false, []kubecontract.DNSOption{})(empty)
	require.Equal(t, "kubernetes-sandbox-pod/v3:control=1:runtime:cilium=false", empty.WarmPoolContract())
}

func TestDNSAdmissionIndependentSettings(t *testing.T) {
	for _, disable := range []bool{false, true} {
		for _, options := range [][]kubecontract.DNSOption{nil, configuredDNSOptions()} {
			t.Run(fmt.Sprintf("disabled=%t/options=%d", disable, len(options)), func(t *testing.T) {
				r := &Runtime{namespace: "runtime"}
				WithDNSAdmission(disable, options)(r)
				pod, err := buildOrdinaryPod(r.namespace, sandboxruntime.SandboxSpec{ID: "dns-independent", Image: "example.test/sandbox:v1"})
				require.NoError(t, err)
				baseline := pod.DeepCopy()
				require.NoError(t, r.applyDNSAdmission(pod, nil))
				_, labelled := pod.Labels[kubecontract.NodeLocalDNSInjectionLabel]
				require.Equal(t, disable, labelled)
				require.Len(t, pod.Spec.DNSConfig.Options, len(options))
				require.Equal(t, baseline.Spec.DNSPolicy, pod.Spec.DNSPolicy)
				require.Equal(t, baseline.Spec.DNSConfig.Nameservers, pod.Spec.DNSConfig.Nameservers)
				require.Equal(t, baseline.Spec.DNSConfig.Searches, pod.Spec.DNSConfig.Searches)
				require.Equal(t, disable || len(options) != 0, r.dnsAdmissionContract != "")
				current := pod.DeepCopy()
				current.UID = "fixed-uid"
				require.True(t, r.podIntentMatches(current, pod, false))
			})
		}
	}
}

func TestDNSAdmissionPodConstructionAndLabelProtection(t *testing.T) {
	for _, mode := range []string{"ordinary", "fuse"} {
		t.Run(mode, func(t *testing.T) {
			r, client := newFakeKubernetesRuntime(t, preparedScript())
			r.readyTimeout = time.Second
			WithDNSAdmission(true, configuredDNSOptions())(r)
			var err error
			if mode == "ordinary" {
				_, err = r.CreateSandbox(context.Background(), sandboxruntime.SandboxSpec{ID: "dns-ordinary", Image: "example.test/sandbox:v1"})
			} else {
				_, err = r.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
			}
			require.NoError(t, err)
			pods, err := client.CoreV1().Pods(r.namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			pod := &pods.Items[0]
			require.Equal(t, "disabled", pod.Labels[kubecontract.NodeLocalDNSInjectionLabel])
			require.Equal(t, corev1.DNSNone, pod.Spec.DNSPolicy)
			require.Empty(t, pod.Spec.DNSConfig.Searches)
			require.Len(t, pod.Spec.DNSConfig.Options, 2)
			for _, value := range []*string{nil, ptrForDNS("enabled")} {
				before := len(client.Actions())
				err := r.UpdateLabels(context.Background(), pod.Name, map[string]*string{kubecontract.NodeLocalDNSInjectionLabel: value})
				require.Error(t, err)
				require.Len(t, client.Actions(), before, "label conflict must fail before any Kubernetes action")
			}
		})
	}
	for _, mode := range []string{"ordinary", "fuse", "invalid option"} {
		t.Run("reject "+mode, func(t *testing.T) {
			r, client := newFakeKubernetesRuntime(t, preparedScript())
			WithDNSAdmission(true, configuredDNSOptions())(r)
			spec := sandboxruntime.SandboxSpec{ID: "dns-conflict", Image: "example.test/sandbox:v1"}
			if mode == "fuse" {
				spec = preparedFUSESpecForTest()
			}
			spec.Labels = map[string]string{kubecontract.NodeLocalDNSInjectionLabel: "enabled"}
			if mode == "invalid option" {
				WithDNSAdmission(false, []kubecontract.DNSOption{{Name: "unknown"}})(r)
				spec.Labels = nil
			}
			before := len(client.Actions())
			var err error
			if mode == "fuse" {
				_, err = r.PrepareSandbox(context.Background(), spec)
			} else {
				_, err = r.CreateSandbox(context.Background(), spec)
			}
			require.Error(t, err)
			require.Len(t, client.Actions(), before)
		})
	}
}

func ptrForDNS(value string) *string { return &value }

func TestDNSAdmissionConfiguredIntentStrictness(t *testing.T) {
	r := &Runtime{}
	WithDNSAdmission(true, configuredDNSOptions())(r)
	desired, err := buildOrdinaryPod("runtime", sandboxruntime.SandboxSpec{ID: "dns-intent", Image: "example.test/sandbox:v1"})
	require.NoError(t, err)
	require.NoError(t, r.applyDNSAdmission(desired, nil))
	for _, name := range []string{"exact", "order", "nil switch", "duplicate", "missing", "extra", "value", "nameserver", "search", "policy", "security", "label", "desired label"} {
		t.Run(name, func(t *testing.T) {
			current := desired.DeepCopy()
			requested := desired.DeepCopy()
			current.UID = "fixed-uid"
			current.Spec.Priority = new(int32)
			switch name {
			case "order":
				current.Spec.DNSConfig.Options[0], current.Spec.DNSConfig.Options[1] = current.Spec.DNSConfig.Options[1], current.Spec.DNSConfig.Options[0]
			case "nil switch":
				current.Spec.DNSConfig.Options[0].Value = nil
			case "duplicate":
				current.Spec.DNSConfig.Options[1] = current.Spec.DNSConfig.Options[0]
			case "missing":
				current.Spec.DNSConfig.Options = current.Spec.DNSConfig.Options[:1]
			case "extra":
				current.Spec.DNSConfig.Options = append(current.Spec.DNSConfig.Options, corev1.PodDNSConfigOption{Name: "ndots", Value: ptrForDNS("5")})
			case "value":
				current.Spec.DNSConfig.Options[1].Value = ptrForDNS("3")
			case "nameserver":
				current.Spec.DNSConfig.Nameservers = []string{"169.254.20.10"}
			case "search":
				current.Spec.DNSConfig.Searches = []string{"cluster.local"}
			case "policy":
				current.Spec.DNSPolicy = corev1.DNSClusterFirst
			case "security":
				current.Spec.Containers[0].SecurityContext.Privileged = ptrBoolForDNS(true)
			case "label":
				delete(current.Labels, kubecontract.NodeLocalDNSInjectionLabel)
			case "desired label":
				current.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "enabled"
				requested.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "enabled"
			}
			want := name == "exact" || name == "order" || name == "nil switch"
			require.Equal(t, want, r.podIntentMatches(current, requested, false))
			if want {
				require.Empty(t, r.podIntentMismatchReason(current, desired, false))
			}
			if name == "order" {
				require.False(t, preparedPodIntentMatches(current, desired, false), "unconfigured strict comparison must not change")
			}
		})
	}
	before := desired.DeepCopy()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				current := desired.DeepCopy()
				current.UID = "fixed-uid"
				if !r.podIntentMatches(current, desired, false) {
					t.Error("concurrent comparison failed")
				}
			}
		}()
	}
	wg.Wait()
	require.Equal(t, before, desired)
}

func ptrBoolForDNS(value bool) *bool { return &value }

func TestDNSAdmissionLoaderContract(t *testing.T) {
	for _, name := range []string{"exact", "order", "nil switch", "template changed", "missing template label", "missing pod label", "extra", "nameserver", "search", "policy", "owner", "generation", "volume", "probe", "profile"} {
		t.Run(name, func(t *testing.T) {
			ds, pod, profile := loaderGateFixture()
			ds.Spec.Template.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "disabled"
			pod.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "disabled"
			ds.Spec.Template.Spec.DNSConfig = &corev1.PodDNSConfig{Options: dnsPodOptions(configuredDNSOptions())}
			pod.Spec.DNSConfig = ds.Spec.Template.Spec.DNSConfig.DeepCopy()
			switch name {
			case "order":
				pod.Spec.DNSConfig.Options[0], pod.Spec.DNSConfig.Options[1] = pod.Spec.DNSConfig.Options[1], pod.Spec.DNSConfig.Options[0]
			case "nil switch":
				pod.Spec.DNSConfig.Options[0].Value = nil
			case "template changed":
				ds.Spec.Template.Spec.DNSConfig.Options[1].Value = ptrForDNS("3")
				pod.Spec.DNSConfig = ds.Spec.Template.Spec.DNSConfig.DeepCopy()
			case "missing template label":
				delete(ds.Spec.Template.Labels, kubecontract.NodeLocalDNSInjectionLabel)
			case "missing pod label":
				delete(pod.Labels, kubecontract.NodeLocalDNSInjectionLabel)
			case "extra":
				pod.Spec.DNSConfig.Options = append(pod.Spec.DNSConfig.Options, corev1.PodDNSConfigOption{Name: "ndots", Value: ptrForDNS("5")})
			case "nameserver":
				pod.Spec.DNSConfig.Nameservers = []string{"169.254.20.10"}
			case "search":
				pod.Spec.DNSConfig.Searches = []string{"cluster.local"}
			case "policy":
				pod.Spec.DNSPolicy = corev1.DNSNone
			case "owner":
				pod.OwnerReferences[0].UID = "replacement"
			case "generation":
				ds.Status.ObservedGeneration--
			case "volume":
				pod.Spec.Volumes = []corev1.Volume{{Name: "unexpected"}}
			case "probe":
				pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{InitialDelaySeconds: 1}
			case "profile":
				pod.Spec.Containers[0].Args[0] = "--profile-name=unconfined"
			}
			ready, err := appArmorLoaderObservationReadyWithDNS(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"}, true, configuredDNSOptions())
			want := name == "exact" || name == "order" || name == "nil switch"
			require.Equal(t, want, ready)
			if want {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if name == "order" {
				ready, _ := appArmorLoaderObservationReady(ds, []corev1.Pod{*pod}, profile, map[string]string{"kubernetes.io/os": "linux"})
				require.False(t, ready, "default loader still requires exact option ordering")
			}
		})
	}
}

func TestDNSAdmissionOrdinaryCreateNormalAndAmbiguous(t *testing.T) {
	for _, mode := range []string{"normal", "ambiguous", "unknown mutation"} {
		t.Run(mode, func(t *testing.T) {
			r, client := newFakeKubernetesRuntime(t, preparedScript())
			r.readyTimeout = time.Second // The existing ordinary readiness poll interval is 500ms.
			WithDNSAdmission(true, configuredDNSOptions())(r)
			client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
				current := action.(ktesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				current.UID, current.ResourceVersion = "pod-uid-a", "7"
				current.Status.Phase = corev1.PodRunning
				current.Spec.Priority = new(int32)
				current.Spec.DNSConfig.Options[0], current.Spec.DNSConfig.Options[1] = current.Spec.DNSConfig.Options[1], current.Spec.DNSConfig.Options[0]
				if mode == "unknown mutation" {
					current.Spec.DNSConfig.Options[0].Value = ptrForDNS("3")
				}
				if mode == "ambiguous" {
					current.Spec.NodeName = "scheduler-bound"
				}
				if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), current, r.namespace); err != nil {
					return true, nil, err
				}
				if mode == "ambiguous" {
					return true, nil, apierrors.NewTimeoutError("synthetic create timeout", 1)
				}
				return true, current, nil
			})
			info, err := r.CreateSandbox(context.Background(), sandboxruntime.SandboxSpec{ID: "dns-create", Image: "example.test/sandbox:v1"})
			if mode == "unknown mutation" {
				require.ErrorContains(t, err, "spec.DNSConfig")
				_, getErr := client.CoreV1().Pods(r.namespace).Get(context.Background(), "dns-create", metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(getErr))
				policies, listErr := client.NetworkingV1().NetworkPolicies(r.namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, listErr)
				require.Empty(t, policies.Items)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "pod-uid-a", info.RuntimeUID)
		})
	}
}

func TestDNSAdmissionOrdinaryClaimKeepsOptOutAndCNIIdentity(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	WithDNSAdmission(true, configuredDNSOptions())(r)
	before := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-dns", "sandbox-pool-dns")
	before.Labels[kubecontract.NodeLocalDNSInjectionLabel] = "disabled"
	before, err := client.CoreV1().Pods(r.namespace).Update(context.Background(), before, metav1.UpdateOptions{})
	require.NoError(t, err)
	ref := sandboxruntime.RuntimeRef{ID: before.Name, UID: string(before.UID)}
	require.NoError(t, r.ClaimOrdinaryPool(context.Background(), ref, "dns-owner"))
	after, err := client.CoreV1().Pods(r.namespace).Get(context.Background(), before.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, before.Labels, after.Labels)
	require.Equal(t, "dns-owner", after.Annotations[ordinaryClaimIDAnnotation])
}
