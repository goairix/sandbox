package kubernetes

import (
	"context"
	"errors"
	"io"

	"github.com/goairix/sandbox/internal/runtime"
)

var errNotImplemented = errors.New("kubernetes runtime not yet implemented")

// Runtime implements runtime.Runtime using Kubernetes.
type Runtime struct {
	namespace string
}

// New creates a new Kubernetes runtime.
func New(kubeconfig string, namespace string) (*Runtime, error) {
	return &Runtime{namespace: namespace}, nil
}

func (r *Runtime) CreateSandbox(_ context.Context, _ runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	return nil, errNotImplemented
}

func (r *Runtime) StartSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) StopSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) RemoveSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) GetSandbox(_ context.Context, _ string) (*runtime.SandboxInfo, error) {
	return nil, errNotImplemented
}

func (r *Runtime) Exec(_ context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
	return nil, errNotImplemented
}

func (r *Runtime) ExecStream(_ context.Context, _ string, _ runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return nil, errNotImplemented
}

func (r *Runtime) UploadFile(_ context.Context, _ string, _ string, _ io.Reader) error {
	return errNotImplemented
}

func (r *Runtime) DownloadFile(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

func (r *Runtime) ListFiles(_ context.Context, _ string, _ string) ([]runtime.FileInfo, error) {
	return nil, errNotImplemented
}
