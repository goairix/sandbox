package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Docker.
type Runtime struct {
	cli       *dockerclient.Client
	networkID string
}

// New creates a new Docker runtime.
func New(ctx context.Context, host string) (*Runtime, error) {
	opts := []dockerclient.Opt{
		dockerclient.WithAPIVersionNegotiation(),
	}
	if host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}

	cli, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	netID, err := ensureNetwork(ctx, cli)
	if err != nil {
		return nil, err
	}

	return &Runtime{cli: cli, networkID: netID}, nil
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	containerID, err := createContainer(ctx, r.cli, spec, r.networkID)
	if err != nil {
		return nil, err
	}

	// Start the container
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start container: %w", err)
	}

	return &runtime.SandboxInfo{
		ID:        spec.ID,
		RuntimeID: containerID,
		State:     "running",
		CreatedAt: time.Now(),
	}, nil
}

func (r *Runtime) StartSandbox(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	timeout := 10
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}

	state := "unknown"
	if info.State.Running {
		state = "running"
	} else if info.State.Paused {
		state = "paused"
	} else {
		state = "stopped"
	}

	created, _ := time.Parse(time.RFC3339Nano, info.Created)

	return &runtime.SandboxInfo{
		ID:        id,
		RuntimeID: info.ID,
		State:     state,
		CreatedAt: created,
	}, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	start := time.Now()

	workDir := req.WorkDir
	if workDir == "" {
		workDir = "/workspace"
	}

	// Build command
	cmd := []string{"sh", "-c", req.Command}

	// Build env
	var env []string
	for k, v := range req.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  req.Stdin != "",
		WorkingDir:   workDir,
		Env:          env,
	}

	execResp, err := r.cli.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()

	// Send stdin if provided
	if req.Stdin != "" {
		_, _ = io.Copy(attachResp.Conn, strings.NewReader(req.Stdin))
		_ = attachResp.CloseWrite()
	}

	// Read stdout/stderr
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader)
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}

	// Get exit code
	inspectResp, err := r.cli.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("exec inspect: %w", err)
	}

	return &runtime.ExecResult{
		ExitCode: inspectResp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}, nil
}

func (r *Runtime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = "/workspace"
	}

	cmd := []string{"sh", "-c", req.Command}

	var env []string
	for k, v := range req.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   workDir,
		Env:          env,
	}

	execResp, err := r.cli.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	ch := make(chan runtime.StreamEvent, 64)

	go func() {
		defer close(ch)
		defer attachResp.Close()

		// Use a pipe to demux stdout/stderr
		stdoutPR, stdoutPW := io.Pipe()
		stderrPR, stderrPW := io.Pipe()

		go func() {
			_, _ = stdcopy.StdCopy(stdoutPW, stderrPW, attachResp.Reader)
			stdoutPW.Close()
			stderrPW.Close()
		}()

		// Stream stderr in a goroutine
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, err := stderrPR.Read(buf)
				if n > 0 {
					ch <- runtime.StreamEvent{
						Type:    runtime.StreamStderr,
						Content: string(buf[:n]),
					}
				}
				if err != nil {
					break
				}
			}
		}()

		// Stream stdout in this goroutine
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPR.Read(buf)
			if n > 0 {
				ch <- runtime.StreamEvent{
					Type:    runtime.StreamStdout,
					Content: string(buf[:n]),
				}
			}
			if err != nil {
				break
			}
		}

		<-done

		// Get exit code
		inspectResp, err := r.cli.ContainerExecInspect(context.Background(), execResp.ID)
		if err != nil {
			ch <- runtime.StreamEvent{
				Type:    runtime.StreamError,
				Content: err.Error(),
			}
			return
		}

		ch <- runtime.StreamEvent{
			Type:    runtime.StreamDone,
			Content: fmt.Sprintf("%d", inspectResp.ExitCode),
		}
	}()

	return ch, nil
}

func (r *Runtime) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	return r.cli.CopyToContainer(ctx, id, destPath, reader, types.CopyToContainerOptions{})
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
