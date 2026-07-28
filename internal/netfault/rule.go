package netfault

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Action is what happens to a packet whose rule matched.
type Action int

const (
	// ActionPass explicitly allows a packet. Useful as an early rule that
	// carves an exception out of a broader matcher below it.
	ActionPass Action = iota
	// ActionDrop frees the packet. No RST is sent — the packet simply never
	// existed. This is what makes the half-open blackhole scenario possible;
	// closing a connection would send a RST that clients handle correctly.
	ActionDrop
	// ActionDelay defers the packet by Delay, then releases it.
	ActionDelay
	// ActionDuplicate delivers the packet DuplicateCount times.
	ActionDuplicate
	// ActionReorder holds the packet until ReorderBy later packets on the
	// same flow have gone ahead of it.
	ActionReorder
	// ActionCorrupt mutates payload bytes in place.
	ActionCorrupt
	// ActionReset synthesizes a TCP RST and drops the original.
	ActionReset
	// ActionWindow rewrites the advertised TCP receive window.
	ActionWindow
)

func (a Action) String() string {
	switch a {
	case ActionPass:
		return "pass"
	case ActionDrop:
		return "drop"
	case ActionDelay:
		return "delay"
	case ActionDuplicate:
		return "duplicate"
	case ActionReorder:
		return "reorder"
	case ActionCorrupt:
		return "corrupt"
	case ActionReset:
		return "reset"
	case ActionWindow:
		return "window"
	}
	return "unknown"
}

// Trigger controls when a matching rule actually fires, relative to how many
// packets have already matched it. Mirrors engine.FaultTrigger, plus Every,
// which packet faults need and syscall faults never asked for.
type Trigger int

const (
	// TriggerAlways fires on every matching packet.
	TriggerAlways Trigger = iota
	// TriggerNth fires only on the Nth matching packet (1-indexed).
	TriggerNth
	// TriggerAfter fires on every matching packet after the first N.
	TriggerAfter
	// TriggerEvery fires on every Nth matching packet.
	TriggerEvery
)

// CorruptMode selects how corrupted bytes are mutated.
type CorruptMode string

const (
	CorruptFlip   CorruptMode = "flip"   // XOR 0xFF
	CorruptZero   CorruptMode = "zero"   // overwrite with 0x00
	CorruptRandom CorruptMode = "random" // seeded PRNG, reproducible
)

// ChecksumPolicy decides whether a mutated packet gets a valid checksum.
//
// Both are useful and they test different things: "fix" produces a packet the
// receiver accepts with wrong data (silent corruption reaching the
// application); "break" produces one the receiver's stack discards (packet
// loss that looks like congestion).
type ChecksumPolicy string

const (
	ChecksumFix   ChecksumPolicy = "fix"
	ChecksumBreak ChecksumPolicy = "break"
)

// ProbabilityDecider mirrors engine.SessionConfig.ProbabilityDecider so packet
// faults and syscall faults share one notion of RFC-042 §8.9 exhaustive
// probability fan-out. Returning pinned=false falls back to the seeded RNG.
type ProbabilityDecider func(rule *Rule, occurrence int) (fire bool, pinned bool)

// Rule is one installed packet fault.
//
// Rules are immutable once installed except for the two atomic counters, so a
// RuleSet can be published with a single atomic pointer swap and read on the
// datapath without a lock.
type Rule struct {
	Action Action
	Match  Match
	Label  string

	// Delay applies to ActionDelay.
	Delay time.Duration
	// ReorderBy applies to ActionReorder: how many later packets overtake.
	ReorderBy int
	// DuplicateCount applies to ActionDuplicate: total deliveries, so 2 means
	// the packet is seen twice.
	DuplicateCount int

	// Corruption parameters (ActionCorrupt).
	CorruptOffset int
	CorruptLength int
	CorruptMode   CorruptMode
	Checksum      ChecksumPolicy

	// WindowSize applies to ActionWindow.
	WindowSize uint16

	// Trigger / TriggerN gate firing on match count.
	Trigger  Trigger
	TriggerN int

	// Probability is 0..1, where **zero means unset and fires always**.
	//
	// That reads oddly until you consider the alternative. The DSL documents
	// `probability` as defaulting to "100%", so a rule built without one must
	// fire. If zero meant "never", every Rule constructed as a struct literal
	// without this field would install successfully and then quietly do
	// nothing — the exact silent-no-op class v0.13.2/0.13.3 spent a release
	// stamping out. "Never fire" is not a rule anyone writes on purpose; a
	// rule that fires when you forgot a field is far cheaper to debug.
	//
	// MaxFires and Mode mirror the syscall-fault semantics from RFC-042 §8.9
	// verbatim — packet faults must fan out identically or a mixed spec
	// becomes impossible to reason about.
	Probability float64
	MaxFires    int
	Mode        string

	counter     atomic.Int64
	probCounter atomic.Int64
	fired       atomic.Int64
}

// MatchCount reports how many packets matched this rule's predicates,
// whether or not the trigger let it fire. The runtime surfaces a rule with a
// zero count as "no injections fired", the same diagnostic syscall faults got
// in v0.9.4.
func (r *Rule) MatchCount() int64 { return r.counter.Load() }

// FireCount reports how many times the rule actually acted on a packet.
func (r *Rule) FireCount() int64 { return r.fired.Load() }

// shouldTrigger evaluates the stateful trigger. Always increments the match
// counter, mirroring engine.FaultRule.ShouldFire.
func (r *Rule) shouldTrigger() bool {
	n := r.counter.Add(1)
	switch r.Trigger {
	case TriggerNth:
		return n == int64(r.TriggerN)
	case TriggerAfter:
		return n > int64(r.TriggerN)
	case TriggerEvery:
		if r.TriggerN <= 0 {
			return true
		}
		return n%int64(r.TriggerN) == 0
	default:
		return true
	}
}

// nextProbabilityOccurrence mirrors engine.FaultRule.NextProbabilityOccurrence:
// a zero-based index consumed exactly once per probability consultation.
func (r *Rule) nextProbabilityOccurrence() int {
	return int(r.probCounter.Add(1)) - 1
}

// Validate reports whether the rule is internally coherent. Called at spec
// load so a malformed rule fails loudly there rather than silently doing
// nothing on the datapath — the failure mode v0.13.2/0.13.3 spent a release
// stamping out.
func (r *Rule) Validate() error {
	switch r.Action {
	case ActionDelay:
		if r.Delay <= 0 {
			return fmt.Errorf("packet_delay: duration must be positive, got %s", r.Delay)
		}
	case ActionReorder:
		if r.ReorderBy <= 0 {
			return fmt.Errorf("packet_reorder: by must be >= 1, got %d", r.ReorderBy)
		}
	case ActionDuplicate:
		if r.DuplicateCount < 2 {
			return fmt.Errorf("packet_duplicate: count must be >= 2 (2 means delivered twice), got %d", r.DuplicateCount)
		}
	case ActionCorrupt:
		if r.CorruptLength <= 0 {
			return fmt.Errorf("packet_corrupt: length must be >= 1, got %d", r.CorruptLength)
		}
		if r.CorruptOffset < 0 {
			return fmt.Errorf("packet_corrupt: offset must be >= 0, got %d", r.CorruptOffset)
		}
		switch r.CorruptMode {
		case CorruptFlip, CorruptZero, CorruptRandom:
		default:
			return fmt.Errorf("packet_corrupt: mode must be one of flip, zero, random; got %q", r.CorruptMode)
		}
		switch r.Checksum {
		case ChecksumFix, ChecksumBreak:
		default:
			return fmt.Errorf("packet_corrupt: checksum must be fix or break, got %q", r.Checksum)
		}
	case ActionReset:
		if r.Match.Proto != "" && r.Match.Proto != ProtoTCP {
			return fmt.Errorf("packet_reset: only meaningful for tcp, got proto=%q", r.Match.Proto)
		}
	case ActionWindow:
		if r.Match.Proto != "" && r.Match.Proto != ProtoTCP {
			return fmt.Errorf("packet_window: only meaningful for tcp, got proto=%q", r.Match.Proto)
		}
	}

	if r.Probability < 0 || r.Probability > 1 {
		return fmt.Errorf("probability must be within 0..1, got %v", r.Probability)
	}
	if r.MaxFires > 0 && (r.Probability <= 0 || r.Probability >= 1) {
		return fmt.Errorf("max_fires is only meaningful with 0 < probability < 1")
	}
	if r.Mode == "stochastic" && r.MaxFires > 0 {
		return fmt.Errorf("mode=\"stochastic\" is incompatible with max_fires (which implies exhaustive fan-out)")
	}
	if r.Trigger != TriggerAlways && r.TriggerN <= 0 {
		return fmt.Errorf("trigger requires a positive count, got %d", r.TriggerN)
	}
	return nil
}

// RuleSet is an ordered, immutable list of rules. First match wins.
type RuleSet struct {
	rules []*Rule
}

// NewRuleSet validates and freezes a rule list.
func NewRuleSet(rules ...*Rule) (*RuleSet, error) {
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, r.Action, err)
		}
	}
	return &RuleSet{rules: rules}, nil
}

// Rules exposes the underlying slice for reporting. Callers must not mutate it.
func (rs *RuleSet) Rules() []*Rule {
	if rs == nil {
		return nil
	}
	return rs.rules
}

// decide walks the rule set and returns the first rule that both matches the
// packet and passes its trigger and probability gates.
//
// rnd supplies the seeded RNG; decider is the optional RFC-042 §8.9 plan-leaf
// oracle; onWhereEval counts Starlark predicate calls. All three are injected
// rather than reached for, so the whole decision path is deterministic under
// test.
func (rs *RuleSet) decide(p *PacketView, rnd func() float64, decider ProbabilityDecider, onWhereEval func()) *Rule {
	if rs == nil {
		return nil
	}
	for _, r := range rs.rules {
		if !r.Match.Matches(p, onWhereEval) {
			continue
		}
		if !r.shouldTrigger() {
			continue
		}
		if !r.rollProbability(rnd, decider) {
			continue
		}
		r.fired.Add(1)
		return r
	}
	return nil
}

// rollProbability mirrors the syscall-fault consultation order exactly:
// take the RNG draw first, then let a non-stochastic rule's pinned decider
// override it. Drawing unconditionally keeps the RNG stream aligned between
// pinned and unpinned runs, which is what makes a seed reproducible.
func (r *Rule) rollProbability(rnd func() float64, decider ProbabilityDecider) bool {
	// Zero is "unset" (see the Probability field comment), so both ends of the
	// range mean "always".
	if r.Probability <= 0 || r.Probability >= 1 {
		return true
	}
	fire := false
	if rnd != nil {
		fire = rnd() < r.Probability
	}
	if decider != nil && r.Mode != "stochastic" {
		occ := r.nextProbabilityOccurrence()
		if pinnedFire, pinned := decider(r, occ); pinned {
			fire = pinnedFire
		}
	}
	return fire
}
