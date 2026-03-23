// Package logging provides structured logging for Faultbox with two output modes:
// colored console output for humans and JSON lines for machines.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

// Format selects the log output format.
type Format int

const (
	// FormatAuto detects: console if TTY, JSON if piped.
	FormatAuto Format = iota
	// FormatConsole forces colored, human-readable output.
	FormatConsole
	// FormatJSON forces structured JSON lines output.
	FormatJSON
)

// Config controls logger behavior.
type Config struct {
	// Output destination (default: os.Stderr).
	Output io.Writer
	// Format selects console vs JSON (default: FormatAuto).
	Format Format
	// Level sets minimum log level (default: slog.LevelInfo).
	Level slog.Level
}

// New creates a configured slog.Logger.
func New(cfg Config) *slog.Logger {
	if cfg.Output == nil {
		cfg.Output = os.Stderr
	}

	format := cfg.Format
	if format == FormatAuto {
		format = detectFormat(cfg.Output)
	}

	opts := &slog.HandlerOptions{
		Level: cfg.Level,
	}

	var handler slog.Handler
	switch format {
	case FormatJSON:
		handler = slog.NewJSONHandler(cfg.Output, opts)
	default:
		handler = NewConsoleHandler(cfg.Output, opts)
	}

	return slog.New(handler)
}

// WithComponent returns a logger with a "component" attribute.
func WithComponent(logger *slog.Logger, name string) *slog.Logger {
	return logger.With(slog.String("component", name))
}

type ctxKey struct{}

// NewContext stores a logger in the context.
func NewContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext retrieves the logger from context, or returns the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func detectFormat(w io.Writer) Format {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return FormatConsole
	}
	return FormatJSON
}
