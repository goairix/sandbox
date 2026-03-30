package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/goairix/sandbox/internal/runtime"
)

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	start := time.Now()

	// Enforce timeout
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer cancel()
	}

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
	// Enforce timeout
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer cancel()
	}

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

		stdoutPR, stdoutPW := io.Pipe()
		stderrPR, stderrPW := io.Pipe()

		go func() {
			_, _ = stdcopy.StdCopy(stdoutPW, stderrPW, attachResp.Reader)
			stdoutPW.Close()
			stderrPW.Close()
		}()

		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, readErr := stderrPR.Read(buf)
				if n > 0 {
					ch <- runtime.StreamEvent{
						Type:    runtime.StreamStderr,
						Content: string(buf[:n]),
					}
				}
				if readErr != nil {
					break
				}
			}
		}()

		buf := make([]byte, 4096)
		for {
			n, readErr := stdoutPR.Read(buf)
			if n > 0 {
				ch <- runtime.StreamEvent{
					Type:    runtime.StreamStdout,
					Content: string(buf[:n]),
				}
			}
			if readErr != nil {
				break
			}
		}

		<-done

		inspectResp, inspectErr := r.cli.ContainerExecInspect(context.Background(), execResp.ID)
		if inspectErr != nil {
			ch <- runtime.StreamEvent{
				Type:    runtime.StreamError,
				Content: inspectErr.Error(),
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
