package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/goairix/sandbox/internal/runtime"
)

var partialUploadPodExec = execInPod

var consumePodUpload = consumePodUploadStream

var probePodFile = execInPod

// uploadFileToPod uploads a file into a pod via tar stream through exec.
func uploadFileToPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, destPath string, size int64, reader io.Reader) error {
	if size < 0 {
		return fmt.Errorf("%w: size must be non-negative", runtime.ErrInvalidUploadSize)
	}
	tempPath := filepath.Join(filepath.Dir(destPath), ".sandbox-upload-"+uuid.NewString())
	tarName := strings.TrimPrefix(filepath.Clean(tempPath), "/")
	command := uploadFileCommand(tempPath, destPath)
	// Extract into a same-directory temporary file and publish only after the
	// exact-size tar stream closes successfully.
	pr, pw := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		writeErr := writeSizedTar(pw, tarName, 0o644, 1000, 1000, size, reader)
		_ = pw.CloseWithError(writeErr)
		writeDone <- writeErr
	}()
	consumeErr := consumePodUpload(ctx, client, restConfig, namespace, podName, command, pr)
	_ = pr.CloseWithError(consumeErr)
	writeErr := <-writeDone
	if errors.Is(writeErr, runtime.ErrInvalidUploadSize) {
		return joinUploadCleanupError(writeErr, removePartialPodUpload(ctx, client, restConfig, namespace, podName, tempPath))
	}
	if writeErr != nil {
		return joinUploadCleanupError(writeErr, removePartialPodUpload(ctx, client, restConfig, namespace, podName, tempPath))
	}
	if consumeErr != nil {
		return joinUploadCleanupError(consumeErr, removePartialPodUpload(ctx, client, restConfig, namespace, podName, tempPath))
	}
	return nil
}

func consumePodUploadStream(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, command string, input io.Reader) error {
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"sh", "-c", command},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: input, Stdout: io.Discard, Stderr: io.Discard})
}

func uploadFileCommand(tempPath, destPath string) string {
	return fmt.Sprintf("mkdir -p %s && tar xf - -C / && test ! -d %s && mv -f -- %s %s",
		shellEscape(filepath.Dir(destPath)), shellEscape(destPath), shellEscape(tempPath), shellEscape(destPath))
}

func removePartialPodUpload(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, tempPath string) error {
	result, err := partialUploadPodExec(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{Command: "rm -f -- " + shellEscape(tempPath)})
	if err != nil {
		return fmt.Errorf("exec cleanup: %w", err)
	}
	if result == nil {
		return errors.New("exec cleanup returned no result")
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("exec cleanup exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func joinUploadCleanupError(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("cleanup partial upload: %w", cleanup))
}

func writeSizedTar(dst io.Writer, name string, mode int64, uid, gid int, size int64, reader io.Reader) error {
	if size < 0 {
		return fmt.Errorf("%w: size must be non-negative", runtime.ErrInvalidUploadSize)
	}
	tw := tar.NewWriter(dst)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: mode, Uid: uid, Gid: gid}); err != nil {
		return fmt.Errorf("tar header: %w", err)
	}
	if _, err := io.CopyN(tw, reader, size); err != nil {
		return fmt.Errorf("%w: body shorter than declared size: %v", runtime.ErrInvalidUploadSize, err)
	}
	var extra [1]byte
	n, err := reader.Read(extra[:])
	if n != 0 || err == nil {
		return fmt.Errorf("%w: body exceeds declared size", runtime.ErrInvalidUploadSize)
	}
	if err != io.EOF {
		return fmt.Errorf("read upload trailer: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	return nil
}

// downloadFileFromPod downloads a file from a pod via tar stream.
func downloadFileFromPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, srcPath string) (io.ReadCloser, error) {
	if err := fileExistsInPod(ctx, client, restConfig, namespace, podName, srcPath); err != nil {
		return nil, err
	}
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
	done := make(chan struct{})

	go func() {
		defer close(done)
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
			Stderr: io.Discard,
		})
		pw.CloseWithError(err)
	}()

	return &pipeReadCloser{pr: pr, done: done}, nil
}

func (r *Runtime) FileExists(ctx context.Context, id string, filePath string) error {
	return fileExistsInPod(ctx, r.client, r.restConfig, r.namespace, id, filePath)
}

func fileExistsInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, filePath string) error {
	result, err := probePodFile(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: "LC_ALL=C stat -L -c '%F' -- " + shellEscape(filePath),
		WorkDir: "/",
	})
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	if result == nil {
		return fmt.Errorf("stat file returned no result")
	}
	if result.ExitCode != 0 {
		// Only a confirmed stat ENOENT is a missing file. test -f alone
		// conflates missing paths with inaccessible parent directories.
		if result.ExitCode == 1 && strings.HasSuffix(strings.TrimSpace(result.Stderr), ": No such file or directory") {
			return runtime.ErrFileNotFound
		}
		return fmt.Errorf("stat file exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if strings.TrimSpace(result.Stdout) != "regular file" && strings.TrimSpace(result.Stdout) != "regular empty file" {
		return fmt.Errorf("path is not a regular file")
	}
	return nil
}

func (r *Runtime) ReadFileContent(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	if err := r.FileExists(ctx, id, srcPath); err != nil {
		return nil, err
	}

	execReq := r.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"cat", srcPath},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
			Stderr: io.Discard,
		})
		pw.CloseWithError(err)
	}()

	return &pipeReadCloser{pr: pr, done: done}, nil
}

func (r *Runtime) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	// Find the first wildcard to determine the search root (no wildcards in baseDir).
	firstStar := strings.Index(pattern, "*")
	if firstStar == -1 {
		return nil, fmt.Errorf("invalid pattern: must contain wildcard")
	}
	lastSlashBeforeStar := strings.LastIndex(pattern[:firstStar], "/")
	if lastSlashBeforeStar == -1 {
		return nil, fmt.Errorf("invalid pattern: must contain directory path")
	}
	baseDir := pattern[:lastSlashBeforeStar]
	// Convert glob pattern to find -path format: /workspace/.agent/skills/*/SKILL.md -> */SKILL.md relative to baseDir
	relPattern := pattern[lastSlashBeforeStar+1:]

	execReq := r.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"find", baseDir, "-path", baseDir + "/" + relPattern, "-type", "f"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("find command failed: %w, stderr: %s", err, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []runtime.FileContent{}, nil
	}

	results := make([]runtime.FileContent, len(lines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i, line := range lines {
		wg.Add(1)
		go func(idx int, relPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fullPath := line
			reader, err := downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, fullPath)
			results[idx] = runtime.FileContent{
				Path:    fullPath,
				Content: reader,
				Error:   err,
			}
		}(i, line)
	}
	wg.Wait()

	return results, nil
}

func (r *Runtime) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
	results := make([]runtime.FileContent, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i, path := range paths {
		wg.Add(1)
		go func(idx int, filePath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reader, err := downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, filePath)
			results[idx] = runtime.FileContent{
				Path:    filePath,
				Content: reader,
				Error:   err,
			}
		}(i, path)
	}
	wg.Wait()

	return results, nil
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
	command, err := exactPodCommand(ctx, podName, []string{"tar", "cf", "-", "-C", "/", strings.TrimPrefix(dirPath, "/")})
	if err != nil {
		return nil, err
	}
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdout: pw,
			Stderr: io.Discard,
		})
		pw.CloseWithError(err)
	}()

	return &pipeReadCloser{pr: pr, done: done}, nil
}

// pipeReadCloser wraps a PipeReader and waits for the background streaming
// goroutine to finish before closing the pipe. The Kubernetes SPDY client
// calls runtime.HandleError on any io.Copy error, which prints "Unhandled
// Error: io: read/write on closed pipe" if pr is closed while the goroutine
// is still writing. Waiting for the goroutine first ensures pw is always
// closed by the goroutine itself before we close pr.
type pipeReadCloser struct {
	pr   *io.PipeReader
	done <-chan struct{}
}

func (p *pipeReadCloser) Read(b []byte) (int, error) { return p.pr.Read(b) }
func (p *pipeReadCloser) Close() error {
	// Drain synchronously until the streaming goroutine closes pw (returns EOF).
	// Using a separate goroutine would race: pr.Close() could fire while the
	// drain goroutine is mid-Read, producing the same ErrClosedPipe we're
	// trying to suppress.
	io.Copy(io.Discard, p.pr) //nolint:errcheck
	<-p.done
	return p.pr.Close()
}

// execPipeInPod executes a command in a pod with an io.Reader connected to stdin.
func execPipeInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string, cmd []string, stdin io.Reader) error {
	cmd, err := exactPodCommand(ctx, podName, cmd)
	if err != nil {
		return err
	}
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

// globFilesInPod finds files matching a glob pattern inside a pod with pagination.
func globFilesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, baseDir, pattern string, page int, pageSize int) (*runtime.FileListResult, error) {
	if page < 1 {
		page = 1
	}

	findArgs, maxDepth1 := runtime.GlobToFindArgs(pattern)
	depthArg := ""
	if maxDepth1 {
		depthArg = "-maxdepth 1 "
	}

	countCmd := fmt.Sprintf("find %s -mindepth 1 %s%s -type f | wc -l", shellEscape(baseDir), depthArg, findArgs)
	countResult, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{
		Command: countCmd,
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalCount int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalCount)

	listCmd := fmt.Sprintf(
		"find %s -mindepth 1 %s%s -type f -printf '%%P\\t%%s\\t%%Y\\t%%T@\\n'",
		shellEscape(baseDir), depthArg, findArgs,
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

		var modTimeFloat float64
		fmt.Sscanf(parts[3], "%f", &modTimeFloat)
		sec := int64(modTimeFloat)
		nsec := int64((modTimeFloat - float64(sec)) * 1e9)

		name := filepath.Base(parts[0])
		fullPath := baseDir + "/" + parts[0]

		files = append(files, runtime.FileInfo{
			Name:    name,
			Path:    fullPath,
			Size:    size,
			IsDir:   false,
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

// countReservedFilesInPod counts exact reserved probe basenames without
// materialising matching paths in the API process.
func countReservedFilesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, baseDir string, maxDepth int, globPattern string) (int, error) {
	command := reservedFileCountCommand(baseDir, maxDepth, globPattern)
	result, err := execInPod(ctx, client, restConfig, namespace, podName, runtime.ExecRequest{Command: command, WorkDir: "/workspace"})
	if err != nil {
		return 0, fmt.Errorf("count reserved FUSE files: %w", err)
	}
	if result == nil {
		return 0, fmt.Errorf("count reserved FUSE files: runtime returned no result")
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return 0, fmt.Errorf("count reserved FUSE files: %s", detail)
	}
	var count int
	if scanned, scanErr := fmt.Sscanf(strings.TrimSpace(result.Stdout), "%d", &count); scanErr != nil || scanned != 1 || count < 0 {
		return 0, fmt.Errorf("count reserved FUSE files: invalid count %q", strings.TrimSpace(result.Stdout))
	}
	return count, nil
}

func reservedFileCountCommand(baseDir string, maxDepth int, globPattern string) string {
	depthArg := ""
	predicate := "\\( -type f -o -type d \\)"
	if globPattern == "" {
		if maxDepth > 0 {
			depthArg = fmt.Sprintf("-maxdepth %d ", maxDepth)
		}
	} else {
		findArgs, maxDepth1 := runtime.GlobToFindArgs(globPattern)
		if maxDepth1 {
			depthArg = "-maxdepth 1 "
		}
		predicate = fmt.Sprintf("\\( %s \\) -type f", findArgs)
	}
	findCommand := fmt.Sprintf(
		"find %s -mindepth 1 %s%s -name %s -printf '.\\n'",
		shellEscape(baseDir), depthArg, predicate, shellEscape(runtime.ReservedProbeObjectFindPattern()),
	)
	return fmt.Sprintf(
		"{ %s; sandbox_find_status=$?; printf 'sandbox-find-status:%%d\\n' \"$sandbox_find_status\"; } | "+
			"awk '/^sandbox-find-status:/ { status=$0; sub(/^sandbox-find-status:/, \"\", status); seen=1; if (status != 0) exit status; print count+0; next } { count++ } END { if (!seen) exit 125 }'",
		findCommand,
	)
}

// readFileLinesInPod reads a range of lines from a file inside a pod.
func readFileLinesInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	if err := fileExistsInPod(ctx, client, restConfig, namespace, podName, filePath); err != nil {
		return nil, err
	}

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
	if err := fileExistsInPod(ctx, client, restConfig, namespace, podName, filePath); err != nil {
		return err
	}

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
	if err := fileExistsInPod(ctx, client, restConfig, namespace, podName, filePath); err != nil {
		return err
	}

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
