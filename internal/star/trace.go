package star

import (
	"fmt"
	"time"

	"go.starlark.net/starlark"
)

// TraceVal is the `trace` object passed to predicate lambdas in
// eventually(), always(), await_event(), and monitor(check/update).
// It exposes the event log query API defined in RFC-041 §8.5.
type TraceVal struct {
	log *EventLog
}

var _ starlark.HasAttrs = (*TraceVal)(nil)

func (t *TraceVal) String() string        { return "<trace>" }
func (t *TraceVal) Type() string          { return "trace" }
func (t *TraceVal) Freeze()               {}
func (t *TraceVal) Truth() starlark.Bool  { return true }
func (t *TraceVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: trace") }
func (t *TraceVal) AttrNames() []string {
	return []string{"causal_chain", "count", "event", "events", "events_between", "events_within", "first", "last"}
}

func (t *TraceVal) Attr(name string) (starlark.Value, error) {
	switch name {
	case "event":
		return starlark.NewBuiltin("trace.event", t.traceEvent), nil
	case "events":
		return starlark.NewBuiltin("trace.events", t.traceEvents), nil
	case "first":
		return starlark.NewBuiltin("trace.first", t.traceFirst), nil
	case "last":
		return starlark.NewBuiltin("trace.last", t.traceLast), nil
	case "count":
		return starlark.NewBuiltin("trace.count", t.traceCount), nil
	case "events_between":
		return starlark.NewBuiltin("trace.events_between", t.traceEventsBetween), nil
	case "events_within":
		return starlark.NewBuiltin("trace.events_within", t.traceEventsWithin), nil
	case "causal_chain":
		return starlark.NewBuiltin("trace.causal_chain", t.traceCausalChain), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("trace has no attribute %q", name))
}

// trace.event(type=..., **fields) or trace.event(matcher)
// Returns the most recent matching event (or None).
func (t *TraceVal) traceEvent(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, err := argsToMatcher("trace.event", args, kwargs)
	if err != nil {
		return nil, err
	}
	ev, ok := t.log.LastMatching(m)
	if !ok {
		return starlark.None, nil
	}
	return newEventVal(ev, t.log), nil
}

// trace.events(matcher) or trace.events(type=..., **fields)
// Returns all matching events in emission order as an EventListVal.
func (t *TraceVal) traceEvents(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, err := argsToMatcher("trace.events", args, kwargs)
	if err != nil {
		return nil, err
	}
	evs := t.log.MatchingEvents(m)
	return newEventListVal(evs, t.log), nil
}

// trace.first(matcher) — earliest matching event, or None.
func (t *TraceVal) traceFirst(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, err := argsToMatcher("trace.first", args, kwargs)
	if err != nil {
		return nil, err
	}
	ev, ok := t.log.FirstMatching(m)
	if !ok {
		return starlark.None, nil
	}
	return newEventVal(ev, t.log), nil
}

// trace.last(matcher) — most recent matching event, or None.
func (t *TraceVal) traceLast(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, err := argsToMatcher("trace.last", args, kwargs)
	if err != nil {
		return nil, err
	}
	ev, ok := t.log.LastMatching(m)
	if !ok {
		return starlark.None, nil
	}
	return newEventVal(ev, t.log), nil
}

// trace.count(matcher) — integer count of matching events.
func (t *TraceVal) traceCount(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, err := argsToMatcher("trace.count", args, kwargs)
	if err != nil {
		return nil, err
	}
	return starlark.MakeInt(t.log.CountMatching(m)), nil
}

// trace.events_between(start, end) — events between two EventVal anchors.
// start and end may be EventVal or starlark.None (open interval).
func (t *TraceVal) traceEventsBetween(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var startV, endV starlark.Value = starlark.None, starlark.None
	if err := starlark.UnpackPositionalArgs("trace.events_between", args, kwargs, 2, &startV, &endV); err != nil {
		return nil, err
	}
	startSeq := int64(-1)
	if ev, ok := startV.(*EventVal); ok {
		startSeq = ev.ev.Seq
	}
	endSeq := int64(-1)
	if ev, ok := endV.(*EventVal); ok {
		endSeq = ev.ev.Seq
	}
	all := t.log.Events()
	var out []Event
	for _, ev := range all {
		if startSeq >= 0 && ev.Seq <= startSeq {
			continue
		}
		if endSeq >= 0 && ev.Seq >= endSeq {
			break
		}
		out = append(out, ev)
	}
	return newEventListVal(out, t.log), nil
}

// trace.events_within(matcher, window, of=event) — events matching within a
// duration window relative to the anchor event.
func (t *TraceVal) traceEventsWithin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var matcherArg starlark.Value
	var windowStr string
	var ofArg starlark.Value = starlark.None
	if err := starlark.UnpackArgs("trace.events_within", args, kwargs,
		"matcher", &matcherArg,
		"window", &windowStr,
		"of?", &ofArg,
	); err != nil {
		return nil, err
	}
	m, err := matcherOrPredFromArg(matcherArg)
	if err != nil {
		return nil, fmt.Errorf("trace.events_within(): %w", err)
	}
	window, err := parseStarDuration(windowStr)
	if err != nil {
		return nil, fmt.Errorf("trace.events_within() bad window: %w", err)
	}
	var anchor time.Time
	if ev, ok := ofArg.(*EventVal); ok {
		anchor = ev.ev.Timestamp
	}
	all := t.log.Events()
	var out []Event
	for _, ev := range all {
		if !m.Matches(ev) {
			continue
		}
		if !anchor.IsZero() && absDuration(ev.Timestamp.Sub(anchor)) > window {
			continue
		}
		out = append(out, ev)
	}
	return newEventListVal(out, t.log), nil
}

// trace.causal_chain(event) — all events causally preceding event
// (those with strictly smaller vector clocks on the event's service).
func (t *TraceVal) traceCausalChain(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var evArg starlark.Value
	if err := starlark.UnpackPositionalArgs("trace.causal_chain", args, kwargs, 1, &evArg); err != nil {
		return nil, err
	}
	ev, ok := evArg.(*EventVal)
	if !ok {
		return nil, fmt.Errorf("trace.causal_chain(): argument must be an event, got %s", evArg.Type())
	}
	all := t.log.Events()
	var out []Event
	for _, candidate := range all {
		if candidate.Seq >= ev.ev.Seq {
			break
		}
		if vcHappensBefore(candidate.VectorClock, ev.ev.VectorClock) {
			out = append(out, candidate)
		}
	}
	return newEventListVal(out, t.log), nil
}

// ---------------------------------------------------------------------------
// EventVal — Starlark wrapper around a single Event.
// ---------------------------------------------------------------------------

// EventVal is a Starlark value exposing a single Event's fields and causal
// operators. Returned by trace.event(), trace.first(), trace.last(), etc.
type EventVal struct {
	ev  Event
	log *EventLog // for causal queries; may be nil in tests
}

var _ starlark.HasAttrs = (*EventVal)(nil)

func newEventVal(ev Event, log *EventLog) *EventVal { return &EventVal{ev: ev, log: log} }

func (e *EventVal) String() string {
	return fmt.Sprintf("<event seq=%d type=%s service=%s>", e.ev.Seq, e.ev.Type, e.ev.Service)
}
func (e *EventVal) Type() string          { return "event" }
func (e *EventVal) Freeze()               {}
func (e *EventVal) Truth() starlark.Bool  { return true }
func (e *EventVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: event") }
func (e *EventVal) AttrNames() []string {
	return []string{
		"concurrent_with", "directly_caused_by", "duration_since",
		"followed_by", "followed_by_within",
		"happens_after", "happens_before",
		"preceded_by", "preceded_by_within",
		"same_correlation_as", "same_service_as",
		"seq", "service", "timestamp", "type",
	}
}

func (e *EventVal) Attr(name string) (starlark.Value, error) {
	switch name {
	// Plain field accessors.
	case "type":
		return starlark.String(e.ev.Type), nil
	case "service":
		return starlark.String(e.ev.Service), nil
	case "seq":
		return starlark.MakeInt64(e.ev.Seq), nil
	case "timestamp":
		return starlark.String(e.ev.Timestamp.Format(time.RFC3339Nano)), nil

	// Method accessors — return Starlark builtins bound to this event.
	case "happens_before":
		return starlark.NewBuiltin("event.happens_before", e.happensBefore), nil
	case "happens_after":
		return starlark.NewBuiltin("event.happens_after", e.happensAfter), nil
	case "concurrent_with":
		return starlark.NewBuiltin("event.concurrent_with", e.concurrentWith), nil
	case "same_service_as":
		return starlark.NewBuiltin("event.same_service_as", e.sameServiceAs), nil
	case "same_correlation_as":
		return starlark.NewBuiltin("event.same_correlation_as", e.sameCorrelationAs), nil
	case "duration_since":
		return starlark.NewBuiltin("event.duration_since", e.durationSince), nil
	case "preceded_by":
		return starlark.NewBuiltin("event.preceded_by", e.precededBy), nil
	case "followed_by":
		return starlark.NewBuiltin("event.followed_by", e.followedBy), nil
	case "preceded_by_within":
		return starlark.NewBuiltin("event.preceded_by_within", e.precededByWithin), nil
	case "followed_by_within":
		return starlark.NewBuiltin("event.followed_by_within", e.followedByWithin), nil
	case "directly_caused_by":
		return starlark.NewBuiltin("event.directly_caused_by", e.directlyCausedBy), nil
	}

	// Field lookup for any other key.
	if v, ok := e.ev.Fields[name]; ok {
		return starlark.String(v), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("event has no attribute %q", name))
}

// event.happens_before(other) — A → B in vector-clock partial order.
func (e *EventVal) happensBefore(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.happens_before", args)
	if err != nil {
		return nil, err
	}
	return starlark.Bool(vcHappensBefore(e.ev.VectorClock, other.ev.VectorClock)), nil
}

// event.happens_after(other) — B → A.
func (e *EventVal) happensAfter(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.happens_after", args)
	if err != nil {
		return nil, err
	}
	return starlark.Bool(vcHappensBefore(other.ev.VectorClock, e.ev.VectorClock)), nil
}

// event.concurrent_with(other) — neither A → B nor B → A.
func (e *EventVal) concurrentWith(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.concurrent_with", args)
	if err != nil {
		return nil, err
	}
	ab := vcHappensBefore(e.ev.VectorClock, other.ev.VectorClock)
	ba := vcHappensBefore(other.ev.VectorClock, e.ev.VectorClock)
	return starlark.Bool(!ab && !ba), nil
}

// event.same_service_as(other).
func (e *EventVal) sameServiceAs(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.same_service_as", args)
	if err != nil {
		return nil, err
	}
	return starlark.Bool(e.ev.Service != "" && e.ev.Service == other.ev.Service), nil
}

// event.same_correlation_as(other) — same correlation ID in fields.
func (e *EventVal) sameCorrelationAs(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.same_correlation_as", args)
	if err != nil {
		return nil, err
	}
	corr := e.ev.Fields["correlation_id"]
	otherCorr := other.ev.Fields["correlation_id"]
	return starlark.Bool(corr != "" && corr == otherCorr), nil
}

// event.duration_since(other) — elapsed time as a string like "1.5s".
func (e *EventVal) durationSince(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	other, err := unpackEventArg("event.duration_since", args)
	if err != nil {
		return nil, err
	}
	d := e.ev.Timestamp.Sub(other.ev.Timestamp)
	return starlark.String(d.String()), nil
}

// event.preceded_by(matcher) — some earlier event in the log matches.
func (e *EventVal) precededBy(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	m, err := unpackMatcherArg("event.preceded_by", args)
	if err != nil {
		return nil, err
	}
	if e.log == nil {
		return starlark.False, nil
	}
	all := e.log.Events()
	for _, ev := range all {
		if ev.Seq >= e.ev.Seq {
			break
		}
		if m.Matches(ev) {
			return starlark.True, nil
		}
	}
	return starlark.False, nil
}

// event.followed_by(matcher) — some later event in the log matches.
func (e *EventVal) followedBy(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	m, err := unpackMatcherArg("event.followed_by", args)
	if err != nil {
		return nil, err
	}
	if e.log == nil {
		return starlark.False, nil
	}
	all := e.log.Events()
	for i := len(all) - 1; i >= 0; i-- {
		ev := all[i]
		if ev.Seq <= e.ev.Seq {
			break
		}
		if m.Matches(ev) {
			return starlark.True, nil
		}
	}
	return starlark.False, nil
}

// event.preceded_by_within(matcher, window) — earlier event within a duration.
func (e *EventVal) precededByWithin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, window, err := unpackMatcherWindow("event.preceded_by_within", args, kwargs)
	if err != nil {
		return nil, err
	}
	if e.log == nil {
		return starlark.False, nil
	}
	all := e.log.Events()
	for _, ev := range all {
		if ev.Seq >= e.ev.Seq {
			break
		}
		if m.Matches(ev) && e.ev.Timestamp.Sub(ev.Timestamp) <= window {
			return starlark.True, nil
		}
	}
	return starlark.False, nil
}

// event.followed_by_within(matcher, window) — later event within a duration.
func (e *EventVal) followedByWithin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	m, window, err := unpackMatcherWindow("event.followed_by_within", args, kwargs)
	if err != nil {
		return nil, err
	}
	if e.log == nil {
		return starlark.False, nil
	}
	all := e.log.Events()
	for _, ev := range all {
		if ev.Seq <= e.ev.Seq {
			continue
		}
		if m.Matches(ev) && ev.Timestamp.Sub(e.ev.Timestamp) <= window {
			return starlark.True, nil
		}
	}
	return starlark.False, nil
}

// event.directly_caused_by(matcher) — direct causal predecessor (immediate
// predecessor in vector-clock graph that matches).
func (e *EventVal) directlyCausedBy(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
	m, err := unpackMatcherArg("event.directly_caused_by", args)
	if err != nil {
		return nil, err
	}
	if e.log == nil {
		return starlark.False, nil
	}
	all := e.log.Events()
	// Walk backwards to find the most recent event that causally precedes e.
	for i := len(all) - 1; i >= 0; i-- {
		ev := all[i]
		if ev.Seq >= e.ev.Seq {
			continue
		}
		if vcHappensBefore(ev.VectorClock, e.ev.VectorClock) && m.Matches(ev) {
			return starlark.True, nil
		}
	}
	return starlark.False, nil
}

// ---------------------------------------------------------------------------
// EventListVal — Starlark sequence wrapping []Event with functional methods.
// ---------------------------------------------------------------------------

// EventListVal is returned by trace.events(), trace.events_between(), etc.
// It implements starlark.Sequence and HasAttrs to expose .map(), .filter(),
// .reduce(), .sum(), .first(), .last(), .count() as chainable methods.
type EventListVal struct {
	events []Event
	log    *EventLog
}

var _ starlark.Sequence = (*EventListVal)(nil)
var _ starlark.HasAttrs = (*EventListVal)(nil)

func newEventListVal(evs []Event, log *EventLog) *EventListVal {
	return &EventListVal{events: evs, log: log}
}

func (l *EventListVal) String() string        { return fmt.Sprintf("<events count=%d>", len(l.events)) }
func (l *EventListVal) Type() string          { return "events" }
func (l *EventListVal) Freeze()               {}
func (l *EventListVal) Truth() starlark.Bool  { return starlark.Bool(len(l.events) > 0) }
func (l *EventListVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: events") }
func (l *EventListVal) Len() int              { return len(l.events) }

func (l *EventListVal) Iterate() starlark.Iterator {
	vals := make([]starlark.Value, len(l.events))
	for i, ev := range l.events {
		vals[i] = newEventVal(ev, l.log)
	}
	return &sliceIterator{vals: vals}
}

func (l *EventListVal) AttrNames() []string {
	return []string{"count", "filter", "first", "last", "map", "reduce", "sum"}
}

func (l *EventListVal) Attr(name string) (starlark.Value, error) {
	switch name {
	case "map":
		return starlark.NewBuiltin("events.map", l.listMap), nil
	case "filter":
		return starlark.NewBuiltin("events.filter", l.listFilter), nil
	case "reduce":
		return starlark.NewBuiltin("events.reduce", l.listReduce), nil
	case "sum":
		return starlark.NewBuiltin("events.sum", l.listSum), nil
	case "first":
		if len(l.events) == 0 {
			return starlark.None, nil
		}
		return newEventVal(l.events[0], l.log), nil
	case "last":
		if len(l.events) == 0 {
			return starlark.None, nil
		}
		return newEventVal(l.events[len(l.events)-1], l.log), nil
	case "count":
		return starlark.MakeInt(len(l.events)), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("events has no attribute %q", name))
}

func (l *EventListVal) listMap(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fn starlark.Callable
	if err := starlark.UnpackPositionalArgs("events.map", args, kwargs, 1, &fn); err != nil {
		return nil, err
	}
	out := make([]starlark.Value, 0, len(l.events))
	for _, ev := range l.events {
		v, err := starlark.Call(thread, fn, starlark.Tuple{newEventVal(ev, l.log)}, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return starlark.NewList(out), nil
}

func (l *EventListVal) listFilter(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fn starlark.Callable
	if err := starlark.UnpackPositionalArgs("events.filter", args, kwargs, 1, &fn); err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range l.events {
		v, err := starlark.Call(thread, fn, starlark.Tuple{newEventVal(ev, l.log)}, nil)
		if err != nil {
			return nil, err
		}
		if v.Truth() == starlark.True {
			out = append(out, ev)
		}
	}
	return newEventListVal(out, l.log), nil
}

func (l *EventListVal) listReduce(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var fn starlark.Callable
	var initial starlark.Value
	if err := starlark.UnpackArgs("events.reduce", args, kwargs, "fn", &fn, "initial", &initial); err != nil {
		return nil, err
	}
	acc := initial
	var err error
	for _, ev := range l.events {
		acc, err = starlark.Call(thread, fn, starlark.Tuple{acc, newEventVal(ev, l.log)}, nil)
		if err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// events.sum() — sum the integer/float values of a mapped list (convenience).
// Expects each event to have a numeric attribute; use .map() first to extract.
func (l *EventListVal) listSum(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 0 || len(kwargs) != 0 {
		return nil, fmt.Errorf("events.sum() takes no arguments; chain .map(fn).sum() instead")
	}
	// Not useful on EventListVal directly — sum() is designed to be chained
	// after .map() which returns a plain starlark.List. Kept for API symmetry.
	return nil, fmt.Errorf("events.sum() requires mapping to a numeric field first; use .map(lambda e: int(e.field)).sum()")
}

// ---------------------------------------------------------------------------
// Vector-clock helpers.
// ---------------------------------------------------------------------------

// vcHappensBefore reports whether vc1 → vc2 in the partial order:
// for every key k, vc1[k] ≤ vc2[k], with at least one strict inequality.
func vcHappensBefore(vc1, vc2 map[string]int64) bool {
	if len(vc1) == 0 {
		return false // no information → can't establish order
	}
	strictLess := false
	for k, v1 := range vc1 {
		v2 := vc2[k]
		if v1 > v2 {
			return false // vc1 is NOT dominated by vc2 on this key
		}
		if v1 < v2 {
			strictLess = true
		}
	}
	return strictLess
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// argsToMatcher builds a MatcherVal from (positional matcher | kwargs criteria).
func argsToMatcher(name string, args starlark.Tuple, kwargs []starlark.Tuple) (*MatcherVal, error) {
	if len(args) == 1 {
		return matcherOrPredFromArg(args[0])
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("%s() takes at most one positional argument", name)
	}
	// kwargs form: build a criteria matcher.
	criteria := make(map[string]string, len(kwargs))
	for _, kv := range kwargs {
		k, _ := starlark.AsString(kv[0])
		v, _ := starlark.AsString(kv[1])
		criteria[k] = v
	}
	return &MatcherVal{
		name:    name + "(" + fmtCriteria(criteria) + ")",
		matchFn: func(ev Event) bool { return matchEventCriteria(ev, criteria) },
	}, nil
}

func unpackEventArg(name string, args starlark.Tuple) (*EventVal, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s() takes exactly one argument", name)
	}
	ev, ok := args[0].(*EventVal)
	if !ok {
		return nil, fmt.Errorf("%s() argument must be an event, got %s", name, args[0].Type())
	}
	return ev, nil
}

func unpackMatcherArg(name string, args starlark.Tuple) (*MatcherVal, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s() takes exactly one argument (a matcher)", name)
	}
	m, err := matcherOrPredFromArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("%s(): %w", name, err)
	}
	return m, nil
}

func unpackMatcherWindow(name string, args starlark.Tuple, kwargs []starlark.Tuple) (*MatcherVal, time.Duration, error) {
	var matcherArg starlark.Value
	var windowStr string
	if err := starlark.UnpackArgs(name, args, kwargs, "matcher", &matcherArg, "window", &windowStr); err != nil {
		return nil, 0, err
	}
	m, err := matcherOrPredFromArg(matcherArg)
	if err != nil {
		return nil, 0, fmt.Errorf("%s(): %w", name, err)
	}
	d, err := parseStarDuration(windowStr)
	if err != nil {
		return nil, 0, fmt.Errorf("%s() bad window: %w", name, err)
	}
	return m, d, nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// sliceIterator iterates over a pre-built []starlark.Value.
type sliceIterator struct {
	vals []starlark.Value
	idx  int
}

func (it *sliceIterator) Next(p *starlark.Value) bool {
	if it.idx >= len(it.vals) {
		return false
	}
	*p = it.vals[it.idx]
	it.idx++
	return true
}

func (it *sliceIterator) Done() {}
