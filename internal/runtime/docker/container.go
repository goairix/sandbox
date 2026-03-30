package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	"github.com/goairix/sandbox/internal/runtime"
)

// imageForSpec returns the Docker image to use for a sandbox spec.
func imageForSpec(spec runtime.SandboxSpec) string {
	if spec.Image != "" {
		return spec.Image
	}
	// Default images based on labels
	lang, ok := spec.Labels["language"]
	if !ok {
		return "sandbox-bash:latest"
	}
	switch lang {
	case "python":
		return "sandbox-python:latest"
	case "nodejs":
		return "sandbox-nodejs:latest"
	default:
		return "sandbox-bash:latest"
	}
}

// createContainerConfig builds Docker container configuration from a SandboxSpec.
func createContainerConfig(spec runtime.SandboxSpec) (*container.Config, *container.HostConfig, error) {
	config := &container.Config{
		Image:      imageForSpec(spec),
		Labels:     spec.Labels,
		WorkingDir: "/workspace",
		Tty:        false,
		// Keep container running with a sleep process
		Cmd: []string{"sleep", "infinity"},
	}

	// Parse memory limit
	var memoryBytes int64
	if spec.Memory != "" {
		var err error
		memoryBytes, err = parseMemory(spec.Memory)
		if err != nil {
			return nil, nil, fmt.Errorf("parse memory: %w", err)
		}
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memoryBytes,
			PidsLimit: int64Ptr(int64(spec.PidLimit)),
		},
		ReadonlyRootfs: spec.ReadOnlyRootFS,
		SecurityOpt:    []string{},
		// Tmpfs for writable directories on read-only root
		Tmpfs: map[string]string{
			"/tmp": "size=50m",
		},
	}

	if spec.SeccompProfile != "" {
		hostConfig.SecurityOpt = append(hostConfig.SecurityOpt,
			fmt.Sprintf("seccomp=%s", spec.SeccompProfile))
	}

	// Drop all capabilities, add only needed ones
	hostConfig.CapDrop = []string{"ALL"}
	hostConfig.CapAdd = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"}

	// Add NET_ADMIN if network whitelist rules need to be applied via iptables
	if spec.NetworkEnabled && len(spec.NetworkWhitelist) > 0 {
		hostConfig.CapAdd = append(hostConfig.CapAdd, "NET_ADMIN")
	}

	// Run as non-root user
	if spec.RunAsUser > 0 {
		config.User = fmt.Sprintf("%d", spec.RunAsUser)
	}

	return config, hostConfig, nil
}

// parseMemory converts "256Mi" to bytes. Returns an error if the input is invalid.
func parseMemory(s string) (int64, error) {
	var value int64
	var unit string
	n, _ := fmt.Sscanf(s, "%d%s", &value, &unit)
	if n == 0 || value <= 0 {
		return 0, fmt.Errorf("invalid memory format: %q", s)
	}
	switch unit {
	case "Ki":
		return value * 1024, nil
	case "Mi":
		return value * 1024 * 1024, nil
	case "Gi":
		return value * 1024 * 1024 * 1024, nil
	case "":
		return value, nil
	default:
		return 0, fmt.Errorf("unknown memory unit: %q in %q", unit, s)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

// createContainer creates a Docker container from spec.
func createContainer(ctx context.Context, cli *dockerclient.Client, spec runtime.SandboxSpec, networkID string) (string, error) {
	config, hostConfig, err := createContainerConfig(spec)
	if err != nil {
		return "", err
	}

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, spec.ID)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	// Connect to sandbox network
	if networkID != "" {
		if err := cli.NetworkConnect(ctx, networkID, resp.ID, nil); err != nil {
			// cleanup on failure
			_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return "", fmt.Errorf("connect network: %w", err)
		}
	}

	return resp.ID, nil
}
