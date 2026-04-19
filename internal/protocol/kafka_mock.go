package protocol

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/twmb/franz-go/pkg/kfake"
)

// ServeMock implements MockHandler for Kafka. It stands up an in-process
// broker via github.com/twmb/franz-go/pkg/kfake — a battle-tested Go
// implementation of the Kafka wire protocol that real clients (kafka-go,
// franz-go, sarama) speak to without modification.
//
// Configuration keys (from spec.Config):
//
//	topics: map[string]any — topic name → list of seed messages (or empty
//	  list to just create the topic). Seed messages are not yet loaded
//	  into the log (kfake limitation); topics are created with 1 partition.
//	partitions: int — default partition count when not inferable (default 1).
//
// Routes/Default on spec are ignored — Kafka has no route table in the
// HTTP sense. Interception of specific API calls is a future feature
// using kfake.Control().
func (p *kafkaProtocol) ServeMock(ctx context.Context, addr string, spec MockSpec, emit MockEmitter) error {
	port, err := portFromAddr(addr)
	if err != nil {
		return fmt.Errorf("mock kafka addr %q: %w", addr, err)
	}

	partitions := int32(1)
	if v, ok := spec.Config["partitions"].(int32); ok && v > 0 {
		partitions = v
	} else if v, ok := spec.Config["partitions"].(int); ok && v > 0 {
		partitions = int32(v)
	}

	topics := extractTopicNames(spec.Config)

	opts := []kfake.Opt{
		kfake.Ports(port),
		kfake.NumBrokers(1),
		kfake.AllowAutoTopicCreation(),
		kfake.DefaultNumPartitions(int(partitions)),
	}
	if len(topics) > 0 {
		opts = append(opts, kfake.SeedTopics(partitions, topics...))
	}

	cluster, err := kfake.NewCluster(opts...)
	if err != nil {
		return fmt.Errorf("mock kafka start: %w", err)
	}

	emitWith(emit, "started", map[string]string{
		"port":   strconv.Itoa(port),
		"topics": strings.Join(topics, ","),
	})

	<-ctx.Done()
	cluster.Close()
	emitWith(emit, "stopped", nil)
	return nil
}

// extractTopicNames returns the topic names declared in Config. Accepts
// either a Go native map[string]any (each key is a topic name) or any
// iterable shape that contains string keys. Values (message lists) are
// ignored in v0.8 — kfake doesn't expose a pre-populate API yet.
func extractTopicNames(config map[string]any) []string {
	raw, ok := config["topics"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

// portFromAddr extracts the integer port from a "host:port" string.
func portFromAddr(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(portStr)
}
