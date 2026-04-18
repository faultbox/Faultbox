package protocol

import (
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoProtocolRegistered(t *testing.T) {
	p, ok := Get("mongodb")
	if !ok {
		t.Fatal("mongodb protocol not registered")
	}
	want := []string{"find", "insert", "insert_many", "update", "delete", "count", "command"}
	if !reflect.DeepEqual(p.Methods(), want) {
		t.Errorf("Methods() = %v, want %v", p.Methods(), want)
	}
}

func TestToBSONDoc_Map(t *testing.T) {
	in := map[string]any{
		"name": "alice",
		"meta": map[string]any{"role": "admin", "active": true},
		"tags": []any{"a", "b"},
	}
	got, err := toBSONDoc(in)
	if err != nil {
		t.Fatalf("toBSONDoc: %v", err)
	}
	// Nested map must be bson.M (not map[string]any) so the driver encodes correctly.
	if _, ok := got["meta"].(bson.M); !ok {
		t.Errorf("nested map not normalized to bson.M: got %T", got["meta"])
	}
	// Nested array must be bson.A.
	if _, ok := got["tags"].(bson.A); !ok {
		t.Errorf("nested list not normalized to bson.A: got %T", got["tags"])
	}
}

func TestToBSONDoc_JSONString(t *testing.T) {
	got, err := toBSONDoc(`{"role":"admin","count":3}`)
	if err != nil {
		t.Fatalf("toBSONDoc: %v", err)
	}
	if got["role"] != "admin" {
		t.Errorf("role = %v, want admin", got["role"])
	}
	// JSON numbers decode as float64 — that's fine for BSON which has a
	// double type. Caller can cast if they need int.
	if got["count"] != float64(3) {
		t.Errorf("count = %v, want 3", got["count"])
	}
}

func TestToBSONDoc_EmptyString(t *testing.T) {
	got, err := toBSONDoc("")
	if err != nil {
		t.Fatalf("toBSONDoc: %v", err)
	}
	if got != nil {
		t.Errorf("empty string should produce nil doc, got %v", got)
	}
}

func TestParseBSONFromKwarg_Absent(t *testing.T) {
	got, err := parseBSONFromKwarg(map[string]any{}, "filter")
	if err != nil {
		t.Fatalf("parseBSONFromKwarg: %v", err)
	}
	if got != nil {
		t.Errorf("absent kwarg should be nil, got %v", got)
	}
}

func TestNormalizeBSONDoc_ObjectID(t *testing.T) {
	oid := bson.NewObjectID()
	doc := bson.M{
		"_id":  oid,
		"name": "alice",
	}
	got := normalizeBSONDoc(doc)
	if got["_id"] != oid.Hex() {
		t.Errorf("ObjectID should be stringified: got %v (%T)", got["_id"], got["_id"])
	}
}

func TestNormalizeBSONDoc_NestedArray(t *testing.T) {
	doc := bson.M{
		"items": bson.A{bson.M{"name": "a"}, bson.M{"name": "b"}},
	}
	got := normalizeBSONDoc(doc)
	items, ok := got["items"].([]any)
	if !ok {
		t.Fatalf("items not normalized to []any: got %T", got["items"])
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d, want 2", len(items))
	}
	if m, ok := items[0].(map[string]any); !ok || m["name"] != "a" {
		t.Errorf("items[0] = %v, want {name:a}", items[0])
	}
}

func TestGetIntKwarg(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int(5), 5},
		{int64(7), 7},
		{float64(3.9), 3},
		{"not-a-number", 42},
		{nil, 42},
	}
	for _, c := range cases {
		kw := map[string]any{}
		if c.in != nil {
			kw["x"] = c.in
		}
		got := getIntKwarg(kw, "x", 42)
		if got != c.want {
			t.Errorf("getIntKwarg(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
