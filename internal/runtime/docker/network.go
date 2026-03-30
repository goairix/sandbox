package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"

	"github.com/goairix/sandbox/internal/runtime"
)

const (
	sandboxNetworkName = "sandbox-network"
)

// ensureNetwork creates the sandbox network if it doesn't exist.
func ensureNetwork(ctx context.Context, cli *dockerclient.Client) (string, error) {
	networks, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}

	for _, n := range networks {
		if n.Name == sandboxNetworkName {
			return n.ID, nil
		}
	}

	resp, err := cli.NetworkCreate(ctx, sandboxNetworkName, network.CreateOptions{
		Driver:     "bridge",
		Internal:   true, // no external access by default
		Attachable: true,
	})
	if err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}
	return resp.ID, nil
}

// applyNetworkWhitelist configures iptables rules inside the container to allow
// outbound traffic only to whitelisted destinations. This is called after
// container creation when NetworkEnabled=true and a whitelist is provided.
func applyNetworkWhitelist(ctx context.Context, r *Runtime, containerID string, spec runtime.SandboxSpec) error {
	if !spec.NetworkEnabled || len(spec.NetworkWhitelist) == 0 {
		return nil
	}

	// Build iptables rules: default deny OUTPUT, allow only whitelisted
	var rules []string
	rules = append(rules, "iptables -P OUTPUT DROP")
	// Allow loopback
	rules = append(rules, "iptables -A OUTPUT -o lo -j ACCEPT")
	// Allow established connections
	rules = append(rules, "iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT")
	// Allow DNS
	rules = append(rules, "iptables -A OUTPUT -p udp --dport 53 -j ACCEPT")
	rules = append(rules, "iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT")

	for _, dest := range spec.NetworkWhitelist {
		rules = append(rules, fmt.Sprintf("iptables -A OUTPUT -d %s -j ACCEPT", dest))
	}

	cmd := strings.Join(rules, " && ")

	// Execute as root inside the container (need NET_ADMIN capability)
	execConfig := container.ExecOptions{
		Cmd:  []string{"sh", "-c", cmd},
		User: "root",
	}

	execResp, err := r.cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("create whitelist exec: %w", err)
	}

	if err := r.cli.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start whitelist exec: %w", err)
	}

	return nil
}
