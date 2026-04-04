package eventsource

import (
	"context"
	"testing"
)

type testSource struct{ name string }

func (s *testSource) Name() string                              { return s.name }
func (s *testSource) Start(ctx context.Context, cfg SourceConfig) error { return nil }
func (s *testSource) Stop() error                               { return nil }

type testDecoder struct{ name string }

func (d *testDecoder) Name() string                               { return d.name }
func (d *testDecoder) Decode(raw []byte) (map[string]string, error) {
	return map[string]string{"raw": string(raw)}, nil
}

func resetRegistries(t *testing.T) func() {
	t.Helper()
	sourceMu.Lock()
	oldSources := sourceRegistry
	sourceRegistry = make(map[string]SourceFactory)
	sourceMu.Unlock()
	decoderMu.Lock()
	oldDecoders := decoderRegistry
	decoderRegistry = make(map[string]DecoderFactory)
	decoderMu.Unlock()
	return func() {
		sourceMu.Lock()
		sourceRegistry = oldSources
		sourceMu.Unlock()
		decoderMu.Lock()
		decoderRegistry = oldDecoders
		decoderMu.Unlock()
	}
}

func TestSourceRegistry(t *testing.T) {
	defer resetRegistries(t)()

	RegisterSource("test-src", func(params map[string]string, decoder Decoder) (EventSource, error) {
		return &testSource{name: "test-src"}, nil
	})

	factory, ok := GetSource("test-src")
	if !ok {
		t.Fatal("expected to find test-src")
	}
	src, err := factory(nil, nil)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	if src.Name() != "test-src" {
		t.Fatalf("Name() = %q, want test-src", src.Name())
	}

	_, ok = GetSource("nonexistent")
	if ok {
		t.Fatal("expected nonexistent to not be found")
	}
}

func TestDecoderRegistry(t *testing.T) {
	defer resetRegistries(t)()

	RegisterDecoder("test-dec", func(params map[string]string) (Decoder, error) {
		return &testDecoder{name: "test-dec"}, nil
	})

	factory, ok := GetDecoder("test-dec")
	if !ok {
		t.Fatal("expected to find test-dec")
	}
	dec, err := factory(nil)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	fields, err := dec.Decode([]byte("hello"))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if fields["raw"] != "hello" {
		t.Fatalf("fields[raw] = %q, want hello", fields["raw"])
	}
}

func TestDuplicateSourcePanics(t *testing.T) {
	defer resetRegistries(t)()

	RegisterSource("dup", func(map[string]string, Decoder) (EventSource, error) { return nil, nil })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate source registration")
		}
	}()
	RegisterSource("dup", func(map[string]string, Decoder) (EventSource, error) { return nil, nil })
}

func TestDuplicateDecoderPanics(t *testing.T) {
	defer resetRegistries(t)()

	RegisterDecoder("dup", func(map[string]string) (Decoder, error) { return nil, nil })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate decoder registration")
		}
	}()
	RegisterDecoder("dup", func(map[string]string) (Decoder, error) { return nil, nil })
}
