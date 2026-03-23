//go:build !linux

package engine

import (
	"fmt"
	"os/exec"
	"runtime"
)

func (s *Session) applyNamespaces(cmd *exec.Cmd) error {
	ns := s.cfg.Namespaces

	if !ns.PID && !ns.Network && !ns.Mount && !ns.User {
		s.log.Info("no namespaces requested, running without isolation")
		return nil
	}

	return fmt.Errorf("namespace isolation requires Linux (current OS: %s); use the Lima VM: make env-exec CMD=\"...\"", runtime.GOOS)
}
