package star

import (
	"testing"

	"go.starlark.net/starlark"
)

func kwargs(pairs ...any) []starlark.Tuple {
	var out []starlark.Tuple
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, starlark.Tuple{
			starlark.String(pairs[i].(string)),
			pairs[i+1].(starlark.Value),
		})
	}
	return out
}

func dict(t *testing.T, pairs ...any) *starlark.Dict {
	t.Helper()
	d := starlark.NewDict(len(pairs) / 2)
	for i := 0; i < len(pairs); i += 2 {
		if err := d.SetKey(starlark.String(pairs[i].(string)), pairs[i+1].(starlark.Value)); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// The bug: starlarkKwargsToMap read dict values with starlark.AsString, which
// returns "" for anything that is not a String. Every integer, float, bool,
// nested dict and list inside a dict kwarg silently became an empty string.
//
// Observed against a real MongoDB: inserting {"id": 1, "payload": "row-1"}
// stored {"id": "", "payload": "row-1"}. It hid because the mangling was
// self-consistent — a filter of {"id": 1} was flattened the same way, so it
// matched the mangled document and the round trip looked correct.
func TestDictKwargPreservesNonStringValues(t *testing.T) {
	m := starlarkKwargsToMap(kwargs("document", dict(t,
		"id", starlark.MakeInt(1),
		"payload", starlark.String("row-1"),
		"score", starlark.Float(2.5),
		"active", starlark.Bool(true),
	)))

	doc, ok := m["document"].(map[string]any)
	if !ok {
		t.Fatalf("document is %T, want map[string]any", m["document"])
	}
	if got := doc["id"]; got != int64(1) {
		t.Errorf(`doc["id"] = %#v (%T), want int64(1) — this is the bug`, got, got)
	}
	if got := doc["payload"]; got != "row-1" {
		t.Errorf(`doc["payload"] = %#v`, got)
	}
	if got := doc["score"]; got != 2.5 {
		t.Errorf(`doc["score"] = %#v (%T), want 2.5`, got, got)
	}
	if got := doc["active"]; got != true {
		t.Errorf(`doc["active"] = %#v (%T), want true`, got, got)
	}
}

// Nested structure must survive too — a BSON document is routinely a tree.
func TestDictKwargPreservesNesting(t *testing.T) {
	inner := dict(t, "qty", starlark.MakeInt(7))
	list := starlark.NewList([]starlark.Value{starlark.MakeInt(1), starlark.String("two")})
	m := starlarkKwargsToMap(kwargs("document", dict(t,
		"item", inner,
		"tags", list,
	)))

	doc := m["document"].(map[string]any)
	nested, ok := doc["item"].(map[string]any)
	if !ok {
		t.Fatalf(`doc["item"] is %T, want map[string]any`, doc["item"])
	}
	if got := nested["qty"]; got != int64(7) {
		t.Errorf(`nested qty = %#v, want int64(7)`, got)
	}
	tags, ok := doc["tags"].([]any)
	if !ok {
		t.Fatalf(`doc["tags"] is %T, want []any`, doc["tags"])
	}
	if len(tags) != 2 || tags[0] != int64(1) || tags[1] != "two" {
		t.Errorf(`doc["tags"] = %#v`, tags)
	}
}

// A list kwarg previously fell through to v.String() and reached plugins as
// Starlark source text, so mongodb.insert_many's `raw.([]any)` assertion could
// never succeed and Redis's documented command(args=[...]) form dropped every
// argument.
func TestListKwargBecomesASlice(t *testing.T) {
	m := starlarkKwargsToMap(kwargs("args", starlark.NewList([]starlark.Value{
		starlark.String("key"), starlark.String("value"),
	})))
	got, ok := m["args"].([]any)
	if !ok {
		t.Fatalf("args is %T (%#v), want []any", m["args"], m["args"])
	}
	if len(got) != 2 || got[0] != "key" || got[1] != "value" {
		t.Errorf("args = %#v", got)
	}
}

func TestListOfDictsKwarg(t *testing.T) {
	m := starlarkKwargsToMap(kwargs("documents", starlark.NewList([]starlark.Value{
		dict(t, "id", starlark.MakeInt(1)),
		dict(t, "id", starlark.MakeInt(2)),
	})))
	docs, ok := m["documents"].([]any)
	if !ok {
		t.Fatalf("documents is %T, want []any", m["documents"])
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents", len(docs))
	}
	for i, d := range docs {
		dm, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("documents[%d] is %T, want map[string]any", i, d)
		}
		if dm["id"] != int64(i+1) {
			t.Errorf("documents[%d][id] = %#v, want int64(%d)", i, dm["id"], i+1)
		}
	}
}

// Scalars at the top level must keep the types plugins already rely on —
// getStringKwarg and friends are built around them.
func TestTopLevelScalarsUnchanged(t *testing.T) {
	m := starlarkKwargsToMap(kwargs(
		"sql", starlark.String("SELECT 1"),
		"limit", starlark.MakeInt(10),
		"ratio", starlark.Float(0.5),
		"strict", starlark.Bool(false),
	))
	if m["sql"] != "SELECT 1" {
		t.Errorf("sql = %#v", m["sql"])
	}
	if m["limit"] != int64(10) {
		t.Errorf("limit = %#v (%T)", m["limit"], m["limit"])
	}
	if m["ratio"] != 0.5 {
		t.Errorf("ratio = %#v", m["ratio"])
	}
	if m["strict"] != false {
		t.Errorf("strict = %#v", m["strict"])
	}
}

// None must not become the string "None" — a plugin checking for a nil filter
// would otherwise see a non-empty value.
func TestNoneBecomesNil(t *testing.T) {
	m := starlarkKwargsToMap(kwargs("filter", starlark.None))
	if m["filter"] != nil {
		t.Errorf("filter = %#v, want nil", m["filter"])
	}
}

// Values starlarkToGo cannot represent keep the previous string rendering
// rather than erroring, since some plugins accept them as opaque labels.
func TestUnrepresentableValueFallsBackToString(t *testing.T) {
	// A dict with a non-string key cannot be JSON/BSON encoded.
	d := starlark.NewDict(1)
	if err := d.SetKey(starlark.MakeInt(1), starlark.String("v")); err != nil {
		t.Fatal(err)
	}
	m := starlarkKwargsToMap(kwargs("weird", d))
	if _, isMap := m["weird"].(map[string]any); isMap {
		t.Error("a dict with non-string keys should not convert cleanly")
	}
	if s, ok := m["weird"].(string); !ok || s == "" {
		t.Errorf("weird = %#v, want a non-empty string fallback", m["weird"])
	}
}
