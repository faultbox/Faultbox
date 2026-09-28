package star

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/bundle"
	"go.starlark.net/starlark"
)

func TestResourcesReplayOutsideOriginalTree(t *testing.T) {
	root := t.TempDir()
	specDir := filepath.Join(root, "spec")
	for _, dir := range []string{"spec", "a", "b", "runtime/sub"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{"a/input.sql": "first", "b/input.sql": "second", "runtime/sub/data": "working", "binary": "executable fixture"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	pb := writeTestDescriptorSet(t)
	data, err := os.ReadFile(pb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "schema.pb"), data, 0644); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
a=load_file("../a/input.sql")
b=load_file("../b/input.sql")
payload=proto_encode(descriptors="../schema.pb",message="test.config.Setting",body={"id":42})
work=resource("../runtime")
cfg=resource("../a/input.sql")
sut=service("sut",binary=%q,cwd=work,env={"CONFIG_FILE_PATH":cfg})
`, filepath.Join(root, "binary"))
	path := filepath.Join(specDir, "test.star")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	rt := New(testLogger())
	if err := rt.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := rt.captureBinary(rt.services["sut"]); err != nil {
		t.Fatal(err)
	}
	specs := rt.LoadedSpecs()
	w, _, err := bundle.Build(bundle.BuildInput{SpecRoot: path, Specs: specs, FaultboxVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "run.fb")
	if err := w.WriteTo(archive); err != nil {
		t.Fatal(err)
	}
	r, err := bundle.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := r.Extract(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	replayed := New(testLogger())
	if err := replayed.LoadFile(filepath.Join(dst, "spec", "test.star")); err != nil {
		t.Fatal(err)
	}
	if string(replayed.globals["a"].(starlark.String)) != "first" || string(replayed.globals["b"].(starlark.String)) != "second" {
		t.Fatal("external basename collision")
	}
	if err := replayed.captureBinary(replayed.services["sut"]); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(replayed.services["sut"].Binary)
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatalf("binary mode: %v %v", info, err)
	}
	work := string(replayed.globals["work"].(starlark.String))
	if got, err := os.ReadFile(filepath.Join(work, "sub/data")); err != nil || string(got) != "working" {
		t.Fatalf("cwd=%q: %v", got, err)
	}
	if r.Env().BinaryDigests["sut"] == "" {
		t.Fatal("missing binary digest")
	}
	if err := os.WriteFile(replayed.services["sut"].Binary, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := New(testLogger()).LoadFile(filepath.Join(dst, "spec", "test.star")); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tamper: %v", err)
	}
}

func TestProtoEncodeReturnsBinaryStepPayload(t *testing.T) {
	rt := New(testLogger())
	pb := writeTestDescriptorSet(t)
	if err := rt.LoadString("encode.star", fmt.Sprintf(`payload=proto_encode(descriptors=%q,message="test.config.Setting",body={"id":128})`, pb)); err != nil {
		t.Fatal(err)
	}
	wire, ok := rt.globals["payload"].(starlark.Bytes)
	if !ok {
		t.Fatal("not bytes")
	}
	kwargs := starlarkKwargsToMap([]starlark.Tuple{{starlark.String("data"), wire}})
	if got := kwargs["data"].([]byte); string(got) != "\x08\x80\x01" {
		t.Fatalf("wire %x", got)
	}
}
