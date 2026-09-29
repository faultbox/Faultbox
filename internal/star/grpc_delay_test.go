package star

import "testing"

func TestGRPCDelayHitPrecedesScopeRemoval(t *testing.T) {
	rt := statefulGRPCRuntime(t, statefulAnswer, `
def delayed():
    result = api.get_setting(id=42, timeout="100ms")
    assert_eq(result.ok, False)
    assert_true("DeadlineExceeded" in result.error)
def test_delay():
    fault(config.main, delay(method="*", delay="5s"), run=delayed)
    assert_eq(api.get_setting(id=42).data["name"], "DENIED")
`)
	r := runMockTest(t, rt, "test_delay")
	if r.Result != "pass" || r.FaultBypassed {
		t.Fatalf("%s bypass=%v: %s", r.Result, r.FaultBypassed, r.Reason)
	}
	if ds := protocolFaultDiagnostics(r.Events); len(ds) != 0 {
		t.Fatalf("false bypass: %+v", ds)
	}
	var hit, removed int64
	for _, ev := range r.Events {
		if ev.Type == "proxy" && ev.Fields["action"] == "delay" {
			hit = ev.Seq
		}
		if ev.Type == "proxy_fault_removed" {
			removed = ev.Seq
		}
	}
	if hit == 0 || removed <= hit {
		t.Fatalf("hit=%d removed=%d", hit, removed)
	}
}
