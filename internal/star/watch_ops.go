package star

import (
	"fmt"
	"sort"
	"strings"

	"github.com/faultbox/Faultbox/internal/gvisor"
)

// Reconciling what a spec asks to observe against what the host will deliver
// (RFC-056 M3).
//
// These are two independent sets and nothing previously compared them:
//
//   - watchableOps — the ops gVisor is capable of tracing, which watch()
//     accepts. Includes read and close.
//   - the installed trace session — the points the HOST is configured to send,
//     fixed at `faultbox setup-trace` time. Excludes read and close by
//     default, because M0c measured them at roughly double the traffic and
//     1,488 dropped points on a read-heavy workload where the default set
//     dropped none.
//
// So `watch(ops=["read"])` on a default install is accepted, installs cleanly,
// receives nothing, and passes. That is the vacuous green v0.14.0 withdrew
// watch() to avoid, reintroduced through a different door — and it is worse
// here, because it looks like a working feature rather than a missing one.
//
// The precedent for the fix is already in the file next door: ops=["fsync"]
// is rejected at spec load naming gVisor's missing trace point, rather than
// accepted and silently emitting nothing. Same treatment, different cause.

// opTracePoints maps a watch() op to the trace points that can produce it.
// An op is deliverable when the installed session requests any of them.
var opTracePoints = map[string][]string{
	"open":    {"syscall/openat/exit"},
	"write":   {"syscall/write/exit", "syscall/pwrite64/exit", "syscall/writev/exit"},
	"read":    {"syscall/read/exit", "syscall/pread64/exit"},
	"close":   {"syscall/close/exit"},
	"connect": {"syscall/connect/exit"},
}

// setupFlagForOp names the flag that would make an op deliverable, so the
// error tells the user what to run rather than what went wrong.
var setupFlagForOp = map[string]string{
	"read":    "--with-read",
	"close":   "--with-close",
	"connect": "--with-connect",
}

// checkOpsDeliverable reports ops the installed trace session cannot produce.
//
// installed is the point list from the host config; an empty list means no
// registration, which is reported by the caller as its own error rather than
// as "every op is undeliverable".
func checkOpsDeliverable(ops []string, installed []string) error {
	if len(installed) == 0 || len(ops) == 0 {
		return nil
	}
	have := make(map[string]bool, len(installed))
	for _, p := range installed {
		have[p] = true
	}

	var missing []string
	for _, op := range ops {
		points, known := opTracePoints[op]
		if !known {
			continue // validated elsewhere (watchableOps / untraceableOps)
		}
		deliverable := false
		for _, p := range points {
			if have[p] {
				deliverable = true
				break
			}
		}
		if !deliverable {
			missing = append(missing, op)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	var flags []string
	for _, op := range missing {
		if f, ok := setupFlagForOp[op]; ok {
			flags = append(flags, f)
		}
	}
	fix := "sudo faultbox setup-trace"
	if len(flags) > 0 {
		fix += " " + strings.Join(dedupe(flags), " ")
	}

	return fmt.Errorf(
		"watch(ops=%v): this host's trace session does not send %s, so the watch would "+
			"observe nothing and its assertions would pass having seen no such operation. "+
			"Enable it and restart Docker:\n    %s\n"+
			"These are off by default because they roughly double trace traffic and risk "+
			"dropped points, which fail a test",
		ops, strings.Join(missing, ", "), fix)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// installedTracePoints reads the host's configured points, or nil when the
// host is not registered.
func installedTracePoints() []string {
	pts, err := gvisor.InstalledPoints("")
	if err != nil {
		return nil
	}
	return pts
}
