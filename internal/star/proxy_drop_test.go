package star

import (
	"testing"

	"go.starlark.net/starlark"
)

// kwTuples builds a []starlark.Tuple of string key/value kwargs.
func kwTuples(pairs ...string) []starlark.Tuple {
	var kw []starlark.Tuple
	for i := 0; i+1 < len(pairs); i += 2 {
		kw = append(kw, starlark.Tuple{starlark.String(pairs[i]), starlark.String(pairs[i+1])})
	}
	return kw
}

// TestProxyDrop_QueryCommandKwargs guards #137: drop(query=)/drop(command=)
// must populate the SQL/command match pattern. An empty pattern matches every
// request (sqlmatch.Match / MatchRequest return true on ""), so a dropped
// pattern silently turned a targeted drop into a global one.
func TestProxyDrop_QueryCommandKwargs(t *testing.T) {
	thread := &starlark.Thread{}

	v, err := builtinProxyDrop(thread, nil, nil, kwTuples("query", "INSERT INTO orders*"))
	if err != nil {
		t.Fatalf("drop(query=): %v", err)
	}
	pf := v.(*ProxyFaultDef)
	if pf.Query != "INSERT INTO orders*" {
		t.Errorf("drop(query=): Query = %q, want %q (empty pattern drops ALL traffic, #137)",
			pf.Query, "INSERT INTO orders*")
	}

	v, err = builtinProxyDrop(thread, nil, nil, kwTuples("command", "SET"))
	if err != nil {
		t.Fatalf("drop(command=): %v", err)
	}
	pf = v.(*ProxyFaultDef)
	if pf.Command != "SET" {
		t.Errorf("drop(command=): Command = %q, want %q (empty pattern drops ALL commands, #137)",
			pf.Command, "SET")
	}

	// Parity: error() already handled these; confirm drop() now matches.
	v, err = builtinProxyError(thread, nil, nil, kwTuples("query", "DELETE*"))
	if err != nil {
		t.Fatalf("error(query=): %v", err)
	}
	if v.(*ProxyFaultDef).Query != "DELETE*" {
		t.Errorf("error(query=) regression: Query = %q", v.(*ProxyFaultDef).Query)
	}
}
