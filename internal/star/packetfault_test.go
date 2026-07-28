package star

import (
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/netfault"
)

// evalPacketBuiltin evaluates a single packet_* call and returns the def.
func evalPacketBuiltin(t *testing.T, expr string) (*PacketFaultDef, error) {
	t.Helper()
	rt := New(testLogger())
	thread := &starlark.Thread{Name: "test"}
	v, err := starlark.Eval(thread, "test.star", expr, rt.builtins())
	if err != nil {
		return nil, err
	}
	pf, ok := v.(*PacketFaultDef)
	if !ok {
		t.Fatalf("expression %q returned %s, want packet_fault", expr, v.Type())
	}
	return pf, nil
}

func mustPacketBuiltin(t *testing.T, expr string) *PacketFaultDef {
	t.Helper()
	pf, err := evalPacketBuiltin(t, expr)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return pf
}

func TestPacketBuiltinsParse(t *testing.T) {
	tests := []struct {
		expr   string
		action string
		check  func(*testing.T, *PacketFaultDef)
	}{
		{`packet_drop()`, "drop", nil},
		{`packet_pass()`, "pass", nil},
		{`packet_reset()`, "reset", nil},
		{`packet_delay("250ms")`, "delay", func(t *testing.T, p *PacketFaultDef) {
			if p.Delay != 250*time.Millisecond {
				t.Errorf("Delay = %v, want 250ms", p.Delay)
			}
		}},
		{`packet_delay(duration = "1s")`, "delay", func(t *testing.T, p *PacketFaultDef) {
			if p.Delay != time.Second {
				t.Errorf("Delay = %v, want 1s", p.Delay)
			}
		}},
		{`packet_reorder(by = 3)`, "reorder", func(t *testing.T, p *PacketFaultDef) {
			if p.ReorderBy != 3 {
				t.Errorf("ReorderBy = %d, want 3", p.ReorderBy)
			}
		}},
		{`packet_duplicate(count = 3)`, "duplicate", func(t *testing.T, p *PacketFaultDef) {
			if p.DuplicateCount != 3 {
				t.Errorf("DuplicateCount = %d, want 3", p.DuplicateCount)
			}
		}},
		{`packet_corrupt(offset = 4, length = 8, corrupt_mode = "zero", checksum = "break")`, "corrupt",
			func(t *testing.T, p *PacketFaultDef) {
				if p.CorruptOffset != 4 || p.CorruptLength != 8 {
					t.Errorf("offset/length = %d/%d, want 4/8", p.CorruptOffset, p.CorruptLength)
				}
				if p.CorruptMode != "zero" || p.Checksum != "break" {
					t.Errorf("mode/checksum = %s/%s, want zero/break", p.CorruptMode, p.Checksum)
				}
			}},
		{`packet_window(size = 0)`, "window", func(t *testing.T, p *PacketFaultDef) {
			if p.WindowSize != 0 {
				t.Errorf("WindowSize = %d, want 0", p.WindowSize)
			}
		}},
		{`packet_drop(dir = "c2s", proto = "tcp", flags = "PSH,ACK", port = 5432)`, "drop",
			func(t *testing.T, p *PacketFaultDef) {
				if p.Dir != "c2s" || p.Proto != "tcp" || p.Flags != "PSH,ACK" || p.Port != 5432 {
					t.Errorf("matcher not captured: %+v", p)
				}
			}},
		{`packet_drop(len_gt = 1400, len_lt = 9000)`, "drop", func(t *testing.T, p *PacketFaultDef) {
			if !p.HasLenGT || p.LenGT != 1400 || !p.HasLenLT || p.LenLT != 9000 {
				t.Errorf("length bounds not captured: %+v", p)
			}
		}},
		{`packet_drop(len = 0)`, "drop", func(t *testing.T, p *PacketFaultDef) {
			// Zero must be distinguishable from unset — a bare ACK is a real
			// and interesting target.
			if !p.HasLen || p.Len != 0 {
				t.Errorf("len=0 not captured as set: HasLen=%v Len=%d", p.HasLen, p.Len)
			}
		}},
		{`packet_drop(payload_prefix = "GET ", payload_contains = "admin")`, "drop",
			func(t *testing.T, p *PacketFaultDef) {
				if p.PayloadPrefix != "GET " || p.PayloadContains != "admin" {
					t.Errorf("payload matchers not captured: %+v", p)
				}
			}},
		{`packet_drop(nth = 3)`, "drop", func(t *testing.T, p *PacketFaultDef) {
			if p.Nth != 3 {
				t.Errorf("Nth = %d, want 3", p.Nth)
			}
		}},
		{`packet_drop(every = 4, label = "loss")`, "drop", func(t *testing.T, p *PacketFaultDef) {
			if p.Every != 4 || p.Label != "loss" {
				t.Errorf("every/label = %d/%q", p.Every, p.Label)
			}
		}},
		{`packet_drop(probability = "30%")`, "drop", func(t *testing.T, p *PacketFaultDef) {
			if p.Probability < 0.29 || p.Probability > 0.31 {
				t.Errorf("Probability = %v, want ~0.3", p.Probability)
			}
		}},
		{`packet_drop(probability = 0.25, max_fires = 3)`, "drop", func(t *testing.T, p *PacketFaultDef) {
			if p.MaxFires != 3 {
				t.Errorf("MaxFires = %d, want 3", p.MaxFires)
			}
		}},
		{`packet_delay("100ms", where = lambda p: p.len > 100)`, "delay", func(t *testing.T, p *PacketFaultDef) {
			if p.Where == nil {
				t.Error("where= not captured")
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			pf := mustPacketBuiltin(t, tc.expr)
			if pf.Action != tc.action {
				t.Errorf("Action = %q, want %q", pf.Action, tc.action)
			}
			if tc.check != nil {
				tc.check(t, pf)
			}
		})
	}
}

// TestPacketBuiltinsRejectUnknownKwargs is the guard the v0.13.2/0.13.3 audit
// earned: a typo'd matcher kwarg must fail at spec load, never silently widen
// the rule to match everything.
func TestPacketBuiltinsRejectUnknownKwargs(t *testing.T) {
	exprs := []string{
		`packet_drop(direction = "c2s")`, // near-miss for dir=
		`packet_drop(protocol = "tcp")`,  // near-miss for proto=
		`packet_delay("1s", delay = "2s")`,
		`packet_reorder(by = 2, amount = 3)`,
		`packet_duplicate(count = 2, times = 3)`,
		`packet_corrupt(length = 1, mode_of_corruption = "flip")`,
		`packet_window(size = 0, win = 1)`,
		`packet_reset(reason = "because")`,
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			_, err := evalPacketBuiltin(t, expr)
			if err == nil {
				t.Fatalf("%s was accepted; unknown kwargs must fail at spec load", expr)
			}
			if !strings.Contains(err.Error(), "unknown keyword argument") {
				t.Errorf("error should name the unknown kwarg, got: %v", err)
			}
			if !strings.Contains(err.Error(), "valid:") {
				t.Errorf("error should list valid kwargs, got: %v", err)
			}
		})
	}
}

func TestPacketBuiltinsValidateValues(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr string
	}{
		{`packet_drop(dir = "sideways")`, `dir must be`},
		{`packet_drop(proto = "sctp")`, `proto must be`},
		{`packet_drop(flags = "NOPE")`, `unknown TCP flag`},
		{`packet_drop(flags = "SYN,!SYN")`, `both requires and forbids`},
		{`packet_drop(port = 0)`, `port must be within`},
		{`packet_drop(port = 70000)`, `port must be within`},
		{`packet_drop(len_gt = -1)`, `must be >= 0`},
		{`packet_delay("0s")`, `must be positive`},
		{`packet_delay("nonsense")`, `invalid duration`},
		{`packet_delay(123)`, `must be a string`},
		{`packet_reorder(by = 0)`, `must be >= 1`},
		{`packet_duplicate(count = 1)`, `must be >= 2`},
		{`packet_corrupt(length = 1, corrupt_mode = "shuffle")`, `corrupt_mode must be`},
		{`packet_corrupt(length = 1, checksum = "maybe")`, `checksum must be`},
		{`packet_window(size = 99999)`, `must be within 0..65535`},
		{`packet_drop(nth = 0)`, `must be > 0`},
		{`packet_drop(nth = 2, every = 3)`, `mutually exclusive`},
		{`packet_drop(nth = 2, after = 3)`, `mutually exclusive`},
		{`packet_drop(where = "not a function")`, `where must be a function`},
		{`packet_drop(probability = 1.0, max_fires = 2)`, `only meaningful with probability < 1`},
		{`packet_drop("positional")`, `takes only keyword arguments`},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := evalPacketBuiltin(t, tc.expr)
			if err == nil {
				t.Fatalf("%s was accepted, want error containing %q", tc.expr, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestPacketFaultCompiles(t *testing.T) {
	pf := mustPacketBuiltin(t,
		`packet_delay("250ms", dir = "c2s", proto = "tcp", flags = "PSH,ACK", port = 5432, len_gt = 1400, nth = 2, label = "slow")`)

	r, err := pf.Compile(nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r.Action != netfault.ActionDelay {
		t.Errorf("Action = %s, want delay", r.Action)
	}
	if r.Delay != 250*time.Millisecond {
		t.Errorf("Delay = %v", r.Delay)
	}
	if r.Label != "slow" {
		t.Errorf("Label = %q", r.Label)
	}
	if r.Trigger != netfault.TriggerNth || r.TriggerN != 2 {
		t.Errorf("Trigger = %v/%d, want nth/2", r.Trigger, r.TriggerN)
	}
	if r.Match.Dir == nil || *r.Match.Dir != netfault.DirC2S {
		t.Error("Dir not compiled")
	}
	if r.Match.Proto != netfault.ProtoTCP {
		t.Errorf("Proto = %q", r.Match.Proto)
	}
	if r.Match.FlagsSet != netfault.FlagPSH|netfault.FlagACK {
		t.Errorf("FlagsSet = %v", netfault.FlagNames(r.Match.FlagsSet))
	}
	if r.Match.Port != 5432 {
		t.Errorf("Port = %d", r.Match.Port)
	}
	if !r.Match.HasLenGT || r.Match.LenGT != 1400 {
		t.Errorf("LenGT = %d (has=%v)", r.Match.LenGT, r.Match.HasLenGT)
	}
}

func TestPacketFaultCompileDefaults(t *testing.T) {
	// duplicate without count → 2 (delivered twice), per the documented default.
	r, err := mustPacketBuiltin(t, `packet_duplicate()`).Compile(nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r.DuplicateCount != 2 {
		t.Errorf("DuplicateCount = %d, want default 2", r.DuplicateCount)
	}

	// corrupt without mode/checksum → flip + fix.
	r, err = mustPacketBuiltin(t, `packet_corrupt()`).Compile(nil, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r.CorruptMode != netfault.CorruptFlip {
		t.Errorf("CorruptMode = %q, want flip", r.CorruptMode)
	}
	if r.Checksum != netfault.ChecksumFix {
		t.Errorf("Checksum = %q, want fix", r.Checksum)
	}
	if r.CorruptLength != 1 {
		t.Errorf("CorruptLength = %d, want 1", r.CorruptLength)
	}
}

// TestPacketFaultRejectsResetOnUDP proves validation runs at spec load, not at
// packet time where nothing would surface it.
func TestPacketFaultRejectsResetOnUDP(t *testing.T) {
	_, err := evalPacketBuiltin(t, `packet_reset(proto = "udp")`)
	if err == nil {
		t.Fatal("packet_reset(proto=\"udp\") was accepted")
	}
	if !strings.Contains(err.Error(), "only meaningful for tcp") {
		t.Errorf("error = %v", err)
	}
}

// ─── the Packet value ──────────────────────────────────────────────────────

func TestStarlarkPacketFields(t *testing.T) {
	dir := netfault.DirC2S
	pv := &netfault.PacketView{
		Proto: netfault.ProtoTCP, Dir: dir,
		SrcIP: "10.0.0.2", DstIP: "10.99.0.5",
		SrcPort: 40000, DstPort: 8080,
		PayloadLen: 5, Payload: []byte("hello"),
		Flags: netfault.FlagPSH | netfault.FlagACK,
		Seq:   1000, Ack: 2000, Window: 65535,
		Index: 7, Flow: "tcp|a-b",
	}
	p := newStarlarkPacket(pv)

	checks := map[string]string{
		"proto": `"tcp"`, "dir": `"c2s"`,
		"src_ip": `"10.0.0.2"`, "dst_ip": `"10.99.0.5"`,
		"src_port": "40000", "dst_port": "8080",
		"len": "5", "seq": "1000", "ack": "2000", "window": "65535",
		"index": "7", "flow": `"tcp|a-b"`,
	}
	for name, want := range checks {
		v, err := p.Attr(name)
		if err != nil {
			t.Errorf("Attr(%q): %v", name, err)
			continue
		}
		if v == nil {
			t.Errorf("Attr(%q) = nil", name)
			continue
		}
		if got := v.String(); got != want {
			t.Errorf("pkt.%s = %s, want %s", name, got, want)
		}
	}

	flags, _ := p.Attr("flags")
	if flags.String() != `["PSH", "ACK"]` {
		t.Errorf("pkt.flags = %s, want [\"PSH\", \"ACK\"]", flags.String())
	}
	// payload is a Starlark *string*, not bytes. Starlark's bytes type has
	// exactly one method (elems()), so `p.payload.startswith(...)` — how the
	// RFC's headline example was written — would fail on bytes. go.starlark.net
	// strings are arbitrary byte sequences, so String is binary-safe and
	// carries the full method set.
	payload, _ := p.Attr("payload")
	if s, ok := payload.(starlark.String); !ok || string(s) != "hello" {
		t.Errorf("pkt.payload = %v (%s), want the string \"hello\"", payload, payload.Type())
	}
	// payload_bytes remains for slicing / elems().
	pb, _ := p.Attr("payload_bytes")
	if b, ok := pb.(starlark.Bytes); !ok || string(b) != "hello" {
		t.Errorf("pkt.payload_bytes = %v, want b\"hello\"", pb)
	}
}

// TestPayloadSupportsStringMethods is the regression guard for the bug this
// DSL review found: the RFC's own headline example silently matched nothing
// because Starlark bytes has no startswith.
func TestPayloadSupportsStringMethods(t *testing.T) {
	rt := New(testLogger())
	th := &starlark.Thread{Name: "test"}

	cases := []struct {
		expr    string
		payload []byte
		want    bool
	}{
		{`packet_drop(where = lambda p: p.payload.startswith("\x00\x00\x00"))`, []byte{0, 0, 0, 9}, true},
		{`packet_drop(where = lambda p: p.payload.startswith("\x00\x00\x00"))`, []byte("GET /"), false},
		{`packet_drop(where = lambda p: p.payload.startswith("GET "))`, []byte("GET /x"), true},
		{`packet_drop(where = lambda p: "admin" in p.payload)`, []byte("user=admin"), true},
		{`packet_drop(where = lambda p: p.payload.endswith("\r\n\r\n"))`, []byte("GET /\r\n\r\n"), true},
		{`packet_drop(where = lambda p: p.payload_bytes[0:3] == b"\x00\x00\x00")`, []byte{0, 0, 0, 1}, true},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			v, err := starlark.Eval(th, "t.star", c.expr, rt.builtins())
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			var predErr error
			r, err := v.(*PacketFaultDef).Compile(th, func(e error) { predErr = e })
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got := r.Match.Where(&netfault.PacketView{Payload: c.payload, PayloadLen: len(c.payload)})
			if predErr != nil {
				t.Fatalf("predicate errored (it must not): %v", predErr)
			}
			if got != c.want {
				t.Errorf("= %v, want %v", got, c.want)
			}
		})
	}
}

// TestWherePredicateErrorIsReported: a lambda that throws matches nothing, so
// the fault never fires and the test would pass. It must be reported.
func TestWherePredicateErrorIsReported(t *testing.T) {
	rt := New(testLogger())
	th := &starlark.Thread{Name: "test"}
	v, err := starlark.Eval(th, "t.star",
		`packet_drop(where = lambda p: p.no_such_field == 1)`, rt.builtins())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	var reported error
	r, err := v.(*PacketFaultDef).Compile(th, func(e error) { reported = e })
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if r.Match.Where(&netfault.PacketView{Proto: netfault.ProtoTCP}) {
		t.Error("a predicate that errored was treated as a match")
	}
	if reported == nil {
		t.Fatal("predicate failure was swallowed; the fault would silently never fire")
	}
	if !strings.Contains(reported.Error(), "where=") {
		t.Errorf("report should name the where= predicate, got: %v", reported)
	}
}

func TestWherePredicateNonBoolIsReported(t *testing.T) {
	rt := New(testLogger())
	th := &starlark.Thread{Name: "test"}
	v, _ := starlark.Eval(th, "t.star", `packet_drop(where = lambda p: "yes")`, rt.builtins())
	var reported error
	r, err := v.(*PacketFaultDef).Compile(th, func(e error) { reported = e })
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if r.Match.Where(&netfault.PacketView{}) {
		t.Error("non-bool return was treated as a match")
	}
	if reported == nil || !strings.Contains(reported.Error(), "want a bool") {
		t.Errorf("non-bool return not reported clearly, got: %v", reported)
	}
}

// TestStarlarkPacketUnknownFieldErrors: a typo must name itself, not silently
// evaluate to None and match nothing.
func TestStarlarkPacketUnknownFieldErrors(t *testing.T) {
	p := newStarlarkPacket(&netfault.PacketView{Proto: netfault.ProtoTCP})
	v, err := p.Attr("payloadd")
	if err != nil {
		return // an explicit error is fine too
	}
	if v != nil {
		t.Errorf("pkt.payloadd returned %v; a typo must not resolve", v)
	}
}

func TestWherePredicateInvokedFromStarlark(t *testing.T) {
	rt := New(testLogger())
	thread := &starlark.Thread{Name: "test"}
	v, err := starlark.Eval(thread, "test.star",
		`packet_drop(where = lambda p: p.len > 10 and p.dir == "c2s")`, rt.builtins())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	pf := v.(*PacketFaultDef)

	r, err := pf.Compile(thread, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if r.Match.Where == nil {
		t.Fatal("Where predicate was not compiled")
	}

	big := &netfault.PacketView{Dir: netfault.DirC2S, PayloadLen: 100}
	small := &netfault.PacketView{Dir: netfault.DirC2S, PayloadLen: 1}
	wrongDir := &netfault.PacketView{Dir: netfault.DirS2C, PayloadLen: 100}

	if !r.Match.Where(big) {
		t.Error("predicate rejected a packet it should match")
	}
	if r.Match.Where(small) {
		t.Error("predicate matched a short packet")
	}
	if r.Match.Where(wrongDir) {
		t.Error("predicate matched the wrong direction")
	}
}
