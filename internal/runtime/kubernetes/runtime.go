package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Kubernetes.
type Runtime struct {
	client     kubernetes.Interface
	dynClient  dynamic.Interface
	restConfig *rest.Config
	namespace  string
	hasCilium  bool // whether CiliumNetworkPolicy CRD is available on this cluster
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

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic k8s client: %w", err)
	}

	hasCilium := detectCilium(client)
	if hasCilium {
		logger.Info(context.Background(), "Cilium CNI detected: CiliumNetworkPolicy will be used for private range enforcement")
	} else {
		logger.Info(context.Background(), "Cilium CNI not detected: relying on standard NetworkPolicy only")
	}
	return &Runtime{
		client:     client,
		dynClient:  dynClient,
		restConfig: restConfig,
		namespace:  namespace,
		hasCilium:  hasCilium,
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

	// Always apply a NetworkPolicy. Without one, K8s allows all egress by default.
	// updateNetworkPolicy handles all modes: isolation, whitelist, block-private, open.
	if err := updateNetworkPolicy(ctx, r.client, r.namespace, spec.ID, spec.NetworkEnabled, spec.NetworkWhitelist, spec.NetworkBlockPrivate); err != nil {
		_ = deletePod(ctx, r.client, r.namespace, pod.Name)
		return nil, fmt.Errorf("apply network policy: %w", err)
	}

	// When network is enabled without an explicit whitelist, apply a CiliumNetworkPolicy
	// egressDeny to block private ranges. Standard K8s NetworkPolicy IPBlock/Except is
	// unreliable in Cilium (CIDR identity may not be assigned before the "world" catch-all
	// matches), so an eBPF-level deny is the only reliable fix.
	// Whitelist mode is excluded: the whitelist may intentionally allow private CIDRs.
	// On non-Cilium clusters this is skipped; the standard NetworkPolicy suffices.
	if r.hasCilium && spec.NetworkEnabled && len(spec.NetworkWhitelist) == 0 {
		if err := applyCiliumPrivateDeny(ctx, r.dynClient, r.namespace, spec.ID); err != nil {
			_ = deleteNetworkPolicy(ctx, r.client, r.namespace, spec.ID)
			_ = deletePod(ctx, r.client, r.namespace, pod.Name)
			return nil, fmt.Errorf("apply cilium private deny: %w", err)
		}
	}

	return &runtime.SandboxInfo{
		ID:         spec.ID,
		RuntimeID:  pod.Name,
		RuntimeUID: string(pod.UID),
		State:      "running",
		CreatedAt:  pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) PrepareSandbox(context.Context, runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	return nil, runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) AuthorizeWorkspaceMount(context.Context, string, runtime.WorkspaceMountAuthorization) error {
	return runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) WaitSandboxReady(context.Context, string) (*runtime.SandboxInfo, error) {
	return nil, runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) PreparedSandboxHealth(context.Context, string, string) error {
	return runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) WorkspaceHealth(context.Context, string) (*runtime.WorkspaceHealth, error) {
	return nil, runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) QuiesceWorkspace(context.Context, string) (runtime.WorkspaceQuiesceToken, error) {
	return runtime.WorkspaceQuiesceToken{}, runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) ResumeWorkspace(context.Context, string, runtime.WorkspaceQuiesceToken) error {
	return runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) FlushWorkspace(context.Context, string) error {
	return runtime.ErrWorkspaceFUSEUnsupported
}

func (r *Runtime) StartSandbox(_ context.Context, _ string) error {
	// Kubernetes pods do not support pause/resume semantics like Docker containers.
	// Once a pod is created, it runs until deleted. StartSandbox is a no-op for K8s.
	// Callers should use CreateSandbox to launch a new pod instead.
	return nil
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	// Kubernetes pods cannot be stopped and restarted. The only way to "stop" a pod
	// is to delete it. Note this is a destructive operation: the pod and its ephemeral
	// storage are permanently removed. Callers should be aware that StopSandbox on K8s
	// is equivalent to RemoveSandbox without network policy cleanup.
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	// Clean up network policy
	if r.hasCilium {
		_ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
	}
	_ = deleteNetworkPolicy(ctx, r.client, r.namespace, id)
	return deletePod(ctx, r.client, r.namespace, id)
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	pod, err := getPod(ctx, r.client, r.namespace, id)
	if err != nil {
		return nil, err
	}

	return &runtime.SandboxInfo{
		ID:         id,
		RuntimeID:  pod.Name,
		RuntimeUID: string(pod.UID),
		State:      podStateString(pod.Status.Phase),
		CreatedAt:  pod.CreationTimestamp.Time,
	}, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	return execInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return execStreamInPod(ctx, r.client, r.restConfig, r.namespace, id, req)
}

func (r *Runtime) ExecPipe(ctx context.Context, id string, cmd []string, stdin io.Reader) error {
	return execPipeInPod(ctx, r.client, r.restConfig, r.namespace, id, cmd, stdin)
}

func (r *Runtime) UploadFile(ctx context.Context, id, destPath string, _ int64, reader io.Reader) error {
	return uploadFileToPod(ctx, r.client, r.restConfig, r.namespace, id, destPath, reader)
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	return downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, srcPath)
}

func (r *Runtime) UploadArchive(ctx context.Context, id string, destDir string, archive io.Reader) error {
	return uploadArchiveToPod(ctx, r.client, r.restConfig, r.namespace, id, destDir, archive)
}

func (r *Runtime) DownloadDir(ctx context.Context, id string, dirPath string) (io.ReadCloser, error) {
	return downloadDirFromPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath)
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	return listFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath)
}

func (r *Runtime) ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	return listFilesRecursiveInPod(ctx, r.client, r.restConfig, r.namespace, id, dirPath, maxDepth, page, pageSize)
}

func (r *Runtime) GlobFiles(ctx context.Context, id string, baseDir string, pattern string, page int, pageSize int) (*runtime.FileListResult, error) {
	return globFilesInPod(ctx, r.client, r.restConfig, r.namespace, id, baseDir, pattern, page, pageSize)
}

func (r *Runtime) ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	return readFileLinesInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, startLine, endLine)
}

func (r *Runtime) EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error {
	return editFileInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, oldStr, newStr, replaceAll)
}

func (r *Runtime) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	return editFileLinesInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath, startLine, endLine, newContent)
}

func (r *Runtime) UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string, blockPrivate bool) error {
	if err := updateNetworkPolicy(ctx, r.client, r.namespace, id, enabled, whitelist, blockPrivate); err != nil {
		return err
	}
	if !r.hasCilium {
		return nil
	}
	if enabled && len(whitelist) == 0 {
		return applyCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
	}
	return deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, id)
}

func (r *Runtime) RenameSandbox(_ context.Context, _ string, _ string) error {
	// Kubernetes pods cannot be renamed; this is a no-op.
	return nil
}

func (r *Runtime) UpdateLabels(ctx context.Context, id string, labels map[string]*string) error {
	// Build a merge-patch that only touches the labels we care about.
	// Using Patch avoids the GET+PUT race (409 Conflict on resourceVersion mismatch)
	// and sidesteps admission webhooks that reject full pod Updates.
	labelMap := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		if v == nil {
			labelMap[k] = nil // JSON merge-patch: null removes the key
		} else {
			labelMap[k] = *v
		}
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": labelMap,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal label patch: %w", err)
	}
	_, err = r.client.CoreV1().Pods(r.namespace).Patch(ctx, id, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch pod labels: %w", err)
	}
	return nil
}

func (r *Runtime) ListSandboxes(ctx context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	var parts []string
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	selector := ""
	if len(parts) > 0 {
		selector = joinStrings(parts, ",")
	}

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	result := make([]runtime.SandboxInfo, 0, len(pods.Items))
	for _, pod := range pods.Items {
		result = append(result, runtime.SandboxInfo{
			ID:         pod.Labels["sandbox.id"],
			RuntimeID:  pod.Name,
			RuntimeUID: string(pod.UID),
			State:      podStateString(pod.Status.Phase),
			CreatedAt:  pod.CreationTimestamp.Time,
		})
	}
	return result, nil
}

func (r *Runtime) IsStateful() bool {
	// Kubernetes pods survive a process restart independently; they must be
	// restored (not recreated) on startup.
	return true
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
