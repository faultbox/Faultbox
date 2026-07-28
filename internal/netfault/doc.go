// Package netfault implements packet-level fault injection on top of gVisor's
// userspace TCP/IP stack (RFC-054, Path B).
//
// Faultbox mediates at two layers today: individual syscalls (seccomp-notify)
// and parsed L7 protocol messages (internal/proxy). Between them a packet is
// not an object anywhere, so faults like "drop this TCP segment", "delay every
// ACK from the server", or "advertise a zero receive window" are inexpressible.
// This package closes that gap.
//
// The single interception point is FaultEndpoint, a stack.LinkEndpoint
// decorator modeled on gvisor.dev/gvisor/pkg/tcpip/link/sniffer — which
// implements the same shape for observation only. FaultEndpoint is a sniffer
// that is also allowed to say no.
//
// Everything here is independent of how packets reach the stack. Tests run
// over link/channel, which has no OS dependency, so the whole rule engine is
// exercised on any host including macOS. The Linux-only insertion path
// (link/fdbased over a TUN device) lands separately.
package netfault

import (
	// Pinned per RFC-054 decision record M0.1: gVisor head does not build as
	// a Go module dependency (pkg/tcpip/stack carries two different external
	// test package names in one directory). Do not `go get -u` this.
	_ "gvisor.dev/gvisor/pkg/tcpip/stack"
)
