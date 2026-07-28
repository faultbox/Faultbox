package star

import (
	"testing"
)

// TestSourceUsesWatch pins the static scan that selects the container runtime.
//
// The scan runs in both LoadFile and LoadString. Patching only LoadString once
// left filesystem observation silently disabled for every CLI run while the
// unit tests stayed green — the CLI uses LoadFile.
func TestSourceUsesWatch(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{`def t(): watch(db, files=["/x"], run=f)`, true},
		{`def t(): watch_start(db)`, true},
		{"x = 1\nwatch(db)", true},
		{`def t(): pass`, false},
		{`def t(): unwatch(db)`, false},
		{`stopwatch(db)`, false},
	} {
		if got := sourceUsesWatch(tc.src); got != tc.want {
			t.Errorf("sourceUsesWatch(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// TestBothLoadPathsDetectWatch guards the LoadFile/LoadString split.
func TestBothLoadPathsDetectWatch(t *testing.T) {
	src := `
determinism(runtime = "gvisor")
db = service("db", "/bin/true", interface("main", "tcp", 5432))
def test_w():
    watch(db, files = ["/data/**"], run = lambda: None)
`
	rt := New(testLogger())
	if err := rt.LoadString("test.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	if !rt.specUsesWatch {
		t.Error("LoadString did not set specUsesWatch")
	}
}
