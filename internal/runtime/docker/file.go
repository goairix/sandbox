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
