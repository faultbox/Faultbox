package star

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFailedReadinessDoesNotLeakWorkerPortIntoNextTest(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler needed for live-worker fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "worker")
	if out, err := exec.Command(cc, "-pthread", "-o", binary, "../engine/testdata/leader_exit.c").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	port := freePort(t)
	witness, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close()
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	src := fmt.Sprintf(`worker=service("worker",%q,interface("main","http",%d),args=[%q,%q],env={"FB_TEST_PORT":%q},healthcheck=http("localhost:%d/health",timeout="100ms"))
def test_first(): fail("unreachable: fixture never answers HTTP")
def test_second(): fail("unreachable: fixture never answers HTTP")
`, binary, port, filepath.Join(dir, "ready"), filepath.Join(dir, "release"), fmt.Sprint(port), port)
	if err := rt.LoadString("startup.star", src); err != nil {
		t.Fatal(err)
	}
	result, err := rt.RunAll(context.Background(), RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tests) != 2 || result.Aborted != 0 {
		t.Fatalf("cleanup poisoned the next test: %+v", result)
	}
	for _, test := range result.Tests {
		found := false
		for _, e := range test.Events {
			if e.Type == "service_start_error" {
				found = true
				if e.Fields["code"] != "HEALTHCHECK_TIMEOUT" {
					t.Fatalf("unexpected startup cause: %v", e)
				}
			}
			if e.Type == "service_stop_error" {
				t.Fatalf("cleanup failed: %v", e)
			}
		}
		if !found {
			t.Fatalf("missing failed-readiness evidence: %+v", test)
		}
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal("SUT listener leaked", err)
	}
	ln.Close()
	conn, err := net.Dial("tcp", witness.Addr().String())
	if err != nil {
		t.Fatal("foreign listener changed", err)
	}
	conn.Close()
}
