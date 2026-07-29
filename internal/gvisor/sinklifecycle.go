package gvisor

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/faultbox/Faultbox/internal/gvisor/seccheck"
)

// Per-run sink lifecycle (RFC-056 M2).
//
// The sandbox learns where to report from host configuration written once by
// `faultbox setup-trace`. A run therefore does not choose its socket path — it
// reads the path the host was configured with, and binds that. Choosing its own
// would produce a sink nothing connects to: observation would be silently
// empty, which is the failure this feature exists to rule out.

// SinkEndpoint reports the socket path the installed trace config tells
// sandboxes to report to.
//
// The error distinguishes "no registration" from "registration is broken",
// because the remedies differ and the user should not have to guess which they
// are looking at.
func SinkEndpoint(traceConfigPath string) (string, error) {
	if traceConfigPath == "" {
		traceConfigPath = TraceConfigPath
	}
	raw, err := os.ReadFile(traceConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(
				"filesystem observation is not registered on this host (%s does not exist). "+
					"Run: sudo faultbox setup-trace", traceConfigPath)
		}
		return "", fmt.Errorf("read %s: %w", traceConfigPath, err)
	}
	var cfg podInitConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf(
			"%s is not valid JSON (%w). Re-run: sudo faultbox setup-trace", traceConfigPath, err)
	}
	if len(cfg.TraceSession.Sinks) == 0 {
		return "", fmt.Errorf(
			"%s declares no sink, so sandboxes have nowhere to report. "+
				"Re-run: sudo faultbox setup-trace", traceConfigPath)
	}
	endpoint, _ := cfg.TraceSession.Sinks[0].Config["endpoint"].(string)
	if endpoint == "" {
		return "", fmt.Errorf(
			"%s declares a sink with no endpoint. Re-run: sudo faultbox setup-trace",
			traceConfigPath)
	}
	return endpoint, nil
}

// AcquireSink binds this run's sink at the host-configured endpoint.
//
// Fails rather than falling back when the host is not registered: a run that
// quietly continued without a sink would report `watch()` assertions that
// observed nothing, which is precisely the vacuous green v0.14.0 withdrew the
// feature to avoid.
func AcquireSink(traceConfigPath string, onIO seccheck.OnFileIO, onErr func(error)) (*seccheck.Sink, error) {
	endpoint, err := SinkEndpoint(traceConfigPath)
	if err != nil {
		return nil, err
	}
	sink, err := seccheck.Listen(seccheck.Config{
		Path:     endpoint,
		OnFileIO: onIO,
		OnError:  onErr,
	})
	if err != nil {
		return nil, err
	}
	return sink, nil
}

// SinkReport summarises what a sink observed, for the end-of-test guards.
//
// Points and Dropped are separate because they fail a test for different
// reasons: zero points means the channel never carried anything (a
// misconfigured host, or a service that ran under the wrong runtime), while
// dropped points mean the channel overflowed and the observation is a subset
// of what happened. An audit cannot prove "never" from a subset.
type SinkReport struct {
	Points  int64
	Dropped int64
}

// Report snapshots a sink's counters.
func Report(s *seccheck.Sink) SinkReport {
	if s == nil {
		return SinkReport{}
	}
	return SinkReport{Points: s.Points(), Dropped: s.Dropped()}
}
