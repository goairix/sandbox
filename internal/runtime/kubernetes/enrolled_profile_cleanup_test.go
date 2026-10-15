package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestEnrolledCleanupAcceptsOnlyOriginalTuple(t *testing.T) {
	r := enrolledPodRequest(t)
	binding := cleanupBinding()
	c, err := launcherconfig.Decode(r.Config)
	require.NoError(t, err)
	c.Binding = launcherconfig.Binding(binding.Authority)
	c.Enrollment = launcherconfig.Enrollment{NodeUID: binding.NodeUID, NodeBootID: binding.NodeBootID}
	r.Config, err = json.Marshal(c)
	require.NoError(t, err)
	r.NodeUID, r.NodeBootID, r.CreationIntentID = binding.NodeUID, binding.NodeBootID, binding.CreationIntentID
	binding.ContractDigest = c.ContractDigest
	pod, err := BuildEnrolledPlainPod("runtimes", r)
	require.NoError(t, err)
	pod.UID, pod.ResourceVersion = types.UID(binding.Runtime.UID), "1"
	server, _ := cleanupServer(t, pod)
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(server.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	handle, err := client.Prepare(context.Background(), binding, nil)
	require.NoError(t, err)
	require.NotEmpty(t, handle.Descriptor())
	for name, mutate := range map[string]func(*TaskRuntimeCleanupBinding){
		"authority":  func(b *TaskRuntimeCleanupBinding) { b.Authority.AuthorityID = "other" },
		"generation": func(b *TaskRuntimeCleanupBinding) { b.Generation++ },
		"node":       func(b *TaskRuntimeCleanupBinding) { b.NodeUID = "88888888-8888-4888-8888-888888888888" },
		"boot":       func(b *TaskRuntimeCleanupBinding) { b.NodeBootID = "88888888-8888-4888-8888-888888888888" },
		"contract":   func(b *TaskRuntimeCleanupBinding) { b.ContractDigest = cleanupBinding().ContractDigest },
	} {
		t.Run(name, func(t *testing.T) {
			copy := binding
			mutate(&copy)
			_, err := client.Prepare(context.Background(), copy, nil)
			require.Error(t, err)
		})
	}
	for _, marker := range []string{"unknown", ""} {
		pod.Annotations[kubecontract.EnrolledProfileAnnotation] = marker
		if marker == "" {
			delete(pod.Annotations, kubecontract.EnrolledProfileAnnotation)
		}
		_, err := client.Prepare(context.Background(), binding, nil)
		require.Error(t, err)
	}
}

func TestEnrolledCleanupRejectsSelfConsistentImageReplacement(t *testing.T) {
	r := enrolledPodRequest(t)
	binding := cleanupBinding()
	c, err := launcherconfig.Decode(r.Config)
	require.NoError(t, err)
	c.Binding = launcherconfig.Binding(binding.Authority)
	c.Enrollment = launcherconfig.Enrollment{NodeUID: binding.NodeUID, NodeBootID: binding.NodeBootID}
	r.Config, err = json.Marshal(c)
	require.NoError(t, err)
	r.NodeUID, r.NodeBootID, r.CreationIntentID = binding.NodeUID, binding.NodeBootID, binding.CreationIntentID
	binding.ContractDigest = c.ContractDigest
	originalBinding := binding
	pod, err := BuildEnrolledPlainPod("runtimes", r)
	require.NoError(t, err)
	pod.UID, pod.ResourceVersion = types.UID(binding.Runtime.UID), "1"
	server, _ := cleanupServer(t, pod)
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(server.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	handle, err := client.Prepare(context.Background(), binding, nil)
	require.NoError(t, err)
	require.NotEmpty(t, handle.Descriptor())

	replacement := pod.DeepCopy()
	replacement.Spec.Containers[0].Image = "example.test/sandbox@sha256:" + strings.Repeat("c", 64)
	replacement.Annotations[kubecontract.EnrolledImageDigestAnnotation] = strings.Repeat("c", 64)
	replacementConfig, err := launcherconfig.Decode([]byte(replacement.Spec.Containers[0].Env[0].Value))
	require.NoError(t, err)
	replacementConfig.ContractDigest, err = kubecontract.EnrolledPlainContractDigest(replacement.Spec.Containers[0].Image)
	require.NoError(t, err)
	replacementWire, err := json.Marshal(replacementConfig)
	require.NoError(t, err)
	replacement.Spec.Containers[0].Env[0].Value = string(replacementWire)
	digest := sha256.Sum256(replacementWire)
	replacement.Annotations[kubecontract.EnrolledConfigDigestAnnotation] = hex.EncodeToString(digest[:])
	checked, err := kubecontract.InspectEnrolledPlainPod(replacement)
	require.NoError(t, err, "replacement must pass structural validation")
	require.NotEqual(t, binding.ContractDigest, checked.Config.ContractDigest)
	replacementServer, requests := cleanupServer(t, replacement)
	replacementClient, err := NewTaskRuntimeCleanupClient(cleanupOptions(replacementServer.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, replacementClient.Close()) })
	_, err = replacementClient.Prepare(context.Background(), binding, nil)
	require.ErrorIs(t, err, errTaskCleanupRefused)
	require.Positive(t, requests.Load(), "consumer must fetch the candidate Pod")
	require.Equal(t, originalBinding, binding, "the original cleanup authority must stay unchanged")
}
