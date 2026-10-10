package star

import (
	"context"
	"testing"
)

func TestKafkaDiagnosticsCapturedForLegacyFailure(t *testing.T) {
	rt := New(testLogger())
	t.Cleanup(rt.cleanup)
	if err := rt.LoadString("legacy.star", `def test_legacy(): fail("manual readiness timeout")`); err != nil {
		t.Fatal(err)
	}
	old := rt.kafkaSnapshotRegistration("bus", "main")
	old(func() []map[string]string {
		return []map[string]string{{"group": "old", "state": "no_fetch_observed", "ready": "false"}}
	})
	current := rt.kafkaSnapshotRegistration("bus", "main")
	current(func() []map[string]string {
		return []map[string]string{{"group": "new", "state": "no_fetch_for_current_assignment", "ready": "false", "epoch": "88", "assignment": "94", "last_request_sequence": "59"}}
	})
	old(nil) // closing an old broker must not unregister its replacement
	result := rt.RunTest(context.Background(), "test_legacy")
	if result.Result != "fail" {
		t.Fatal(result)
	}
	var snapshot Event
	for _, e := range result.Events {
		if e.Type == "mock.kafka.readiness_diagnostic" {
			snapshot = e
		}
	}
	if snapshot.Fields["group"] != "new" || snapshot.Fields["trigger"] != "test_failed" {
		t.Fatalf("snapshot lost: %v", snapshot)
	}
	current(nil)
	if len(rt.kafkaSnapshots) != 0 {
		t.Fatal("observer retained after close")
	}
}
