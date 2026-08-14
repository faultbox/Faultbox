package star

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// newRunNonce returns a short random token identifying this process's run.
// Falls back to a fixed token if the system RNG fails — a stable name is
// worse than a random one here, but it is better than refusing to run.
func newRunNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "norand"
	}
	return hex.EncodeToString(b)
}

// kafkaGroupSafe strips characters Kafka rejects in a group ID.
var kafkaGroupSafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// defaultKafkaGroup builds the consumer group a `consume()` step uses when
// the spec did not name one.
//
// Why this is not a constant: it used to be, and that made every
// Kafka-consuming spec non-deterministic at a fixed seed.
//
// A consumer group's committed offsets live in the broker, in
// `__consumer_offsets`, and outlive the reader that wrote them. With a
// single fixed group ID, the second test to consume resumes after
// whatever the first test committed — and a broker container kept across
// tests (which is normal, brokers are slow to start) carries that state
// between runs too. What a test saw therefore depended on what had run
// before it, at any seed. The reporting team hit exactly this and worked
// around it by seeding consumer-group offsets themselves.
//
// A group scoped to (run, test) has never committed anything, so
// kafka-go's `StartOffset` default of `FirstOffset` applies and the read
// begins at the start of the topic every time. The test sees exactly the
// messages it produced.
//
// The nonce is per-process rather than per-spec on purpose: two runs of
// the same spec against a reused broker must not share offsets, and the
// seed cannot supply that because a fixed seed is the case being fixed.
//
// Specs that are *about* consumer-group semantics — rebalances,
// redelivery, offset commits — pass `group=` explicitly and get the
// stable name they need.
func defaultKafkaGroup(nonce, testName string) string {
	name := testName
	if name == "" {
		name = "spec"
	}
	name = kafkaGroupSafe.ReplaceAllString(name, "-")
	if len(name) > 80 {
		name = name[:80]
	}
	return fmt.Sprintf("faultbox-%s-%s", nonce, name)
}

// applyKafkaGroupDefault injects the per-test consumer group into a
// kafka consume step that did not specify one.
//
// Injected after the step_send event is built, deliberately: the group
// name carries a per-run nonce, and putting it in the trace would make
// two runs of the same spec differ in a field nothing asserts on.
func (rt *Runtime) applyKafkaGroupDefault(protocolName, method string, args map[string]any) {
	if protocolName != "kafka" || method != "consume" {
		return
	}
	if g, ok := args["group"]; ok {
		if s, isStr := g.(string); !isStr || s != "" {
			return // spec named one
		}
	}
	args["group"] = defaultKafkaGroup(rt.runNonce, rt.currentTestName)
}
