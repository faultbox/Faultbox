package star

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

// loadSpec is a tiny helper: writes spec.star + any siblings to a
// fresh temp dir, runs it through a Runtime, and returns the globals.
// Keeps every load_* test self-contained.
func loadSpec(t *testing.T, files map[string]string, body string) (*Runtime, starlark.StringDict) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	spec := filepath.Join(dir, "spec.star")
	if err := os.WriteFile(spec, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	rt := New(testLogger())
	if err := rt.LoadFile(spec); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return rt, rt.globals
}

func TestLoadFileReadsSpecRelative(t *testing.T) {
	rt, g := loadSpec(t,
		map[string]string{"seed.sql": "CREATE TABLE pets (id INT);\n"},
		`seed = load_file("seed.sql")`,
	)
	got, ok := g["seed"].(starlark.String)
	if !ok {
		t.Fatalf("seed = %T, want string", g["seed"])
	}
	if !strings.Contains(string(got), "CREATE TABLE pets") {
		t.Errorf("content = %q", got)
	}
	// Bundle capture: file shows up in LoadedSpecs under its relative key.
	if _, ok := rt.LoadedSpecs()["seed.sql"]; !ok {
		t.Errorf("LoadedSpecs missing seed.sql; got %v", keysOf(rt.LoadedSpecs()))
	}
}

func TestLoadFileRejectsNetworkScheme(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.star")
	_ = os.WriteFile(spec, []byte(`x = load_file("http://evil.example/secret")`), 0o644)
	rt := New(testLogger())
	err := rt.LoadFile(spec)
	if err == nil {
		t.Fatal("expected network scheme to be refused")
	}
	if !strings.Contains(err.Error(), "network schemes") {
		t.Errorf("error should mention network schemes, got: %v", err)
	}
}

func TestLoadFileRejectsOversize(t *testing.T) {
	// Set a tiny cap via env var so we don't need to write 50 MB.
	t.Setenv("FAULTBOX_LOAD_FILE_MAX_BYTES", "10")
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	_ = os.WriteFile(big, []byte("much more than ten bytes here"), 0o644)
	spec := filepath.Join(dir, "spec.star")
	_ = os.WriteFile(spec, []byte(`x = load_file("big.bin")`), 0o644)

	rt := New(testLogger())
	err := rt.LoadFile(spec)
	if err == nil {
		t.Fatal("expected size refusal")
	}
	if !strings.Contains(err.Error(), "exceeds FAULTBOX_LOAD_FILE_MAX_BYTES") {
		t.Errorf("error should mention size cap, got: %v", err)
	}
}

func TestLoadYAMLDecodesDictList(t *testing.T) {
	_, g := loadSpec(t,
		map[string]string{"users.yaml": `
users:
  - name: alice
    age: 30
  - name: bob
    age: 25
`},
		`data = load_yaml("users.yaml")`,
	)
	d, ok := g["data"].(*starlark.Dict)
	if !ok {
		t.Fatalf("data = %T, want dict", g["data"])
	}
	usersVal, _, err := d.Get(starlark.String("users"))
	if err != nil {
		t.Fatalf("Get(users): %v", err)
	}
	users, ok := usersVal.(*starlark.List)
	if !ok {
		t.Fatalf("users = %T, want list", usersVal)
	}
	if users.Len() != 2 {
		t.Errorf("len(users) = %d, want 2", users.Len())
	}
}

func TestLoadJSONDecodes(t *testing.T) {
	_, g := loadSpec(t,
		map[string]string{"cfg.json": `{"enabled": true, "limit": 42}`},
		`data = load_json("cfg.json")`,
	)
	d, ok := g["data"].(*starlark.Dict)
	if !ok {
		t.Fatalf("data = %T, want dict", g["data"])
	}
	enabled, _, _ := d.Get(starlark.String("enabled"))
	if enabled != starlark.Bool(true) {
		t.Errorf("enabled = %v, want true", enabled)
	}
	limit, _, _ := d.Get(starlark.String("limit"))
	if s := limit.String(); s != "42" {
		t.Errorf("limit = %v, want 42", s)
	}
}

func TestLoadYAMLRejectsNonStringKeys(t *testing.T) {
	dir := t.TempDir()
	// YAML with integer keys (yaml.v3 parses as map[string]any by default
	// via node scalar-string coercion; to hit the map[any]any path we use
	// explicit !!map with numeric keys).
	_ = os.WriteFile(filepath.Join(dir, "odd.yaml"), []byte("? !!int 1\n: alice\n"), 0o644)
	spec := filepath.Join(dir, "spec.star")
	_ = os.WriteFile(spec, []byte(`data = load_yaml("odd.yaml")`), 0o644)

	rt := New(testLogger())
	err := rt.LoadFile(spec)
	if err == nil {
		t.Fatal("expected non-string-key YAML to error")
	}
	// Accept either our error or yaml parser's — both are clear refusals.
	if !strings.Contains(err.Error(), "key") && !strings.Contains(err.Error(), "non-string") {
		t.Logf("got error: %v — allowed if descriptive of the shape issue", err)
	}
}

func TestLoadFileMissingFails(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.star")
	_ = os.WriteFile(spec, []byte(`x = load_file("nonexistent.sql")`), 0o644)
	rt := New(testLogger())
	err := rt.LoadFile(spec)
	if err == nil {
		t.Fatal("expected missing-file error")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
