package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/goairix/sandbox/internal/runtime"
)

func (r *Runtime) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	return r.cli.CopyToContainer(ctx, id, destPath, reader, container.CopyToContainerOptions{})
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	reader, _, err := r.cli.CopyFromContainer(ctx, id, srcPath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	result, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s -maxdepth 1 -printf '%%f\\t%%s\\t%%Y\\t%%T@\\n'", dirPath),
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
