package redisbootstrap

import (
	"context"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// typed fakes have a typed-nil REST client; their object-store operations retain
// the same fixed-resource API contract without an actual HTTP transport.
func identityREST(c corev1.CoreV1Interface) rest.Interface {
	client := c.RESTClient()
	if concrete, ok := client.(*rest.RESTClient); ok && concrete == nil {
		return nil
	}
	return client
}

func identityGET(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, resource, name string, result runtime.Object) error {
	return identityREST(c).Get().Namespace(o.Namespace).Resource(resource).Name(name).
		VersionedParams(&metav1.GetOptions{}, scheme.ParameterCodec).
		WarningHandlerWithContext(rest.NoWarnings{}).MaxRetries(0).Do(ctx).Into(result)
}

// Every read shares the caller's bounded budget. Malformed returned objects are
// validated by the caller and are not retryable. No private API error is logged.
func retryIdentityGET[T any](ctx context.Context, get func() (T, error)) (T, error) {
	for {
		if err := ctx.Err(); err != nil {
			var zero T
			return zero, err
		}
		result, err := get()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err == nil || apierrors.IsNotFound(err) {
			return result, err
		}
		if err := waitIdentityRetry(ctx); err != nil {
			var zero T
			return zero, err
		}
	}
}

func identityGetCM(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions) (*api.ConfigMap, error) {
	return retryIdentityGET(ctx, func() (*api.ConfigMap, error) {
		if identityREST(c) == nil {
			return c.ConfigMaps(o.Namespace).Get(ctx, o.StateConfigMap, metav1.GetOptions{})
		}
		obj := &api.ConfigMap{}
		err := identityGET(ctx, c, o, "configmaps", o.StateConfigMap, obj)
		return obj, err
	})
}

func identityGetPVC(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, name string) (*api.PersistentVolumeClaim, error) {
	return retryIdentityGET(ctx, func() (*api.PersistentVolumeClaim, error) {
		if identityREST(c) == nil {
			return c.PersistentVolumeClaims(o.Namespace).Get(ctx, name, metav1.GetOptions{})
		}
		obj := &api.PersistentVolumeClaim{}
		err := identityGET(ctx, c, o, "persistentvolumeclaims", name, obj)
		return obj, err
	})
}

func identityGetSecret(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions) (*api.Secret, error) {
	return retryIdentityGET(ctx, func() (*api.Secret, error) {
		if identityREST(c) == nil {
			return c.Secrets(o.Namespace).Get(ctx, o.SecretName, metav1.GetOptions{})
		}
		obj := &api.Secret{}
		err := identityGET(ctx, c, o, "secrets", o.SecretName, obj)
		return obj, err
	})
}

func identityCreateSecret(ctx context.Context, c corev1.CoreV1Interface, o IdentitySecretOptions, obj *api.Secret) (*api.Secret, error) {
	if identityREST(c) == nil {
		return c.Secrets(o.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	}
	result := &api.Secret{}
	// A committed 500/429 + Retry-After must not replay this non-idempotent POST.
	err := identityREST(c).Post().Namespace(o.Namespace).Resource("secrets").
		VersionedParams(&metav1.CreateOptions{}, scheme.ParameterCodec).Body(obj).
		WarningHandlerWithContext(rest.NoWarnings{}).MaxRetries(0).Do(ctx).Into(result)
	return result, err
}
