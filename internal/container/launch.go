package container

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

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
	SkipPull   bool              // skip image pull (for locally built images)
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
// Launch pulls the image, creates and starts a container with the faultbox-shim
// as entrypoint, waits for the seccomp listener fd, and returns it.
// If SyscallNrs is empty, launches without the shim (no seccomp interception).
func Launch(ctx context.Context, client *Client, cfg LaunchConfig, log *slog.Logger) (*LaunchResult, error) {
	// Pull the image (skip for locally built images).
	if !cfg.SkipPull {
		if err := client.PullImage(ctx, cfg.Image); err != nil {
			return nil, err
		}
	}

	// No syscalls to intercept — launch without shim for best performance.
	if len(cfg.SyscallNrs) == 0 {
		return launchSimple(ctx, client, cfg, log)
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
	ackPath := filepath.Join(socketDir, "ack")
	os.Remove(reportPath) // clean up from previous run
	os.Remove(ackPath)

	// Build shim config.
	shimCfg := ShimConfig{
		SyscallNrs: cfg.SyscallNrs,
		Entrypoint: origEntrypoint,
		Cmd:        origCmd,
		ReportPath: "/var/run/faultbox/listener-fd",
		AckPath:    "/var/run/faultbox/ack",
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

	// Get host PID (retry briefly — Docker may need a moment to register it).
	var hostPID int
	for attempt := 0; attempt < 10; attempt++ {
		hostPID, err = client.ContainerPID(ctx, containerID)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			client.RemoveContainer(ctx, containerID)
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil {
		// Log container logs for debugging.
		log.Error("container failed to start", slog.String("name", cfg.Name))
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

	// Signal the shim that we've acquired the fd — it can now exec the entrypoint.
	os.WriteFile(ackPath, []byte("ok"), 0644)

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

// launchSimple starts a container without the faultbox-shim (no seccomp filter).
// Used when no fault rules reference any syscalls — pure Docker orchestration.
func launchSimple(ctx context.Context, client *Client, cfg LaunchConfig, log *slog.Logger) (*LaunchResult, error) {
	// Build binds from user volumes only (no shim, no socket dir).
	var binds []string
	for hostPath, containerPath := range cfg.Volumes {
		binds = append(binds, hostPath+":"+containerPath)
	}

	// Build env.
	var env []string
	env = append(env, cfg.Env...)

	containerID, err := client.CreateContainer(ctx, CreateOpts{
		Name:      "faultbox-" + cfg.Name,
		Image:     cfg.Image,
		Env:       env,
		Binds:     binds,
		Ports:     cfg.Ports,
		NetworkID: cfg.NetworkID,
	})
	if err != nil {
		return nil, err
	}

	if err := client.StartContainer(ctx, containerID); err != nil {
		client.RemoveContainer(ctx, containerID)
		return nil, fmt.Errorf("start container %s: %w", cfg.Name, err)
	}

	log.Info("container started (no seccomp)",
		slog.String("name", cfg.Name),
		slog.String("id", containerID[:12]),
	)

	// Resolve host ports.
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
		HostPID:     0,  // no seccomp — no PID tracking needed
		ListenerFd:  -1, // no listener
		HostPorts:   hostPorts,
	}, nil
}
