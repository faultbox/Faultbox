package star

import (
	"path/filepath"
	"testing"
)

// TestRaftSpecLoads keeps the Antithesis-port spec honest: it must load with
// no startup ordering and no depends_on, which is the whole point of the mesh
// fix.
func TestRaftSpecLoads(t *testing.T) {
	p := filepath.Join("..", "..", "poc", "raft-cluster", "faultbox.star")
	rt := New(testLogger())
	if err := rt.LoadFile(p); err != nil {
		t.Fatalf("raft spec failed to load:\n%v", err)
	}
	tests := rt.DiscoverTests()
	if len(tests) < 5 {
		t.Errorf("spec declares %d tests, want >= 5", len(tests))
	}
	for _, n := range []string{"node1", "node2", "node3"} {
		svc := rt.services[n]
		if svc == nil {
			t.Fatalf("service %q not registered", n)
		}
		if len(svc.DependsOn) != 0 {
			t.Errorf("%s declares depends_on %v — the mesh fix removes the ordering hack", n, svc.DependsOn)
		}
	}
}
