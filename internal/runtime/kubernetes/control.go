package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	workspaceMounterContainer = "workspace-mounter"
	sandboxContainer          = "sandbox"
	workspaceProbeBinary      = fuseprotocol.ProbeBinary
	maxControlJSONBytes       = fuseprotocol.MaxJSONBytes
	controlWireVersion        = fuseprotocol.Version
)

var (
	ErrInvalidControlContainer = errors.New("invalid workspace control container")
	ErrInvalidControlCommand   = errors.New("invalid workspace control command")
)

type podCommandExecutor interface {
	Exec(ctx context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error)
}

type spdyPodCommandExecutor struct {
	client     kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func (e *spdyPodCommandExecutor) Exec(ctx context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error) {
	if e.client == nil || e.restConfig == nil {
		return nil, fmt.Errorf("Kubernetes control executor is not configured")
	}
	request := e.client.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(e.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   append([]string(nil), argv...),
			Stdin:     len(stdin) != 0,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", request.URL())
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes control executor: %w", err)
	}
	stdout := &boundedBuffer{limit: maxControlJSONBytes}
	stderr := &boundedBuffer{limit: maxControlJSONBytes}
	if err := executor.StreamWithContext(ctx, controlStreamOptions(stdin, stdout, stderr)); err != nil {
		// stderr can contain supervisor input-derived diagnostics; do not include it.
		return nil, fmt.Errorf("execute Kubernetes control command: %w", err)
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("Kubernetes control output exceeds limit")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func controlStreamOptions(stdin []byte, stdout, stderr *boundedBuffer) remotecommand.StreamOptions {
	options := remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr}
	if len(stdin) != 0 {
		options.Stdin = strings.NewReader(string(stdin))
	}
	return options
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = b.overflow || original > 0
		return original, nil
	}
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

type mounterStatusWire = fuseprotocol.MounterStatus
type controlAckWire = fuseprotocol.ControlAck
type authorizeRequestWire = fuseprotocol.AuthorizeRequest
type controlRequestWire = fuseprotocol.ControlRequest
type probeResumeRequestWire = fuseprotocol.ProbeResumeRequest
type shutdownAckWire = fuseprotocol.ShutdownAck
type probeStatusWire = fuseprotocol.ProbeStatus

func (r *Runtime) execControl(ctx context.Context, pod, container string, argv []string, stdin []byte) ([]byte, error) {
	if container != workspaceMounterContainer {
		return nil, ErrInvalidControlContainer
	}
	if !allowedMounterCommand(argv) {
		return nil, ErrInvalidControlCommand
	}
	if len(stdin) > maxControlJSONBytes {
		return nil, fmt.Errorf("workspace control input exceeds limit")
	}
	if r.controlExecutor == nil {
		return nil, fmt.Errorf("workspace control executor is unavailable")
	}
	return r.controlExecutor.Exec(ctx, pod, container, append([]string(nil), argv...), append([]byte(nil), stdin...))
}

func (r *Runtime) execSandboxProbe(ctx context.Context, pod string, argv []string, stdin []byte) ([]byte, error) {
	if !allowedProbeCommand(argv) {
		return nil, ErrInvalidControlCommand
	}
	if len(stdin) > maxControlJSONBytes {
		return nil, fmt.Errorf("workspace probe input exceeds limit")
	}
	if r.controlExecutor == nil {
		return nil, fmt.Errorf("workspace control executor is unavailable")
	}
	return r.controlExecutor.Exec(ctx, pod, sandboxContainer, append([]string(nil), argv...), append([]byte(nil), stdin...))
}

func allowedMounterCommand(argv []string) bool {
	return fuseprotocol.AllowedMounterCommand(argv)
}

func allowedProbeCommand(argv []string) bool {
	return fuseprotocol.AllowedProbeCommand(argv)
}

func validControlIdentity(value string) bool {
	return fuseprotocol.ValidIdentity(value)
}

func decodeMounterStatus(raw []byte) (mounterStatusWire, error) {
	var result mounterStatusWire
	if err := strictDecodeControlJSON(raw, &result); err != nil {
		return result, err
	}
	if result.Version != controlWireVersion {
		return mounterStatusWire{}, fmt.Errorf("unsupported workspace control protocol version")
	}
	return result, nil
}

func decodeControlAck(raw []byte) (controlAckWire, error) {
	var result controlAckWire
	if err := strictDecodeControlJSON(raw, &result); err != nil {
		return result, err
	}
	if result.Version != controlWireVersion {
		return controlAckWire{}, fmt.Errorf("unsupported workspace control protocol version")
	}
	return result, nil
}

func decodeShutdownAck(raw []byte) (shutdownAckWire, error) {
	var result shutdownAckWire
	if err := strictDecodeControlJSON(raw, &result); err != nil {
		return result, err
	}
	if result.Version != controlWireVersion {
		return shutdownAckWire{}, fmt.Errorf("unsupported workspace control protocol version")
	}
	return result, nil
}

func decodeProbeStatus(raw []byte) (probeStatusWire, error) {
	var result probeStatusWire
	if err := strictDecodeControlJSON(raw, &result); err != nil {
		return result, err
	}
	if result.Version != controlWireVersion {
		return probeStatusWire{}, fmt.Errorf("unsupported workspace control protocol version")
	}
	return result, nil
}

func strictDecodeControlJSON(raw []byte, out any) error {
	return fuseprotocol.DecodeExact(raw, out)
}
