package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ANSI color codes.
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
)

// ConsoleHandler formats log records as colored, human-readable output.
// Format: "HH:MM:SS.mmm LEVEL [component] message key=value ..."
type ConsoleHandler struct {
	w     io.Writer
	opts  slog.HandlerOptions
	attrs []slog.Attr
	group string
	mu    *sync.Mutex
}

// NewConsoleHandler creates a console handler.
func NewConsoleHandler(w io.Writer, opts *slog.HandlerOptions) *ConsoleHandler {
	if opts == nil {
		opts = &slog.HandlerOptions{}
	}
	return &ConsoleHandler{
		w:    w,
		opts: *opts,
		mu:   &sync.Mutex{},
	}
}

func (h *ConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

func (h *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Timestamp
	ts := r.Time.Format(time.TimeOnly) + fmt.Sprintf(".%03d", r.Time.UnixMilli()%1000)

	// Level with color
	levelStr := colorLevel(r.Level)

	// Component prefix
	component := ""
	for _, a := range h.attrs {
		if a.Key == "component" {
			component = fmt.Sprintf("%s[%s]%s ", colorCyan, a.Value.String(), colorReset)
		}
	}

	// Message
	msg := r.Message

	// Extra attributes
	attrs := ""
	r.Attrs(func(a slog.Attr) bool {
		attrs += fmt.Sprintf(" %s%s%s=%v", colorGray, a.Key, colorReset, a.Value)
		return true
	})
	// Include handler-level attrs that aren't "component"
	for _, a := range h.attrs {
		if a.Key != "component" {
			attrs += fmt.Sprintf(" %s%s%s=%v", colorGray, a.Key, colorReset, a.Value)
		}
	}

	line := fmt.Sprintf("%s%s%s %s %s%s%s%s\n",
		colorGray, ts, colorReset,
		levelStr,
		component,
		msg,
		attrs,
		colorReset,
	)

	_, err := io.WriteString(h.w, line)
	return err
}

func (h *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ConsoleHandler{
		w:     h.w,
		opts:  h.opts,
		attrs: append(h.attrs[:len(h.attrs):len(h.attrs)], attrs...),
		group: h.group,
		mu:    h.mu,
	}
}

func (h *ConsoleHandler) WithGroup(name string) slog.Handler {
	return &ConsoleHandler{
		w:     h.w,
		opts:  h.opts,
		attrs: h.attrs,
		group: name,
		mu:    h.mu,
	}
}

func colorLevel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return colorRed + colorBold + "ERR" + colorReset
	case level >= slog.LevelWarn:
		return colorYellow + "WRN" + colorReset
	case level >= slog.LevelInfo:
		return colorGreen + "INF" + colorReset
	default:
		return colorGray + "DBG" + colorReset
	}
}
