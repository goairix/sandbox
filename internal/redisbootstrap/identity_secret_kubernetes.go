package redisbootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const identityTimeout = 2 * time.Minute
const identityRetry = 250 * time.Millisecond

var (
	// ErrIdentityInvalid never embeds private data or Kubernetes response bodies.
	ErrIdentityInvalid = errors.New("Redis bootstrap identity or installation is invalid")
	// ErrIdentityMissing requires restoration, not replacement key generation.
	ErrIdentityMissing = errors.New("retained Redis identity is missing; restore the original identity Secret")
	ErrIdentityAPI     = errors.New("Redis identity Kubernetes API request failed")
)

// IdentitySecretOptions scopes an install-only identity operation.
type IdentitySecretOptions struct {
	Namespace, StatefulSetName, SecretName, StateConfigMap string
	Members                                                [3]string
	FreshClusterID                                         string
	Timeout                                                time.Duration
}

// EnsureIdentitySecret creates only explicitly authorized fresh identity.
func EnsureIdentitySecret(ctx context.Context, client corev1.CoreV1Interface, o IdentitySecretOptions) error {
	if ctx == nil || client == nil {
		return ErrIdentityInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Timeout == 0 {
		o.Timeout = identityTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for {
		state, err := readIdentityState(ctx, client, o)
		if err != nil {
			if errors.Is(err, ErrIdentityInvalid) || errors.Is(err, ErrIdentityMissing) {
				return err
			}
			if err := waitIdentityRetry(ctx); err != nil {
				return err
			}
			continue
		}
		secret, err := readServerIdentity(ctx, client, o)
		if err == nil {
			return verifyExistingIdentity(ctx, client, o, state, secret)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !apierrors.IsNotFound(err) {
			if err := waitIdentityRetry(ctx); err != nil {
				return err
			}
			continue
		}
		if o.FreshClusterID == "" || state.Cluster.ClusterID != o.FreshClusterID || state.Cluster.Phase != Pending || state.Registration != nil {
			return ErrIdentityMissing
		}
		return createFreshIdentity(ctx, client, o, state)
	}
}

// readIdentityState waits for ordinary resources, but a retained PVC without
// its namespace state cannot authorize recreating an installation.
func readIdentityState(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions) (NamespaceBootstrap, error) {
	cm, err := identityGetCM(ctx, c, o)
	if ctx.Err() != nil {
		return NamespaceBootstrap{}, ctx.Err()
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			for i := range 3 {
				pvc, e := identityGetPVC(ctx, c, o, fmt.Sprintf("data-%s-%d", o.StatefulSetName, i))
				if ctx.Err() != nil {
					return NamespaceBootstrap{}, ctx.Err()
				}
				if e == nil && pvc != nil {
					// Helm generated a fresh permit before any old PVC existed.
					// A new, matching PVC may race ordinary CM creation; it does
					// not grant creation without the later validated CM.
					if o.FreshClusterID == "" || pvc.Name != fmt.Sprintf("data-%s-%d", o.StatefulSetName, i) || pvc.Namespace != o.Namespace || pvc.UID == "" || pvc.ResourceVersion == "" || pvc.DeletionTimestamp != nil || pvc.Annotations[IdentityClusterAnnotation] != o.FreshClusterID {
						return NamespaceBootstrap{}, ErrIdentityMissing
					}
				}
				if e != nil && !apierrors.IsNotFound(e) {
					return NamespaceBootstrap{}, ErrIdentityAPI
				}
			}
		}
		return NamespaceBootstrap{}, ErrIdentityAPI
	}
	state, err := parseNamespaceBootstrap(cm, o.Namespace, o.StateConfigMap)
	if err != nil || cm.DeletionTimestamp != nil || state.Cluster.Members != o.Members {
		return NamespaceBootstrap{}, ErrIdentityInvalid
	}
	return state, nil
}

// identityReadError keeps reason typing without exposing a server body.
type identityReadError struct{ cause error }

func (identityReadError) Error() string   { return ErrIdentityAPI.Error() }
func (e identityReadError) Unwrap() error { return e.cause }

func readServerIdentity(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions) (*api.Secret, error) {
	s, err := identityGetSecret(ctx, c, o)
	if ctx.Err() != nil {
		wipeIdentitySeeds(s)
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, identityReadError{cause: err}
	}
	return s, nil
}

func sameIdentityInstallation(a, b NamespaceBootstrap) bool {
	return a.UID == b.UID && a.Cluster.ClusterID == b.Cluster.ClusterID && a.Cluster.Members == b.Cluster.Members
}

func verifyExistingIdentity(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, state NamespaceBootstrap, s *api.Secret) error {
	defer wipeIdentitySeeds(s)
	if err := validateServerIdentity(s, o, state); err != nil {
		return err
	}
	next, err := readIdentityState(ctx, c, o)
	if err != nil {
		return err
	}
	if !sameIdentityInstallation(state, next) || (state.Registration != nil && (next.Registration == nil || state.Registration.KeyDigest != next.Registration.KeyDigest)) || (state.Cluster.Phase == Initialized && next.Cluster.Phase != Initialized) {
		return ErrIdentityInvalid
	}
	return validateServerIdentity(s, o, next)
}

type identityPVC struct {
	Present         bool
	UID             types.UID
	ResourceVersion string
}

func readFreshPVCs(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions) ([3]identityPVC, error) {
	var result [3]identityPVC
	for i := range 3 {
		name := fmt.Sprintf("data-%s-%d", o.StatefulSetName, i)
		pvc, err := identityGetPVC(ctx, c, o, name)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return result, ErrIdentityAPI
		}
		if pvc == nil || pvc.Name != name || pvc.Namespace != o.Namespace || pvc.UID == "" || pvc.ResourceVersion == "" || pvc.DeletionTimestamp != nil || pvc.Annotations[IdentityClusterAnnotation] != o.FreshClusterID {
			return result, ErrIdentityInvalid
		}
		result[i] = identityPVC{Present: true, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion}
	}
	return result, nil
}

func createFreshIdentity(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, state NamespaceBootstrap) error {
	first, err := readFreshPVCs(ctx, c, o)
	if err != nil {
		return err
	}
	secondState, err := readIdentityState(ctx, c, o)
	if err != nil {
		return err
	}
	if !sameIdentityInstallation(state, secondState) || state.ResourceVersion != secondState.ResourceVersion || secondState.Cluster.Phase != Pending || secondState.Registration != nil {
		return ErrIdentityInvalid
	}
	second, err := readFreshPVCs(ctx, c, o)
	if err != nil {
		return err
	}
	for i := range 3 {
		// New PVC appearance is normal under a Parallel StatefulSet. Previously
		// observed objects must not disappear, change identity or change version.
		if first[i].Present && first[i] != second[i] {
			return ErrIdentityInvalid
		}
	}
	s, err := readServerIdentity(ctx, c, o)
	if err == nil {
		return verifyExistingIdentity(ctx, c, o, state, s)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !apierrors.IsNotFound(err) {
		return ErrIdentityAPI
	}
	generated, err := GenerateIdentitySecret(o.Namespace, o.StatefulSetName)
	if err != nil {
		return ErrIdentityInvalid
	}
	defer wipeIdentitySeeds(generated)
	generated.Annotations = map[string]string{"helm.sh/resource-policy": "keep", IdentityClusterAnnotation: o.FreshClusterID, "sandbox/redis-identity-version": "1"}
	if err := ctx.Err(); err != nil {
		return err
	}
	created, createErr := identityCreateSecret(ctx, c, o, generated)
	wipeIdentitySeeds(created)
	// Never repeat Create after an uncertain result. Resolve only the server's
	// original object, including AlreadyExists from another installer.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for {
		stored, err := readServerIdentity(ctx, c, o)
		if err == nil {
			postPVCs, err := readFreshPVCs(ctx, c, o)
			if err != nil {
				wipeIdentitySeeds(stored)
				return err
			}
			for i := range 3 {
				// Binding can advance RV normally after Create; installation UID
				// must stay pinned and observed PVCs must not disappear.
				if second[i].Present && (!postPVCs[i].Present || second[i].UID != postPVCs[i].UID) {
					wipeIdentitySeeds(stored)
					return ErrIdentityInvalid
				}
			}
			return verifyExistingIdentity(ctx, c, o, state, stored)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if createErr == nil && !apierrors.IsNotFound(err) {
			return ErrIdentityAPI
		}
		if err := waitIdentityRetry(ctx); err != nil {
			return err
		}
	}
}

func waitIdentityRetry(ctx context.Context) error {
	timer := time.NewTimer(identityRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
