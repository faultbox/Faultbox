package eventsource

import (
	"bufio"
	"context"
	"io"
	"sync"
)

func init() {
	RegisterSource("stderr", func(params map[string]string, decoder Decoder) (EventSource, error) {
		return &stderrSource{decoder: decoder}, nil
	})
}

// stderrSource is the stdout-source twin for the service's stderr stream.
// Customer ask (inDrive Freight, 2026-04-30): every Go service using zap
// or logrus defaults to stderr; the v0.12.15.x arc worked around that
// with an FB_LOG_TO_STDOUT env-gate, but a one-line
// observe=[stderr(decoder=...)] removes the SUT-side change for every
// future adopter.
//
// Same shape as stdoutSource — the decoder, scanner, emit pattern, and
// shutdown semantics are identical. The only thing that differs is which
// fd the runtime wires to the pipe (rt.runtime.go's
// captureServiceObservations branch chooses based on SourceName) and the
// event type stamped on each emission ("stderr" vs "stdout").
type stderrSource struct {
	decoder Decoder
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (s *stderrSource) Name() string { return "stderr" }

// Start is the placeholder hook required by the EventSource interface.
// Real entry is StartWithReader; the runtime materialises a pipe whose
// write end is wired to the service process's Stderr fd, then hands the
// read end here.
func (s *stderrSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)
	reader, ok := cfg.Params["_reader_ptr"]
	if !ok {
		return nil
	}
	_ = reader

	return nil
}

// StartWithReader is the actual entry point — the runtime passes an io.Reader.
func (s *stderrSource) StartWithReader(ctx context.Context, cfg SourceConfig, reader io.Reader) {
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
					cfg.Emit("stderr", map[string]string{"raw": string(line)})
					continue
				}
				cfg.Emit("stderr", fields)
			} else {
				cfg.Emit("stderr", map[string]string{"raw": string(line)})
			}
		}
	}()
}

func (s *stderrSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}

// StderrSource returns a new stderr event source (exported for runtime use).
func StderrSource(decoder Decoder) *stderrSource {
	return &stderrSource{decoder: decoder}
}

// StderrSourceHandle holds a running stderr source and its pipe for
// cleanup. Mirrors StdoutSourceHandle so the runtime's lifecycle code
// can treat the two streams uniformly.
type StderrSourceHandle struct {
	Source    *stderrSource
	PipeWrite io.WriteCloser
}
