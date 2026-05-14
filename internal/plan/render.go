package plan

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MarshalJSON returns the indented JSON encoding of the plan tree per
// RFC-042 §5.2. Bytes are stable across calls against the same tree
// because Enumerate sorts every map key during construction.
func (pt *PlanTree) MarshalJSONIndent() ([]byte, error) {
	return json.MarshalIndent(pt, "", "  ")
}

// WriteJSON writes the JSON encoding to w with a trailing newline so
// `faultbox plan --format=json | jq` works without complaint.
func WriteJSON(w io.Writer, pt *PlanTree) error {
	b, err := pt.MarshalJSONIndent()
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
	return err
}

// WriteDOT writes a Graphviz DOT representation of the plan tree. The
// graph is a tree rooted at "spec" → tests → composition axes; matrix
// cells are leaves under their axis values. Useful for piping into
// `dot -Tsvg` for documentation or for the report's static asset
// pipeline once the Plan tab lands (PR 6).
//
// The format is intentionally simple — one node per test and per axis
// — so the output stays readable even for medium specs. Probability /
// interleaving fan-outs (rc2) will gain their own node shapes.
func WriteDOT(w io.Writer, pt *PlanTree) error {
	if _, err := fmt.Fprintln(w, "digraph plan {"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "  rankdir=LR;"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "  node [shape=box, fontname=\"Helvetica\"];"); err != nil {
		return err
	}

	rootLabel := "spec"
	if pt.SpecPath != "" {
		rootLabel = pt.SpecPath
	}
	fmt.Fprintf(w, "  spec [label=%q, shape=oval];\n", rootLabel)

	for i, t := range pt.Tests {
		testID := fmt.Sprintf("t%d", i)
		label := fmt.Sprintf("%s\\n[%s, %d inst]", t.Name, t.Kind, t.Instances)
		fmt.Fprintf(w, "  %s [label=%q];\n", testID, label)
		fmt.Fprintf(w, "  spec -> %s;\n", testID)

		for ci, comp := range t.Compositions {
			compID := fmt.Sprintf("%s_c%d", testID, ci)
			fmt.Fprintf(w, "  %s [label=%q, shape=ellipse, style=dashed];\n", compID, string(comp.Kind))
			fmt.Fprintf(w, "  %s -> %s;\n", testID, compID)
			for ai, ax := range comp.Axes {
				axID := fmt.Sprintf("%s_a%d", compID, ai)
				axLabel := ax.Name + ": " + strings.Join(ax.Values, ", ")
				fmt.Fprintf(w, "  %s [label=%q, shape=note];\n", axID, axLabel)
				fmt.Fprintf(w, "  %s -> %s;\n", compID, axID)
			}
		}
	}

	if _, err := fmt.Fprintln(w, "}"); err != nil {
		return err
	}
	return nil
}
