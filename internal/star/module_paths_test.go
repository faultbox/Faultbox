package star

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/bundle"
)

func writeModuleFixture(t *testing.T, root, name, data string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestModuleRelativeLoadFilesAndReplay(t *testing.T) {
	root := t.TempDir()
	writeModuleFixture(t, root, "gen/main.star", `load("../lib/a/helper.star", a="read")
load("../lib/b/helper.star", b="read")
def test_paths():
    assert_eq(a(), "A")
    assert_eq(b(), "B")
`)
	for _, name := range []string{"a", "b"} {
		writeModuleFixture(t, root, "lib/"+name+"/helper.star", `load("./dep.star", "value")
ASSET=resource("./value.txt")
def read():
    assert_eq(load_file(ASSET), value)
    return load_file("./value.txt")
`)
		writeModuleFixture(t, root, "lib/"+name+"/dep.star", `value = load_file("value.txt")`)
		writeModuleFixture(t, root, "lib/"+name+"/value.txt", strings.ToUpper(name))
	}
	rt := New(testLogger())
	if err := rt.LoadFile(filepath.Join(root, "gen/main.star")); err != nil {
		t.Fatal(err)
	}
	if r := rt.RunTest(context.Background(), "test_paths"); r.Result != "pass" {
		t.Fatal(r.Reason)
	}
	// Extract exactly the resource tree used by .fb replay, then remove originals.
	resources := rt.bundledResources()
	rootKey := rt.bundleSpecKey(filepath.Join(root, "gen/main.star"))
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for generation := 0; generation < 2; generation++ {
		extracted := t.TempDir()
		for name, data := range resources {
			p := filepath.Join(extracted, name)
			os.MkdirAll(filepath.Dir(p), 0755)
			if err := os.WriteFile(p, data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := bundle.PrepareResources(extracted); err != nil {
			t.Fatal(err)
		}
		replay := New(testLogger())
		if err := replay.LoadFile(filepath.Join(extracted, rootKey)); err != nil {
			t.Fatal(err)
		}
		if r := replay.RunTest(context.Background(), "test_paths"); r.Result != "pass" {
			t.Fatal(r.Reason)
		}
		resources = replay.bundledResources()
		rootKey = replay.bundleSpecKey(filepath.Join(extracted, rootKey))
		os.RemoveAll(extracted)
	}
}

func TestRecursionWorksAndRunawayFails(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("recursion.star", `
def flatten(value):
    if type(value) != "list":
        return [value]
    result=[]
    for item in value:
        result.extend(flatten(item))
    return result
def forever():
    return forever()
def test_recursive():
    assert_eq(flatten([1,[2,[3]]]), [1,2,3])
def test_runaway():
    forever()
`); err != nil {
		t.Fatal(err)
	}
	if r := rt.RunTest(context.Background(), "test_recursive"); r.Result != "pass" {
		t.Fatal(r.Reason)
	}
	if r := rt.RunTest(context.Background(), "test_runaway"); r.Result != "fail" || !strings.Contains(r.Reason, "recursion depth") {
		t.Fatalf("runaway: %+v", r)
	}
	if err := New(testLogger()).LoadString("loop.star", `def recurse(): return recurse()
x=recurse()`); err == nil || !strings.Contains(err.Error(), "recursion depth") {
		t.Fatalf("load recursion: %v", err)
	}
}

func TestModuleAliasExecutesOnce(t *testing.T) {
	root := t.TempDir()
	writeModuleFixture(t, root, "main.star", `load("lib/a.star", "svc")
load("lib/../lib/a.star", same="svc")
def test_identity(): assert_eq(svc, same)
`)
	writeModuleFixture(t, root, "lib/a.star", `svc=service("db",image="redis:7",reuse=True)`)
	rt := New(testLogger())
	if err := rt.LoadFile(filepath.Join(root, "main.star")); err != nil {
		t.Fatal(err)
	}
	if len(rt.services) != 1 {
		t.Fatal("duplicate module declaration")
	}
}
