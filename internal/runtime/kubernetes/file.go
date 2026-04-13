package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/goairix/sandbox/internal/runtime"
)

// uploadFileToPod uploads a file into a pod via tar stream through exec.
func uploadFileToPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, destPath string, reader io.Reader) error {
	// Read all content
	content, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read upload content: %w", err)
	}

	// Create tar archive
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: destPath,
		Size: int64(len(content)),
		Mode: 0644,
		Uid:  1000,
		Gid:  1000,
	}); err != nil {
		return fmt.Errorf("tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("tar write: %w", err)
	}
	tw.Close()

	// Extract inside pod
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"tar", "xf", "-", "-C", "/"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  &buf,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
}

// downloadFileFromPod downloads a file from a pod via tar stream.
func downloadFileFromPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, srcPath string) (io.ReadCloser, error) {
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"tar", "cf", "-", srcPath},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	pr, pw := io.Pipe()

	go func() {
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
			Stderr: io.Discard,
		})
		pw.CloseWithError(err)
	}()

	return pr, nil
}

// uploadArchiveToPod uploads a tar archive into a pod, extracting at destDir.
func uploadArchiveToPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, destDir string, archive io.Reader) error {
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"tar", "xf", "-", "-C", destDir},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, archive); err != nil {
		return fmt.Errorf("buffer archive: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  &buf,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
}

// downloadDirFromPod downloads an entire directory from a pod as a tar archive.
func downloadDirFromPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, dirPath string) (io.ReadCloser, error) {
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"tar", "cf", "-", "-C", "/", strings.TrimPrefix(dirPath, "/")},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	pr, pw := io.Pipe()

	go func() {
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
			Stderr: io.Discard,
		})
		pw.CloseWithError(err)
	}()

	return pr, nil
}

// execPipeInPod executes a command in a pod with an io.Reader connected to stdin.
func execPipeInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string, cmd []string, stdin io.Reader) error {
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  io.NopCloser(stdin),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
}

// listFilesInPod lists files in a directory inside a pod.
func listFilesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, dirPath string) ([]runtime.FileInfo, error) {
	result, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s -maxdepth 1 -printf '%%f\\t%%s\\t%%Y\\t%%T@\\n'", shellEscape(dirPath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}

	var files []runtime.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 || parts[0] == "." {
			continue
		}

		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		isDir := parts[2] == "d"

		var modTime int64
		fmt.Sscanf(parts[3], "%d", &modTime)

		files = append(files, runtime.FileInfo{
			Name:    parts[0],
			Path:    dirPath + "/" + parts[0],
			Size:    size,
			IsDir:   isDir,
			ModTime: modTime,
		})
	}

	return files, nil
}
