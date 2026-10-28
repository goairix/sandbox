package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/goairix/sandbox/internal/runtime"
)

const fileV2Helper = "/usr/local/bin/sandbox-file-op"

var _ runtime.FileV2Transport = (*Runtime)(nil)

// RunFileOp starts exactly one non-TTY helper. The caller owns the returned
// stream and must close it to join the exec and release its operation gate.
func (r *Runtime) RunFileOp(ctx context.Context, id string, argv []string, stdin io.Reader) (io.ReadCloser, error) {
	if len(argv) < 2 || len(argv[1]) > 4096 || strings.IndexByte(argv[1], 0) >= 0 {
		return nil, errors.New("invalid file helper arguments")
	}
	user, err := r.publicExecUser(ctx, id)
	if err != nil {
		return nil, err
	}
	command := append([]string{fileV2Helper}, argv...)
	created, err := r.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: command, User: user, AttachStdin: stdin != nil,
		AttachStdout: true, AttachStderr: true, Tty: false,
	})
	if err != nil {
		return nil, fmt.Errorf("create file helper: %w", err)
	}
	attached, err := r.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach file helper: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stopInput := context.AfterFunc(streamCtx, func() {
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	pr, pw := io.Pipe()
	finished := make(chan error, 1)
	stopWatch := watchDockerAttachContext(streamCtx, attached.Close)
	go func() {
		defer stopInput()
		defer stopWatch()
		defer attached.Close()
		var inputDone <-chan error
		if stdin == nil {
			_ = attached.CloseWrite()
		} else {
			input := make(chan error, 1)
			inputDone = input
			go func() {
				_, copyErr := io.Copy(attached.Conn, stdin)
				input <- errors.Join(copyErr, attached.CloseWrite())
			}()
		}
		stderr := &limitedControlBuffer{limit: 4096}
		copyErr := copyDockerExecFrames(pw, stderr, attached.Reader)
		if inputDone != nil {
			copyErr = errors.Join(copyErr, <-inputDone)
		}
		if copyErr == nil {
			if err := streamCtx.Err(); err != nil {
				copyErr = err
			} else {
				inspected, err := r.cli.ContainerExecInspect(streamCtx, created.ID)
				if err != nil {
					copyErr = fmt.Errorf("inspect file helper: %w", err)
				} else if inspected.Running {
					copyErr = errors.New("file helper stream closed before process exit")
				} else if inspected.ExitCode != 0 {
					copyErr = runtime.FileV2ExitError(inspected.ExitCode)
				}
			}
		}
		finished <- copyErr
		_ = pw.CloseWithError(copyErr)
	}()
	return runtime.NewFileOpStream(pr, cancel, finished), nil
}
