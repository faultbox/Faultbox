package container

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker/api/types/network"
)

// EnsureNetwork creates the Faultbox Docker bridge network if it doesn't exist.
// Returns the network ID.
func (c *Client) EnsureNetwork(ctx context.Context) (string, error) {
	defaultNetworkName := c.ownedName("net")
	// Check if this run already created its network.
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == defaultNetworkName && n.Labels["io.faultbox.run"] == c.runID {
			c.log.Debug("network exists", slog.String("name", defaultNetworkName), slog.String("id", n.ID[:12]))
			return n.ID, nil
		}
	}

	// Create the network.
	resp, err := c.cli.NetworkCreate(ctx, defaultNetworkName, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"io.faultbox.run": c.runID},
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
