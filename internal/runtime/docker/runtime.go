package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Docker.
type Runtime struct {
	cli               *dockerclient.Client
	isolatedNetworkID string
	openNetworkID     string
	gatewayImage      string
}

// New creates a new Docker runtime.
func New(ctx context.Context, host, gatewayImage string) (*Runtime, error) {
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

	isolatedID, openID, err := ensureNetworks(ctx, cli)
	if err != nil {
		return nil, err
	}

	// Best-effort cleanup of orphaned resources from previous runs
	_ = cleanupOrphanedResources(ctx, cli)

	if gatewayImage == "" {
		gatewayImage = defaultGatewayImage
	}

	return &Runtime{
		cli:               cli,
		isolatedNetworkID: isolatedID,
		openNetworkID:     openID,
		gatewayImage:      gatewayImage,
	}, nil
}

// Close releases resources held by the Docker runtime.
func (r *Runtime) Close() error {
	return r.cli.Close()
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	var networkID string
	var pairNetworkID, gatewayIP string

	if spec.NetworkEnabled {
		// Create a gateway sidecar pair for network-enabled sandboxes.
		// The sandbox connects to an isolated pair network, and the gateway
		// bridges it to the open network with optional whitelist filtering.
		var gatewayID string
		var err error
		pairNetworkID, gatewayID, gatewayIP, err = createSandboxPair(
			ctx, r.cli, spec.ID, r.openNetworkID, r.gatewayImage, spec.NetworkWhitelist,
		)
		if err != nil {
			return nil, fmt.Errorf("create gateway pair: %w", err)
		}
		_ = gatewayID // tracked via labels, cleaned up in RemoveSandbox
		networkID = pairNetworkID
	} else {
		// No network access — use the shared isolated network
		networkID = r.isolatedNetworkID
	}

	containerID, err := createContainer(ctx, r.cli, spec, networkID)
	if err != nil {
		if pairNetworkID != "" {
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
		}
		return nil, err
	}

	// Start the container
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		if pairNetworkID != "" {
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
		}
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Set the sandbox's default route to the gateway
	if gatewayIP != "" {
		if err := setupSandboxRoute(ctx, r.cli, containerID, gatewayIP); err != nil {
			_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
			_ = removeSandboxPair(ctx, r.cli, spec.ID)
			return nil, fmt.Errorf("setup sandbox route: %w", err)
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
	// Inspect container to find sandbox ID for gateway/network cleanup
	var sandboxID string
	info, err := r.cli.ContainerInspect(ctx, id)
	if err == nil && info.Config != nil && info.Config.Labels != nil {
		sandboxID = info.Config.Labels["sandbox.id"]
	}

	// Remove the sandbox container
	removeErr := r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	if removeErr != nil && dockerclient.IsErrNotFound(removeErr) {
		removeErr = nil
	}

	// Always attempt gateway/network cleanup regardless of container removal result
	if sandboxID != "" {
		_ = removeSandboxPair(ctx, r.cli, sandboxID)
	}

	if removeErr != nil {
		return fmt.Errorf("remove container: %w", removeErr)
	}
	return nil
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

func (r *Runtime) UpdateNetwork(ctx context.Context, containerID string, enabled bool, whitelist []string) error {
	// Get sandbox ID from container — use the container name which equals spec.ID
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	// Docker container names start with "/", strip it
	sandboxID := strings.TrimPrefix(info.Name, "/")

	hasGateway := hasExistingGateway(ctx, r.cli, sandboxID)

	if enabled && !hasGateway {
		// Enable network: create gateway pair and connect sandbox
		pairNetworkID, _, gatewayIP, err := createSandboxPair(
			ctx, r.cli, sandboxID, r.openNetworkID, r.gatewayImage, whitelist,
		)
		if err != nil {
			return fmt.Errorf("create gateway pair: %w", err)
		}

		// Connect sandbox container to the pair network
		if err := r.cli.NetworkConnect(ctx, pairNetworkID, containerID, nil); err != nil {
			_ = removeSandboxPair(ctx, r.cli, sandboxID)
			return fmt.Errorf("connect sandbox to pair network: %w", err)
		}

		// Set default route through gateway
		if err := setupSandboxRoute(ctx, r.cli, containerID, gatewayIP); err != nil {
			_ = removeSandboxPair(ctx, r.cli, sandboxID)
			return fmt.Errorf("setup sandbox route: %w", err)
		}

		return nil
	}

	if enabled && hasGateway {
		// Update whitelist: re-run iptables rules in existing gateway
		resolved, err := resolveWhitelist(whitelist)
		if err != nil {
			return err
		}

		gatewayID, err := findGatewayID(ctx, r.cli, sandboxID)
		if err != nil {
			return err
		}

		// Flush and rebuild iptables rules
		flushCmd := "iptables -F FORWARD && iptables -t nat -F POSTROUTING"
		iptablesCmd := flushCmd + " && " + buildGatewayIptablesCmd(resolved)

		execCfg := container.ExecOptions{
			Cmd:  []string{"sh", "-c", iptablesCmd},
			User: "root",
		}
		execResp, err := r.cli.ContainerExecCreate(ctx, gatewayID, execCfg)
		if err != nil {
			return fmt.Errorf("create iptables exec: %w", err)
		}
		if err := r.cli.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
			return fmt.Errorf("start iptables exec: %w", err)
		}
		return waitExecDone(ctx, r.cli, execResp.ID)
	}

	if !enabled && hasGateway {
		// Disable network: disconnect sandbox from pair network and clean up
		pairNetName := pairNetworkPrefix + sandboxID
		_ = r.cli.NetworkDisconnect(ctx, pairNetName, containerID, true)
		return removeSandboxPair(ctx, r.cli, sandboxID)
	}

	// !enabled && !hasGateway — already disabled, nothing to do
	return nil
}

func (r *Runtime) RenameSandbox(ctx context.Context, id string, newName string) error {
	return r.cli.ContainerRename(ctx, id, newName)
}
