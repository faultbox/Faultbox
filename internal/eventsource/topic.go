package eventsource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// groupNonce identifies this process's run. Generated once, because two
// readers in the same run observing the same topic should share a group;
// two separate runs must not.
var groupNonce = newGroupNonce()

func newGroupNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "norand"
	}
	return hex.EncodeToString(b)
}

var groupSafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// defaultTopicGroup names the consumer group an observer uses when the
// spec did not name one.
//
// Why this is not the constant it used to be: a consumer group's committed
// offsets live in the broker's `__consumer_offsets` and outlive the reader
// that wrote them. Under one fixed ID, a broker container kept across runs
// hands the next run whatever the last one committed, so what an observer
// sees depends on what ran before it — the same defect fixed for the
// `consume()` step in a8689e1, which this path did not actually receive
// despite that commit's message.
//
// Scoped per (run, topic) rather than per (run, test): an event source is
// constructed from a flat param map with no access to the running test.
// That is weaker than the step path, and enough to stop offsets leaking
// between runs, which is the part that made results irreproducible.
func defaultTopicGroup(topic string) string {
	name := groupSafe.ReplaceAllString(topic, "-")
	if len(name) > 80 {
		name = name[:80]
	}
	return fmt.Sprintf("faultbox-%s-%s", groupNonce, name)
}

func init() {
	RegisterSource("topic", func(params map[string]string, decoder Decoder) (EventSource, error) {
		broker := params["broker"]
		if broker == "" {
			broker = "localhost:9092"
		}
		topic := params["topic"]
		if topic == "" {
			return nil, nil
		}
		group := params["group"]
		if group == "" {
			group = defaultTopicGroup(topic)
		}
		return &topicSource{
			broker:  broker,
			topic:   topic,
			group:   group,
			decoder: decoder,
		}, nil
	})
}

type topicSource struct {
	broker  string
	topic   string
	group   string
	decoder Decoder
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (s *topicSource) Name() string { return "topic" }

func (s *topicSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{s.broker},
		Topic:    s.topic,
		GroupID:  s.group,
		MaxWait:  1 * time.Second,
		MinBytes: 1,
		MaxBytes: 10e6,
	})

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer reader.Close()

		for {
			msg, err := reader.ReadMessage(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}

			fields := map[string]string{
				"topic":     msg.Topic,
				"partition": json.Number(json.Number(string(rune(msg.Partition + '0')))).String(),
				"key":       string(msg.Key),
			}

			// Decode the message value.
			if s.decoder != nil {
				decoded, err := s.decoder.Decode(msg.Value)
				if err == nil {
					for k, v := range decoded {
						fields[k] = v
					}
				} else {
					fields["value"] = string(msg.Value)
				}
			} else {
				fields["value"] = string(msg.Value)
			}

			// Store full message as JSON in "data" for auto-decoding.
			msgData, _ := json.Marshal(map[string]any{
				"topic":     msg.Topic,
				"partition": msg.Partition,
				"offset":    msg.Offset,
				"key":       string(msg.Key),
				"value":     string(msg.Value),
			})
			fields["data"] = string(msgData)

			cfg.Emit("topic", fields)
		}
	}()

	return nil
}

func (s *topicSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
