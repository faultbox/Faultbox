package star

import (
	"strings"
	"testing"
)

// TestKafkaConsumerGroupIsScopedPerRunAndTest.
//
// A consumer group's committed offsets live in the broker and outlive the
// reader. With one fixed group ID, the second test to consume resumed
// after whatever the first committed, and a broker kept across tests
// carried that between runs — so what a test saw depended on what ran
// before it, at any seed. That is the reported Kafka reproducibility gap.
func TestKafkaConsumerGroupIsScopedPerRunAndTest(t *testing.T) {
	t.Run("two tests in one run do not share a group", func(t *testing.T) {
		a := defaultKafkaGroup("abc123", "test_publish")
		b := defaultKafkaGroup("abc123", "test_consume")
		if a == b {
			t.Errorf("two tests share the consumer group %q, so the second resumes after the first", a)
		}
	})

	t.Run("two runs of the same test do not share a group", func(t *testing.T) {
		a := defaultKafkaGroup("run1", "test_consume")
		b := defaultKafkaGroup("run2", "test_consume")
		if a == b {
			t.Errorf("two runs share the consumer group %q, so a reused broker leaks offsets between them", a)
		}
	})

	t.Run("the same test in one run is stable", func(t *testing.T) {
		// Consuming twice inside one test must continue, not restart.
		a := defaultKafkaGroup("abc123", "test_consume")
		b := defaultKafkaGroup("abc123", "test_consume")
		if a != b {
			t.Errorf("group is unstable within a test: %q vs %q", a, b)
		}
	})

	t.Run("characters Kafka rejects are stripped", func(t *testing.T) {
		got := defaultKafkaGroup("n1", "test/with spaces:and#junk")
		if strings.ContainsAny(got, "/ :#") {
			t.Errorf("group %q contains characters Kafka rejects", got)
		}
	})

	t.Run("a very long test name is bounded", func(t *testing.T) {
		got := defaultKafkaGroup("n1", strings.Repeat("x", 500))
		if len(got) > 120 {
			t.Errorf("group is %d chars, too long for a Kafka group ID", len(got))
		}
	})

	t.Run("an unnamed context still gets a group", func(t *testing.T) {
		if got := defaultKafkaGroup("n1", ""); got == "" || strings.HasSuffix(got, "-") {
			t.Errorf("empty test name produced %q", got)
		}
	})
}

// TestApplyKafkaGroupDefaultRespectsAnExplicitGroup guards the escape
// hatch. A spec that is *about* consumer-group semantics — rebalances,
// redelivery, offset commits — needs a stable name, and overriding it
// must win.
func TestApplyKafkaGroupDefaultRespectsAnExplicitGroup(t *testing.T) {
	rt := &Runtime{runNonce: "n1", currentTestName: "test_x"}

	t.Run("explicit group is left alone", func(t *testing.T) {
		args := map[string]any{"topic": "orders", "group": "my-group"}
		rt.applyKafkaGroupDefault("kafka", "consume", args)
		if args["group"] != "my-group" {
			t.Errorf("explicit group was overwritten with %v", args["group"])
		}
	})

	t.Run("missing group is filled in", func(t *testing.T) {
		args := map[string]any{"topic": "orders"}
		rt.applyKafkaGroupDefault("kafka", "consume", args)
		g, _ := args["group"].(string)
		if !strings.HasPrefix(g, "faultbox-n1-test_x") {
			t.Errorf("group = %q, want it scoped to the run and test", g)
		}
	})

	t.Run("an empty group counts as missing", func(t *testing.T) {
		args := map[string]any{"topic": "orders", "group": ""}
		rt.applyKafkaGroupDefault("kafka", "consume", args)
		if g, _ := args["group"].(string); g == "" {
			t.Error("empty group was left empty")
		}
	})

	t.Run("other protocols and methods are untouched", func(t *testing.T) {
		for _, tc := range []struct{ proto, method string }{
			{"kafka", "publish"},
			{"redis", "consume"},
			{"http", "get"},
		} {
			args := map[string]any{"topic": "orders"}
			rt.applyKafkaGroupDefault(tc.proto, tc.method, args)
			if _, ok := args["group"]; ok {
				t.Errorf("%s.%s had a consumer group injected", tc.proto, tc.method)
			}
		}
	})
}

// TestRunNonceIsUniquePerRuntime asserts the property the whole fix rests
// on: two runtimes in the same process do not collide.
func TestRunNonceIsUniquePerRuntime(t *testing.T) {
	a := New(testLogger())
	b := New(testLogger())
	if a.runNonce == "" {
		t.Fatal("run nonce is empty")
	}
	if a.runNonce == b.runNonce {
		t.Errorf("two runtimes share the nonce %q", a.runNonce)
	}
}
