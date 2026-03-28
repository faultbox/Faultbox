package container

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/faultbox/Faultbox/internal/seccomp"
)

// LaunchConfig describes how to launch a container with seccomp fault injection.
type LaunchConfig struct {
	Name       string            // container name (= service name)
	Image      string            // image reference (e.g., "postgres:16")
	Env        []string          // environment variables
	Ports      map[int]int       // container_port → host_port (0 = auto)
	Volumes    map[string]string // host_path → container_path
	SyscallNrs []uint32          // syscalls to intercept
	ShimPath   string            // host path to faultbox-shim binary
	NetworkID  string            // Docker network ID
}

// LaunchResult contains the result of launching a container.
type LaunchResult struct {
	ContainerID string
	HostPID     int
	ListenerFd  int
	HostPorts   map[int]int // container_port → actual host_port
}

// Launch pulls the image, creates and starts a container with the faultbox-shim
// as entrypoint, waits for the seccomp listener fd, and returns it.
func Launch(ctx context.Context, client *Client, cfg LaunchConfig, log *slog.Logger) (*LaunchResult, error) {
	// Pull the image.
	if err := client.PullImage(ctx, cfg.Image); err != nil {
		return nil, err
	}

	// Inspect image to get original entrypoint/cmd.
	origEntrypoint, origCmd, err := client.ImageEntrypoint(ctx, cfg.Image)
	if err != nil {
		return nil, err
	}
	log.Debug("image inspected",
		slog.String("image", cfg.Image),
		slog.Any("entrypoint", origEntrypoint),
		slog.Any("cmd", origCmd),
	)

	// Create host-side directory for fd reporting.
	socketDir := filepath.Join(os.TempDir(), "faultbox-sockets", cfg.Name)
	os.MkdirAll(socketDir, 0755)
	reportPath := filepath.Join(socketDir, "listener-fd")
	os.Remove(reportPath) // clean up from previous run

	// Build shim config.
	shimCfg := ShimConfig{
		SyscallNrs: cfg.SyscallNrs,
		Entrypoint: origEntrypoint,
		Cmd:        origCmd,
		ReportPath: "/var/run/faultbox/listener-fd",
	}

	// Build binds.
	binds := []string{
		cfg.ShimPath + ":/faultbox-shim:ro",
		socketDir + ":/var/run/faultbox:rw",
	}
	for hostPath, containerPath := range cfg.Volumes {
		binds = append(binds, hostPath+":"+containerPath)
	}

	// Build env: shim config + user env.
	env := append([]string{
		"_FAULTBOX_SHIM_CONFIG=" + ShimConfigJSON(shimCfg),
	}, cfg.Env...)

	// Resolve syscall numbers if not provided.
	if len(cfg.SyscallNrs) == 0 {
		for _, name := range []string{"write", "read", "connect", "openat", "fsync", "sendto", "recvfrom", "writev"} {
			nr := seccomp.SyscallNumber(name)
			if nr >= 0 {
				cfg.SyscallNrs = append(cfg.SyscallNrs, uint32(nr))
			}
		}
		shimCfg.SyscallNrs = cfg.SyscallNrs
		env[0] = "_FAULTBOX_SHIM_CONFIG=" + ShimConfigJSON(shimCfg)
	}

	// Create container.
	containerID, err := client.CreateContainer(ctx, CreateOpts{
		Name:       "faultbox-" + cfg.Name,
		Image:      cfg.Image,
		Entrypoint: []string{"/faultbox-shim"},
		Cmd:        nil,
		Env:        env,
		Binds:      binds,
		Ports:      cfg.Ports,
		NetworkID:  cfg.NetworkID,
	})
	if err != nil {
		return nil, err
	}

	// Start container.
	if err := client.StartContainer(ctx, containerID); err != nil {
		client.RemoveContainer(ctx, containerID)
		return nil, fmt.Errorf("start container %s: %w", cfg.Name, err)
	}

	// Get host PID.
	hostPID, err := client.ContainerPID(ctx, containerID)
	if err != nil {
		client.StopContainer(ctx, containerID, 5)
		client.RemoveContainer(ctx, containerID)
		return nil, err
	}

	log.Info("container started",
		slog.String("name", cfg.Name),
		slog.String("id", containerID[:12]),
		slog.Int("host_pid", hostPID),
	)

	// Wait for the shim to report the listener fd (platform-specific: uses pidfd on Linux).
	listenerFd, err := waitForListenerFd(ctx, reportPath, hostPID)
	if err != nil {
		client.StopContainer(ctx, containerID, 5)
		client.RemoveContainer(ctx, containerID)
		return nil, fmt.Errorf("wait for listener fd: %w", err)
	}

	log.Info("seccomp listener acquired",
		slog.String("name", cfg.Name),
		slog.Int("listener_fd", listenerFd),
	)

	// Resolve actual host ports.
	hostPorts := make(map[int]int)
	for containerPort := range cfg.Ports {
		hp, err := client.ContainerHostPort(ctx, containerID, containerPort)
		if err != nil {
			log.Warn("could not resolve host port", slog.Int("container_port", containerPort), slog.String("error", err.Error()))
			continue
		}
		hostPorts[containerPort] = hp
	}

	return &LaunchResult{
		ContainerID: containerID,
		HostPID:     hostPID,
		ListenerFd:  listenerFd,
		HostPorts:   hostPorts,
	}, nil
}
