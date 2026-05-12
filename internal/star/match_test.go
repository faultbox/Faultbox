package star

import (
	"testing"

	"go.starlark.net/starlark"
)

func TestMatchEvent(t *testing.T) {
	ev := Event{Type: "syscall", Service: "db", Fields: map[string]string{"syscall": "write", "decision": "allow"}}

	cases := []struct {
		name     string
		criteria map[string]string
		want     bool
	}{
		{"type match", map[string]string{"type": "syscall"}, true},
		{"type no match", map[string]string{"type": "other"}, false},
		{"service match", map[string]string{"service": "db"}, true},
		{"service no match", map[string]string{"service": "api"}, false},
		{"field match", map[string]string{"decision": "allow"}, true},
		{"field no match", map[string]string{"decision": "deny"}, false},
		{"multi match", map[string]string{"type": "syscall", "service": "db"}, true},
		{"multi partial", map[string]string{"type": "syscall", "service": "api"}, false},
		{"empty criteria matches all", map[string]string{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MatcherVal{matchFn: func(e Event) bool { return matchEventCriteria(e, tc.criteria) }}
			if got := m.Matches(ev); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchAny(t *testing.T) {
	evA := Event{Type: "A"}
	evB := Event{Type: "B"}
	evC := Event{Type: "C"}

	mA := &MatcherVal{matchFn: func(e Event) bool { return e.Type == "A" }}
	mB := &MatcherVal{matchFn: func(e Event) bool { return e.Type == "B" }}

	any, _ := builtinMatchAny(nil, nil, starlark.Tuple{mA, mB}, nil)
	m := any.(*MatcherVal)

	if !m.Matches(evA) {
		t.Error("any(A,B) should match A")
	}
	if !m.Matches(evB) {
		t.Error("any(A,B) should match B")
	}
	if m.Matches(evC) {
		t.Error("any(A,B) should not match C")
	}
}

func TestMatchAll(t *testing.T) {
	ev := Event{Type: "syscall", Service: "db"}

	mType := &MatcherVal{matchFn: func(e Event) bool { return e.Type == "syscall" }}
	mSvc := &MatcherVal{matchFn: func(e Event) bool { return e.Service == "db" }}
	mOther := &MatcherVal{matchFn: func(e Event) bool { return e.Service == "api" }}

	all, _ := builtinMatchAll(nil, nil, starlark.Tuple{mType, mSvc}, nil)
	m := all.(*MatcherVal)
	if !m.Matches(ev) {
		t.Error("all(type=syscall, service=db) should match")
	}

	allFail, _ := builtinMatchAll(nil, nil, starlark.Tuple{mType, mOther}, nil)
	if allFail.(*MatcherVal).Matches(ev) {
		t.Error("all(type=syscall, service=api) should not match db event")
	}

	// No args → matches everything.
	allEmpty, _ := builtinMatchAll(nil, nil, starlark.Tuple{}, nil)
	if !allEmpty.(*MatcherVal).Matches(ev) {
		t.Error("all() with no args should match everything")
	}
}

func TestMatchNever(t *testing.T) {
	never, _ := builtinMatchNever(nil, nil, nil, nil)
	m := never.(*MatcherVal)
	ev := Event{Type: "anything"}
	if m.Matches(ev) {
		t.Error("never() should never match")
	}
}

func TestMatcherOrPredFromArg_Matcher(t *testing.T) {
	m := &MatcherVal{matchFn: func(Event) bool { return true }}
	got, err := matcherOrPredFromArg(m)
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{Type: "x"}
	if !got.Matches(ev) {
		t.Error("should match via MatcherVal pass-through")
	}
}

func TestMatcherOrPredFromArg_InvalidType(t *testing.T) {
	_, err := matcherOrPredFromArg(starlark.String("bad"))
	if err == nil {
		t.Error("expected error for non-matcher/callable arg")
	}
}

func TestMatchBuiltinErrors(t *testing.T) {
	// match.event() rejects positional args.
	_, err := builtinMatchEvent(nil, nil, starlark.Tuple{starlark.String("x")}, nil)
	if err == nil {
		t.Error("expected error for positional arg to match.event()")
	}

	// match.any() rejects kwargs.
	_, err = builtinMatchAny(nil, nil, nil, []starlark.Tuple{{starlark.String("k"), starlark.String("v")}})
	if err == nil {
		t.Error("expected error for kwargs in match.any()")
	}

	// match.any() rejects non-matchers.
	_, err = builtinMatchAny(nil, nil, starlark.Tuple{starlark.String("not-a-matcher")}, nil)
	if err == nil {
		t.Error("expected error for non-matcher in match.any()")
	}
}
