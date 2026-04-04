package eventsource

import (
	"bufio"
	"context"
	"io"
	"sync"
)

func init() {
	RegisterSource("stdout", func(params map[string]string, decoder Decoder) (EventSource, error) {
		return &stdoutSource{decoder: decoder}, nil
	})
}

// stdoutSource reads lines from a pipe (connected to service stdout)
// and emits each decoded line as an event.
type stdoutSource struct {
	decoder Decoder
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (s *stdoutSource) Name() string { return "stdout" }

// Start reads from cfg.Params["reader"] (an io.Reader) and emits events.
// In practice, the runtime sets up a pipe and passes the read end here.
func (s *stdoutSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)
	reader, ok := cfg.Params["_reader_ptr"]
	if !ok {
		return nil // no reader configured
	}
	_ = reader // placeholder — actual reader passed via StartWithReader

	return nil
}

// StartWithReader is the actual entry point — the runtime passes an io.Reader.
func (s *stdoutSource) StartWithReader(ctx context.Context, cfg SourceConfig, reader io.Reader) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			if s.decoder != nil {
				fields, err := s.decoder.Decode(line)
				if err != nil {
					// Emit raw line if decode fails.
					cfg.Emit("stdout", map[string]string{"raw": string(line)})
					continue
				}
				cfg.Emit("stdout", fields)
			} else {
				cfg.Emit("stdout", map[string]string{"raw": string(line)})
			}
		}
	}()
}

func (s *stdoutSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}

// StdoutSource returns a new stdout event source (exported for runtime use).
func StdoutSource(decoder Decoder) *stdoutSource {
	return &stdoutSource{decoder: decoder}
}

// StdoutSourceHandle holds a running stdout source and its pipe for cleanup.
type StdoutSourceHandle struct {
	Source    *stdoutSource
	PipeWrite io.WriteCloser
}
