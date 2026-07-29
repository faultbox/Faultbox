package star

import (
	"fmt"
	"sort"

	"go.starlark.net/syntax"
)

// Static discovery of choose() axes, and why `faultbox plan` needs it.
//
// The plan tree is static analysis: it loads the spec and walks the declared
// tests without launching services or executing bodies. choose() axes are
// discovered the other way round — runTestFanout executes the body once
// against a synthetic discovery leaf and collects the choose() calls that
// actually ran. The two approaches cannot meet: a body calls into services, so
// `plan` cannot execute it.
//
// The consequence shipped in v0.13.0 and survived until v0.14.1: `plan`
// reported "Total: 2 plan instances" for a spec that runs 24 leaves. That is
// not merely imprecise — `--check-cost --max-instances N` exists to catch
// fan-out blowups before they run, and it was blind to the single construct
// most likely to cause one. A cost gate that under-reports is worse than no
// cost gate, because it is trusted.
//
// So: parse the spec and read the choose() calls out of the AST. This gives
// the right answer whenever the option list is a literal, which is the
// overwhelmingly common form. Where it is not, the axis is reported with an
// unknown cardinality rather than silently assumed to be 1 — an estimate that
// is honest about its gaps is useful; one that quietly rounds down is how the
// original bug read as a working feature.

// ChooseAxis is one statically-discovered choose() axis.
type ChooseAxis struct {
	// Name is the axis label from choose("name", [...]). Anonymous
	// choose([...]) calls get a positional placeholder, since they still
	// multiply the leaf count even though assume() cannot reference them.
	Name string
	// Values are the stringified options, empty when Known is false.
	Values []string
	// Known reports whether the option list was a literal the scanner could
	// read. When false, Cardinality is a floor of 1 and the axis is rendered
	// with a "?" so the total is visibly a lower bound.
	Known bool
}

// Cardinality is the number of leaves this axis multiplies by.
func (a ChooseAxis) Cardinality() int {
	if !a.Known || len(a.Values) == 0 {
		return 1
	}
	return len(a.Values)
}

// ChooseAxesByTest maps a test function name to the choose() axes reachable
// from its body, keyed by the discoverable "test_" name.
//
// Scope is the enclosing top-level def: a choose() inside a helper called by
// the body is attributed to no test, because static analysis cannot resolve
// the call graph without executing it. Under-reporting there is deliberate and
// visible — the alternative is guessing, and a plan tree that invents axes is
// worse than one that admits it missed some.
func (rt *Runtime) ChooseAxesByTest() map[string][]ChooseAxis {
	return chooseAxesInSource(rt.rootSpec, rt.sourceText)
}

func chooseAxesInSource(filename, src string) map[string][]ChooseAxis {
	if src == "" {
		return nil
	}
	file, err := syntax.Parse(filename, src, 0)
	if err != nil {
		// A spec that does not parse has a canonical error from ExecFile;
		// the plan tree is not the place to report it a second time.
		return nil
	}

	out := make(map[string][]ChooseAxis)
	for _, stmt := range file.Stmts {
		def, ok := stmt.(*syntax.DefStmt)
		if !ok {
			continue
		}
		axes := chooseAxesInNode(def)
		if len(axes) > 0 {
			out[def.Name.Name] = axes
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// chooseAxesInNode collects choose() calls in source order. Order matters:
// enumerateLeaves fans out in axis order, so a plan tree that reordered them
// would print a different leaf sequence than the run produces.
func chooseAxesInNode(root syntax.Node) []ChooseAxis {
	var axes []ChooseAxis
	anon := 0
	syntax.Walk(root, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		callee, ok := call.Fn.(*syntax.Ident)
		if !ok || callee.Name != "choose" {
			return true
		}
		axis := ChooseAxis{}
		switch len(call.Args) {
		case 1:
			// choose([...]) — anonymous, still multiplies the leaf count.
			anon++
			axis.Name = fmt.Sprintf("choose#%d", anon)
			axis.Values, axis.Known = literalOptions(call.Args[0])
		case 2:
			// choose("name", [...])
			name, ok := stringLiteral(call.Args[0])
			if !ok {
				anon++
				name = fmt.Sprintf("choose#%d", anon)
			}
			axis.Name = name
			axis.Values, axis.Known = literalOptions(call.Args[1])
		default:
			// Malformed; the runtime reports it properly at load time.
			return true
		}
		axes = append(axes, axis)
		return true
	})
	return axes
}

// literalOptions reads a list literal's elements. Non-literal elements are
// stringified structurally where possible so the plan tree still shows the
// right cardinality — the count is what a cost gate needs; the labels are for
// humans.
func literalOptions(n syntax.Node) (values []string, known bool) {
	list, ok := n.(*syntax.ListExpr)
	if !ok {
		return nil, false
	}
	for _, el := range list.List {
		values = append(values, literalString(el))
	}
	return values, true
}

func stringLiteral(n syntax.Node) (string, bool) {
	lit, ok := n.(*syntax.Literal)
	if !ok {
		return "", false
	}
	s, ok := lit.Value.(string)
	return s, ok
}

// literalString renders one option for display. Anything that is not a plain
// literal becomes "<expr>": it occupies a slot in the cross product, which is
// what the count depends on, but its value is not knowable without executing.
func literalString(n syntax.Node) string {
	if lit, ok := n.(*syntax.Literal); ok {
		switch v := lit.Value.(type) {
		case string:
			return v
		default:
			return fmt.Sprint(v)
		}
	}
	if id, ok := n.(*syntax.Ident); ok {
		switch id.Name {
		case "True", "False", "None":
			return id.Name
		}
		return id.Name
	}
	return "<expr>"
}

// ChooseLeafCount is the number of leaves the axes fan out to — the product of
// their cardinalities, or 1 when there are none.
func ChooseLeafCount(axes []ChooseAxis) int {
	n := 1
	for _, a := range axes {
		n *= a.Cardinality()
	}
	if n < 1 {
		return 1
	}
	return n
}

// ChooseAxesComplete reports whether every axis had a readable option list. A
// false here is what a renderer turns into "at least N" rather than "N".
func ChooseAxesComplete(axes []ChooseAxis) bool {
	for _, a := range axes {
		if !a.Known {
			return false
		}
	}
	return true
}

// sortedAxisNames is a test/debug helper kept next to the type it describes.
func sortedAxisNames(axes []ChooseAxis) []string {
	names := make([]string, 0, len(axes))
	for _, a := range axes {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}
