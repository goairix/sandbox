package kubernetes

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Kubernetes.
type Runtime struct {
	client     kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

// New creates a new Kubernetes runtime.
func New(kubeconfig string, namespace string) (*Runtime, error) {
	var restConfig *rest.Config
	var err error

	if kubeconfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("build k8s config: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}

	return &Runtime{
		client:     client,
		restConfig: restConfig,
		namespace:  namespace,
	}, nil
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	pod, err := createPod(ctx, r.client, r.namespace, spec)
	if err != nil {
		return nil, err
	}

	// Wait for pod to be ready
	if err := waitForPodReady(ctx, r.client, r.namespace, pod.Name, 60*time.Second); err != nil {
		_ = deletePod(ctx, r.client, r.namespace, pod.Name)
		return nil, fmt.Errorf("wait for pod: %w", err)
	}

	// Apply network policy if whitelist is configured
	if spec.NetworkEnabled && len(spec.NetworkWhitelist) > 0 {
		if err := ensureNetworkPolicy(ctx, r.client, r.namespace, spec.ID, spec.NetworkWhitelist); err != nil {
			_ = err // non-fatal
		}
	}

	return &runtime.SandboxInfo{
		ID:        spec.ID,
		RuntimeID: pod.Name,
		State:     "running",
		CreatedAt: pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) StartSandbox(_ context.Context, _ string) error {
	// Pods don't have a start/stop semantic like containers
	return nil
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	// Clean up network policy
	_ = deleteNetworkPolicy(ctx, r.client, r.namespace, id)
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	pod, err := getPod(ctx, r.client, r.namespace, id)
	if err != nil {
		return nil, err
	}

	return &runtime.SandboxInfo{
		ID:        id,
		RuntimeID: pod.Name,
		State:     podStateString(pod.Status.Phase),
		CreatedAt: pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	return execInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return execStreamInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	return uploadFileToPod(ctx, r.client, r.restConfig, r.namespace, id, destPath, reader)
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	return downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, srcPath)
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	return listFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath)
}
