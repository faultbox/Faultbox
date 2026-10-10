// Package portowner provides best-effort listener diagnostics. It never sends
// signals. Missing permissions or an incomplete scan must be reported as such.
package portowner

import "context"

type Owner struct {
	PID     int    `json:"pid"`
	Command string `json:"command"` // comm only; never argv, environment or credentials
	Start   string `json:"start_time"`
	Service string `json:"service,omitempty"`
	Test    string `json:"test,omitempty"`
}

func Lookup(ctx context.Context, network, host string, port int) ([]Owner, error) {
	return lookup(ctx, network, host, port)
}
func Start(pid int) string { return start(pid) }
