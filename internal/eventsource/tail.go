package eventsource

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"

	"github.com/fsnotify/fsnotify"
)

func init() {
	RegisterSource("tail", func(params map[string]string, decoder Decoder) (EventSource, error) {
		path := params["path"]
		if path == "" {
			return nil, nil
		}
		return &tailSource{path: path, decoder: decoder}, nil
	})
}

type tailSource struct {
	path    string
	decoder Decoder
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (s *tailSource) Name() string { return "tail" }

func (s *tailSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.tailFile(ctx, cfg)
	}()

	return nil
}

func (s *tailSource) tailFile(ctx context.Context, cfg SourceConfig) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer watcher.Close()

	// Open the file and seek to end (only read new content).
	f, err := os.Open(s.path)
	if err != nil {
		// File might not exist yet — watch the directory and wait.
		return
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	if err := watcher.Add(s.path); err != nil {
		return
	}

	reader := bufio.NewScanner(f)

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				// Read new lines.
				for reader.Scan() {
					line := reader.Bytes()
					if len(line) == 0 {
						continue
					}
					if s.decoder != nil {
						fields, err := s.decoder.Decode(line)
						if err != nil {
							cfg.Emit("tail", map[string]string{"raw": string(line), "path": s.path})
							continue
						}
						fields["path"] = s.path
						cfg.Emit("tail", fields)
					} else {
						cfg.Emit("tail", map[string]string{"raw": string(line), "path": s.path})
					}
				}
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (s *tailSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
