package star

import (
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// Packet-level fault DSL (RFC-054 §"DSL extensions").
//
// Why `packet_*` and not another overload of delay()/drop(): those two names
// already mean different things depending on whether a positional duration is
// present (syscall level) or not (protocol level). The docs carry a warning
// about it and v0.13.2/0.13.3 shipped a round of fixes for kwargs being
// swallowed by the wrong overload. A third context-dependent meaning would be
// actively harmful, so packet faults get an unambiguous prefix.

// PacketFaultDef is the spec-level description of one packet fault. It is
// compiled to a netfault.Rule when the gateway installs it.
type PacketFaultDef struct {
	Action string

	// Matcher (all optional, ANDed).
	Dir             string
	Proto           string
	Flags           string
	Port            int
	Len             int
	HasLen          bool
	LenGT           int
	HasLenGT        bool
	LenLT           int
	HasLenLT        bool
	PayloadPrefix   string
	PayloadContains string
	Where           starlark.Callable

	// Occurrence selectors.
	Nth   int
	After int
	Every int

	// Probability fan-out — same semantics as the syscall faults.
	Probability float64
	MaxFires    int
	Mode        string

	// Action parameters.
	Delay          time.Duration
	ReorderBy      int
	DuplicateCount int
	CorruptOffset  int
	CorruptLength  int
	CorruptMode    string
	Checksum       string
	WindowSize     int
	Rate           string
	MTU            int

	Label string
}

var _ starlark.Value = (*PacketFaultDef)(nil)

func (p *PacketFaultDef) String() string {
	return fmt.Sprintf("<packet_fault %s>", p.Action)
}
func (p *PacketFaultDef) Type() string          { return "packet_fault" }
func (p *PacketFaultDef) Freeze()               {}
func (p *PacketFaultDef) Truth() starlark.Bool  { return true }
func (p *PacketFaultDef) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: packet_fault") }

// packetMatcherKwargs is the shared kwarg list quoted in error messages.
const packetMatcherKwargs = "dir, proto, flags, port, len, len_gt, len_lt, " +
	"payload_prefix, payload_contains, where, nth, after, every, probability, max_fires, mode, label"

// parsePacketMatcherKwarg handles the kwargs every packet_* builtin shares.
// Returns false when the key is not a shared one, so the caller can handle its
// action-specific keys and reject true unknowns.
//
// Same discipline as parseProxyMatcherKwarg: a typo'd matcher must never
// silently degrade into "match everything" (#137, #140).
func parsePacketMatcherKwarg(pf *PacketFaultDef, builtin, key string, v starlark.Value) (handled bool, err error) {
	switch key {
	case "dir":
		s, _ := starlark.AsString(v)
		if _, ok := netfault.ParseDirection(s); !ok && s != "both" {
			return true, fmt.Errorf("%s(): dir must be \"c2s\", \"s2c\" or \"both\", got %q", builtin, s)
		}
		pf.Dir = s
	case "proto":
		s, _ := starlark.AsString(v)
		switch s {
		case "tcp", "udp", "icmp":
			pf.Proto = s
		default:
			return true, fmt.Errorf("%s(): proto must be \"tcp\", \"udp\" or \"icmp\", got %q", builtin, s)
		}
	case "flags":
		s, _ := starlark.AsString(v)
		if _, _, e := netfault.ParseFlagSpec(s); e != nil {
			return true, fmt.Errorf("%s(): flags= %w", builtin, e)
		}
		pf.Flags = s
	case "port":
		n, e := starlark.AsInt32(v)
		if e != nil {
			return true, fmt.Errorf("%s(): port must be an integer, got %s", builtin, v.Type())
		}
		if n <= 0 || n > 65535 {
			return true, fmt.Errorf("%s(): port must be within 1..65535, got %d", builtin, n)
		}
		pf.Port = n
	case "len", "len_gt", "len_lt":
		n, e := starlark.AsInt32(v)
		if e != nil {
			return true, fmt.Errorf("%s(): %s must be an integer, got %s", builtin, key, v.Type())
		}
		if n < 0 {
			return true, fmt.Errorf("%s(): %s must be >= 0, got %d", builtin, key, n)
		}
		switch key {
		case "len":
			pf.Len, pf.HasLen = n, true
		case "len_gt":
			pf.LenGT, pf.HasLenGT = n, true
		case "len_lt":
			pf.LenLT, pf.HasLenLT = n, true
		}
	case "payload_prefix":
		pf.PayloadPrefix, _ = starlark.AsString(v)
	case "payload_contains":
		pf.PayloadContains, _ = starlark.AsString(v)
	case "where":
		c, ok := v.(starlark.Callable)
		if !ok {
			return true, fmt.Errorf("%s(): where must be a function, got %s", builtin, v.Type())
		}
		pf.Where = c
	case "nth", "after", "every":
		n, e := starlark.AsInt32(v)
		if e != nil {
			return true, fmt.Errorf("%s(): %s must be an integer, got %s", builtin, key, v.Type())
		}
		if n <= 0 {
			return true, fmt.Errorf("%s(): %s must be > 0, got %d", builtin, key, n)
		}
		switch key {
		case "nth":
			pf.Nth = n
		case "after":
			pf.After = n
		case "every":
			pf.Every = n
		}
	case "probability":
		pf.Probability = parseProbability(v)
	case "max_fires", "mode":
		// Consumed after the loop by parseProbabilityFanoutKwargs, which needs
		// the final probability value to validate against.
	case "label":
		pf.Label, _ = starlark.AsString(v)
	default:
		return false, nil
	}
	return true, nil
}

// finishPacketFault applies the shared post-loop validation: probability
// fan-out kwargs and mutually exclusive occurrence selectors.
func finishPacketFault(pf *PacketFaultDef, builtin string, kwargs []starlark.Tuple) error {
	maxFires, mode, err := parseProbabilityFanoutKwargs(builtin, kwargs, pf.Probability)
	if err != nil {
		return err
	}
	pf.MaxFires, pf.Mode = maxFires, mode

	set := 0
	for _, n := range []int{pf.Nth, pf.After, pf.Every} {
		if n > 0 {
			set++
		}
	}
	if set > 1 {
		return fmt.Errorf("%s(): nth=, after= and every= are mutually exclusive; pick one", builtin)
	}
	return nil
}

// packetBuiltin builds one packet_* builtin from a spec of its own kwargs.
// Sharing the loop keeps unknown-kwarg rejection identical across all of them,
// which is the property the v0.13.3 audit was about.
func packetBuiltin(name, action string, extra map[string]func(pf *PacketFaultDef, v starlark.Value) error, extraNames string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		pf := &PacketFaultDef{Action: action}

		// A positional argument is accepted only where the builtin documents
		// one (packet_delay's duration, bandwidth's rate, mtu's size).
		if len(args) > 0 {
			h, ok := extra["__positional__"]
			if !ok {
				return nil, fmt.Errorf("%s(): takes only keyword arguments", name)
			}
			if len(args) > 1 {
				return nil, fmt.Errorf("%s(): takes at most 1 positional argument, got %d", name, len(args))
			}
			if err := h(pf, args[0]); err != nil {
				return nil, err
			}
		}

		for _, kv := range kwargs {
			key, _ := starlark.AsString(kv[0])
			handled, err := parsePacketMatcherKwarg(pf, name, key, kv[1])
			if err != nil {
				return nil, err
			}
			if handled {
				continue
			}
			h, ok := extra[key]
			if !ok {
				valid := packetMatcherKwargs
				if extraNames != "" {
					valid += ", " + extraNames
				}
				return nil, fmt.Errorf("%s(): unknown keyword argument %q; valid: %s", name, key, valid)
			}
			if err := h(pf, kv[1]); err != nil {
				return nil, err
			}
		}

		if err := finishPacketFault(pf, name, kwargs); err != nil {
			return nil, err
		}
		// Compile eagerly so a malformed combination fails at spec load rather
		// than at packet time, where nothing would surface it.
		if _, err := pf.Compile(nil, nil); err != nil {
			return nil, fmt.Errorf("%s(): %w", name, err)
		}
		return pf, nil
	})
}

func starDuration(builtin string, v starlark.Value) (time.Duration, error) {
	s, ok := starlark.AsString(v)
	if !ok {
		return 0, fmt.Errorf("%s(): duration must be a string like \"250ms\", got %s", builtin, v.Type())
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s(): invalid duration %q: %w", builtin, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s(): duration must be positive, got %q", builtin, s)
	}
	return d, nil
}

func starPositiveInt(builtin, key string, v starlark.Value, min int) (int, error) {
	n, err := starlark.AsInt32(v)
	if err != nil {
		return 0, fmt.Errorf("%s(): %s must be an integer, got %s", builtin, key, v.Type())
	}
	if n < min {
		return 0, fmt.Errorf("%s(): %s must be >= %d, got %d", builtin, key, min, n)
	}
	return n, nil
}

// packetFaultBuiltins returns the packet_* family for registration.
func packetFaultBuiltins() map[string]*starlark.Builtin {
	delayExtra := map[string]func(*PacketFaultDef, starlark.Value) error{
		"__positional__": func(pf *PacketFaultDef, v starlark.Value) error {
			d, err := starDuration("packet_delay", v)
			if err != nil {
				return err
			}
			pf.Delay = d
			return nil
		},
		"duration": func(pf *PacketFaultDef, v starlark.Value) error {
			d, err := starDuration("packet_delay", v)
			if err != nil {
				return err
			}
			pf.Delay = d
			return nil
		},
	}

	reorderExtra := map[string]func(*PacketFaultDef, starlark.Value) error{
		"by": func(pf *PacketFaultDef, v starlark.Value) error {
			n, err := starPositiveInt("packet_reorder", "by", v, 1)
			pf.ReorderBy = n
			return err
		},
	}

	dupExtra := map[string]func(*PacketFaultDef, starlark.Value) error{
		"count": func(pf *PacketFaultDef, v starlark.Value) error {
			n, err := starPositiveInt("packet_duplicate", "count", v, 2)
			pf.DuplicateCount = n
			return err
		},
	}

	corruptExtra := map[string]func(*PacketFaultDef, starlark.Value) error{
		"offset": func(pf *PacketFaultDef, v starlark.Value) error {
			n, err := starPositiveInt("packet_corrupt", "offset", v, 0)
			pf.CorruptOffset = n
			return err
		},
		"length": func(pf *PacketFaultDef, v starlark.Value) error {
			n, err := starPositiveInt("packet_corrupt", "length", v, 1)
			pf.CorruptLength = n
			return err
		},
		// Named corrupt_mode rather than mode: `mode` is already taken by the
		// RFC-042 probability fan-out kwarg that every fault builtin accepts.
		"corrupt_mode": func(pf *PacketFaultDef, v starlark.Value) error {
			s, _ := starlark.AsString(v)
			switch s {
			case "flip", "zero", "random":
				pf.CorruptMode = s
				return nil
			}
			return fmt.Errorf("packet_corrupt(): corrupt_mode must be \"flip\", \"zero\" or \"random\", got %q", s)
		},
		"checksum": func(pf *PacketFaultDef, v starlark.Value) error {
			s, _ := starlark.AsString(v)
			switch s {
			case "fix", "break":
				pf.Checksum = s
				return nil
			}
			return fmt.Errorf("packet_corrupt(): checksum must be \"fix\" or \"break\", got %q", s)
		},
	}

	windowExtra := map[string]func(*PacketFaultDef, starlark.Value) error{
		"size": func(pf *PacketFaultDef, v starlark.Value) error {
			n, err := starPositiveInt("packet_window", "size", v, 0)
			if err != nil {
				return err
			}
			if n > 65535 {
				return fmt.Errorf("packet_window(): size must be within 0..65535, got %d", n)
			}
			pf.WindowSize = n
			return nil
		},
	}

	return map[string]*starlark.Builtin{
		"packet_drop":      packetBuiltin("packet_drop", "drop", nil, ""),
		"packet_pass":      packetBuiltin("packet_pass", "pass", nil, ""),
		"packet_reset":     packetBuiltin("packet_reset", "reset", nil, ""),
		"packet_delay":     packetBuiltin("packet_delay", "delay", delayExtra, "duration"),
		"packet_reorder":   packetBuiltin("packet_reorder", "reorder", reorderExtra, "by"),
		"packet_duplicate": packetBuiltin("packet_duplicate", "duplicate", dupExtra, "count"),
		"packet_corrupt":   packetBuiltin("packet_corrupt", "corrupt", corruptExtra, "offset, length, corrupt_mode, checksum"),
		"packet_window":    packetBuiltin("packet_window", "window", windowExtra, "size"),
	}
}

// Compile lowers the spec-level definition to an engine rule.
//
// thread is the Starlark thread used to invoke a where= predicate; nil means
// "validate only", which is what spec load does so a bad combination fails
// there instead of at packet time. onWhereError receives the first failure
// from a where= lambda so the runtime can fail the test instead of reporting
// a pass for a fault that never fired.
func (p *PacketFaultDef) Compile(thread *starlark.Thread, onWhereError func(error)) (*netfault.Rule, error) {
	r := &netfault.Rule{
		Label:       p.Label,
		Probability: p.Probability,
		MaxFires:    p.MaxFires,
		Mode:        p.Mode,
	}

	switch p.Action {
	case "drop":
		r.Action = netfault.ActionDrop
	case "pass":
		r.Action = netfault.ActionPass
	case "reset":
		r.Action = netfault.ActionReset
	case "delay":
		r.Action = netfault.ActionDelay
		r.Delay = p.Delay
	case "reorder":
		r.Action = netfault.ActionReorder
		r.ReorderBy = p.ReorderBy
	case "duplicate":
		r.Action = netfault.ActionDuplicate
		r.DuplicateCount = p.DuplicateCount
		if r.DuplicateCount == 0 {
			r.DuplicateCount = 2 // documented default: delivered twice
		}
	case "corrupt":
		r.Action = netfault.ActionCorrupt
		r.CorruptOffset = p.CorruptOffset
		r.CorruptLength = p.CorruptLength
		if r.CorruptLength == 0 {
			r.CorruptLength = 1
		}
		r.CorruptMode = netfault.CorruptMode(p.CorruptMode)
		if r.CorruptMode == "" {
			r.CorruptMode = netfault.CorruptFlip
		}
		r.Checksum = netfault.ChecksumPolicy(p.Checksum)
		if r.Checksum == "" {
			r.Checksum = netfault.ChecksumFix
		}
	case "window":
		r.Action = netfault.ActionWindow
		r.WindowSize = uint16(p.WindowSize)
	default:
		return nil, fmt.Errorf("unknown packet fault action %q", p.Action)
	}

	switch {
	case p.Nth > 0:
		r.Trigger, r.TriggerN = netfault.TriggerNth, p.Nth
	case p.After > 0:
		r.Trigger, r.TriggerN = netfault.TriggerAfter, p.After
	case p.Every > 0:
		r.Trigger, r.TriggerN = netfault.TriggerEvery, p.Every
	}

	m := netfault.Match{
		Port:     uint16(p.Port),
		Len:      p.Len,
		HasLen:   p.HasLen,
		LenGT:    p.LenGT,
		HasLenGT: p.HasLenGT,
		LenLT:    p.LenLT,
		HasLenLT: p.HasLenLT,
	}
	if p.Dir != "" && p.Dir != "both" {
		d, _ := netfault.ParseDirection(p.Dir)
		m.Dir = &d
	}
	if p.Proto != "" {
		m.Proto = netfault.Protocol(p.Proto)
	}
	if p.Flags != "" {
		set, clear, err := netfault.ParseFlagSpec(p.Flags)
		if err != nil {
			return nil, err
		}
		m.FlagsSet, m.FlagsClear = set, clear
	}
	if p.PayloadPrefix != "" {
		m.PayloadPrefix = []byte(p.PayloadPrefix)
	}
	if p.PayloadContains != "" {
		m.PayloadContains = []byte(p.PayloadContains)
	}
	if p.Where != nil && thread != nil {
		m.Where = makeWherePredicate(thread, p.Where, onWhereError)
	}
	r.Match = m

	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// makeWherePredicate bridges a Starlark lambda onto the datapath.
//
// A predicate that errors, or returns a non-bool, cannot match — there is no
// sane way to abort a test from inside a packet callback. But it must not fail
// *silently*: a lambda that throws on every packet would inject nothing and
// the test would pass, and the author would conclude their service tolerates
// the fault. `p.payload.startswith(...)` is exactly such a lambda (Starlark's
// bytes type has only elems()), and it is how this RFC's own headline example
// was originally written.
//
// So the first failure is reported through onError, and the runtime fails the
// test with it.
func makeWherePredicate(thread *starlark.Thread, fn starlark.Callable, onError func(error)) func(*netfault.PacketView) bool {
	report := func(err error) {
		if onError != nil {
			onError(err)
		}
	}
	return func(pv *netfault.PacketView) bool {
		pkt := newStarlarkPacket(pv)
		res, err := starlark.Call(thread, fn, starlark.Tuple{pkt}, nil)
		if err != nil {
			report(fmt.Errorf("where= predicate failed on %s: %w", pv.String(), err))
			return false
		}
		b, ok := res.(starlark.Bool)
		if !ok {
			report(fmt.Errorf("where= predicate returned %s, want a bool (packet %s)", res.Type(), pv.String()))
			return false
		}
		return bool(b)
	}
}

// ─── the Packet value handed to a where= lambda ────────────────────────────

// starlarkPacket exposes a PacketView to spec code as a read-only struct-like
// value. Fields are materialised lazily: a lambda that only reads pkt.len must
// not pay to copy a 4 KiB payload.
type starlarkPacket struct {
	pv *netfault.PacketView
}

var _ starlark.Value = (*starlarkPacket)(nil)
var _ starlark.HasAttrs = (*starlarkPacket)(nil)

func newStarlarkPacket(pv *netfault.PacketView) *starlarkPacket {
	return &starlarkPacket{pv: pv}
}

func (p *starlarkPacket) String() string        { return "<packet " + p.pv.String() + ">" }
func (p *starlarkPacket) Type() string          { return "packet" }
func (p *starlarkPacket) Freeze()               {}
func (p *starlarkPacket) Truth() starlark.Bool  { return true }
func (p *starlarkPacket) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: packet") }

var packetAttrNames = []string{
	"ack", "dir", "dst_ip", "dst_port", "flags", "flow", "index", "len",
	"payload", "proto", "seq", "src_ip", "src_port", "window",
}

func (p *starlarkPacket) AttrNames() []string { return packetAttrNames }

func (p *starlarkPacket) Attr(name string) (starlark.Value, error) {
	switch name {
	case "proto":
		return starlark.String(p.pv.Proto), nil
	case "dir":
		return starlark.String(p.pv.Dir.String()), nil
	case "src_ip":
		return starlark.String(p.pv.SrcIP), nil
	case "dst_ip":
		return starlark.String(p.pv.DstIP), nil
	case "src_port":
		return starlark.MakeInt(int(p.pv.SrcPort)), nil
	case "dst_port":
		return starlark.MakeInt(int(p.pv.DstPort)), nil
	case "len":
		return starlark.MakeInt(p.pv.PayloadLen), nil
	case "payload":
		// A Starlark string, not bytes.
		//
		// Starlark's bytes type has exactly one method — elems() — so
		// `p.payload.startswith(b"...")` fails, which is how the RFC's own
		// headline example was written. Starlark strings in go.starlark.net
		// are arbitrary byte sequences (not UTF-8 validated), so String gives
		// binary-safe storage *plus* the full method set: startswith,
		// endswith, find, count, index. It also matches the declarative
		// payload_prefix= / payload_contains= kwargs, which are strings.
		return starlark.String(p.pv.Payload), nil
	case "payload_bytes":
		// Escape hatch for slicing and elems() when a spec really wants the
		// bytes type.
		return starlark.Bytes(p.pv.Payload), nil
	case "flags":
		names := netfault.FlagNames(p.pv.Flags)
		items := make([]starlark.Value, len(names))
		for i, n := range names {
			items[i] = starlark.String(n)
		}
		return starlark.NewList(items), nil
	case "seq":
		return starlark.MakeInt64(int64(p.pv.Seq)), nil
	case "ack":
		return starlark.MakeInt64(int64(p.pv.Ack)), nil
	case "window":
		return starlark.MakeInt(int(p.pv.Window)), nil
	case "index":
		return starlark.MakeInt(p.pv.Index), nil
	case "flow":
		return starlark.String(p.pv.Flow), nil
	}
	// Returning (nil, nil) makes Starlark report "has no .<name> field",
	// which names the typo instead of yielding None and matching nothing.
	return nil, nil
}

// describePacketFaults renders installed packet rules for diagnostics.
func describePacketFaults(defs []*PacketFaultDef) string {
	if len(defs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(defs))
	for _, d := range defs {
		s := "packet_" + d.Action
		if d.Label != "" {
			s += "[" + d.Label + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}
