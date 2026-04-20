//go:build linux

package container

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// waitForListenerFd starts a Unix socket listener, waits for the container
// shim to connect and send the seccomp listener fd via SCM_RIGHTS, then
// sends an ACK byte back.
//
// This replaces the old pidfd_getfd approach — no PID tracking, no
// CAP_SYS_PTRACE, works with multi-process entrypoints (Java/shell/fork).
//
// Diagnostics (RFC-022 Phase 0): every phase of the handoff emits a Debug
// log. On hangs (MySQL 8 mysqld_safe wrapper, some JVM images), the last
// phase=<name> step=start without a matching step=done pinpoints where
// the handoff stalled. Match against shim-side "component=faultbox-shim"
// JSON lines captured via `docker logs <container>`.
func waitForListenerFd(ctx context.Context, socketPath string, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "faultbox-host", "socket_path", socketPath)

	// Clean up stale socket from previous run.
	os.Remove(socketPath)

	log.Debug("phase", "phase", "listen", "step", "start")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Debug("phase", "phase", "listen", "step", "error", "error", err.Error())
		return -1, fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer listener.Close()
	log.Debug("phase", "phase", "listen", "step", "done")

	// Allow any user (including container users) to connect.
	// Docker bind-mounts the socket into the container, but the socket is owned
	// by the host user (root), and Unix connect() requires write permission.
	// Without 0777, non-root container users (e.g., UID 1000 in apache/kafka)
	// get EACCES when the shim tries to connect.
	log.Debug("phase", "phase", "chmod", "step", "start")
	if err := os.Chmod(socketPath, 0777); err != nil {
		// Chmod failures don't abort — just log. Some filesystems (FUSE mounts
		// in exotic setups) reject chmod but still accept Unix connects.
		log.Debug("phase", "phase", "chmod", "step", "error", "error", err.Error())
	} else {
		log.Debug("phase", "phase", "chmod", "step", "done")
	}

	// Accept with context cancellation.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	log.Debug("phase", "phase", "accept", "step", "start")
	acceptStart := time.Now()
	go func() {
		conn, err := listener.Accept()
		ch <- acceptResult{conn, err}
	}()

	var conn net.Conn
	select {
	case <-ctx.Done():
		log.Debug("phase", "phase", "accept", "step", "canceled",
			"waited_ms", time.Since(acceptStart).Milliseconds(),
			"reason", ctx.Err().Error())
		listener.Close() // unblock Accept
		return -1, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			log.Debug("phase", "phase", "accept", "step", "error",
				"waited_ms", time.Since(acceptStart).Milliseconds(),
				"error", result.err.Error())
			return -1, fmt.Errorf("accept: %w", result.err)
		}
		conn = result.conn
		log.Debug("phase", "phase", "accept", "step", "done",
			"waited_ms", time.Since(acceptStart).Milliseconds())
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		log.Debug("phase", "phase", "accept", "step", "error", "error", "not a unix connection")
		return -1, fmt.Errorf("not a unix connection")
	}

	// Receive the fd via SCM_RIGHTS.
	log.Debug("phase", "phase", "read_scm", "step", "start")
	buf := make([]byte, 4)
	oob := make([]byte, unix.CmsgLen(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		log.Debug("phase", "phase", "read_scm", "step", "error", "error", err.Error())
		return -1, fmt.Errorf("read fd: %w", err)
	}
	log.Debug("phase", "phase", "read_scm", "step", "done", "oob_bytes", oobn)

	log.Debug("phase", "phase", "parse_scm", "step", "start")
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		log.Debug("phase", "phase", "parse_scm", "step", "error", "error", err.Error())
		return -1, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) == 0 {
		log.Debug("phase", "phase", "parse_scm", "step", "error", "error", "no control message (shim may have failed)")
		return -1, fmt.Errorf("no control message received (shim may have failed)")
	}
	log.Debug("phase", "phase", "parse_scm", "step", "done", "cmsg_count", len(scms))

	log.Debug("phase", "phase", "extract_fd", "step", "start")
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		log.Debug("phase", "phase", "extract_fd", "step", "error", "error", err.Error())
		return -1, fmt.Errorf("parse unix rights: %w", err)
	}
	if len(fds) == 0 {
		log.Debug("phase", "phase", "extract_fd", "step", "error", "error", "no fd via SCM_RIGHTS")
		return -1, fmt.Errorf("no fd received via SCM_RIGHTS")
	}
	log.Debug("phase", "phase", "extract_fd", "step", "done", "fd", fds[0])

	// Send ACK byte — shim is waiting for this before exec.
	log.Debug("phase", "phase", "send_ack", "step", "start")
	_, err = uc.Write([]byte("k"))
	if err != nil {
		log.Debug("phase", "phase", "send_ack", "step", "error", "error", err.Error())
		return -1, fmt.Errorf("send ack: %w", err)
	}
	log.Debug("phase", "phase", "send_ack", "step", "done")

	return fds[0], nil
}
