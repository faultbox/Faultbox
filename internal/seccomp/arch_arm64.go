//go:build linux && arm64

package seccomp

const auditArch = 0xC00000B7 // AUDIT_ARCH_AARCH64

func nativeArch() uint32 { return auditArch }

// ARM64 syscall numbers (from asm-generic/unistd.h).
var syscallNames = map[int32]string{
	56:  "openat",
	57:  "close",
	63:  "read",
	64:  "write",
	67:  "pread64",
	68:  "pwrite64",
	82:  "fsync",
	203: "connect",
	198: "socket",
	200: "bind",
	201: "listen",
	202: "accept",
	204: "getsockname",
	206: "sendto",
	207: "recvfrom",
	222: "mmap",
	226: "mprotect",
	233: "mkdirat",
	35:  "unlinkat",
	48:  "faccessat",
	79:  "fstatat",
	61:  "getdents64",
	78:  "readlinkat",
	160: "uname",
	172: "getpid",
	214: "brk",
	260: "wait4",
	220: "clone",
	221: "execve",
	93:  "exit",
	94:  "exit_group",
	261: "prlimit64",
	29:  "ioctl",
	25:  "fcntl",
	66:  "writev",
	65:  "readv",
	278: "getrandom",
	73:  "ppoll",
	72:  "pselect6",
	134: "sigaction",
	135: "sigprocmask",
	139: "sigreturn",
	101: "nanosleep",
	113: "clock_gettime",
	115: "clock_nanosleep",
	422: "futex",
}
