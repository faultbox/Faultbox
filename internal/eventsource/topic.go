package eventsource

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

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
			group = "faultbox"
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
