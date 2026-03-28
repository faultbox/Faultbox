package container

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker/api/types/network"
)

const defaultNetworkName = "faultbox-net"

// EnsureNetwork creates the Faultbox Docker bridge network if it doesn't exist.
// Returns the network ID.
func (c *Client) EnsureNetwork(ctx context.Context) (string, error) {
	// Check if network already exists.
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == defaultNetworkName {
			c.log.Debug("network exists", slog.String("name", defaultNetworkName), slog.String("id", n.ID[:12]))
			return n.ID, nil
		}
	}

	// Create the network.
	resp, err := c.cli.NetworkCreate(ctx, defaultNetworkName, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", defaultNetworkName, err)
	}
	c.log.Info("network created", slog.String("name", defaultNetworkName), slog.String("id", resp.ID[:12]))
	return resp.ID, nil
}

// RemoveNetwork removes the Faultbox Docker network.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.cli.NetworkRemove(ctx, id)
}
