package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/goairix/sandbox/internal/runtime"
)

// shellEscape wraps a string in single quotes with proper escaping
// to prevent shell injection.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}


func (r *Runtime) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	content, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read file content: %w", err)
	}

	// Docker CopyToContainer expects a tar archive.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:    filepath.Base(destPath),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
		Uid:     1000,
		Gid:     1000,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("write tar content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	dir := filepath.Dir(destPath)
	if _, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("mkdir -p %s", shellEscape(dir)),
	}); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return r.cli.CopyToContainer(ctx, id, dir, &buf, container.CopyToContainerOptions{})
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	tarReader, _, err := r.cli.CopyFromContainer(ctx, id, srcPath)
	if err != nil {
		return nil, err
	}

	// Docker returns a tar archive; extract the single file from it.
	tr := tar.NewReader(tarReader)
	_, err = tr.Next()
	if err != nil {
		tarReader.Close()
		return nil, fmt.Errorf("read tar entry: %w", err)
	}

	// Return a reader that reads file content and closes the underlying tar stream.
	return &tarEntryReader{Reader: tr, closer: tarReader}, nil
}

// tarEntryReader wraps a tar.Reader entry and closes the underlying stream.
type tarEntryReader struct {
	io.Reader
	closer io.Closer
}

func (r *tarEntryReader) Close() error {
	return r.closer.Close()
}

func (r *Runtime) UploadArchive(ctx context.Context, id string, destDir string, archive io.Reader) error {
	return r.cli.CopyToContainer(ctx, id, destDir, archive, container.CopyToContainerOptions{})
}

func (r *Runtime) DownloadDir(ctx context.Context, id string, dirPath string) (io.ReadCloser, error) {
	reader, _, err := r.cli.CopyFromContainer(ctx, id, dirPath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	result, err := r.Exec(ctx, id, runtime.ExecRequest{
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

func (r *Runtime) ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	// Normalise page before computing offset
	if page < 1 {
		page = 1
	}

	// Build find command args
	maxDepthArg := ""
	if maxDepth > 0 {
		maxDepthArg = fmt.Sprintf("-maxdepth %d ", maxDepth)
	}

	// Get total count (exclude root dir with -mindepth 1)
	countResult, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s -mindepth 1 %s\\( -type f -o -type d \\) | wc -l", shellEscape(dirPath), maxDepthArg),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalCount int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalCount)

	// Build paginated listing command (exclude root dir with -mindepth 1)
	listCmd := fmt.Sprintf(
		"find %s -mindepth 1 %s\\( -type f -o -type d \\) -printf '%%P\\t%%s\\t%%Y\\t%%T@\\n'",
		shellEscape(dirPath), maxDepthArg,
	)
	if pageSize > 0 {
		offset := (page - 1) * pageSize
		listCmd = fmt.Sprintf("%s | tail -n +%d | head -n %d", listCmd, offset+1, pageSize)
	}

	listResult, err := r.Exec(ctx, id, runtime.ExecRequest{
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

func (r *Runtime) ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	if startLine < 1 {
		startLine = 1
	}

	// Get total line count
	countResult, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("wc -l < %s", shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalLines int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalLines)

	// Build sed range: endLine 0 means read to end of file
	var sedRange string
	if endLine <= 0 || endLine > totalLines {
		endLine = totalLines
		sedRange = fmt.Sprintf("%d,$p", startLine)
	} else {
		sedRange = fmt.Sprintf("%d,%dp", startLine, endLine)
	}

	readResult, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("sed -n %s %s", shellEscape(sedRange), shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}

	var lines []string
	if strings.TrimSpace(readResult.Stdout) != "" {
		lines = strings.Split(readResult.Stdout, "\n")
		// Remove trailing empty string from final newline
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

func (r *Runtime) EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error {
	// Read the file content from the container.
	readResult, err := r.Exec(ctx, id, runtime.ExecRequest{
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

	// Check that oldStr exists in the file.
	if !strings.Contains(content, oldStr) {
		return fmt.Errorf("string not found in file: %s", oldStr)
	}

	// Perform the replacement in Go — supports multiline strings naturally.
	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		newContent = strings.Replace(content, oldStr, newStr, 1)
	}

	// Write the result back via stdin pipe to avoid shell escaping issues.
	tmpFile := fmt.Sprintf("/tmp/sandbox-edit-%d", time.Now().UnixNano())
	if err := r.ExecPipe(ctx, id, []string{"sh", "-c", fmt.Sprintf("cat > %s", shellEscape(tmpFile))}, strings.NewReader(newContent)); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	_, err = r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("mv %s %s", shellEscape(tmpFile), shellEscape(filePath)),
		WorkDir: "/workspace",
	})
	return err
}

func (r *Runtime) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	if startLine < 1 {
		startLine = 1
	}

	tmpEdit := fmt.Sprintf("/tmp/sandbox-edit-%d", time.Now().UnixNano())
	tmpContent := fmt.Sprintf("/tmp/sandbox-content-%d", time.Now().UnixNano())

	// Stream new content into a temp file via stdin to avoid ARG_MAX limits
	if err := r.ExecPipe(ctx, id, []string{"sh", "-c", fmt.Sprintf("cat > %s", shellEscape(tmpContent))}, strings.NewReader(newContent)); err != nil {
		return fmt.Errorf("write content: %w", err)
	}

	// Build the replacement command:
	// 1. Write lines before startLine to tmpEdit
	// 2. Append new content
	// 3. Append lines after endLine (if endLine > 0)
	// 4. Move tmpEdit back to filePath
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

	_, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: buildCmd,
		WorkDir: "/workspace",
	})
	return err
}

func (r *Runtime) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	lastSlash := strings.LastIndex(pattern, "/")
	if lastSlash == -1 {
		return nil, fmt.Errorf("invalid pattern: must contain directory path")
	}
	baseDir := pattern[:lastSlash]
	globPattern := pattern[lastSlash+1:]

	execResp, err := r.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          []string{"sh", "-c", fmt.Sprintf("cd %s && find . -name '%s' -type f", baseDir, globPattern)},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	defer attachResp.Close()

	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, attachResp.Reader); err != nil {
		return nil, fmt.Errorf("read exec output: %w", err)
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

			fullPath := filepath.Join(baseDir, strings.TrimPrefix(relPath, "./"))
			reader, err := r.downloadFile(ctx, id, fullPath)
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

			reader, err := r.downloadFile(ctx, id, filePath)
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

func (r *Runtime) downloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	execResp, err := r.cli.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          []string{"tar", "cf", "-", srcPath},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, attachResp.Reader)
		attachResp.Close()
		pw.CloseWithError(err)
	}()

	return pr, nil
}
