package redisbootstrap

import (
	"context"

	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// stableFreshPVCs retries only ordinary binding version changes. Installation
// identity and every observed PVC UID stay pinned for the entire shared budget.
// No keys are generated and no writes are attempted while stabilization runs.
func stableFreshPVCs(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, state NamespaceBootstrap) ([3]identityPVC, error) {
	var pinned [3]identityPVC
	for {
		first, err := readFreshPVCs(ctx, c, o)
		if err != nil {
			return pinned, err
		}
		if err := pinFreshPVCs(&pinned, first); err != nil {
			return pinned, err
		}
		next, err := readIdentityState(ctx, c, o)
		if err != nil {
			return pinned, err
		}
		if !sameIdentityInstallation(state, next) || state.ResourceVersion != next.ResourceVersion || next.Cluster.Phase != Pending || next.Registration != nil {
			return pinned, ErrIdentityInvalid
		}
		second, err := readFreshPVCs(ctx, c, o)
		if err != nil {
			return pinned, err
		}
		if err := pinFreshPVCs(&pinned, second); err != nil {
			return pinned, err
		}
		drift := false
		for i := range first {
			if first[i].Present && first[i].ResourceVersion != second[i].ResourceVersion {
				drift = true
			}
		}
		if !drift {
			return second, nil
		}
		if err := waitIdentityRetry(ctx); err != nil {
			return pinned, err
		}
	}
}

func pinFreshPVCs(pinned *[3]identityPVC, observed [3]identityPVC) error {
	for i := range observed {
		if pinned[i].Present && (!observed[i].Present || pinned[i].UID != observed[i].UID) {
			return ErrIdentityInvalid
		}
		if observed[i].Present {
			pinned[i] = observed[i]
		}
	}
	return nil
}
