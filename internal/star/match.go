package star

import (
	"fmt"
	"strings"

	"go.starlark.net/starlark"
)

// MatcherVal is a Starlark-accessible event matcher created by the match.*
// builtins. It wraps a Go predicate over Event so it can be stored in
// monitor(on=...), await_event(...), and await_stable(ignore=...) kwargs.
type MatcherVal struct {
	name    string
	matchFn func(Event) bool
}

var _ starlark.Value = (*MatcherVal)(nil)

func (m *MatcherVal) String() string        { return fmt.Sprintf("<matcher %s>", m.name) }
func (m *MatcherVal) Type() string          { return "matcher" }
func (m *MatcherVal) Freeze()               {}
func (m *MatcherVal) Truth() starlark.Bool  { return true }
func (m *MatcherVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: matcher") }

// Matches reports whether ev satisfies this matcher. Safe on nil receiver.
func (m *MatcherVal) Matches(ev Event) bool {
	if m == nil || m.matchFn == nil {
		return false
	}
	return m.matchFn(ev)
}

// matchNamespace is the `match` value exposed in Starlark globals.
// It is a HasAttrs so users write match.event(...), match.any(...), etc.
type matchNamespace struct{}

var _ starlark.HasAttrs = (*matchNamespace)(nil)

func (*matchNamespace) String() string        { return "<match>" }
func (*matchNamespace) Type() string          { return "match_namespace" }
func (*matchNamespace) Freeze()               {}
func (*matchNamespace) Truth() starlark.Bool  { return true }
func (*matchNamespace) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: match") }
func (*matchNamespace) AttrNames() []string {
	return []string{"all", "any", "event", "never"}
}

func (m *matchNamespace) Attr(name string) (starlark.Value, error) {
	switch name {
	case "event":
		return starlark.NewBuiltin("match.event", builtinMatchEvent), nil
	case "any":
		return starlark.NewBuiltin("match.any", builtinMatchAny), nil
	case "all":
		return starlark.NewBuiltin("match.all", builtinMatchAll), nil
	case "never":
		return starlark.NewBuiltin("match.never", builtinMatchNever), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("match has no attribute %q", name))
}

// match.event(type=..., service=..., **fields) — matches events whose type,
// service, and field values all satisfy the given patterns. Patterns may
// include a trailing '*' wildcard; empty string matches anything.
func builtinMatchEvent(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("match.event() takes only keyword arguments; got %d positional", len(args))
	}
	criteria := make(map[string]string, len(kwargs))
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		val, _ := starlark.AsString(kv[1])
		criteria[key] = val
	}
	name := "event(" + fmtCriteria(criteria) + ")"
	return &MatcherVal{
		name:    name,
		matchFn: func(ev Event) bool { return matchEventCriteria(ev, criteria) },
	}, nil
}

// match.any(*matchers) — OR composition; matches if any sub-matcher matches.
func builtinMatchAny(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) != 0 {
		return nil, fmt.Errorf("match.any() takes only positional arguments")
	}
	matchers, err := parseMatcherArgs("match.any", args)
	if err != nil {
		return nil, err
	}
	return &MatcherVal{
		name: "any(...)",
		matchFn: func(ev Event) bool {
			for _, m := range matchers {
				if m.Matches(ev) {
					return true
				}
			}
			return false
		},
	}, nil
}

// match.all(*matchers) — AND composition; matches if every sub-matcher matches.
// With no arguments, matches every event (identity for AND).
func builtinMatchAll(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) != 0 {
		return nil, fmt.Errorf("match.all() takes only positional arguments")
	}
	matchers, err := parseMatcherArgs("match.all", args)
	if err != nil {
		return nil, err
	}
	return &MatcherVal{
		name: "all(...)",
		matchFn: func(ev Event) bool {
			for _, m := range matchers {
				if !m.Matches(ev) {
					return false
				}
			}
			return true
		},
	}, nil
}

// match.never() — never matches any event (useful for testing / disabling monitors).
func builtinMatchNever(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 0 || len(kwargs) != 0 {
		return nil, fmt.Errorf("match.never() takes no arguments")
	}
	return &MatcherVal{name: "never", matchFn: func(Event) bool { return false }}, nil
}

// matchEventCriteria checks ev against a map of field-name → pattern pairs.
// Special keys: "type" checks ev.Type, "service" checks ev.Service; all
// other keys look up ev.Fields[key]. Patterns support trailing '*' wildcard.
func matchEventCriteria(ev Event, criteria map[string]string) bool {
	for key, pattern := range criteria {
		var actual string
		switch key {
		case "type":
			actual = ev.Type
		case "service":
			actual = ev.Service
		default:
			actual = ev.Fields[key]
		}
		if !matchValue(actual, pattern) {
			return false
		}
	}
	return true
}

// matcherOrPredFromArg converts a Starlark value to either a *MatcherVal
// (direct) or a synthetic matcher that calls a Starlark callable with an
// EventVal. Returns an error if the value is neither.
func matcherOrPredFromArg(arg starlark.Value) (*MatcherVal, error) {
	switch v := arg.(type) {
	case *MatcherVal:
		return v, nil
	case starlark.Callable:
		return &MatcherVal{
			name: "predicate",
			matchFn: func(ev Event) bool {
				t := &starlark.Thread{Name: "matcher-pred"}
				res, err := starlark.Call(t, v, starlark.Tuple{newEventVal(ev, nil)}, nil)
				if err != nil {
					return false
				}
				return res.Truth() == starlark.True
			},
		}, nil
	}
	return nil, fmt.Errorf("expected matcher or callable, got %s", arg.Type())
}

func parseMatcherArgs(name string, args starlark.Tuple) ([]*MatcherVal, error) {
	matchers := make([]*MatcherVal, 0, len(args))
	for _, arg := range args {
		m, ok := arg.(*MatcherVal)
		if !ok {
			return nil, fmt.Errorf("%s() arguments must be matchers, got %s", name, arg.Type())
		}
		matchers = append(matchers, m)
	}
	return matchers, nil
}

func fmtCriteria(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, ", ")
}
