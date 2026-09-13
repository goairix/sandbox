package kubernetes

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestGetSandboxNormalizesOnlyPodNotFound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		missing bool
	}{
		{name: "missing", err: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pod-a"), missing: true},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod-a", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("get", "pods", func(ktesting.Action) (bool, k8sruntime.Object, error) { return true, nil, tc.err })
			r := &Runtime{client: client, namespace: "runtime"}
			_, err := r.GetSandbox(context.Background(), "pod-a")
			if tc.missing {
				require.ErrorIs(t, err, runtime.ErrNotFound)
			} else {
				require.ErrorIs(t, err, tc.err)
				require.NotErrorIs(t, err, runtime.ErrNotFound)
			}
		})
	}
}
