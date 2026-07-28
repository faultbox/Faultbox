package star

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScenarioCorpusLoads keeps the shipped RFC-054 scenario corpus honest:
// every scenario must parse and validate against the real builtins, so a DSL
// change that breaks a documented example fails here rather than in a demo.
func TestScenarioCorpusLoads(t *testing.T) {
	path := filepath.Join("..", "..", "poc", "gvisor-rfc054", "faultbox.star")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	rt := New(testLogger())
	if err := rt.LoadString(path, string(src)); err != nil {
		t.Fatalf("RFC-054 scenario corpus failed to load:\n%v", err)
	}
	tests := rt.DiscoverTests()
	if len(tests) < 10 {
		t.Errorf("corpus declares %d tests, want the full scenario set (>=10)", len(tests))
	}
}
