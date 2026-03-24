//go:build linux

// Package seccomp provides seccomp-notify based syscall interception.
// Pure Go implementation — no cgo or libseccomp dependency.
package seccomp

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp() syscall constants not yet in x/sys/unix.
const (
	SECCOMP_SET_MODE_FILTER = 1

	SECCOMP_FILTER_FLAG_NEW_LISTENER        = 1 << 3
	SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV  = 1 << 5

	SECCOMP_RET_ALLOW      = 0x7fff0000
	SECCOMP_RET_USER_NOTIF = 0x7fc00000
)

// BPF instruction constants.
const (
	bpfLD  = 0x00
	bpfJMP = 0x05
	bpfRET = 0x06
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJEQ = 0x10
	bpfK   = 0x00
)

// seccomp_data offsets.
const (
	offsetNr   = 0  // offsetof(struct seccomp_data, nr)
	offsetArch = 4  // offsetof(struct seccomp_data, arch)
)

// SockFprog matches struct sock_fprog for the seccomp() syscall.
// On 64-bit platforms, the pointer field is 8-byte aligned, requiring
// 6 bytes of padding after the uint16 Len field.
type SockFprog struct {
	Len    uint16
	_      [6]byte // padding for 8-byte pointer alignment
	Filter *unix.SockFilter
}

// InstallFilter builds and installs a BPF filter that sends notifications
// for the specified syscall numbers. Returns the listener fd.
//
// allowFdForWrite is a file descriptor that write() calls should be ALLOWED
// to (not intercepted). This is needed for the shim pipe — the child must
// write the listener fd number to the parent before exec. Set to -1 to disable.
//
// Must be called from the process that should be filtered (the child).
// The filter survives exec().
func InstallFilter(syscallNrs []uint32, allowFdForWrite int) (int, error) {
	if len(syscallNrs) == 0 {
		return -1, fmt.Errorf("no syscalls to filter")
	}

	prog := buildFilter(syscallNrs, allowFdForWrite)

	fprog := SockFprog{
		Len:    uint16(len(prog)),
		Filter: &prog[0],
	}

	flags := uintptr(SECCOMP_FILTER_FLAG_NEW_LISTENER)

	// Try with WAIT_KILLABLE_RECV (kernel 5.19+) — avoids SIGURG spin loop
	// with Go targets. Fall back without it if unsupported.
	flagsWithKillable := flags | SECCOMP_FILTER_FLAG_WAIT_KILLABLE_RECV
	fd, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		SECCOMP_SET_MODE_FILTER,
		flagsWithKillable,
		uintptr(unsafe.Pointer(&fprog)),
	)
	if errno == 0 {
		return int(fd), nil
	}
	// Fall back without WAIT_KILLABLE_RECV
	_ = flagsWithKillable

	fd, _, errno = unix.Syscall(
		unix.SYS_SECCOMP,
		SECCOMP_SET_MODE_FILTER,
		flags,
		uintptr(unsafe.Pointer(&fprog)),
	)
	if errno != 0 {
		return -1, fmt.Errorf("seccomp(SET_MODE_FILTER): %w", errno)
	}

	return int(fd), nil
}

// seccomp_data field offsets for args.
const (
	offsetArgs0 = 16 // offsetof(struct seccomp_data, args[0])
)

// buildFilter creates a BPF program:
//
//	load arch → check arch → skip if wrong
//	load syscall nr → for each target: if match → check exceptions → RET USER_NOTIF
//	default → RET ALLOW
//
// If allowFdForWrite >= 0 and "write" (or equivalent) is in syscallNrs,
// the filter allows write() calls where arg0 == allowFdForWrite. This
// prevents deadlock when the shim child needs to write to the parent pipe.
func buildFilter(syscallNrs []uint32, allowFdForWrite int) []unix.SockFilter {
	arch := nativeArch()
	insns := make([]unix.SockFilter, 0, 32)

	// Load architecture
	insns = append(insns, unix.SockFilter{
		Code: bpfLD | bpfW | bpfABS,
		K:    offsetArch,
	})

	// We'll patch the arch-fail jump target after building the rest.
	archCheckIdx := len(insns)
	insns = append(insns, unix.SockFilter{
		Code: bpfJMP | bpfJEQ | bpfK,
		Jt:   0, // match: continue
		Jf:   0, // patched later
		K:    arch,
	})

	// Load syscall number
	insns = append(insns, unix.SockFilter{
		Code: bpfLD | bpfW | bpfABS,
		K:    offsetNr,
	})

	// Determine which syscall nrs are "write-like" (need the fd exception).
	writeLike := make(map[uint32]bool)
	if allowFdForWrite >= 0 {
		// write, writev, pwrite64 — any syscall where arg0 is the fd and
		// we don't want to intercept writes to the pipe.
		for _, nr := range syscallNrs {
			name := SyscallName(int32(nr))
			if name == "write" || name == "writev" || name == "pwrite64" {
				writeLike[nr] = true
			}
		}
	}

	// For each target syscall
	for _, nr := range syscallNrs {
		if writeLike[nr] {
			// For write-like syscalls: check if arg0 == allowFd → ALLOW, else NOTIFY.
			// JEQ nr → check arg0, else skip 5 instructions
			// (load arg0 + jeq fd + ret notif + ret allow + reload nr = 5)
			insns = append(insns,
				unix.SockFilter{
					Code: bpfJMP | bpfJEQ | bpfK,
					Jt:   0, // match: fall through
					Jf:   5, // no match: skip this block
					K:    nr,
				},
				// Load arg0 (fd)
				unix.SockFilter{
					Code: bpfLD | bpfW | bpfABS,
					K:    offsetArgs0,
				},
				// If arg0 == allowFd → ALLOW (skip 1 to RET ALLOW)
				unix.SockFilter{
					Code: bpfJMP | bpfJEQ | bpfK,
					Jt:   1, // match: skip NOTIF, go to ALLOW
					Jf:   0, // no match: fall through to NOTIF
					K:    uint32(allowFdForWrite),
				},
				// RET USER_NOTIF
				unix.SockFilter{
					Code: bpfRET,
					K:    SECCOMP_RET_USER_NOTIF,
				},
				// RET ALLOW (for the exception fd)
				unix.SockFilter{
					Code: bpfRET,
					K:    SECCOMP_RET_ALLOW,
				},
			)
			// Reload syscall nr for next check (we clobbered A with arg0)
			insns = append(insns, unix.SockFilter{
				Code: bpfLD | bpfW | bpfABS,
				K:    offsetNr,
			})
		} else {
			// Simple case: JEQ nr → NOTIF, else skip
			insns = append(insns,
				unix.SockFilter{
					Code: bpfJMP | bpfJEQ | bpfK,
					Jt:   0, // match: fall through to NOTIF
					Jf:   1, // no match: skip NOTIF
					K:    nr,
				},
				unix.SockFilter{
					Code: bpfRET,
					K:    SECCOMP_RET_USER_NOTIF,
				},
			)
		}
	}

	// Default: ALLOW
	insns = append(insns, unix.SockFilter{
		Code: bpfRET,
		K:    SECCOMP_RET_ALLOW,
	})

	// Patch the arch-check jump: if wrong arch, skip to the final ALLOW.
	// Jf = number of instructions to skip from the NEXT instruction to reach
	// the final ALLOW. From archCheckIdx, next is archCheckIdx+1, and we want
	// to land at len(insns)-1 (the final RET ALLOW).
	insns[archCheckIdx].Jf = uint8(len(insns) - archCheckIdx - 2)

	return insns
}

// nativeArch returns the AUDIT_ARCH constant for the current platform.
// Implemented per-architecture in arch_*.go files.

// Notification handling types and functions.

// NotifReq represents a seccomp notification (struct seccomp_notif).
type NotifReq struct {
	ID    uint64
	PID   uint32
	Flags uint32
	Data  NotifData
}

// NotifData contains the syscall details (struct seccomp_data).
type NotifData struct {
	Nr                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

// NotifResp is the response to a notification (struct seccomp_notif_resp).
type NotifResp struct {
	ID    uint64
	Val   int64
	Error int32
	Flags uint32
}

// SECCOMP_USER_NOTIF_FLAG_CONTINUE tells the kernel to execute the syscall normally.
const SECCOMP_USER_NOTIF_FLAG_CONTINUE = 0x00000001

// ioctl request numbers.
var (
	ioctlNotifRecv    uintptr
	ioctlNotifSend    uintptr
	ioctlNotifIDValid uintptr
)

func init() {
	// These are architecture-dependent due to struct sizes.
	// Calculated as _IOWR('!', 0, struct seccomp_notif) etc.
	// Values for 64-bit Linux:
	ioctlNotifRecv = 0xc0502100
	ioctlNotifSend = 0xc0182101
	ioctlNotifIDValid = 0x40082102
}

// Receive blocks until a syscall notification arrives on the listener fd.
// Retries on EINTR (caused by Go runtime's SIGURG for goroutine preemption).
func Receive(listenerFd int) (*NotifReq, error) {
	req := &NotifReq{}
	for {
		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(listenerFd),
			ioctlNotifRecv,
			uintptr(unsafe.Pointer(req)),
		)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return nil, fmt.Errorf("ioctl(SECCOMP_IOCTL_NOTIF_RECV): %w", errno)
		}
		return req, nil
	}
}

// Poll checks if the listener fd has a pending notification.
// Returns true if ready, false if timeout. Returns error on POLLHUP/POLLERR
// (fd closed, process exited).
func Poll(listenerFd int, timeoutMs int) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(listenerFd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeoutMs)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if n > 0 && (fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL)) != 0 {
			return false, fmt.Errorf("listener fd closed (revents=0x%x)", fds[0].Revents)
		}
		return n > 0, nil
	}
}

// Respond sends a response to a notification.
func Respond(listenerFd int, resp *NotifResp) error {
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(listenerFd),
		ioctlNotifSend,
		uintptr(unsafe.Pointer(resp)),
	)
	if errno != 0 {
		return fmt.Errorf("ioctl(SECCOMP_IOCTL_NOTIF_SEND): %w", errno)
	}
	return nil
}

// Allow tells the kernel to execute the syscall normally.
func Allow(listenerFd int, id uint64) error {
	return Respond(listenerFd, &NotifResp{
		ID:    id,
		Flags: SECCOMP_USER_NOTIF_FLAG_CONTINUE,
	})
}

// Deny tells the kernel to fail the syscall with the given errno.
func Deny(listenerFd int, id uint64, errno int32) error {
	return Respond(listenerFd, &NotifResp{
		ID:    id,
		Error: -errno, // kernel expects negative errno
	})
}

// SyscallName returns a human-readable name for common syscall numbers.
func SyscallName(nr int32) string {
	if name, ok := syscallNames[nr]; ok {
		return name
	}
	return fmt.Sprintf("syscall_%d", nr)
}

// SyscallNumber returns the syscall number for a name, or -1 if unknown.
func SyscallNumber(name string) int32 {
	for nr, n := range syscallNames {
		if n == name {
			return nr
		}
	}
	return -1
}

// ReadStringFromProcess reads a null-terminated string from another process's memory
// via /proc/pid/mem. Used to inspect path arguments from syscalls like open/openat.
// Note: subject to TOCTOU — the target could modify this memory.
func ReadStringFromProcess(pid uint32, addr uint64, maxLen int) (string, error) {
	if addr == 0 {
		return "", nil
	}

	path := fmt.Sprintf("/proc/%d/mem", pid)
	f, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(f)

	buf := make([]byte, maxLen)
	n, err := unix.Pread(f, buf, int64(addr))
	if err != nil {
		return "", fmt.Errorf("pread %s at 0x%x: %w", path, addr, err)
	}

	// Find null terminator
	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf[:n]), nil
}

