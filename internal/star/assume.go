package star

import (
	"fmt"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

var _ syntax.Node = nil // syntax kept in import list; Source field uses it once §8.7 AST validation lands.

// AssumePredicate is one assume() constraint registered against a test
// (or the whole spec). Body is the user predicate; Name is the
// callable's name for error messages.
//
// Source captures the syntax tree of a lambda body so spec-load
// validation can walk it for sandbox compliance. Nil for non-lambda
// callables (e.g. `def`s) — those slip through the AST denylist (a
// known limitation also documented for monitor predicates).
type AssumePredicate struct {
	Name   string
	Body   starlark.Callable
	Source syntax.Node
}

// Evaluate runs the predicate against choices. The choices dict maps
// every recorded choose("name", ...) call's name to its currently-
// selected option. rc1 is single-leaf, so each name maps to options[0];
// rc2 will hand the per-leaf assignment in.
//
// Returns (true, nil) if the predicate held. Returns (false, "reason")
// to halt the leaf. Returns (false, err) on any Starlark evaluation
// error.
func (a *AssumePredicate) Evaluate(choices starlark.Value) (bool, string, error) {
	if a == nil || a.Body == nil {
		return true, "", nil
	}
	thread := newSandboxThread("assume:" + a.Name)
	res, err := starlark.Call(thread, a.Body, starlark.Tuple{choices}, nil)
	if err != nil {
		return false, "", fmt.Errorf("assume(%s) raised: %w", a.Name, err)
	}
	if res.Truth() == starlark.True {
		return true, "", nil
	}
	return false, "assume(" + a.Name + ") not satisfied", nil
}

// builtinAssume implements RFC-043 §5.4's top-level form:
//
//	assume(predicate)        # filters all plan-tree leaves spec-wide
//
// rc1 single-leaf semantics: evaluate the predicate immediately at
// spec load against the current choice snapshot. If it fails, return
// an error so the spec load itself fails — the user sees the
// constraint violation clearly. rc2 will defer evaluation to
// per-leaf and prune the plan tree instead of erroring.
//
// The predicate must be a callable; bare booleans are accepted as a
// convenience and treated as `lambda choices: <bool>`.
func (rt *Runtime) builtinAssume(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) > 0 || len(args) != 1 {
		return nil, fmt.Errorf("assume(predicate): takes exactly one positional argument")
	}
	pred, err := assumeFromArg(args[0])
	if err != nil {
		return nil, fmt.Errorf("assume(): %w", err)
	}
	rt.specAssumes = append(rt.specAssumes, pred)

	// rc1 single-leaf: evaluate now against the current choice
	// snapshot so violations surface at spec load time. rc2 will
	// defer this to per-leaf evaluation.
	ok, msg, err := pred.Evaluate(rt.currentChoicesDict())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("assume(%s) violated at spec load: %s", pred.Name, msg)
	}
	return starlark.None, nil
}

// assumeFromArg wraps a user-supplied predicate value into an
// AssumePredicate. Accepts a callable (preferred — runs against the
// choices dict at evaluation time) or a bare boolean (constant
// predicate, mostly useful for asserting on spec-load conditions).
func assumeFromArg(v starlark.Value) (*AssumePredicate, error) {
	switch val := v.(type) {
	case starlark.Callable:
		ap := &AssumePredicate{Name: val.Name(), Body: val}
		if lam, ok := val.(*starlark.Function); ok {
			// Function bodies have no AST handle exposed through the
			// starlark.Value interface; this is the same limitation
			// noted for monitor predicates. Leave Source nil; spec-
			// load validation falls back to a runtime-error path for
			// denied builtins.
			_ = lam
		}
		return ap, nil
	case starlark.Bool:
		// Bare-boolean form. Wrap in a no-op closure so Evaluate has a
		// callable to invoke; the result is constant either way.
		constName := "True"
		if val != starlark.True {
			constName = "False"
		}
		return &AssumePredicate{
			Name: "const_" + constName,
			Body: starlark.NewBuiltin("assume.const", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
				return val, nil
			}),
		}, nil
	}
	return nil, fmt.Errorf("predicate must be callable or bool, got %s", v.Type())
}

// currentChoicesDict snapshots every recorded choose("name", opts)
// call into a Starlark dict the predicate can read. Unnamed choose()
// calls are skipped — they have no key the predicate could match on.
//
// When rt.currentLeaf is set (rc2 plan-tree fan-out), each named
// axis resolves to the leaf's pinned option index instead of the
// first option. This is what makes per-leaf assume= evaluation
// meaningful: predicates see what *this* leaf actually observed,
// not what the discovery run happened to use.
func (rt *Runtime) currentChoicesDict() *starlark.Dict {
	leaf := rt.snapshotCurrentLeaf()
	d := starlark.NewDict(0)
	// Prefer the persisted plan-axes schema when available — that
	// covers body-time choose() calls across re-executed leaves
	// where rt.choices has been reset for the upcoming body run.
	axes := rt.planAxes
	if len(axes) == 0 {
		axes = rt.Choices()
	}
	seen := make(map[string]bool, len(axes))
	for _, c := range axes {
		if c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		_ = d.SetKey(starlark.String(c.Name), c.Selected(leaf))
	}
	return d
}
