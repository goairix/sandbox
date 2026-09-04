package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	workspaceMounterContainer = "workspace-mounter"
	sandboxContainer          = "sandbox"
	workspaceProbeBinary      = "/usr/local/bin/workspace-probe"
	maxControlJSONBytes       = 64 << 10
	controlWireVersion        = 1
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
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: strings.NewReader(string(stdin)), Stdout: stdout, Stderr: stderr,
	}); err != nil {
		// stderr can contain supervisor input-derived diagnostics; do not include it.
		return nil, fmt.Errorf("execute Kubernetes control command: %w", err)
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("Kubernetes control output exceeds limit")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
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

type mounterStatusWire struct {
	Version         int    `json:"version"`
	State           string `json:"state"`
	RuntimeUID      string `json:"runtime_uid"`
	PoolKey         string `json:"pool_key"`
	MountType       string `json:"mount_type"`
	Generation      int64  `json:"generation"`
	RestartDetected bool   `json:"restart_detected"`
}

type controlAckWire struct {
	Version    int    `json:"version"`
	Accepted   bool   `json:"accepted"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
}

type authorizeRequestWire struct {
	Version         int    `json:"version"`
	RuntimeUID      string `json:"runtime_uid"`
	PoolKey         string `json:"pool_key"`
	WorkspaceHash   string `json:"workspace_hash"`
	Prefix          string `json:"prefix"`
	LeaseGeneration int64  `json:"lease_generation"`
	MountAttempt    uint8  `json:"mount_attempt"`
}

type controlRequestWire struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
}

type probeResumeRequestWire struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	Token      string `json:"token"`
}

type shutdownAckWire struct {
	Version         int    `json:"version"`
	RuntimeUID      string `json:"runtime_uid"`
	Generation      int64  `json:"generation"`
	GracefulUnmount bool   `json:"graceful_unmount"`
}

type probeStatusWire struct {
	Version    int    `json:"version"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Token      string `json:"token"`
}

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
	if len(argv) == 2 && argv[0] == mounterBinary {
		switch argv[1] {
		case "authorize", "flush", "shutdown":
			return true
		}
	}
	return len(argv) == 3 && argv[0] == mounterBinary && argv[1] == "health" && (argv[2] == "prepared" || argv[2] == "ready")
}

func allowedProbeCommand(argv []string) bool {
	if len(argv) != 6 && len(argv) != 7 {
		return false
	}
	if argv[0] != workspaceProbeBinary || (argv[1] != "write-read-delete" && argv[1] != "quiesce" && argv[1] != "resume") || argv[2] != "--runtime-uid" || !validControlIdentity(argv[3]) || argv[4] != "--generation" {
		return false
	}
	generation, err := strconv.ParseInt(argv[5], 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != argv[5] {
		return false
	}
	if argv[1] == "resume" {
		return len(argv) == 7 && argv[6] == "--token-stdin"
	}
	return len(argv) == 6
}

func validControlIdentity(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char == 0 || unicode.IsControl(char) {
			return false
		}
	}
	return true
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
	if len(raw) == 0 || len(raw) > maxControlJSONBytes {
		return fmt.Errorf("workspace control response has invalid size")
	}
	typ := reflect.TypeOf(out)
	if typ.Kind() != reflect.Pointer || typ.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("workspace control decoder requires a struct pointer")
	}
	expected := make(map[string]struct{}, typ.Elem().NumField())
	for i := 0; i < typ.Elem().NumField(); i++ {
		name := strings.Split(typ.Elem().Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			expected[name] = struct{}{}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("workspace control response must be one JSON object")
	}
	seen := make(map[string]struct{}, len(expected))
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok {
			return fmt.Errorf("workspace control response contains an invalid field")
		}
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("workspace control response contains an unknown field")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("workspace control response contains a duplicate field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("workspace control response contains an invalid value")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("workspace control response contains a null field")
		}
		seen[key] = struct{}{}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return fmt.Errorf("workspace control response is malformed")
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("workspace control response is missing a field")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return fmt.Errorf("workspace control response contains trailing data")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("workspace control response has invalid field types")
	}
	return nil
}
