package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A successful write response for each policy is not a coherent pair: another
// writer can replace the allow policy while the deny write is in progress.
func (r *Runtime) verifyOrdinaryNetworkIntent(ctx context.Context, identity ordinaryNetworkIdentity, attempt string, allow *networkingv1.NetworkPolicy, deny *unstructured.Unstructured) error {
	verifyCtx, cancel := networkCleanupContext(ctx)
	defer cancel()
	current, err := r.client.NetworkingV1().NetworkPolicies(r.namespace).Get(verifyCtx, allow.Name, metav1.GetOptions{})
	if err != nil || allow.UID == "" || !ordinaryNetworkPolicyIntentMatches(current, allow) || validateOrdinaryNetworkPolicy(current, identity, attempt) != nil {
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary allow policy changed during joint network verification"), err)
	}
	if r.hasCilium {
		currentDeny, err := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(r.namespace).Get(verifyCtx, "sandbox-private-deny-"+identity.logicalID, metav1.GetOptions{})
		if deny == nil {
			if !apierrors.IsNotFound(err) {
				return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary deny policy absence is unconfirmed during joint verification"), err)
			}
		} else if err != nil || deny.GetUID() == "" || !ordinaryCiliumPolicyIntentMatches(currentDeny, deny) || validateOrdinaryCiliumPolicy(currentDeny, identity, attempt) != nil {
			return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary deny policy changed during joint network verification"), err)
		}
	}
	pod, err := r.client.CoreV1().Pods(r.namespace).Get(verifyCtx, identity.runtimeID, metav1.GetOptions{})
	if err != nil {
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary runtime Pod cannot be verified after network update"), err)
	}
	actual, err := ordinaryIdentityFromPod(pod, identity.runtimeID)
	if err != nil || actual != identity {
		return errors.Join(runtime.ErrNetworkStateUncertain, fmt.Errorf("ordinary runtime Pod identity changed during network update"), err)
	}
	return nil
}
