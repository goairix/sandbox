package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/goairix/sandbox/internal/runtime"
)

// shellEscape wraps a string in single quotes with proper escaping
// to prevent shell injection.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellEscapeSed escapes special characters in a string for use in a sed s/// expression.
// The delimiter used is '/', so '/', '\', and '&' must be escaped.
func shellEscapeSed(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "/", "\\/")
	s = strings.ReplaceAll(s, "&", "\\&")
	return s
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
	// Build find command
	maxDepthArg := ""
	if maxDepth > 0 {
		maxDepthArg = fmt.Sprintf("-maxdepth %d ", maxDepth)
	}

	// Get total count
	countResult, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s %s\\( -type f -o -type d \\) | wc -l", shellEscape(dirPath), maxDepthArg),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}
	var totalCount int
	fmt.Sscanf(strings.TrimSpace(countResult.Stdout), "%d", &totalCount)

	// Build paginated listing command
	// Use -printf to get: relative path, size, type, mod time
	listCmd := fmt.Sprintf(
		"find %s %s\\( -type f -o -type d \\) -printf '%%P\\t%%s\\t%%Y\\t%%T@\\n'",
		shellEscape(dirPath), maxDepthArg,
	)
	if pageSize > 0 {
		offset := (page - 1) * pageSize
		if offset < 0 {
			offset = 0
		}
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

		fullPath := dirPath + "/" + parts[0]

		files = append(files, runtime.FileInfo{
			Name:    parts[0],
			Path:    fullPath,
			Size:    size,
			IsDir:   isDir,
			ModTime: time.Unix(sec, nsec),
		})
	}

	if page < 1 {
		page = 1
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
	flag := ""
	if replaceAll {
		flag = "g"
	}

	escapedOld := shellEscapeSed(oldStr)
	escapedNew := shellEscapeSed(newStr)

	tmpFile := fmt.Sprintf("/tmp/sandbox-edit-%d", time.Now().UnixNano())
	cmd := fmt.Sprintf(
		"sed 's/%s/%s/%s' %s > %s && mv %s %s",
		escapedOld, escapedNew, flag,
		shellEscape(filePath), shellEscape(tmpFile),
		shellEscape(tmpFile), shellEscape(filePath),
	)

	_, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: cmd,
		WorkDir: "/workspace",
	})
	return err
}

func (r *Runtime) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	return fmt.Errorf("not implemented")
}
