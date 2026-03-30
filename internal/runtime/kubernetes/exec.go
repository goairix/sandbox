package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/goairix/sandbox/internal/runtime"
)

// execInPod executes a command synchronously inside a pod.
func execInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
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

	cmd := []string{"sh", "-c", fmt.Sprintf("cd %s && %s", workDir, req.Command)}

	// Build env prefix
	var envPrefix string
	for k, v := range req.Env {
		envPrefix += fmt.Sprintf("export %s=%s; ", k, v)
	}
	if envPrefix != "" {
		cmd = []string{"sh", "-c", envPrefix + fmt.Sprintf("cd %s && %s", workDir, req.Command)}
	}

	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   cmd,
			Stdin:     req.Stdin != "",
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if req.Stdin != "" {
		stdin = strings.NewReader(req.Stdin)
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	exitCode := 0
	if err != nil {
		// Try to extract exit code from error
		if exitErr, ok := err.(interface{ ExitStatus() int }); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			return nil, fmt.Errorf("exec stream: %w", err)
		}
	}

	return &runtime.ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}, nil
}

// execStreamInPod executes a command in a pod with streaming output.
func execStreamInPod(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, namespace, podName string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
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

	cmd := []string{"sh", "-c", fmt.Sprintf("cd %s && %s", workDir, req.Command)}

	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	ch := make(chan runtime.StreamEvent, 64)

	stdoutPR, stdoutPW := io.Pipe()
	stderrPR, stderrPW := io.Pipe()

	go func() {
		defer close(ch)

		// Run exec in background
		execDone := make(chan error, 1)
		go func() {
			execDone <- executor.StreamWithContext(ctx, remotecommand.StreamOptions{
				Stdout: stdoutPW,
				Stderr: stderrPW,
			})
			stdoutPW.Close()
			stderrPW.Close()
		}()

		// Stream stderr
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, readErr := stderrPR.Read(buf)
				if n > 0 {
					ch <- runtime.StreamEvent{Type: runtime.StreamStderr, Content: string(buf[:n])}
				}
				if readErr != nil {
					break
				}
			}
		}()

		// Stream stdout
		buf := make([]byte, 4096)
		for {
			n, readErr := stdoutPR.Read(buf)
			if n > 0 {
				ch <- runtime.StreamEvent{Type: runtime.StreamStdout, Content: string(buf[:n])}
			}
			if readErr != nil {
				break
			}
		}

		<-done

		execErr := <-execDone
		if execErr != nil {
			ch <- runtime.StreamEvent{Type: runtime.StreamError, Content: execErr.Error()}
		} else {
			ch <- runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}
		}
	}()

	return ch, nil
}
