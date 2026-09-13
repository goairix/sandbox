package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
)

const appArmorDigestKey = "sandbox.apparmor.profile-digest"

func cloneNodeSelector(selector map[string]string) map[string]string {
	if len(selector) == 0 {
		return nil
	}
	result := make(map[string]string, len(selector))
	for key, value := range selector {
		result[key] = value
	}
	return result
}

// WithNodeSelector installs an owned scheduling selector for ordinary and FUSE Pods.
// The option and every runtime receiving it have independent map ownership.
func WithNodeSelector(selector map[string]string) Option {
	owned := cloneNodeSelector(selector)
	return func(r *Runtime) { r.nodeSelector = cloneNodeSelector(owned) }
}

// WithAppArmorLoader enables bounded startup verification and the existing private
// prepared/authorization enforcement guard. Inspection/drain must omit this option.
func WithAppArmorLoader(name, namespace, profile string, timeout time.Duration) Option {
	return func(r *Runtime) {
		if name == "" {
			return
		}
		r.appArmorLoaderName = name
		r.appArmorLoaderNamespace = namespace
		r.appArmorLoaderTimeout = timeout
		r.appArmorProfile = profile
	}
}

func (r *Runtime) validateAppArmorLoaderOptions() error {
	if len(r.nodeSelector) > 64 {
		return errors.New("kubernetes runtime node selector exceeds 64 entries")
	}
	selector := cloneNodeSelector(r.nodeSelector)
	for key, value := range selector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return errors.New("invalid Kubernetes runtime node selector")
		}
	}
	if r.appArmorLoaderName != "" {
		if len(validation.IsDNS1123Subdomain(r.appArmorLoaderName)) != 0 || r.appArmorLoaderNamespace == "" || len(validation.IsDNS1123Label(r.appArmorLoaderNamespace)) != 0 || !immutableAppArmorProfile.MatchString(r.appArmorProfile) || r.appArmorLoaderTimeout <= 0 || r.appArmorLoaderTimeout > 600*time.Second {
			return errors.New("invalid AppArmor loader runtime options")
		}
		if value, ok := selector["kubernetes.io/os"]; ok && value != "linux" {
			return errors.New("AppArmor loader selector conflicts with Linux")
		}
		if selector == nil {
			selector = map[string]string{}
		}
		selector["kubernetes.io/os"] = "linux"
		if len(selector) > 64 {
			return errors.New("effective Linux runtime node selector exceeds 64 entries")
		}
	}
	r.nodeSelector = selector
	return nil
}

func exactLoaderArgs(args []string, profile, digest string) bool {
	names, digests := 0, 0
	for _, arg := range args {
		if strings.HasPrefix(arg, "--profile-name") {
			if arg != "--profile-name="+profile {
				return false
			}
			names++
		}
		if strings.HasPrefix(arg, "--profile-digest") {
			if arg != "--profile-digest="+digest {
				return false
			}
			digests++
		}
	}
	return names == 1 && digests == 1
}

func validLoaderDaemonSet(ds *appsv1.DaemonSet, profile string, selector map[string]string) error {
	if ds == nil || ds.UID == "" || ds.Generation <= 0 || ds.DeletionTimestamp != nil || ds.Status.ObservedGeneration != ds.Generation || ds.Status.DesiredNumberScheduled <= 0 {
		return errors.New("AppArmor loader DaemonSet identity/generation or scheduling is not ready")
	}
	if !immutableAppArmorProfile.MatchString(profile) {
		return errors.New("invalid loader profile contract")
	}
	digest := strings.TrimPrefix(profile, "sandbox-fuse-")
	if ds.Annotations[appArmorDigestKey] != digest || ds.Spec.Template.Annotations[appArmorDigestKey] != digest || ds.Spec.Template.Labels[appArmorDigestKey] != digest[:63] {
		return errors.New("AppArmor loader DaemonSet policy digest mismatch")
	}
	if !reflect.DeepEqual(ds.Spec.Template.Spec.NodeSelector, selector) {
		return errors.New("AppArmor loader and runtime scheduling selectors differ")
	}
	if ds.Spec.Selector == nil {
		return errors.New("AppArmor loader DaemonSet selector missing")
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil || labelSelector.Empty() || !labelSelector.Matches(labels.Set(ds.Spec.Template.Labels)) {
		return errors.New("AppArmor loader DaemonSet selector invalid")
	}
	if len(ds.Spec.Template.Spec.Containers) != 1 || len(ds.Spec.Template.Spec.InitContainers) != 0 {
		return errors.New("unexpected AppArmor loader container template")
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.Name != "apparmor-loader" || container.Image == "" || !exactLoaderArgs(container.Args, profile, digest) {
		return errors.New("AppArmor loader container policy arguments mismatch")
	}
	return nil
}

func loaderPodTemplateSpecMatches(pod *corev1.Pod, template corev1.PodTemplateSpec) bool {
	current := pod.DeepCopy()
	desired := &corev1.Pod{Spec: *template.Spec.DeepCopy()}
	kubescheme.Scheme.Default(current)
	kubescheme.Scheme.Default(desired)
	if !normalizeLoaderDaemonSetScheduling(current, desired) {
		return false
	}
	current.Spec.NodeName = ""
	desired.Spec.NodeName = ""
	if desired.Spec.ServiceAccountName == "" && current.Spec.ServiceAccountName == "default" {
		current.Spec.ServiceAccountName = ""
		current.Spec.DeprecatedServiceAccount = ""
	}
	if len(desired.Spec.ImagePullSecrets) == 0 && validServiceAccountImagePullSecrets(current.Spec.ImagePullSecrets) {
		current.Spec.ImagePullSecrets = nil
	}
	// Trust Priority admission to resolve only an explicitly selected, unchanged
	// class. Never mask class drift or values explicitly pinned by the template.
	if desired.Spec.PriorityClassName != "" && current.Spec.PriorityClassName == desired.Spec.PriorityClassName {
		if desired.Spec.Priority == nil {
			current.Spec.Priority = nil
		}
		if desired.Spec.PreemptionPolicy == nil {
			current.Spec.PreemptionPolicy = nil
		}
	}
	if desired.Spec.PriorityClassName == "" && desired.Spec.Priority == nil && current.Spec.Priority != nil && *current.Spec.Priority == 0 {
		current.Spec.Priority = nil
	}
	if desired.Spec.PriorityClassName == "" && desired.Spec.PreemptionPolicy == nil && current.Spec.PreemptionPolicy != nil && *current.Spec.PreemptionPolicy == corev1.PreemptLowerPriority {
		current.Spec.PreemptionPolicy = nil
	}
	return apiequality.Semantic.DeepEqual(current.Spec, desired.Spec)
}

// The DaemonSet controller replaces required node affinity with the assigned
// node-name match and adds a fixed set of health tolerations. Normalize only
// these documented controller mutations; all other Pod/container/volume/probe
// fields must retain exact current-template semantics after Kubernetes defaults.
func normalizeLoaderDaemonSetScheduling(current, desired *corev1.Pod) bool {
	if desired.Spec.Affinity != nil && desired.Spec.Affinity.NodeAffinity != nil && desired.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		return false
	} // cannot recover original selector from controller-derived Pod
	if a := current.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		terms := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 1 || len(terms[0].MatchExpressions) != 0 || len(terms[0].MatchFields) != 1 {
			return false
		}
		field := terms[0].MatchFields[0]
		if field.Key != "metadata.name" || field.Operator != corev1.NodeSelectorOpIn || len(field.Values) != 1 || current.Spec.NodeName == "" || field.Values[0] != current.Spec.NodeName {
			return false
		}
		a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		if len(a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
			a.NodeAffinity = nil
		}
		if a.PodAffinity == nil && a.PodAntiAffinity == nil && a.NodeAffinity == nil {
			current.Spec.Affinity = nil
		}
	}
	filtered := make([]corev1.Toleration, 0, len(current.Spec.Tolerations))
	for _, tol := range current.Spec.Tolerations {
		expected := false
		for _, want := range desired.Spec.Tolerations {
			if apiequality.Semantic.DeepEqual(tol, want) {
				expected = true
				break
			}
		}
		automatic := tol.Operator == corev1.TolerationOpExists && tol.Value == "" && tol.TolerationSeconds == nil && ((tol.Effect == corev1.TaintEffectNoExecute && (tol.Key == corev1.TaintNodeNotReady || tol.Key == corev1.TaintNodeUnreachable)) || (tol.Effect == corev1.TaintEffectNoSchedule && (tol.Key == corev1.TaintNodeDiskPressure || tol.Key == corev1.TaintNodeMemoryPressure || tol.Key == corev1.TaintNodePIDPressure || tol.Key == corev1.TaintNodeUnschedulable || (tol.Key == corev1.TaintNodeNetworkUnavailable && desired.Spec.HostNetwork))))
		if expected || !automatic {
			filtered = append(filtered, tol)
		}
	}
	if len(filtered) == 0 {
		filtered = nil
	}
	current.Spec.Tolerations = filtered
	less := func(tolerations []corev1.Toleration) func(int, int) bool {
		return func(i, j int) bool {
			a, _ := json.Marshal(tolerations[i])
			b, _ := json.Marshal(tolerations[j])
			return string(a) < string(b)
		}
	} // these structs contain no unmarshalable values
	sort.Slice(current.Spec.Tolerations, less(current.Spec.Tolerations))
	sort.Slice(desired.Spec.Tolerations, less(desired.Spec.Tolerations))
	return true
}

func appArmorLoaderObservationReady(ds *appsv1.DaemonSet, pods []corev1.Pod, profile string, selector map[string]string) (bool, error) {
	if err := validLoaderDaemonSet(ds, profile, selector); err != nil {
		return false, err
	}
	digest := strings.TrimPrefix(profile, "sandbox-fuse-")
	for _, pod := range pods {
		if pod.Namespace != ds.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || pod.Labels[appArmorDigestKey] != digest[:63] || pod.Annotations[appArmorDigestKey] != digest || !loaderPodTemplateSpecMatches(&pod, ds.Spec.Template) {
			continue
		}
		owned := false
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && owner.Name == ds.Name && owner.UID == ds.UID {
				owned = true
			}
		}
		if !owned {
			continue
		}
		matches := true
		for key, value := range ds.Spec.Template.Labels {
			if got, exists := pod.Labels[key]; !exists || got != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		for key, value := range ds.Spec.Template.Annotations {
			if got, exists := pod.Annotations[key]; !exists || got != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
	}
	return false, errors.New("no current-template owned AppArmor loader Pod is Ready")
}

// waitForAppArmorLoader uses only exact release DS get and selector-scoped Pod
// list, never Node reads/writes or all-node readiness. UID is pinned on first
// observation; a same-name replacement aborts this startup attempt.
func (r *Runtime) waitForAppArmorLoader(ctx context.Context) error {
	if r.appArmorLoaderName == "" {
		return nil
	}
	if err := r.validateAppArmorLoaderOptions(); err != nil {
		return err
	}
	if r.client == nil {
		return errors.New("AppArmor loader gate needs Kubernetes client")
	}
	bounded, cancel := context.WithTimeout(ctx, r.appArmorLoaderTimeout)
	defer cancel()
	interval := r.pollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	var expectedUID types.UID
	var lastErr error
	for {
		ds, err := r.client.AppsV1().DaemonSets(r.appArmorLoaderNamespace).Get(bounded, r.appArmorLoaderName, metav1.GetOptions{})
		if err == nil {
			if ds.UID != "" {
				if expectedUID != "" && expectedUID != ds.UID {
					return errors.New("AppArmor loader DaemonSet identity changed during startup")
				}
				expectedUID = ds.UID
			}
			err = validLoaderDaemonSet(ds, r.appArmorProfile, r.nodeSelector)
			if err == nil {
				selector, selectorErr := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
				if selectorErr != nil {
					return selectorErr
				}
				pods, listErr := r.client.CoreV1().Pods(r.appArmorLoaderNamespace).List(bounded, metav1.ListOptions{LabelSelector: selector.String()})
				err = listErr
				if err == nil {
					ready, observationErr := appArmorLoaderObservationReady(ds, pods.Items, r.appArmorProfile, r.nodeSelector)
					err = observationErr
					if ready {
						current, getErr := r.client.AppsV1().DaemonSets(r.appArmorLoaderNamespace).Get(bounded, r.appArmorLoaderName, metav1.GetOptions{})
						err = getErr
						if getErr == nil {
							if current.UID != expectedUID {
								return errors.New("AppArmor loader DaemonSet identity changed during startup")
							}
							if current.Generation == ds.Generation && reflect.DeepEqual(current.Spec.Template, ds.Spec.Template) && reflect.DeepEqual(current.Spec.Selector, ds.Spec.Selector) {
								if err := validLoaderDaemonSet(current, r.appArmorProfile, r.nodeSelector); err == nil {
									return bounded.Err()
								} else {
									getErr = err
								}
							}
							err = errors.Join(getErr, errors.New("AppArmor loader template changed during verification"))
						}
					}
				}
			}
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return fmt.Errorf("AppArmor loader startup permission denied: %w", err)
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-bounded.Done():
			return fmt.Errorf("AppArmor loader startup gate incomplete (%v): %w", lastErr, bounded.Err())
		case <-timer.C:
		}
	}
}
