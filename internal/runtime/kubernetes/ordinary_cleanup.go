package kubernetes

import (
	"context"
	"errors"

	"github.com/goairix/sandbox/internal/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (r *Runtime) CleanupOrdinarySandboxPolicies(ctx context.Context, ref runtime.RuntimeRef, logicalID string) error {
	if ref.Validate() != nil || logicalID == "" || len(validation.IsDNS1123Subdomain(logicalID)) != 0 {
		return runtime.ErrInvalidRuntimeRef
	}
	_, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, ref.ID, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return errors.Join(runtime.ErrTerminationUnconfirmed, err)
	}
	// A migration can leave both old pool and logical policy names. Each
	// deletion validates the recorded UID and policy object UID; no legacy
	// unbound policy is eligible for this recovery capability.
	for _, id := range []string{logicalID, ref.ID} {
		identity := ordinaryNetworkIdentity{runtimeID: ref.ID, runtimeUID: types.UID(ref.UID), logicalID: id}
		if _, err := deleteOwnedOrdinaryNetworkPolicy(ctx, r.client, r.namespace, identity, false); err != nil {
			return err
		}
		if r.hasCilium {
			if _, err := deleteOwnedOrdinaryCiliumPrivateDeny(ctx, r.dynClient, r.namespace, identity, false); err != nil {
				return err
			}
		}
	}
	return nil
}
