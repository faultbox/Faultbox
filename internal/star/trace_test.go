package star

import (
	"testing"
	"time"

	"go.starlark.net/starlark"
)

func makeTestLog(events []Event) *EventLog {
	l := NewEventLog()
	for _, ev := range events {
		l.Emit(ev.Type, ev.Service, ev.Fields)
	}
	return l
}

func TestTraceEvent(t *testing.T) {
	log := NewEventLog()
	log.Emit("req", "api", map[string]string{"path": "/health"})
	log.Emit("req", "api", map[string]string{"path": "/order"})
	log.Emit("req", "db", map[string]string{"path": "/query"})

	tr := &TraceVal{log: log}

	// trace.event(type="req") should return the LAST matching event.
	v, err := tr.traceEvent(nil, nil, nil, []starlark.Tuple{
		{starlark.String("type"), starlark.String("req")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := v.(*EventVal)
	if ev.ev.Service != "db" {
		t.Errorf("expected last req event from db, got service=%q", ev.ev.Service)
	}

	// trace.event with no match returns None.
	v, err = tr.traceEvent(nil, nil, nil, []starlark.Tuple{
		{starlark.String("type"), starlark.String("nonexistent")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != starlark.None {
		t.Errorf("expected None for no match, got %v", v)
	}
}

func TestTraceFirst(t *testing.T) {
	log := NewEventLog()
	log.Emit("write", "db", nil)
	log.Emit("write", "api", nil)

	tr := &TraceVal{log: log}
	v, err := tr.traceFirst(nil, nil, starlark.Tuple{
		&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "write" }},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := v.(*EventVal)
	if ev.ev.Service != "db" {
		t.Errorf("first write should be from db, got %q", ev.ev.Service)
	}
}

func TestTraceLast(t *testing.T) {
	log := NewEventLog()
	log.Emit("write", "db", nil)
	log.Emit("write", "api", nil)

	tr := &TraceVal{log: log}
	v, err := tr.traceLast(nil, nil, starlark.Tuple{
		&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "write" }},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := v.(*EventVal)
	if ev.ev.Service != "api" {
		t.Errorf("last write should be from api, got %q", ev.ev.Service)
	}
}

func TestTraceCount(t *testing.T) {
	log := NewEventLog()
	log.Emit("write", "db", nil)
	log.Emit("read", "db", nil)
	log.Emit("write", "api", nil)

	tr := &TraceVal{log: log}
	v, err := tr.traceCount(nil, nil, starlark.Tuple{
		&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "write" }},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := v.(starlark.Int)
	if !ok {
		t.Fatalf("expected Int, got %T", v)
	}
	if n.BigInt().Int64() != 2 {
		t.Errorf("expected count=2, got %v", n)
	}
}

func TestTraceEvents(t *testing.T) {
	log := NewEventLog()
	log.Emit("err", "api", nil)
	log.Emit("ok", "api", nil)
	log.Emit("err", "db", nil)

	tr := &TraceVal{log: log}
	v, err := tr.traceEvents(nil, nil, starlark.Tuple{
		&MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "err" }},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := v.(*EventListVal)
	if list.Len() != 2 {
		t.Errorf("expected 2 err events, got %d", list.Len())
	}
}

func TestTraceEventsBetween(t *testing.T) {
	log := NewEventLog()
	log.Emit("start", "svc", nil)
	log.Emit("middle", "svc", nil)
	log.Emit("end", "svc", nil)

	all := log.Events()
	startEv := newEventVal(all[0], log)
	endEv := newEventVal(all[2], log)

	tr := &TraceVal{log: log}
	v, err := tr.traceEventsBetween(nil, nil, starlark.Tuple{startEv, endEv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := v.(*EventListVal)
	if list.Len() != 1 || list.events[0].Type != "middle" {
		t.Errorf("events_between should return only middle event, got %v", list)
	}
}

func TestEventValHappensBefore(t *testing.T) {
	// vc1 → vc2: A's clock is strictly dominated by B's.
	evA := Event{
		Type:    "a",
		Service: "svc",
		VectorClock: map[string]int64{"svc": 1},
	}
	evB := Event{
		Type:    "b",
		Service: "svc",
		VectorClock: map[string]int64{"svc": 2},
	}

	a := newEventVal(evA, nil)
	b := newEventVal(evB, nil)

	res, err := a.happensBefore(nil, nil, starlark.Tuple{b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != starlark.True {
		t.Error("A should happen before B")
	}

	res, err = b.happensBefore(nil, nil, starlark.Tuple{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != starlark.False {
		t.Error("B should not happen before A")
	}
}

func TestEventValConcurrentWith(t *testing.T) {
	evA := Event{VectorClock: map[string]int64{"x": 1, "y": 0}}
	evB := Event{VectorClock: map[string]int64{"x": 0, "y": 1}}

	a := newEventVal(evA, nil)
	b := newEventVal(evB, nil)

	res, err := a.concurrentWith(nil, nil, starlark.Tuple{b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != starlark.True {
		t.Error("concurrent events should be concurrent")
	}
}

func TestEventValDurationSince(t *testing.T) {
	now := time.Now()
	evA := Event{Timestamp: now}
	evB := Event{Timestamp: now.Add(500 * time.Millisecond)}

	a := newEventVal(evA, nil)
	b := newEventVal(evB, nil)

	// b.duration_since(a) should be ~500ms (500_000_000 ns).
	res, err := b.durationSince(nil, nil, starlark.Tuple{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ni, ok := res.(starlark.Int)
	if !ok {
		t.Fatalf("expected starlark.Int, got %T", res)
	}
	n, ok := ni.Int64()
	if !ok {
		t.Fatalf("duration overflowed int64: %v", ni)
	}
	wantMin := int64(400 * time.Millisecond)
	wantMax := int64(600 * time.Millisecond)
	if n < wantMin || n > wantMax {
		t.Errorf("expected ~500ms in nanoseconds, got %d (want %d..%d)", n, wantMin, wantMax)
	}
}

func TestEventValPrecededBy(t *testing.T) {
	log := NewEventLog()
	log.Emit("auth", "svc", nil)
	log.Emit("write", "svc", nil)

	all := log.Events()
	writeEv := newEventVal(all[1], log)

	m := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "auth" }}

	res, err := writeEv.precededBy(nil, nil, starlark.Tuple{m}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != starlark.True {
		t.Error("write should be preceded by auth")
	}

	mMiss := &MatcherVal{matchFn: func(ev Event) bool { return ev.Type == "nonexistent" }}
	res, err = writeEv.precededBy(nil, nil, starlark.Tuple{mMiss}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != starlark.False {
		t.Error("write should not be preceded by nonexistent")
	}
}

func TestEventListValMap(t *testing.T) {
	log := NewEventLog()
	log.Emit("ev", "svc", map[string]string{"val": "x"})
	log.Emit("ev", "svc", map[string]string{"val": "y"})

	list := newEventListVal(log.Events(), log)

	thread := &starlark.Thread{Name: "test"}
	fn := starlark.NewBuiltin("fn", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		ev := args[0].(*EventVal)
		return ev.Attr("val")
	})

	v, err := list.listMap(thread, nil, starlark.Tuple{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := v.(*starlark.List)
	if result.Len() != 2 {
		t.Errorf("map should return 2 elements, got %d", result.Len())
	}
}

func TestEventListValFilter(t *testing.T) {
	log := NewEventLog()
	log.Emit("write", "db", nil)
	log.Emit("read", "db", nil)
	log.Emit("write", "api", nil)

	list := newEventListVal(log.Events(), log)

	thread := &starlark.Thread{Name: "test"}
	fn := starlark.NewBuiltin("fn", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
		ev := args[0].(*EventVal)
		return starlark.Bool(ev.ev.Type == "write"), nil
	})

	v, err := list.listFilter(thread, nil, starlark.Tuple{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered := v.(*EventListVal)
	if filtered.Len() != 2 {
		t.Errorf("filter should return 2 write events, got %d", filtered.Len())
	}
}

func TestEventLogSecondaryIndexes(t *testing.T) {
	log := NewEventLog()
	log.Emit("write", "db", nil)
	log.Emit("read", "api", nil)
	log.Emit("write", "api", nil)

	writes := log.EventsByType("write")
	if len(writes) != 2 {
		t.Errorf("expected 2 write events, got %d", len(writes))
	}
	dbEvs := log.EventsByService("db")
	if len(dbEvs) != 1 {
		t.Errorf("expected 1 db event, got %d", len(dbEvs))
	}

	// After Reset, indexes are cleared.
	log.Reset()
	writes = log.EventsByType("write")
	if len(writes) != 0 {
		t.Error("indexes should be empty after Reset")
	}
}

func TestMatchNamespaceAttr(t *testing.T) {
	m := &matchNamespace{}
	for _, name := range []string{"event", "any", "all", "never"} {
		v, err := m.Attr(name)
		if err != nil {
			t.Errorf("match.%s attr error: %v", name, err)
		}
		if v == nil {
			t.Errorf("match.%s returned nil", name)
		}
	}
	_, err := m.Attr("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent attr")
	}
}

func TestVcHappensBefore(t *testing.T) {
	cases := []struct {
		name string
		vc1  map[string]int64
		vc2  map[string]int64
		want bool
	}{
		{"before", map[string]int64{"a": 1}, map[string]int64{"a": 2}, true},
		{"equal", map[string]int64{"a": 1}, map[string]int64{"a": 1}, false},
		{"after", map[string]int64{"a": 2}, map[string]int64{"a": 1}, false},
		{"concurrent", map[string]int64{"a": 1, "b": 0}, map[string]int64{"a": 0, "b": 1}, false},
		// Vector-clock semantics: an event with no recorded clock state
		// strictly precedes any event with a non-empty clock (missing keys
		// are treated as zero, and zero < v means progress on that dim).
		{"empty vc1 vs non-empty vc2", nil, map[string]int64{"a": 1}, true},
		{"both empty", nil, nil, false},
		// Asymmetric key sets: vc1 = {a:1}, vc2 = {a:1, b:5}.
		// vc1 has progressed on `a` but not on `b`; vc2 progressed on
		// both. vc1 strictly precedes vc2.
		{"asymmetric vc1 subset", map[string]int64{"a": 1}, map[string]int64{"a": 1, "b": 5}, true},
		// Reverse: vc1 has b=3 that vc2 doesn't know about. NOT before.
		{"asymmetric vc1 superset", map[string]int64{"a": 1, "b": 3}, map[string]int64{"a": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcHappensBefore(tc.vc1, tc.vc2); got != tc.want {
				t.Errorf("vcHappensBefore(%v, %v) = %v, want %v", tc.vc1, tc.vc2, got, tc.want)
			}
		})
	}
}
