package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/goairix/sandbox/internal/runtime"
)

const fileV2Helper = "/usr/local/bin/sandbox-file-op"

var _ runtime.FileV2Transport = (*Runtime)(nil)

func fileV2PodCommand(ctx context.Context, id string, argv []string) ([]string, error) {
	if len(argv) < 2 || len(argv[1]) > 4096 || strings.IndexByte(argv[1], 0) >= 0 {
		return nil, errors.New("invalid file helper arguments")
	}
	command := append([]string{fileV2Helper}, argv...)
	command, err := exactPodCommand(ctx, id, command)
	if err != nil {
		return nil, err
	}
	return sandboxCommand(command), nil
}

// RunFileOp uses the existing sandbox container's SPDY exec stream. It keeps
// stdout raw and stderr separate so JSON metadata and file bytes never mix.
func (r *Runtime) RunFileOp(ctx context.Context, id string, argv []string, stdin io.Reader) (io.ReadCloser, error) {
	command, err := fileV2PodCommand(ctx, id, argv)
	if err != nil {
		return nil, err
	}
	if r.client == nil || r.client.CoreV1().RESTClient() == nil || r.restConfig == nil {
		return nil, errors.New("kubernetes file helper transport is not configured")
	}
	request := r.client.CoreV1().RESTClient().Post().
		Resource("pods").Name(id).Namespace(r.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox", Command: command,
			Stdin: stdin != nil, Stdout: true, Stderr: true, TTY: false,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", request.URL())
	if err != nil {
		return nil, fmt.Errorf("create file helper executor: %w", err)
	}
	return streamFileV2Pod(ctx, executor, stdin), nil
}

func streamFileV2Pod(ctx context.Context, executor remotecommand.Executor, stdin io.Reader) io.ReadCloser {
	streamCtx, cancel := context.WithCancel(ctx)
	stopInput := context.AfterFunc(streamCtx, func() {
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	pr, pw := io.Pipe()
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		defer stopInput()
		defer close(done)
		stderr := &boundedBuffer{limit: 4096}
		err := executor.StreamWithContext(streamCtx, remotecommand.StreamOptions{
			Stdin: stdin, Stdout: pw, Stderr: stderr,
		})
		finished <- err
		_ = pw.CloseWithError(err)
	}()
	// Cancel first, then drain any in-flight SPDY write before closing the
	// pipe. This avoids a closed-pipe error being logged by client-go.
	return runtime.NewFileOpStream(&pipeReadCloser{pr: pr, done: done}, cancel, finished)
}
