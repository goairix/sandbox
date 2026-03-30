package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

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

	// Apply network whitelist if configured
	if spec.NetworkEnabled && len(spec.NetworkWhitelist) > 0 {
		if err := applyNetworkWhitelist(ctx, r, containerID, spec); err != nil {
			// Non-fatal: log but don't fail sandbox creation
			_ = err
		}
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
