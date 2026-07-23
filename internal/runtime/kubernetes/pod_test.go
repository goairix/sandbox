package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestCreatePodUsesConfiguredTmpDiskLimit(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:      "sandbox-test",
		Image:   "sandbox:latest",
		TmpDisk: "200Mi",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(200*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}

func TestCreatePodDefaultsTmpDiskLimitTo50Mi(t *testing.T) {
	client := fake.NewSimpleClientset()

	pod, err := createPod(context.Background(), client, "default", runtime.SandboxSpec{
		ID:    "sandbox-test",
		Image: "sandbox:latest",
	})

	require.NoError(t, err)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			require.NotNil(t, volume.EmptyDir)
			require.NotNil(t, volume.EmptyDir.SizeLimit)
			assert.Equal(t, int64(50*1024*1024), volume.EmptyDir.SizeLimit.Value())
			return
		}
	}
	t.Fatal("tmp volume not found")
}
