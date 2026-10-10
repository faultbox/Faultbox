package star

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestExactSelectionPreservesOrderSeedAndLeaves(t *testing.T) {
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	if err := rt.LoadString("selection.star", `def test_a():
    x=choose("axis", [1,2])
    assert_true(x>0)
def test_b(): fail("not selected")
def test_z(): assert_true(True)
`); err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{Tests: []string{"test_z", "test_a", "test_z"}}
	got, err := rt.SelectedTests(cfg)
	if err != nil || !reflect.DeepEqual(got, []string{"test_a", "test_z"}) {
		t.Fatal(got, err)
	}
	seed := uint64(42)
	cfg.Seed = &seed
	result, err := rt.RunAll(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Matched != 2 || len(result.Tests) != 3 {
		t.Fatalf("selection changed leaf expansion: %+v", result)
	}
	for _, test := range result.Tests {
		if test.Result != "pass" || test.Seed != 42 {
			t.Fatal(test)
		}
	}
}
func TestExactSelectionRejectsUnknownBeforeStartingServices(t *testing.T) {
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	if err := rt.LoadString("selection.star", `svc=service("never", "/missing/binary", interface("main","tcp",19019))
def test_a(): pass
`); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []RunConfig{{Tests: []string{"test_a", "test_missing"}}, {Tests: []string{}}, {Tests: []string{"a"}}, {Tests: []string{"test_*"}}, {Tests: []string{"test_a"}, Filter: "a"}} {
		if _, err := rt.RunAll(context.Background(), cfg); err == nil {
			t.Fatal("accepted invalid exact selection", cfg)
		}
		for _, e := range rt.events.Events() {
			if strings.HasPrefix(e.Type, "service_") {
				t.Fatal("started service before validating selection", e)
			}
		}
	}
}
