package netfault

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Link shapers: bandwidth() and mtu().
//
// These are not per-packet rules and deliberately take no matcher (RFC-054
// §"Deferred to v0.14.1"). A rule answers "what should happen to packets that
// look like this"; a shaper answers "what kind of link is this", which is a
// property of the path, not of any packet crossing it.
//
// That distinction is why they were held back from v0.14.0 rather than bolted
// onto the match-and-act pipeline. mtu() in particular: approximating it with
// packet_drop(len_gt=N) drops oversized packets, which looks like a black hole
// and behaves like nothing real — a genuine small-MTU path makes TCP negotiate
// a smaller MSS and makes IP fragment, and the interesting bugs live in that
// behaviour, not in the loss.

// DefaultShaperBacklog bounds how much traffic a bandwidth() shaper will hold
// before it starts discarding.
//
// A rate limiter with an unbounded queue is not a slow link, it is a memory
// leak with latency: a sender that outruns the configured rate would be
// buffered forever and never observe congestion. Real bottlenecks have finite
// queues and drop when they fill, which is what makes a congestion-control
// bug reproducible. Expressed as time rather than bytes so it means the same
// thing at 1 Mbit and at 1 Gbit.
const DefaultShaperBacklog = 250 * time.Millisecond

// ParseRate converts a human rate to bytes per second.
//
// Bit units (the networking convention, and what "1mbps" means to everyone who
// has configured a link) and byte units are both accepted, but byte units must
// say so explicitly with "/s" — "1MB/s" is eight times "1mbit" and guessing
// between them from letter case alone would be a silent factor-of-eight error.
func ParseRate(s string) (bytesPerSec float64, err error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("rate is empty; want e.g. \"1mbit\", \"512kbps\" or \"2MB/s\"")
	}
	lower := strings.ToLower(raw)

	// Longest suffixes first: "mbps" must not match the "bps" arm.
	units := []struct {
		suffix string
		mult   float64 // to bits per second
	}{
		{"gbps", 1e9}, {"gbit", 1e9},
		{"mbps", 1e6}, {"mbit", 1e6},
		{"kbps", 1e3}, {"kbit", 1e3},
		{"bps", 1}, {"bit", 1},
	}
	byteUnits := []struct {
		suffix string
		mult   float64 // to bytes per second
	}{
		{"gb/s", 1e9}, {"mb/s", 1e6}, {"kb/s", 1e3}, {"b/s", 1},
	}

	for _, u := range byteUnits {
		if num, ok := strings.CutSuffix(lower, u.suffix); ok {
			v, e := parsePositiveFloat(num)
			if e != nil {
				return 0, fmt.Errorf("rate %q: %w", raw, e)
			}
			return v * u.mult, nil
		}
	}
	for _, u := range units {
		if num, ok := strings.CutSuffix(lower, u.suffix); ok {
			v, e := parsePositiveFloat(num)
			if e != nil {
				return 0, fmt.Errorf("rate %q: %w", raw, e)
			}
			return v * u.mult / 8, nil
		}
	}
	return 0, fmt.Errorf(
		"rate %q has no unit; want bits (\"1mbit\", \"512kbps\", \"1gbit\") "+
			"or bytes (\"2MB/s\", \"64kB/s\")", raw)
}

func parsePositiveFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("missing a number before the unit")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("must be positive, got %g", v)
	}
	return v, nil
}

// shaper paces one direction of the link.
//
// The model is a single server draining at bytesPerSec: each admitted packet
// occupies the link for size/rate seconds, and the next packet leaves when the
// one before it has finished. nextFree is that finish time, so the wait for an
// arriving packet is simply how far nextFree is in the future — which is also
// the current queue depth, expressed as time. One field, no separate byte
// counter to keep in sync, and the backlog bound falls out of the same value.
type shaper struct {
	mu          sync.Mutex
	bytesPerSec float64
	maxBacklog  time.Duration
	nextFree    time.Time

	admitted atomic.Int64
	dropped  atomic.Int64
	// backlogPeak records the deepest queue observed, so a spec author can
	// tell "the link was saturated" from "the link was irrelevant".
	backlogPeakNanos atomic.Int64
}

func newShaper(bytesPerSec float64, maxBacklog time.Duration) *shaper {
	if maxBacklog <= 0 {
		maxBacklog = DefaultShaperBacklog
	}
	return &shaper{bytesPerSec: bytesPerSec, maxBacklog: maxBacklog}
}

// admit reserves link time for a packet of size bytes.
//
// Returns how long the packet must wait before it may be released, and whether
// it was admitted at all: a packet arriving at a full queue is dropped, which
// is what a real bottleneck does and what makes the sender notice.
func (s *shaper) admit(size int, now time.Time) (wait time.Duration, ok bool) {
	if size <= 0 {
		return 0, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nextFree.Before(now) {
		// Link went idle; no backlog to inherit.
		s.nextFree = now
	}
	backlog := s.nextFree.Sub(now)
	if backlog > s.maxBacklog {
		s.dropped.Add(1)
		return 0, false
	}
	if backlog.Nanoseconds() > s.backlogPeakNanos.Load() {
		s.backlogPeakNanos.Store(backlog.Nanoseconds())
	}
	txTime := time.Duration(float64(size) / s.bytesPerSec * float64(time.Second))
	s.nextFree = s.nextFree.Add(txTime)
	s.admitted.Add(1)
	return backlog, true
}

// Stats reports what the shaper did, so a run can distinguish a link that was
// merely configured from one that actually bit.
type ShaperStats struct {
	Admitted    int64
	Dropped     int64
	PeakBacklog time.Duration
}

func (s *shaper) stats() ShaperStats {
	return ShaperStats{
		Admitted:    s.admitted.Load(),
		Dropped:     s.dropped.Load(),
		PeakBacklog: time.Duration(s.backlogPeakNanos.Load()),
	}
}
