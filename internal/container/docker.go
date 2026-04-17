package container

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types"
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
// If the image already exists locally, the pull is skipped to avoid
// unnecessary network round-trips (useful in offline/CI environments).
func (c *Client) PullImage(ctx context.Context, ref string) error {
	// Check if image already exists locally.
	if _, _, err := c.cli.ImageInspectWithRaw(ctx, ref); err == nil {
		c.log.Info("image already present, skipping pull", slog.String("image", ref))
		return nil
	}

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

	// Use the service name (without "faultbox-" prefix) as hostname for DNS resolution.
	hostname := opts.Name
	if len(hostname) > 10 && hostname[:9] == "faultbox-" {
		hostname = hostname[9:]
	}

	cfg := &container.Config{
		Image:        opts.Image,
		Hostname:     hostname,
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
			"faultbox-net": {
				NetworkID: opts.NetworkID,
				Aliases:   []string{hostname}, // DNS alias = service name
			},
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

// CleanupStale removes all containers and networks with the "faultbox-" prefix.
// Called at suite start to clean up from previous failed/interrupted runs.
func (c *Client) CleanupStale(ctx context.Context) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		c.log.Debug("cleanup: list containers failed", slog.String("error", err.Error()))
		return
	}
	for _, ctr := range containers {
		for _, name := range ctr.Names {
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
			if len(name) > 9 && name[:9] == "faultbox-" {
				c.log.Debug("cleanup: removing stale container", slog.String("name", name))
				c.cli.ContainerStop(ctx, ctr.ID, container.StopOptions{})
				c.cli.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true})
				break
			}
		}
	}

	// Clean up stale networks.
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return
	}
	for _, net := range networks {
		if len(net.Name) > 9 && net.Name[:9] == "faultbox-" {
			c.log.Debug("cleanup: removing stale network", slog.String("name", net.Name))
			c.cli.NetworkRemove(ctx, net.ID)
		}
	}
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

// BuildImage builds a Docker image from a Dockerfile in the given context directory.
// The image is tagged with the given tag.
func (c *Client) BuildImage(ctx context.Context, contextDir, tag string) error {
	c.log.Info("building image", slog.String("context", contextDir), slog.String("tag", tag))

	// Create a tar archive of the build context directory.
	buildCtx, err := createTarContext(contextDir)
	if err != nil {
		return fmt.Errorf("build context for %s: %w", tag, err)
	}
	defer buildCtx.Close()

	resp, err := c.cli.ImageBuild(ctx, buildCtx, types.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("build image %s: %w", tag, err)
	}
	defer resp.Body.Close()
	// Consume build output (required to complete the build).
	io.Copy(io.Discard, resp.Body)
	return nil
}

// createTarContext creates a tar archive from a directory for Docker build context.
func createTarContext(dir string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	tw := tar.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer tw.Close()

		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// Get relative path for the tar header.
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}

			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = rel

			if err := tw.WriteHeader(header); err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil {
			pw.CloseWithError(err)
		}
	}()

	return pr, nil
}

// ShimConfig is the JSON config passed to faultbox-shim via env var.
type ShimConfig struct {
	SyscallNrs []uint32 `json:"syscall_nrs"`
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	SocketPath string   `json:"socket_path,omitempty"`  // Unix socket for fd passing (preferred)
	ReportPath string   `json:"report_path,omitempty"`  // legacy file-based reporting
	AckPath    string   `json:"ack_path,omitempty"`     // legacy ACK file
}

// ShimConfigJSON serializes a ShimConfig to JSON for the env var.
func ShimConfigJSON(cfg ShimConfig) string {
	data, _ := json.Marshal(cfg)
	return string(data)
}
