//go:build linux

package engine

import (
	"log/slog"
	"os/exec"
	"syscall"
)

func (s *Session) applyNamespaces(cmd *exec.Cmd) error {
	ns := s.cfg.Namespaces

	if !ns.PID && !ns.Network && !ns.Mount && !ns.User {
		s.log.Info("no namespaces requested, running without isolation")
		return nil
	}

	var flags uintptr
	if ns.PID {
		flags |= syscall.CLONE_NEWPID
		s.log.Info("namespace enabled", slog.String("type", "PID"))
	}
	if ns.Network {
		flags |= syscall.CLONE_NEWNET
		s.log.Info("namespace enabled", slog.String("type", "NET"))
	}
	if ns.Mount {
		flags |= syscall.CLONE_NEWNS
		s.log.Info("namespace enabled", slog.String("type", "MNT"))
	}
	if ns.User {
		flags |= syscall.CLONE_NEWUSER
		s.log.Info("namespace enabled", slog.String("type", "USER"))
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
	}

	// If USER namespace is requested, map current user to root inside.
	if ns.User {
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: syscall.Getuid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: syscall.Getgid(), Size: 1},
		}
		s.log.Debug("user namespace mapping",
			slog.Int("host_uid", syscall.Getuid()),
			slog.Int("host_gid", syscall.Getgid()),
		)
	}

	return nil
}
