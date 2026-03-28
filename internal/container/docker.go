package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Client wraps the Docker Engine API client.
type Client struct {
	cli *client.Client
	log *slog.Logger
}

// NewClient creates a Docker client from environment variables.
func NewClient(ctx context.Context, log *slog.Logger) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	// Verify connection.
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker ping: %w", err)
	}
	return &Client{cli: cli, log: log}, nil
}

// Close releases the Docker client resources.
func (c *Client) Close() error {
	return c.cli.Close()
}

// PullImage pulls a container image from a registry.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	c.log.Info("pulling image", slog.String("image", ref))
	out, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer out.Close()
	// Consume the pull output (required to complete the pull).
	io.Copy(io.Discard, out)
	return nil
}

// ImageEntrypoint inspects an image and returns its entrypoint and cmd.
func (c *Client) ImageEntrypoint(ctx context.Context, ref string) (entrypoint, cmd []string, err error) {
	inspect, _, err := c.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect image %s: %w", ref, err)
	}
	return inspect.Config.Entrypoint, inspect.Config.Cmd, nil
}

// CreateOpts configures container creation.
type CreateOpts struct {
	Name       string
	Image      string
	Entrypoint []string          // override entrypoint (faultbox-shim)
	Cmd        []string          // original cmd from image
	Env        []string          // KEY=VALUE environment
	Binds      []string          // host:container volume mounts
	Ports      map[int]int       // container_port → host_port (0 = auto)
	NetworkID  string            // Docker network to join
}

// CreateContainer creates a container with the given options.
// Returns the container ID.
func (c *Client) CreateContainer(ctx context.Context, opts CreateOpts) (string, error) {
	// Build port bindings.
	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}
	for containerPort, hostPort := range opts.Ports {
		cp := nat.Port(fmt.Sprintf("%d/tcp", containerPort))
		exposedPorts[cp] = struct{}{}
		binding := nat.PortBinding{HostIP: "0.0.0.0"}
		if hostPort > 0 {
			binding.HostPort = fmt.Sprintf("%d", hostPort)
		}
		// hostPort=0 means Docker picks a random port
		portBindings[cp] = []nat.PortBinding{binding}
	}

	cfg := &container.Config{
		Image:        opts.Image,
		Entrypoint:   opts.Entrypoint,
		Cmd:          opts.Cmd,
		Env:          opts.Env,
		ExposedPorts: exposedPorts,
	}

	hostCfg := &container.HostConfig{
		Binds:        opts.Binds,
		PortBindings: portBindings,
		SecurityOpt:  []string{"seccomp=unconfined"}, // allow our seccomp filter
	}

	netCfg := &network.NetworkingConfig{}
	if opts.NetworkID != "" {
		netCfg.EndpointsConfig = map[string]*network.EndpointSettings{
			"faultbox-net": {NetworkID: opts.NetworkID},
		}
	}

	resp, err := c.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, opts.Name)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", opts.Name, err)
	}

	c.log.Info("container created",
		slog.String("name", opts.Name),
		slog.String("id", resp.ID[:12]),
		slog.String("image", opts.Image),
	)
	return resp.ID, nil
}

// StartContainer starts a created container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.cli.ContainerStart(ctx, id, container.StartOptions{})
}

// StopContainer stops a running container with a timeout.
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	return c.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

// RemoveContainer removes a container (force).
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	return c.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

// ContainerPID returns the host-namespace PID of the container's init process.
func (c *Client) ContainerPID(ctx context.Context, id string) (int, error) {
	inspect, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("inspect container %s: %w", id, err)
	}
	if inspect.State.Pid == 0 {
		return 0, fmt.Errorf("container %s not running", id)
	}
	return inspect.State.Pid, nil
}

// ContainerHostPort returns the host port mapped to a container port.
func (c *Client) ContainerHostPort(ctx context.Context, id string, containerPort int) (int, error) {
	inspect, err := c.cli.ContainerInspect(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("inspect container %s: %w", id, err)
	}
	cp := nat.Port(fmt.Sprintf("%d/tcp", containerPort))
	bindings, ok := inspect.NetworkSettings.Ports[cp]
	if !ok || len(bindings) == 0 {
		return 0, fmt.Errorf("no host port mapping for %d in container %s", containerPort, id)
	}
	var hostPort int
	fmt.Sscanf(bindings[0].HostPort, "%d", &hostPort)
	return hostPort, nil
}

// ShimConfig is the JSON config passed to faultbox-shim via env var.
type ShimConfig struct {
	SyscallNrs []uint32 `json:"syscall_nrs"`
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	ReportPath string   `json:"report_path"`
}

// ShimConfigJSON serializes a ShimConfig to JSON for the env var.
func ShimConfigJSON(cfg ShimConfig) string {
	data, _ := json.Marshal(cfg)
	return string(data)
}
