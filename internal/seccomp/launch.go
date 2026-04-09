// Package seccomp provides seccomp-notify based syscall interception.
package seccomp

// LaunchConfig describes how to launch the target process.
type LaunchConfig struct {
	// TargetBinary is the path to the executable.
	TargetBinary string
	// TargetArgs are arguments to pass.
	TargetArgs []string
	// TargetEnv is the environment for the target. If nil, inherits parent env.
	TargetEnv []string
	// SyscallNrs to intercept via seccomp (empty = no filter).
	SyscallNrs []uint32
	// Cloneflags for namespace creation (e.g., CLONE_NEWPID | CLONE_NEWNET).
	Cloneflags uintptr
	// UidMappings for USER namespace (Linux-specific, ignored on other platforms).
	UidMappings []IDMapping
	// GidMappings for USER namespace (Linux-specific, ignored on other platforms).
	GidMappings []IDMapping
	// StdoutFd overrides the child's stdout. 0 = inherit parent's stdout.
	StdoutFd uintptr
	// StderrFd overrides the child's stderr. 0 = inherit parent's stderr.
	StderrFd uintptr
}

// IDMapping is a portable uid/gid mapping for user namespaces.
type IDMapping struct {
	ContainerID int
	HostID      int
	Size        int
}
