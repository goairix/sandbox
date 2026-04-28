package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

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
		Command: fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -printf '%%f\\t%%s\\t%%Y\\t%%T@\\n'", shellEscape(dirPath)),
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

		var modTimeFloat float64
		fmt.Sscanf(parts[3], "%f", &modTimeFloat)
		sec := int64(modTimeFloat)
		nsec := int64((modTimeFloat - float64(sec)) * 1e9)

		fullPath := dirPath + "/" + parts[0]

		files = append(files, runtime.FileInfo{
			Name:    parts[0],
			Path:    fullPath,
			Size:    size,
			IsDir:   isDir,
			ModTime: time.Unix(sec, nsec),
		})
	}

	return files, nil
}

// listFilesRecursiveInPod lists files recursively in a pod directory with pagination.
func listFilesRecursiveInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	if page < 1 {
		page = 1
	}

	maxDepthArg := ""
	if maxDepth > 0 {
		maxDepthArg = fmt.Sprintf("-maxdepth %d ", maxDepth)
	}

	countResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s -mindepth 1 %s\\( -type f -o -type d \\) | wc -l", shellEscape(dirPath), maxDepthArg),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalCount int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalCount)

	listCmd := fmt.Sprintf(
		"find %s -mindepth 1 %s\\( -type f -o -type d \\) -printf '%%P\\t%%s\\t%%Y\\t%%T@\\n'",
		shellEscape(dirPath), maxDepthArg,
	)
	if pageSize > 0 {
		offset := (page - 1) * pageSize
		listCmd = fmt.Sprintf("%s | tail -n +%d | head -n %d", listCmd, offset+1, pageSize)
	}

	listResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: listCmd,
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}

	var files []runtime.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(listResult.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 || parts[0] == "" {
			continue
		}

		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		isDir := parts[2] == "d"

		var modTimeFloat float64
		fmt.Sscanf(parts[3], "%f", &modTimeFloat)
		sec := int64(modTimeFloat)
		nsec := int64((modTimeFloat - float64(sec)) * 1e9)

		name := filepath.Base(parts[0])
		fullPath := dirPath + "/" + parts[0]

		files = append(files, runtime.FileInfo{
			Name:    name,
			Path:    fullPath,
			Size:    size,
			IsDir:   isDir,
			ModTime: time.Unix(sec, nsec),
		})
	}

	return &runtime.FileListResult{
		Files:      files,
		TotalCount: totalCount,
		Page:       page,
		PageSize:   pageSize,
	}, nil
}

// readFileLinesInPod reads a range of lines from a file inside a pod.
func readFileLinesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	if startLine < 1 {
		startLine = 1
	}

	countResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("wc -l < %s", shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalLines int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalLines)

	var sedRange string
	if endLine <= 0 || endLine > totalLines {
		endLine = totalLines
		sedRange = fmt.Sprintf("%d,$p", startLine)
	} else {
		sedRange = fmt.Sprintf("%d,%dp", startLine, endLine)
	}

	readResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("sed -n %s %s", shellEscape(sedRange), shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}

	var lines []string
	if strings.TrimSpace(readResult.Stdout) != "" {
		lines = strings.Split(readResult.Stdout, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}

	return &runtime.FileLineResult{
		Lines:      lines,
		StartLine:  startLine,
		EndLine:    endLine,
		TotalLines: totalLines,
	}, nil
}

// editFileInPod performs a string replacement in a file inside a pod.
func editFileInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, filePath, oldStr, newStr string, replaceAll bool) error {
	readResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("cat %s", shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	if readResult.ExitCode != 0 {
		return fmt.Errorf("read file: %s", strings.TrimSpace(readResult.Stderr))
	}

	content := readResult.Stdout
	if !strings.Contains(content, oldStr) {
		return fmt.Errorf("string not found in file: %s", oldStr)
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		newContent = strings.Replace(content, oldStr, newStr, 1)
	}

	tmpFile := fmt.Sprintf("/tmp/sandbox-edit-%d", time.Now().UnixNano())
	if err := execPipeInPod(ctx, client, restConfig, namespace, podName,
		[]string{"sh", "-c", fmt.Sprintf("cat > %s", shellEscape(tmpFile))},
		strings.NewReader(newContent),
	); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	_, err = execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: fmt.Sprintf("mv %s %s", shellEscape(tmpFile), shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	return err
}

// editFileLinesInPod replaces a range of lines in a file inside a pod.
func editFileLinesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, filePath string, startLine, endLine int, newContent string) error {
	if startLine < 1 {
		startLine = 1
	}

	tmpEdit := fmt.Sprintf("/tmp/sandbox-edit-%d", time.Now().UnixNano())
	tmpContent := fmt.Sprintf("/tmp/sandbox-content-%d", time.Now().UnixNano())

	if err := execPipeInPod(ctx, client, restConfig, namespace, podName,
		[]string{"sh", "-c", fmt.Sprintf("cat > %s", shellEscape(tmpContent))},
		strings.NewReader(newContent),
	); err != nil {
		return fmt.Errorf("write content: %w", err)
	}

	var buildCmd string
	if startLine > 1 {
		buildCmd = fmt.Sprintf("head -n %d %s > %s", startLine-1, shellEscape(filePath), shellEscape(tmpEdit))
	} else {
		buildCmd = fmt.Sprintf("> %s", shellEscape(tmpEdit))
	}
	buildCmd += fmt.Sprintf(" && cat %s >> %s", shellEscape(tmpContent), shellEscape(tmpEdit))
	if endLine > 0 {
		buildCmd += fmt.Sprintf(" && tail -n +%d %s >> %s", endLine+1, shellEscape(filePath), shellEscape(tmpEdit))
	}
	buildCmd += fmt.Sprintf(" && mv %s %s", shellEscape(tmpEdit), shellEscape(filePath))

	_, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: buildCmd,
		WorkDir: "/workspace",
	})
	return err
}