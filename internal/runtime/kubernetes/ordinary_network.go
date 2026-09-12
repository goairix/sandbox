package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/goairix/sandbox/internal/runtime"
)

const (
	ordinaryPolicyRole              = "ordinary"
	ordinaryPrivateDenyPolicyRole   = "ordinary-private-deny"
	ordinaryPolicyRoleLabel         = "sandbox.policy.role"
	ordinaryRuntimeIDLabel          = "sandbox.runtime.id"
	ordinaryRuntimeUIDAnnotation    = "sandbox.runtime.uid"
	ordinaryPolicyAttemptAnnotation = "sandbox.network.attempt"
)

type ordinaryNetworkIdentity struct {
	runtimeID  string
	runtimeUID types.UID
	logicalID  string
}

func ordinaryNetworkPolicyIntentMatches(current, desired *networkingv1.NetworkPolicy) bool {
	if current == nil || desired == nil || current.UID == "" || current.Name != desired.Name || current.Namespace != desired.Namespace {
		return false
	}
	for key, value := range desired.Labels {
		if current.Labels[key] != value {
			return false
		}
	}
	for key, value := range desired.Annotations {
		if current.Annotations[key] != value {
			return false
		}
	}
	return apiequality.Semantic.DeepEqual(current.Spec, desired.Spec)
}

func ordinaryCiliumPolicyIntentMatches(current, desired *unstructured.Unstructured) bool {
	if current == nil || desired == nil || current.GetUID() == "" || current.GetName() != desired.GetName() || current.GetNamespace() != desired.GetNamespace() {
		return false
	}
	for key, value := range desired.GetLabels() {
		if current.GetLabels()[key] != value {
			return false
		}
	}
	for key, value := range desired.GetAnnotations() {
		if current.GetAnnotations()[key] != value {
			return false
		}
	}
	return reflect.DeepEqual(current.Object["spec"], desired.Object["spec"])
}

func validateOrdinaryNetworkPolicy(policy *networkingv1.NetworkPolicy, identity ordinaryNetworkIdentity, attempt string) error {
	if policy == nil || policy.Name != "sandbox-"+identity.logicalID ||
		policy.Labels["sandbox.managed"] != "true" ||
		policy.Labels["sandbox.id"] != identity.logicalID ||
		policy.Labels[ordinaryPolicyRoleLabel] != ordinaryPolicyRole ||
		policy.Labels[ordinaryRuntimeIDLabel] != identity.runtimeID ||
		len(policy.Spec.PodSelector.MatchLabels) != 1 ||
		policy.Spec.PodSelector.MatchLabels["sandbox.id"] != identity.logicalID {
		return fmt.Errorf("ordinary NetworkPolicy does not match runtime identity")
	}
	if attempt != "" && policy.Annotations[ordinaryPolicyAttemptAnnotation] != attempt {
		return fmt.Errorf("ordinary NetworkPolicy belongs to another attempt")
	}
	if identity.runtimeUID != "" && policy.Annotations[ordinaryRuntimeUIDAnnotation] != string(identity.runtimeUID) {
		return fmt.Errorf("ordinary NetworkPolicy runtime UID does not match")
	}
	return nil
}

func validateOrdinaryCiliumPolicy(policy *unstructured.Unstructured, identity ordinaryNetworkIdentity, attempt string) error {
	if policy == nil || policy.GetName() != "sandbox-private-deny-"+identity.logicalID ||
		policy.GetLabels()["sandbox.managed"] != "true" ||
		policy.GetLabels()["sandbox.id"] != identity.logicalID ||
		policy.GetLabels()[ordinaryPolicyRoleLabel] != ordinaryPrivateDenyPolicyRole ||
		policy.GetLabels()[ordinaryRuntimeIDLabel] != identity.runtimeID {
		return fmt.Errorf("ordinary CiliumNetworkPolicy does not match runtime identity")
	}
	selector, found, err := unstructured.NestedStringMap(policy.Object, "spec", "endpointSelector", "matchLabels")
	if err != nil || !found || len(selector) != 1 || selector["sandbox.id"] != identity.logicalID {
		return fmt.Errorf("ordinary CiliumNetworkPolicy selector does not match runtime identity")
	}
	if attempt != "" && policy.GetAnnotations()[ordinaryPolicyAttemptAnnotation] != attempt {
		return fmt.Errorf("ordinary CiliumNetworkPolicy belongs to another attempt")
	}
	if identity.runtimeUID != "" && policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation] != string(identity.runtimeUID) {
		return fmt.Errorf("ordinary CiliumNetworkPolicy runtime UID does not match")
	}
	return nil
}

func validateMutableOrdinaryNetworkPolicy(policy *networkingv1.NetworkPolicy, identity ordinaryNetworkIdentity) error {
	if policy == nil || policy.Name != "sandbox-"+identity.logicalID ||
		policy.Labels["sandbox.managed"] != "true" ||
		policy.Labels["sandbox.id"] != identity.logicalID ||
		len(policy.Spec.PodSelector.MatchLabels) != 1 ||
		policy.Spec.PodSelector.MatchLabels["sandbox.id"] != identity.logicalID {
		return fmt.Errorf("ordinary NetworkPolicy does not match runtime identity")
	}
	role := policy.Labels[ordinaryPolicyRoleLabel]
	if role == "" {
		if policy.Labels[ordinaryRuntimeIDLabel] != "" || policy.Annotations[ordinaryRuntimeUIDAnnotation] != "" {
			return fmt.Errorf("historical ordinary NetworkPolicy has partial runtime binding")
		}
		return nil
	}
	if role != ordinaryPolicyRole || policy.Labels[ordinaryRuntimeIDLabel] != identity.runtimeID || policy.Annotations[ordinaryRuntimeUIDAnnotation] != string(identity.runtimeUID) {
		return fmt.Errorf("ordinary NetworkPolicy runtime binding does not match")
	}
	return nil
}

func validateMutableOrdinaryCiliumPolicy(policy *unstructured.Unstructured, identity ordinaryNetworkIdentity) error {
	if policy == nil || policy.GetName() != "sandbox-private-deny-"+identity.logicalID ||
		policy.GetLabels()["sandbox.managed"] != "true" ||
		policy.GetLabels()["sandbox.id"] != identity.logicalID {
		return fmt.Errorf("ordinary CiliumNetworkPolicy does not match runtime identity")
	}
	selector, found, err := unstructured.NestedStringMap(policy.Object, "spec", "endpointSelector", "matchLabels")
	if err != nil || !found || len(selector) != 1 || selector["sandbox.id"] != identity.logicalID {
		return fmt.Errorf("ordinary CiliumNetworkPolicy selector does not match")
	}
	role := policy.GetLabels()[ordinaryPolicyRoleLabel]
	if role == "" {
		if policy.GetLabels()[ordinaryRuntimeIDLabel] != "" || policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation] != "" {
			return fmt.Errorf("historical ordinary CiliumNetworkPolicy has partial runtime binding")
		}
		return nil
	}
	if role != ordinaryPrivateDenyPolicyRole || policy.GetLabels()[ordinaryRuntimeIDLabel] != identity.runtimeID || policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation] != string(identity.runtimeUID) {
		return fmt.Errorf("ordinary CiliumNetworkPolicy runtime binding does not match")
	}
	return nil
}

func updateOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace string, identity ordinaryNetworkIdentity, enabled bool, whitelist []string, blockPrivate bool) error {
	attempt, err := newNetworkAttemptToken()
	if err != nil {
		return err
	}
	target, err := buildOrdinaryNetworkPolicy(namespace, identity, attempt, enabled, whitelist, blockPrivate)
	if err != nil {
		return err
	}
	policies := client.NetworkingV1().NetworkPolicies(namespace)
	current, err := policies.Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = createOrdinaryNetworkPolicy(ctx, client, target, identity, attempt)
		return err
	}
	if err != nil {
		return fmt.Errorf("get ordinary NetworkPolicy for update: %w", err)
	}
	if err := validateMutableOrdinaryNetworkPolicy(current, identity); err != nil {
		return err
	}
	target.ResourceVersion = current.ResourceVersion
	target.UID = current.UID
	updated, err := policies.Update(ctx, target, metav1.UpdateOptions{})
	if err != nil {
		verified, getErr := policies.Get(ctx, target.Name, metav1.GetOptions{})
		if getErr == nil && ordinaryNetworkPolicyIntentMatches(verified, target) {
			return nil
		}
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("update ordinary NetworkPolicy: %w", err), getErr)
	}
	if !ordinaryNetworkPolicyIntentMatches(updated, target) {
		return fmt.Errorf("updated ordinary NetworkPolicy does not match requested intent")
	}
	return nil
}

func updateOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace string, identity ordinaryNetworkIdentity) error {
	attempt, err := newNetworkAttemptToken()
	if err != nil {
		return err
	}
	target, err := buildOrdinaryCiliumPrivateDeny(namespace, identity, attempt)
	if err != nil {
		return err
	}
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(namespace)
	current, err := policies.Get(ctx, target.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = createOrdinaryCiliumPrivateDeny(ctx, client, target, identity, attempt)
		return err
	}
	if err != nil {
		return fmt.Errorf("get ordinary CiliumNetworkPolicy for update: %w", err)
	}
	if err := validateMutableOrdinaryCiliumPolicy(current, identity); err != nil {
		return err
	}
	target.SetResourceVersion(current.GetResourceVersion())
	target.SetUID(current.GetUID())
	updated, err := policies.Update(ctx, target, metav1.UpdateOptions{})
	if err != nil {
		verified, getErr := policies.Get(ctx, target.GetName(), metav1.GetOptions{})
		if getErr == nil && ordinaryCiliumPolicyIntentMatches(verified, target) {
			return nil
		}
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("update ordinary CiliumNetworkPolicy: %w", err), getErr)
	}
	if !ordinaryCiliumPolicyIntentMatches(updated, target) {
		return fmt.Errorf("updated ordinary CiliumNetworkPolicy does not match requested intent")
	}
	return nil
}

func deleteMutableOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace string, identity ordinaryNetworkIdentity) error {
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(namespace)
	name := "sandbox-private-deny-" + identity.logicalID
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ordinary CiliumNetworkPolicy for delete: %w", err)
	}
	if err := validateMutableOrdinaryCiliumPolicy(current, identity); err != nil {
		return err
	}
	if err := deleteCreatedCiliumPolicy(ctx, policies, current); err != nil {
		return fmt.Errorf("delete ordinary CiliumNetworkPolicy: %w", err)
	}
	return nil
}

func deleteExactOrdinaryPod(ctx context.Context, client kubernetes.Interface, namespace string, pod *corev1.Pod, pollInterval, timeout time.Duration) error {
	if pod == nil || pod.UID == "" {
		return fmt.Errorf("ordinary Pod has no immutable UID")
	}
	uid := pod.UID
	err := client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete exact ordinary Pod: %w", err)
	}
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		current, getErr := client.CoreV1().Pods(namespace).Get(waitCtx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) || (getErr == nil && current.UID != uid) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("verify exact ordinary Pod deletion: %w", getErr)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("exact ordinary Pod deletion is unconfirmed: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func deleteOwnedOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace string, identity ordinaryNetworkIdentity, allowLegacy bool) (bool, error) {
	policies := client.NetworkingV1().NetworkPolicies(namespace)
	name := "sandbox-" + identity.logicalID
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ordinary NetworkPolicy for delete: %w", err)
	}
	if allowLegacy {
		err = validateMutableOrdinaryNetworkPolicy(current, identity)
	} else {
		err = validateOrdinaryNetworkPolicy(current, identity, "")
	}
	if err != nil {
		return false, err
	}
	if err := deleteCreatedNetworkPolicy(ctx, policies, current); err != nil {
		return true, fmt.Errorf("delete ordinary NetworkPolicy: %w", err)
	}
	return true, nil
}

func deleteOwnedOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace string, identity ordinaryNetworkIdentity, allowLegacy bool) (bool, error) {
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(namespace)
	name := "sandbox-private-deny-" + identity.logicalID
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ordinary CiliumNetworkPolicy for delete: %w", err)
	}
	if allowLegacy {
		err = validateMutableOrdinaryCiliumPolicy(current, identity)
	} else {
		err = validateOrdinaryCiliumPolicy(current, identity, "")
	}
	if err != nil {
		return false, err
	}
	if err := deleteCreatedCiliumPolicy(ctx, policies, current); err != nil {
		return true, fmt.Errorf("delete ordinary CiliumNetworkPolicy: %w", err)
	}
	return true, nil
}

func cleanupBoundOrdinaryPolicies(ctx context.Context, client kubernetes.Interface, dynClient dynamic.Interface, namespace, runtimeID string, hasCilium bool) (bool, error) {
	found := false
	var errs []error
	policies, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{LabelSelector: ordinaryRuntimeIDLabel + "=" + runtimeID})
	if err != nil {
		errs = append(errs, fmt.Errorf("list runtime-bound NetworkPolicies: %w", err))
	} else {
		for index := range policies.Items {
			policy := &policies.Items[index]
			identity := ordinaryNetworkIdentity{runtimeID: runtimeID, runtimeUID: types.UID(policy.Annotations[ordinaryRuntimeUIDAnnotation]), logicalID: policy.Labels["sandbox.id"]}
			if identity.runtimeUID == "" || validateOrdinaryNetworkPolicy(policy, identity, "") != nil {
				continue
			}
			if err := confirmOrdinaryRuntimePodAbsent(ctx, client, namespace, runtimeID); err != nil {
				errs = append(errs, err)
				continue
			}
			matched, deleteErr := deleteOwnedOrdinaryNetworkPolicy(ctx, client, namespace, identity, false)
			found = found || matched
			errs = append(errs, deleteErr)
		}
	}
	if hasCilium {
		list, listErr := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: ordinaryRuntimeIDLabel + "=" + runtimeID})
		if listErr != nil {
			errs = append(errs, fmt.Errorf("list runtime-bound CiliumNetworkPolicies: %w", listErr))
		} else {
			for index := range list.Items {
				policy := &list.Items[index]
				identity := ordinaryNetworkIdentity{runtimeID: runtimeID, runtimeUID: types.UID(policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation]), logicalID: policy.GetLabels()["sandbox.id"]}
				if identity.runtimeUID == "" || validateOrdinaryCiliumPolicy(policy, identity, "") != nil {
					continue
				}
				if err := confirmOrdinaryRuntimePodAbsent(ctx, client, namespace, runtimeID); err != nil {
					errs = append(errs, err)
					continue
				}
				matched, deleteErr := deleteOwnedOrdinaryCiliumPrivateDeny(ctx, dynClient, namespace, identity, false)
				found = found || matched
				errs = append(errs, deleteErr)
			}
		}
	}
	return found, errors.Join(errs...)
}

func confirmOrdinaryRuntimePodAbsent(ctx context.Context, client kubernetes.Interface, namespace, runtimeID string) error {
	_, err := client.CoreV1().Pods(namespace).Get(ctx, runtimeID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("recheck ordinary runtime Pod before policy deletion: %w", err)
	}
	return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary runtime Pod %q reappeared before policy deletion", runtimeID))
}

func classifyOrdinaryNetworkPolicy(policy *networkingv1.NetworkPolicy) (ordinaryNetworkIdentity, bool) {
	if policy == nil || policy.Labels["sandbox.managed"] != "true" {
		return ordinaryNetworkIdentity{}, false
	}
	logicalID := policy.Labels["sandbox.id"]
	if logicalID == "" || policy.Name != "sandbox-"+logicalID || len(policy.Spec.PodSelector.MatchLabels) != 1 || policy.Spec.PodSelector.MatchLabels["sandbox.id"] != logicalID {
		return ordinaryNetworkIdentity{}, false
	}
	role := policy.Labels[ordinaryPolicyRoleLabel]
	if role == "" {
		if policy.Labels[ordinaryRuntimeIDLabel] != "" || policy.Annotations[ordinaryRuntimeUIDAnnotation] != "" {
			return ordinaryNetworkIdentity{}, false
		}
		return ordinaryNetworkIdentity{runtimeID: logicalID, logicalID: logicalID}, true
	}
	if role != ordinaryPolicyRole || policy.Labels[ordinaryRuntimeIDLabel] == "" || policy.Annotations[ordinaryRuntimeUIDAnnotation] == "" {
		return ordinaryNetworkIdentity{}, false
	}
	return ordinaryNetworkIdentity{
		runtimeID: policy.Labels[ordinaryRuntimeIDLabel], runtimeUID: types.UID(policy.Annotations[ordinaryRuntimeUIDAnnotation]), logicalID: logicalID,
	}, true
}

func classifyOrdinaryCiliumPolicy(policy *unstructured.Unstructured) (ordinaryNetworkIdentity, bool) {
	if policy == nil || policy.GetLabels()["sandbox.managed"] != "true" {
		return ordinaryNetworkIdentity{}, false
	}
	logicalID := policy.GetLabels()["sandbox.id"]
	selector, found, err := unstructured.NestedStringMap(policy.Object, "spec", "endpointSelector", "matchLabels")
	if logicalID == "" || policy.GetName() != "sandbox-private-deny-"+logicalID || err != nil || !found || len(selector) != 1 || selector["sandbox.id"] != logicalID {
		return ordinaryNetworkIdentity{}, false
	}
	role := policy.GetLabels()[ordinaryPolicyRoleLabel]
	if role == "" {
		if policy.GetLabels()[ordinaryRuntimeIDLabel] != "" || policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation] != "" {
			return ordinaryNetworkIdentity{}, false
		}
		return ordinaryNetworkIdentity{runtimeID: logicalID, logicalID: logicalID}, true
	}
	if role != ordinaryPrivateDenyPolicyRole || policy.GetLabels()[ordinaryRuntimeIDLabel] == "" || policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation] == "" {
		return ordinaryNetworkIdentity{}, false
	}
	return ordinaryNetworkIdentity{
		runtimeID: policy.GetLabels()[ordinaryRuntimeIDLabel], runtimeUID: types.UID(policy.GetAnnotations()[ordinaryRuntimeUIDAnnotation]), logicalID: logicalID,
	}, true
}

func hasManagedOrdinaryPodForLogicalID(ctx context.Context, client kubernetes.Interface, namespace, logicalID string) (bool, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true,sandbox.id=" + logicalID})
	if err != nil {
		return false, fmt.Errorf("list Pods for ordinary policy %q: %w", logicalID, err)
	}
	return len(pods.Items) != 0, nil
}

func reconcileOrphanedOrdinaryPolicies(ctx context.Context, client kubernetes.Interface, dynClient dynamic.Interface, namespace string, hasCilium bool) error {
	policies, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true"})
	if err != nil {
		return fmt.Errorf("list managed NetworkPolicies: %w", err)
	}
	for index := range policies.Items {
		policy := &policies.Items[index]
		identity, recognized := classifyOrdinaryNetworkPolicy(policy)
		if !recognized {
			continue
		}
		live, err := hasManagedOrdinaryPodForLogicalID(ctx, client, namespace, identity.logicalID)
		if err != nil {
			return err
		}
		if live {
			continue
		}
		allowLegacy := policy.Labels[ordinaryPolicyRoleLabel] == ""
		if _, err := deleteOwnedOrdinaryNetworkPolicy(ctx, client, namespace, identity, allowLegacy); err != nil {
			return fmt.Errorf("reconcile orphaned ordinary NetworkPolicy %q: %w", policy.Name, err)
		}
	}
	if !hasCilium {
		return nil
	}
	ciliumPolicies, err := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true"})
	if err != nil {
		return fmt.Errorf("list managed CiliumNetworkPolicies: %w", err)
	}
	for index := range ciliumPolicies.Items {
		policy := &ciliumPolicies.Items[index]
		identity, recognized := classifyOrdinaryCiliumPolicy(policy)
		if !recognized {
			continue
		}
		live, err := hasManagedOrdinaryPodForLogicalID(ctx, client, namespace, identity.logicalID)
		if err != nil {
			return err
		}
		if live {
			continue
		}
		allowLegacy := policy.GetLabels()[ordinaryPolicyRoleLabel] == ""
		if _, err := deleteOwnedOrdinaryCiliumPrivateDeny(ctx, dynClient, namespace, identity, allowLegacy); err != nil {
			return fmt.Errorf("reconcile orphaned ordinary CiliumNetworkPolicy %q: %w", policy.GetName(), err)
		}
	}
	return nil
}

func (r *Runtime) migrateOrdinaryPoolIdentity(ctx context.Context, pod *corev1.Pod, newLogicalID string, labels map[string]*string) error {
	oldIdentity, err := ordinaryIdentityFromPod(pod, pod.Name)
	if err != nil {
		return err
	}
	if pod.Labels["sandbox.pool"] != "true" {
		return fmt.Errorf("only an unclaimed ordinary pool Pod can change sandbox.id")
	}
	if errs := kvalidation.IsDNS1123Subdomain(newLogicalID); len(errs) != 0 {
		return fmt.Errorf("invalid new ordinary logical ID %q: %s", newLogicalID, errs[0])
	}
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true,sandbox.id=" + newLogicalID})
	if err != nil {
		return fmt.Errorf("check new ordinary logical identity: %w", err)
	}
	for index := range pods.Items {
		if pods.Items[index].Name != pod.Name {
			return fmt.Errorf("ordinary logical sandbox %q already exists", newLogicalID)
		}
	}
	oldPolicies := r.client.NetworkingV1().NetworkPolicies(r.namespace)
	oldPolicy, err := oldPolicies.Get(ctx, "sandbox-"+oldIdentity.logicalID, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get pool NetworkPolicy: %w", err)
	}
	if err := validateMutableOrdinaryNetworkPolicy(oldPolicy, oldIdentity); err != nil {
		return fmt.Errorf("validate pool NetworkPolicy: %w", err)
	}
	attempt, err := newNetworkAttemptToken()
	if err != nil {
		return fmt.Errorf("create pool migration attempt: %w", err)
	}
	newIdentity := ordinaryNetworkIdentity{runtimeID: pod.Name, runtimeUID: pod.UID, logicalID: newLogicalID}
	newPolicy, err := buildOrdinaryNetworkPolicy(r.namespace, newIdentity, attempt, false, nil, false)
	if err != nil {
		return err
	}
	newPolicy.Spec.Egress = append([]networkingv1.NetworkPolicyEgressRule(nil), oldPolicy.Spec.Egress...)
	newPolicy.Spec.PolicyTypes = append([]networkingv1.PolicyType(nil), oldPolicy.Spec.PolicyTypes...)
	created, err := createOrdinaryNetworkPolicy(ctx, r.client, newPolicy, newIdentity, attempt)
	if err != nil {
		return err
	}
	labelMap := make(map[string]any, len(labels))
	for key, value := range labels {
		if value == nil {
			labelMap[key] = nil
		} else {
			labelMap[key] = *value
		}
	}
	patchBytes, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": string(pod.UID), "resourceVersion": pod.ResourceVersion, "labels": labelMap,
	}})
	if err != nil {
		return fmt.Errorf("marshal pool identity patch: %w", err)
	}
	patched, patchErr := r.client.CoreV1().Pods(r.namespace).Patch(ctx, pod.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if patchErr != nil {
		verified, getErr := r.client.CoreV1().Pods(r.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if getErr == nil && ordinaryPoolPatchMatches(verified, pod.UID, newLogicalID, labels) {
			patched = verified
		} else if getErr == nil && verified.UID == pod.UID && verified.Labels["sandbox.id"] == oldIdentity.logicalID && verified.Labels["sandbox.pool"] == "true" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), r.terminationTimeout)
			defer cancel()
			cleanupErr := deleteAttemptOrdinaryNetworkPolicy(cleanupCtx, r.client, r.namespace, created.Name, newIdentity, attempt)
			return errors.Join(fmt.Errorf("patch pool Pod identity: %w", patchErr), cleanupErr)
		} else {
			return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("patch pool Pod identity: %w", patchErr), getErr)
		}
	}
	if !ordinaryPoolPatchMatches(patched, pod.UID, newLogicalID, labels) {
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("patched pool Pod identity does not match requested labels"))
	}
	if err := deleteCreatedNetworkPolicy(ctx, oldPolicies, oldPolicy); err != nil {
		return fmt.Errorf("delete old pool NetworkPolicy: %w", err)
	}
	return nil
}

func ordinaryPoolPatchMatches(pod *corev1.Pod, uid types.UID, newLogicalID string, labels map[string]*string) bool {
	if pod == nil || pod.UID != uid || pod.Labels["sandbox.managed"] != "true" || pod.Labels["sandbox.workspace.mode"] == "fuse" || pod.Labels["sandbox.id"] != newLogicalID {
		return false
	}
	for key, value := range labels {
		if value == nil {
			if _, present := pod.Labels[key]; present {
				return false
			}
		} else if pod.Labels[key] != *value {
			return false
		}
	}
	return true
}

func ensureOrdinaryIdentityAvailable(ctx context.Context, client kubernetes.Interface, namespace string, identity ordinaryNetworkIdentity) error {
	_, err := client.CoreV1().Pods(namespace).Get(ctx, identity.runtimeID, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("ordinary runtime Pod %q already exists", identity.runtimeID)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("check ordinary runtime Pod: %w", err)
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.managed=true,sandbox.id=" + identity.logicalID})
	if err != nil {
		return fmt.Errorf("check ordinary logical identity: %w", err)
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("ordinary logical sandbox %q already exists", identity.logicalID)
	}
	return nil
}

func createOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, policy *networkingv1.NetworkPolicy, identity ordinaryNetworkIdentity, attempt string) (*networkingv1.NetworkPolicy, error) {
	created, err := client.NetworkingV1().NetworkPolicies(policy.Namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err == nil {
		if ordinaryNetworkPolicyIntentMatches(created, policy) {
			return created, nil
		}
		cleanupErr := deleteCreatedNetworkPolicy(ctx, client.NetworkingV1().NetworkPolicies(policy.Namespace), created)
		return nil, errors.Join(fmt.Errorf("created ordinary NetworkPolicy does not match requested intent"), cleanupErr)
	}
	current, getErr := client.NetworkingV1().NetworkPolicies(policy.Namespace).Get(ctx, policy.Name, metav1.GetOptions{})
	if getErr == nil && validateOrdinaryNetworkPolicy(current, identity, attempt) == nil && ordinaryNetworkPolicyIntentMatches(current, policy) {
		return current, nil
	}
	if getErr == nil && validateOrdinaryNetworkPolicy(current, identity, attempt) == nil && current.UID != "" {
		cleanupErr := deleteCreatedNetworkPolicy(ctx, client.NetworkingV1().NetworkPolicies(policy.Namespace), current)
		return nil, errors.Join(fmt.Errorf("create ordinary NetworkPolicy: %w", err), fmt.Errorf("created ordinary NetworkPolicy does not match requested intent"), cleanupErr)
	}
	return nil, fmt.Errorf("create ordinary NetworkPolicy: %w", err)
}

func createOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, policy *unstructured.Unstructured, identity ordinaryNetworkIdentity, attempt string) (*unstructured.Unstructured, error) {
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(policy.GetNamespace())
	created, err := policies.Create(ctx, policy, metav1.CreateOptions{})
	if err == nil {
		if ordinaryCiliumPolicyIntentMatches(created, policy) {
			return created, nil
		}
		cleanupErr := deleteCreatedCiliumPolicy(ctx, policies, created)
		return nil, errors.Join(fmt.Errorf("created ordinary CiliumNetworkPolicy does not match requested intent"), cleanupErr)
	}
	current, getErr := policies.Get(ctx, policy.GetName(), metav1.GetOptions{})
	if getErr == nil && validateOrdinaryCiliumPolicy(current, identity, attempt) == nil && ordinaryCiliumPolicyIntentMatches(current, policy) {
		return current, nil
	}
	if getErr == nil && validateOrdinaryCiliumPolicy(current, identity, attempt) == nil && current.GetUID() != "" {
		cleanupErr := deleteCreatedCiliumPolicy(ctx, policies, current)
		return nil, errors.Join(fmt.Errorf("create ordinary CiliumNetworkPolicy: %w", err), fmt.Errorf("created ordinary CiliumNetworkPolicy does not match requested intent"), cleanupErr)
	}
	return nil, fmt.Errorf("create ordinary CiliumNetworkPolicy: %w", err)
}

func bindOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string, identity ordinaryNetworkIdentity, attempt string, desired *networkingv1.NetworkPolicy) error {
	policies := client.NetworkingV1().NetworkPolicies(namespace)
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get ordinary NetworkPolicy for binding: %w", err)
	}
	unboundIdentity := identity
	unboundIdentity.runtimeUID = ""
	if err := validateOrdinaryNetworkPolicy(current, unboundIdentity, attempt); err != nil {
		return err
	}
	if !ordinaryNetworkPolicyIntentMatches(current, desired) {
		return fmt.Errorf("ordinary NetworkPolicy changed before binding")
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[ordinaryRuntimeUIDAnnotation] = string(identity.runtimeUID)
	updated, err := policies.Update(ctx, current, metav1.UpdateOptions{})
	if err == nil && ordinaryNetworkPolicyIntentMatches(updated, current) {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("bound ordinary NetworkPolicy does not match requested intent")
	}
	verified, getErr := policies.Get(ctx, name, metav1.GetOptions{})
	if getErr == nil && validateOrdinaryNetworkPolicy(verified, identity, attempt) == nil && ordinaryNetworkPolicyIntentMatches(verified, current) {
		return nil
	}
	return errors.Join(fmt.Errorf("bind ordinary NetworkPolicy: %w", err), getErr)
}

func bindOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace, name string, identity ordinaryNetworkIdentity, attempt string, desired *unstructured.Unstructured) error {
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(namespace)
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get ordinary CiliumNetworkPolicy for binding: %w", err)
	}
	unboundIdentity := identity
	unboundIdentity.runtimeUID = ""
	if err := validateOrdinaryCiliumPolicy(current, unboundIdentity, attempt); err != nil {
		return err
	}
	if !ordinaryCiliumPolicyIntentMatches(current, desired) {
		return fmt.Errorf("ordinary CiliumNetworkPolicy changed before binding")
	}
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[ordinaryRuntimeUIDAnnotation] = string(identity.runtimeUID)
	current.SetAnnotations(annotations)
	updated, err := policies.Update(ctx, current, metav1.UpdateOptions{})
	if err == nil && ordinaryCiliumPolicyIntentMatches(updated, current) {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("bound ordinary CiliumNetworkPolicy does not match requested intent")
	}
	verified, getErr := policies.Get(ctx, name, metav1.GetOptions{})
	if getErr == nil && validateOrdinaryCiliumPolicy(verified, identity, attempt) == nil && ordinaryCiliumPolicyIntentMatches(verified, current) {
		return nil
	}
	return errors.Join(fmt.Errorf("bind ordinary CiliumNetworkPolicy: %w", err), getErr)
}

func deleteAttemptOrdinaryNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string, identity ordinaryNetworkIdentity, attempt string) error {
	policies := client.NetworkingV1().NetworkPolicies(namespace)
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ordinary NetworkPolicy for cleanup: %w", err)
	}
	cleanupIdentity := identity
	cleanupIdentity.runtimeUID = ""
	if validateOrdinaryNetworkPolicy(current, cleanupIdentity, attempt) != nil {
		return nil
	}
	boundUID := current.Annotations[ordinaryRuntimeUIDAnnotation]
	if boundUID != "" && identity.runtimeUID != "" && boundUID != string(identity.runtimeUID) {
		return nil
	}
	if err := deleteCreatedNetworkPolicy(ctx, policies, current); err != nil {
		return fmt.Errorf("delete ordinary NetworkPolicy cleanup: %w", err)
	}
	return nil
}

func deleteAttemptOrdinaryCiliumPrivateDeny(ctx context.Context, client dynamic.Interface, namespace, name string, identity ordinaryNetworkIdentity, attempt string) error {
	policies := client.Resource(ciliumNetworkPolicyGVR).Namespace(namespace)
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ordinary CiliumNetworkPolicy for cleanup: %w", err)
	}
	cleanupIdentity := identity
	cleanupIdentity.runtimeUID = ""
	if validateOrdinaryCiliumPolicy(current, cleanupIdentity, attempt) != nil {
		return nil
	}
	boundUID := current.GetAnnotations()[ordinaryRuntimeUIDAnnotation]
	if boundUID != "" && identity.runtimeUID != "" && boundUID != string(identity.runtimeUID) {
		return nil
	}
	if err := deleteCreatedCiliumPolicy(ctx, policies, current); err != nil {
		return fmt.Errorf("delete ordinary CiliumNetworkPolicy cleanup: %w", err)
	}
	return nil
}

func ordinaryLogicalID(spec runtime.SandboxSpec) (string, error) {
	if spec.WorkspaceFUSE != nil || spec.Labels["sandbox.workspace.mode"] == "fuse" {
		return "", fmt.Errorf("FUSE sandbox cannot use ordinary network identity")
	}
	if managed, present := spec.Labels["sandbox.managed"]; present && managed != "true" {
		return "", fmt.Errorf("sandbox.managed is controlled by the Kubernetes runtime")
	}
	logicalID := spec.ID
	if labelled := spec.Labels["sandbox.id"]; labelled != "" {
		logicalID = labelled
	}
	if errs := kvalidation.IsDNS1123Subdomain(logicalID); len(errs) != 0 {
		return "", fmt.Errorf("invalid ordinary sandbox logical ID %q: %s", logicalID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(spec.ID); len(errs) != 0 {
		return "", fmt.Errorf("invalid ordinary sandbox runtime ID %q: %s", spec.ID, errs[0])
	}
	return logicalID, nil
}

func ordinaryIdentityFromPod(pod *corev1.Pod, expectedRuntimeID string) (ordinaryNetworkIdentity, error) {
	if pod == nil || pod.Name != expectedRuntimeID {
		return ordinaryNetworkIdentity{}, fmt.Errorf("ordinary Pod runtime identity does not match %q", expectedRuntimeID)
	}
	if pod.Labels["sandbox.managed"] != "true" {
		return ordinaryNetworkIdentity{}, fmt.Errorf("Pod %q is not a managed sandbox", pod.Name)
	}
	if pod.Labels["sandbox.workspace.mode"] == "fuse" {
		return ordinaryNetworkIdentity{}, fmt.Errorf("Pod %q is a FUSE sandbox", pod.Name)
	}
	logicalID := pod.Labels["sandbox.id"]
	if errs := kvalidation.IsDNS1123Subdomain(pod.Name); len(errs) != 0 {
		return ordinaryNetworkIdentity{}, fmt.Errorf("invalid ordinary runtime ID %q: %s", pod.Name, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(logicalID); len(errs) != 0 {
		return ordinaryNetworkIdentity{}, fmt.Errorf("invalid ordinary logical ID %q: %s", logicalID, errs[0])
	}
	return ordinaryNetworkIdentity{runtimeID: pod.Name, runtimeUID: pod.UID, logicalID: logicalID}, nil
}

func ordinaryPolicyMetadata(namespace string, identity ordinaryNetworkIdentity, role, attempt string) metav1.ObjectMeta {
	labels := map[string]string{
		"sandbox.managed":       "true",
		"sandbox.id":            identity.logicalID,
		ordinaryPolicyRoleLabel: role,
		ordinaryRuntimeIDLabel:  identity.runtimeID,
	}
	annotations := map[string]string{}
	if attempt != "" {
		annotations[ordinaryPolicyAttemptAnnotation] = attempt
	}
	if identity.runtimeUID != "" {
		annotations[ordinaryRuntimeUIDAnnotation] = string(identity.runtimeUID)
	}
	return metav1.ObjectMeta{Namespace: namespace, Labels: labels, Annotations: annotations}
}

func buildOrdinaryNetworkPolicy(namespace string, identity ordinaryNetworkIdentity, attempt string, enabled bool, whitelist []string, blockPrivate bool) (*networkingv1.NetworkPolicy, error) {
	if errs := kvalidation.IsDNS1123Subdomain(identity.runtimeID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary runtime ID %q: %s", identity.runtimeID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(identity.logicalID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary logical ID %q: %s", identity.logicalID, errs[0])
	}
	resolvedCIDRs, err := resolveToCIDRs(whitelist)
	if err != nil {
		return nil, err
	}
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	egress := []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{
		{Protocol: &udp, Port: &intstr.IntOrString{IntVal: 53}},
		{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 53}},
	}}}
	switch {
	case enabled && !blockPrivate && len(resolvedCIDRs) == 0:
		egress = append(egress,
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16"}}}}},
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fe80::/10", "fc00::/7", "::1/128"}}}}},
		)
	case blockPrivate:
		for _, cidr := range resolvedCIDRs {
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
		}
		egress = append(egress,
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "169.254.0.0/16"}}}}},
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fc00::/7", "::1/128", "fe80::/10"}}}}},
		)
	case len(resolvedCIDRs) != 0:
		for _, cidr := range resolvedCIDRs {
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
		}
	}
	metadata := ordinaryPolicyMetadata(namespace, identity, ordinaryPolicyRole, attempt)
	metadata.Name = "sandbox-" + identity.logicalID
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metadata,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.id": identity.logicalID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}, nil
}

func buildOrdinaryCiliumPrivateDeny(namespace string, identity ordinaryNetworkIdentity, attempt string) (*unstructured.Unstructured, error) {
	if errs := kvalidation.IsDNS1123Subdomain(identity.runtimeID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary runtime ID %q: %s", identity.runtimeID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(identity.logicalID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary logical ID %q: %s", identity.logicalID, errs[0])
	}
	metadata := ordinaryPolicyMetadata(namespace, identity, ordinaryPrivateDenyPolicyRole, attempt)
	metadata.Name = "sandbox-private-deny-" + identity.logicalID
	toCIDRSet := []any{
		map[string]any{"cidr": "10.0.0.0/8"}, map[string]any{"cidr": "172.16.0.0/12"},
		map[string]any{"cidr": "192.168.0.0/16"}, map[string]any{"cidr": "127.0.0.0/8"},
		map[string]any{"cidr": "169.254.0.0/16"}, map[string]any{"cidr": "fc00::/7"},
		map[string]any{"cidr": "::1/128"}, map[string]any{"cidr": "fe80::/10"},
	}
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name": metadata.Name, "namespace": namespace,
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{"sandbox.id": identity.logicalID}},
			"egressDeny":       []any{map[string]any{"toCIDRSet": toCIDRSet}},
		},
	}}
	policy.SetLabels(metadata.Labels)
	policy.SetAnnotations(metadata.Annotations)
	return policy, nil
}
