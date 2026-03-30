package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
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
