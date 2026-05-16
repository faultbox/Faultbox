package star

import (
	"fmt"

	"go.starlark.net/starlark"
)

// ChoiceVal is the Starlark value returned by choose(). Plan-tree
// enumeration (RFC-042 + RFC-043 §8.5, deferred to rc2) inspects this
// value to fan out the plan over the option set. In v0.13.0-rc1 the
// value flattens to Options[0] when used in arithmetic / equality /
// indexing — so existing specs that previously hard-coded the first
// option get the same behavior, and the fan-out lands additively in
// rc2 without breaking the user-facing surface.
//
// The Name field is optional and matches RFC-043 §5.2's two-arity
// form `choose("retries", [0,1,3])`. When empty, the plan report
// labels the axis by the call site.
type ChoiceVal struct {
	Name    string
	Options []starlark.Value
}

var _ starlark.Value = (*ChoiceVal)(nil)

// FirstOption returns the option chosen for runtime execution under
// rc1's degenerate-single-leaf semantics. rc2 will reassign this per
// plan-tree leaf when fan-out lands.
func (c *ChoiceVal) FirstOption() starlark.Value {
	if len(c.Options) == 0 {
		return starlark.None
	}
	return c.Options[0]
}

func (c *ChoiceVal) String() string {
	if c.Name != "" {
		return fmt.Sprintf("<choice %s [%d]>", c.Name, len(c.Options))
	}
	return fmt.Sprintf("<choice [%d]>", len(c.Options))
}
func (c *ChoiceVal) Type() string { return "choice" }
func (c *ChoiceVal) Freeze() {
	for _, o := range c.Options {
		o.Freeze()
	}
}
func (c *ChoiceVal) Truth() starlark.Bool {
	// A choice with at least one option is truthy. Callers that want
	// the boolean semantics of `nondet()` should compare against the
	// first option explicitly — once rc2 wires fan-out, the value will
	// be the per-leaf option directly, not the ChoiceVal wrapper.
	return starlark.Bool(len(c.Options) > 0)
}
func (c *ChoiceVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: choice") }

// builtinChoose implements RFC-043 §5.2:
//
//	choose([opt0, opt1, ...])           — N-way choice
//	choose("retries", [0, 1, 3])        — named two-arity form
//
// The options list must be statically evaluable — a Starlark list of
// literal values. Runtime-computed elements are rejected at spec load
// per the RFC's "no symbolic ranges" stance.
//
// rc1 semantics: returns a ChoiceVal whose runtime usage flattens to
// the first option. rc2 will wire body re-execution so each plan-tree
// leaf observes a different option.
func (rt *Runtime) builtinChoose(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) > 0 {
		return nil, fmt.Errorf("choose() takes only positional arguments")
	}
	var name string
	var optList *starlark.List
	switch len(args) {
	case 1:
		l, ok := args[0].(*starlark.List)
		if !ok {
			return nil, fmt.Errorf("choose(options): options must be a list, got %s", args[0].Type())
		}
		optList = l
	case 2:
		s, ok := args[0].(starlark.String)
		if !ok {
			return nil, fmt.Errorf("choose(name, options): name must be a string, got %s", args[0].Type())
		}
		l, ok := args[1].(*starlark.List)
		if !ok {
			return nil, fmt.Errorf("choose(name, options): options must be a list, got %s", args[1].Type())
		}
		name = string(s)
		optList = l
	default:
		return nil, fmt.Errorf("choose() takes 1 or 2 arguments (options) or (name, options); got %d", len(args))
	}

	if optList.Len() == 0 {
		return nil, fmt.Errorf("choose(): options list must be non-empty")
	}

	c := &ChoiceVal{Name: name, Options: make([]starlark.Value, 0, optList.Len())}
	it := optList.Iterate()
	defer it.Done()
	var v starlark.Value
	for it.Next(&v) {
		c.Options = append(c.Options, v)
	}
	rt.recordChoice(c)
	// rc1 single-leaf: return the first option directly so users get
	// concrete values (an int from `choose([0,1,3])` instead of a
	// wrapper). When rc2 wires fan-out, callers will receive the
	// leaf-selected option through the same return path — no spec
	// changes required at the call site.
	return c.FirstOption(), nil
}

// recordChoice tracks every choose() call site for plan-tree
// enumeration. Used in PR 5 to expose the choices as composition
// axes; in rc1 it's collected but not yet rendered by the plan
// command (the integration arrives with §8.5 in rc2).
func (rt *Runtime) recordChoice(c *ChoiceVal) {
	rt.choicesMu.Lock()
	defer rt.choicesMu.Unlock()
	rt.choices = append(rt.choices, c)
}

// Choices returns a snapshot of every recorded choose() call. Used by
// the plan-tree enumerator and tests; safe to call after LoadFile and
// before concurrent RunAll (same contract as the other read-only
// runtime accessors).
func (rt *Runtime) Choices() []*ChoiceVal {
	rt.choicesMu.Lock()
	defer rt.choicesMu.Unlock()
	out := make([]*ChoiceVal, len(rt.choices))
	copy(out, rt.choices)
	return out
}
