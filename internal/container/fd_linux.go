//go:build linux

package container

import (
	"context"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// waitForListenerFd starts a Unix socket listener, waits for the container
// shim to connect and send the seccomp listener fd via SCM_RIGHTS, then
// sends an ACK byte back.
//
// This replaces the old pidfd_getfd approach — no PID tracking, no
// CAP_SYS_PTRACE, works with multi-process entrypoints (Java/shell/fork).
func waitForListenerFd(ctx context.Context, socketPath string) (int, error) {
	// Clean up stale socket from previous run.
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return -1, fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer listener.Close()

	// Allow any user (including container users) to connect.
	// Docker bind-mounts the socket into the container, but the socket is owned
	// by the host user (root), and Unix connect() requires write permission.
	// Without 0777, non-root container users (e.g., UID 1000 in apache/kafka)
	// get EACCES when the shim tries to connect.
	os.Chmod(socketPath, 0777)

	// Accept with context cancellation.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	go func() {
		conn, err := listener.Accept()
		ch <- acceptResult{conn, err}
	}()

	var conn net.Conn
	select {
	case <-ctx.Done():
		listener.Close() // unblock Accept
		return -1, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return -1, fmt.Errorf("accept: %w", result.err)
		}
		conn = result.conn
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("not a unix connection")
	}

	// Receive the fd via SCM_RIGHTS.
	buf := make([]byte, 4)
	oob := make([]byte, unix.CmsgLen(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, fmt.Errorf("read fd: %w", err)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parse control message: %w", err)
	}
	if len(scms) == 0 {
		return -1, fmt.Errorf("no control message received (shim may have failed)")
	}

	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("parse unix rights: %w", err)
	}
	if len(fds) == 0 {
		return -1, fmt.Errorf("no fd received via SCM_RIGHTS")
	}

	// Send ACK byte — shim is waiting for this before exec.
	_, err = uc.Write([]byte("k"))
	if err != nil {
		return -1, fmt.Errorf("send ack: %w", err)
	}

	return fds[0], nil
}
