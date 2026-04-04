package eventsource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

func init() {
	RegisterSource("poll", func(params map[string]string, decoder Decoder) (EventSource, error) {
		url := params["url"]
		if url == "" {
			return nil, nil
		}
		interval := 5 * time.Second
		if s, ok := params["interval"]; ok {
			if d, err := time.ParseDuration(s); err == nil {
				interval = d
			}
		}
		return &pollSource{url: url, interval: interval, decoder: decoder}, nil
	})
}

type pollSource struct {
	url      string
	interval time.Duration
	decoder  Decoder
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func (s *pollSource) Name() string { return "poll" }

func (s *pollSource) Start(ctx context.Context, cfg SourceConfig) error {
	ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pollLoop(ctx, cfg)
	}()

	return nil
}

func (s *pollSource) pollLoop(ctx context.Context, cfg SourceConfig) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fetchAndEmit(ctx, client, cfg)
		}
	}
}

func (s *pollSource) fetchAndEmit(ctx context.Context, client *http.Client, cfg SourceConfig) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.url, nil)
	if err != nil {
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		cfg.Emit("poll", map[string]string{
			"url":   s.url,
			"error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	bodyStr := strings.TrimSpace(string(body))

	if s.decoder != nil {
		fields, err := s.decoder.Decode(body)
		if err != nil {
			cfg.Emit("poll", map[string]string{
				"url":    s.url,
				"status": fmt.Sprintf("%d", resp.StatusCode),
				"body":   bodyStr,
			})
			return
		}
		fields["url"] = s.url
		fields["status"] = fmt.Sprintf("%d", resp.StatusCode)
		cfg.Emit("poll", fields)
	} else {
		cfg.Emit("poll", map[string]string{
			"url":    s.url,
			"status": fmt.Sprintf("%d", resp.StatusCode),
			"body":   bodyStr,
		})
	}
}

func (s *pollSource) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
