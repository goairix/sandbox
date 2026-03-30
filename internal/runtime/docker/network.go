package docker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dnetwork "github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

const (
	isolatedNetworkName = "sandbox-isolated"
	openNetworkName     = "sandbox-open"
	pairNetworkPrefix   = "sandbox-pair-"
	gatewayNamePrefix   = "sandbox-gw-"

	defaultGatewayImage = "sandbox-gateway:latest"
)

// ensureNetworks creates the two static sandbox networks if they don't exist:
//   - sandbox-isolated (internal=true): no external access
//   - sandbox-open (internal=false): full external access
func ensureNetworks(ctx context.Context, cli *dockerclient.Client) (isolatedID, openID string, err error) {
	isolatedID, err = ensureOneNetwork(ctx, cli, isolatedNetworkName, true)
	if err != nil {
		return "", "", err
	}
	openID, err = ensureOneNetwork(ctx, cli, openNetworkName, false)
	if err != nil {
		return "", "", err
	}
	return isolatedID, openID, nil
}

func ensureOneNetwork(ctx context.Context, cli *dockerclient.Client, name string, internal bool) (string, error) {
	networks, err := cli.NetworkList(ctx, dnetwork.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return n.ID, nil
		}
	}

	resp, err := cli.NetworkCreate(ctx, name, dnetwork.CreateOptions{
		Driver:     "bridge",
		Internal:   internal,
		Attachable: true,
	})
	if err != nil {
		// Handle race: another instance might have created it
		if strings.Contains(err.Error(), "already exists") {
			networks2, err2 := cli.NetworkList(ctx, dnetwork.ListOptions{})
			if err2 != nil {
				return "", fmt.Errorf("list networks after conflict: %w", err2)
			}
			for _, n := range networks2 {
				if n.Name == name {
					return n.ID, nil
				}
			}
		}
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

// createSandboxPair creates a per-sandbox network pair:
//  1. An internal bridge network (sandbox-pair-<id>) where sandbox and gateway coexist
//  2. A gateway container connected to both the pair network and sandbox-open
//
// The gateway container performs NAT + whitelist filtering via iptables.
// Returns the pair network ID, gateway container ID, and gateway IP on the pair network.
func createSandboxPair(ctx context.Context, cli *dockerclient.Client, sandboxID, openNetworkID, gatewayImage string, whitelist []string) (pairNetworkID, gatewayID, gatewayIP string, err error) {
	// Resolve whitelist entries: IPs/CIDRs pass through, domain names are resolved to IPs
	resolved, err := resolveWhitelist(whitelist)
	if err != nil {
		return "", "", "", err
	}

	pairNetName := pairNetworkPrefix + sandboxID

	// 1. Create the pair network (internal)
	netResp, err := cli.NetworkCreate(ctx, pairNetName, dnetwork.CreateOptions{
		Driver:     "bridge",
		Internal:   true,
		Attachable: true,
		Labels: map[string]string{
			"sandbox.managed": "true",
			"sandbox.id":     sandboxID,
		},
	})
	if err != nil {
		return "", "", "", fmt.Errorf("create pair network: %w", err)
	}
	pairNetworkID = netResp.ID

	// Cleanup helper for rollback
	cleanup := func() {
		if gatewayID != "" {
			_ = cli.ContainerRemove(ctx, gatewayID, container.RemoveOptions{Force: true})
		}
		_ = cli.NetworkRemove(ctx, pairNetworkID)
	}

	// 2. Create gateway container on the pair network
	if gatewayImage == "" {
		gatewayImage = defaultGatewayImage
	}
	gwName := gatewayNamePrefix + sandboxID

	gwResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image: gatewayImage,
		Cmd:   []string{"sleep", "infinity"},
		Labels: map[string]string{
			"sandbox.managed": "true",
			"sandbox.id":     sandboxID,
			"sandbox.role":   "gateway",
		},
	}, &container.HostConfig{
		CapDrop: []string{"ALL"},
		CapAdd:  []string{"NET_ADMIN", "NET_RAW"},
		Sysctls: map[string]string{
			"net.ipv4.ip_forward": "1",
		},
		Resources: container.Resources{
			Memory:    32 * 1024 * 1024, // 32Mi
			PidsLimit: int64Ptr(20),
		},
	}, &dnetwork.NetworkingConfig{
		EndpointsConfig: map[string]*dnetwork.EndpointSettings{
			pairNetName: {},
		},
	}, nil, gwName)
	if err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("create gateway container: %w", err)
	}
	gatewayID = gwResp.ID

	// 3. Start gateway
	if err := cli.ContainerStart(ctx, gatewayID, container.StartOptions{}); err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("start gateway: %w", err)
	}

	// 4. Connect gateway to the open network (this becomes the second interface with default route)
	if err := cli.NetworkConnect(ctx, openNetworkID, gatewayID, nil); err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("connect gateway to open network: %w", err)
	}

	// 5. Configure NAT + whitelist rules inside the gateway
	iptablesCmd := buildGatewayIptablesCmd(resolved)
	execCfg := container.ExecOptions{
		Cmd:  []string{"sh", "-c", iptablesCmd},
		User: "root",
	}
	execResp, err := cli.ContainerExecCreate(ctx, gatewayID, execCfg)
	if err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("create gateway exec: %w", err)
	}
	if err := cli.ContainerExecStart(ctx, gatewayID, container.ExecStartOptions{}); err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("start gateway exec: %w", err)
	}
	// Wait for exec to complete
	if err := waitExecDone(ctx, cli, execResp.ID); err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("gateway iptables setup failed: %w", err)
	}

	// 6. Get gateway IP on the pair network
	gatewayIP, err = getContainerIP(ctx, cli, gatewayID, pairNetName)
	if err != nil {
		cleanup()
		return "", "", "", fmt.Errorf("get gateway IP: %w", err)
	}

	return pairNetworkID, gatewayID, gatewayIP, nil
}

// buildGatewayIptablesCmd builds the shell command to configure NAT and whitelist
// rules inside the gateway container.
func buildGatewayIptablesCmd(whitelist []string) string {
	var parts []string

	// Find the outbound interface (the one with the default route, from sandbox-open)
	parts = append(parts, `OUT_IF=$(ip route | grep default | awk '{print $5}')`)
	// NAT outbound traffic through the open network interface
	parts = append(parts, "iptables -t nat -A POSTROUTING -o $OUT_IF -j MASQUERADE")

	if len(whitelist) > 0 {
		// Whitelist mode: default deny, allow only specified destinations
		parts = append(parts, "iptables -P FORWARD DROP")
		parts = append(parts, "iptables -A FORWARD -m state --state ESTABLISHED,RELATED -j ACCEPT")
		parts = append(parts, "iptables -A FORWARD -p udp --dport 53 -j ACCEPT")
		parts = append(parts, "iptables -A FORWARD -p tcp --dport 53 -j ACCEPT")
		for _, dest := range whitelist {
			parts = append(parts, fmt.Sprintf("iptables -A FORWARD -d %s -j ACCEPT", dest))
		}
	} else {
		// Full access mode: allow all forwarding
		parts = append(parts, "iptables -P FORWARD ACCEPT")
	}

	return strings.Join(parts, " && ")
}

// removeSandboxPair removes the gateway container and pair network for a sandbox.
func removeSandboxPair(ctx context.Context, cli *dockerclient.Client, sandboxID string) error {
	// Find and remove gateway container by label
	gwContainers, err := cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "sandbox.id="+sandboxID),
			filters.Arg("label", "sandbox.role=gateway"),
		),
	})
	if err == nil {
		for _, gw := range gwContainers {
			_ = cli.ContainerRemove(ctx, gw.ID, container.RemoveOptions{Force: true})
		}
	}

	// Remove pair network
	pairNetName := pairNetworkPrefix + sandboxID
	err = cli.NetworkRemove(ctx, pairNetName)
	if err != nil && !dockerclient.IsErrNotFound(err) {
		return fmt.Errorf("remove pair network: %w", err)
	}
	return nil
}

// setupSandboxRoute configures the default route inside the sandbox container
// to point to the gateway IP. Executed as root via Docker exec.
func setupSandboxRoute(ctx context.Context, cli *dockerclient.Client, containerID, gatewayIP string) error {
	execCfg := container.ExecOptions{
		Cmd:  []string{"ip", "route", "replace", "default", "via", gatewayIP},
		User: "root",
	}
	execResp, err := cli.ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		return fmt.Errorf("create route exec: %w", err)
	}
	if err := cli.ContainerExecStart(ctx, containerID, container.ExecStartOptions{}); err != nil {
		return fmt.Errorf("start route exec: %w", err)
	}
	return waitExecDone(ctx, cli, execResp.ID)
}

// getContainerIP returns the container's IP address on the given network.
func getContainerIP(ctx context.Context, cli *dockerclient.Client, containerID, networkName string) (string, error) {
	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
		return "", fmt.Errorf("container has no network settings")
	}
	netInfo, ok := info.NetworkSettings.Networks[networkName]
	if !ok {
		return "", fmt.Errorf("container not connected to network %s", networkName)
	}
	if netInfo.IPAddress == "" {
		return "", fmt.Errorf("container has no IP on network %s", networkName)
	}
	return netInfo.IPAddress, nil
}

// waitExecDone polls until a Docker exec completes and returns an error if
// the exit code is non-zero. Enforces a 30-second hard timeout to prevent
// indefinite blocking if the caller's context has no deadline.
func waitExecDone(ctx context.Context, cli *dockerclient.Client, execID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		inspect, err := cli.ContainerExecInspect(ctx, execID)
		if err != nil {
			return fmt.Errorf("inspect exec: %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("exec failed with exit code %d", inspect.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveWhitelist processes whitelist entries: IPs and CIDRs pass through unchanged,
// domain names are resolved to their IP addresses.
func resolveWhitelist(entries []string) ([]string, error) {
	var resolved []string
	for _, entry := range entries {
		if net.ParseIP(entry) != nil {
			resolved = append(resolved, entry)
			continue
		}
		if _, _, err := net.ParseCIDR(entry); err == nil {
			resolved = append(resolved, entry)
			continue
		}
		// Treat as domain name, resolve to IPs
		ips, err := net.LookupIP(entry)
		if err != nil {
			return nil, fmt.Errorf("resolve whitelist domain %q: %w", entry, err)
		}
		for _, ip := range ips {
			if ip.To4() != nil {
				resolved = append(resolved, ip.String())
			}
		}
	}
	return resolved, nil
}

// cleanupOrphanedResources cleans up leftover gateway containers, pair networks,
// and old networks from previous runs (crash recovery).
func cleanupOrphanedResources(ctx context.Context, cli *dockerclient.Client) error {
	// Clean up orphaned gateway containers
	gwContainers, err := cli.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", "sandbox.role=gateway"),
			filters.Arg("label", "sandbox.managed=true"),
		),
	})
	if err == nil {
		for _, gw := range gwContainers {
			_ = cli.ContainerRemove(ctx, gw.ID, container.RemoveOptions{Force: true})
		}
	}

	// Clean up orphaned pair networks and old networks
	networks, err := cli.NetworkList(ctx, dnetwork.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range networks {
		shouldClean := strings.HasPrefix(n.Name, pairNetworkPrefix) ||
			strings.HasPrefix(n.Name, "sandbox-net-") || // old per-sandbox networks
			n.Name == "sandbox-network" // pre-refactoring network

		if shouldClean {
			inspect, inspectErr := cli.NetworkInspect(ctx, n.ID, dnetwork.InspectOptions{})
			if inspectErr != nil {
				continue
			}
			if len(inspect.Containers) == 0 {
				_ = cli.NetworkRemove(ctx, n.ID)
			}
		}
	}

	return nil
}
