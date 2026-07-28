//go:build linux

package seccheck

import "gvisor.dev/gvisor/pkg/sentry/seccheck/sinks/remote/wire"

// The wire constants in seccheck.go are duplicated rather than imported,
// because gVisor's wire package transitively pulls in pkg/hostarch, which
// panics on macOS (16K pages). These compile-time assertions run on Linux —
// where importing wire is harmless — so a protocol change upstream breaks the
// build instead of silently corrupting every decoded frame.
const (
	_ = uint(headerStructSize - wire.HeaderStructSize)
	_ = uint(wire.HeaderStructSize - headerStructSize)
	_ = uint(protocolVersion - wire.CurrentVersion)
	_ = uint(wire.CurrentVersion - protocolVersion)
)
