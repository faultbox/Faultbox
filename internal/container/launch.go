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
	SkipPull    bool              // skip image pull (for locally built images)
	PullTimeout time.Duration     // timeout for image pull (default 120s)
}

// LaunchResult contains the result of launching a container.
type LaunchResult struct {
	ContainerID string
	HostPID     int             // container init PID (for process exit detection)
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
		pullTimeout := cfg.PullTimeout
		if pullTimeout == 0 {
			pullTimeout = 120 * time.Second
		}
		pullCtx, pullCancel := context.WithTimeout(ctx, pullTimeout)
		defer pullCancel()
		if err := client.PullImage(pullCtx, cfg.Image); err != nil {
			return nil, fmt.Errorf("pull image %s (timeout %s): %w", cfg.Image, pullTimeout, err)
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

	// Create host-side directory for Unix socket fd passing.
	socketDir := filepath.Join(os.TempDir(), "faultbox-sockets", cfg.Name)
	os.MkdirAll(socketDir, 0755)
	socketPath := filepath.Join(socketDir, "fd.sock")
	os.Remove(socketPath) // clean up from previous run

	// Build shim config — use SocketPath for Unix socket fd passing.
	shimCfg := ShimConfig{
		SyscallNrs: cfg.SyscallNrs,
		Entrypoint: origEntrypoint,
		Cmd:        origCmd,
		SocketPath: "/var/run/faultbox/fd.sock",
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

	// Start the Unix socket listener BEFORE starting the container.
	// The socket file must exist in the bind-mounted directory when the
	// shim tries to connect.
	type fdResult struct {
		fd  int
		err error
	}
	fdCh := make(chan fdResult, 1)
	go func() {
		fd, err := waitForListenerFd(ctx, socketPath)
		fdCh <- fdResult{fd, err}
	}()

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

	log.Info("container started",
		slog.String("name", cfg.Name),
		slog.String("id", containerID[:12]),
	)

	// Wait for the shim to connect and send the listener fd via the socket.
	result := <-fdCh
	listenerFd, err := result.fd, result.err
	if err != nil {
		// Seccomp acquisition failed. Fall back to no-seccomp mode.
		log.Warn("seccomp listener failed — falling back to no-seccomp mode",
			slog.String("name", cfg.Name),
			slog.String("error", err.Error()),
			slog.String("hint", "fault rules on this service will not apply"),
		)

		client.StopContainer(ctx, containerID, 5)
		client.RemoveContainer(ctx, containerID)

		return launchSimple(ctx, client, cfg, log)
	}

	// Get host PID for process exit detection (notification loop needs it).
	// This is safe now — the shim has completed the fd handshake and exec'd
	// the real entrypoint, so the PID is stable.
	hostPID, _ := client.ContainerPID(ctx, containerID)

	log.Info("seccomp listener acquired",
		slog.String("name", cfg.Name),
		slog.Int("listener_fd", listenerFd),
		slog.Int("host_pid", hostPID),
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
