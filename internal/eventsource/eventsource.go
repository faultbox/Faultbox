// Package eventsource defines the EventSource and Decoder plugin interfaces
// and global registries. Event sources produce events from external channels
// (stdout, Kafka topics, Postgres WAL, file tail, HTTP polling) into the
// Faultbox EventLog as first-class trace events.
//
// Plugins self-register via init():
//
//	func init() { eventsource.RegisterSource("stdout", newStdoutSource) }
package eventsource

import (
	"context"
	"fmt"
	"sync"
)

// EventSource produces events from an external channel into the EventLog.
type EventSource interface {
	// Name returns the source identifier (e.g., "stdout", "topic", "wal_stream").
	Name() string

	// Start begins producing events. The SourceConfig provides the Emit callback
	// to send events to the EventLog. Blocks until ctx is cancelled or Stop is called.
	Start(ctx context.Context, cfg SourceConfig) error

	// Stop gracefully shuts down the event source.
	Stop() error
}

// SourceConfig is passed to EventSource.Start with everything needed to operate.
type SourceConfig struct {
	// ServiceName is the service this source is attached to.
	ServiceName string

	// Params are source-specific configuration from Starlark kwargs.
	Params map[string]string

	// Decoder parses raw data into event fields.
	Decoder Decoder

	// Emit sends an event to the EventLog. Type is the event type (e.g., "stdout",
	// "topic", "wal"). Fields are auto-decoded from JSON in the Starlark layer —
	// the "data" field should contain a JSON string for structured payloads.
	Emit func(typ string, fields map[string]string)
}

// Decoder parses raw bytes (a line of output, a message payload) into event fields.
// The output is a flat map — structured data goes in the "data" key as a JSON string,
// which the Starlark runtime auto-decodes into .data on the event object.
type Decoder interface {
	// Name returns the decoder identifier (e.g., "json", "logfmt", "regex").
	Name() string

	// Decode parses a raw line/message into event fields.
	Decode(raw []byte) (map[string]string, error)
}

// SourceFactory creates a new EventSource instance from Starlark kwargs.
type SourceFactory func(params map[string]string, decoder Decoder) (EventSource, error)

// DecoderFactory creates a new Decoder instance from Starlark kwargs.
type DecoderFactory func(params map[string]string) (Decoder, error)

// Source registry.
var (
	sourceMu       sync.RWMutex
	sourceRegistry = make(map[string]SourceFactory)
)

// RegisterSource adds a source factory to the registry.
func RegisterSource(name string, factory SourceFactory) {
	sourceMu.Lock()
	defer sourceMu.Unlock()
	if _, exists := sourceRegistry[name]; exists {
		panic(fmt.Sprintf("event source %q already registered", name))
	}
	sourceRegistry[name] = factory
}

// GetSource returns a source factory by name.
func GetSource(name string) (SourceFactory, bool) {
	sourceMu.RLock()
	defer sourceMu.RUnlock()
	f, ok := sourceRegistry[name]
	return f, ok
}

// Decoder registry.
var (
	decoderMu       sync.RWMutex
	decoderRegistry = make(map[string]DecoderFactory)
)

// RegisterDecoder adds a decoder factory to the registry.
func RegisterDecoder(name string, factory DecoderFactory) {
	decoderMu.Lock()
	defer decoderMu.Unlock()
	if _, exists := decoderRegistry[name]; exists {
		panic(fmt.Sprintf("decoder %q already registered", name))
	}
	decoderRegistry[name] = factory
}

// GetDecoder returns a decoder factory by name.
func GetDecoder(name string) (DecoderFactory, bool) {
	decoderMu.RLock()
	defer decoderMu.RUnlock()
	f, ok := decoderRegistry[name]
	return f, ok
}

// SourceNames returns all registered source names.
func SourceNames() []string {
	sourceMu.RLock()
	defer sourceMu.RUnlock()
	names := make([]string, 0, len(sourceRegistry))
	for name := range sourceRegistry {
		names = append(names, name)
	}
	return names
}

// DecoderNames returns all registered decoder names.
func DecoderNames() []string {
	decoderMu.RLock()
	defer decoderMu.RUnlock()
	names := make([]string, 0, len(decoderRegistry))
	for name := range decoderRegistry {
		names = append(names, name)
	}
	return names
}
