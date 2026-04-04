package decoder

import (
	"testing"

	"github.com/faultbox/Faultbox/internal/eventsource"
)

func TestJSONDecoder(t *testing.T) {
	factory, ok := eventsource.GetDecoder("json")
	if !ok {
		t.Fatal("json decoder not registered")
	}
	dec, err := factory(nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	fields, err := dec.Decode([]byte(`{"level":"INFO","msg":"started","port":8080}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if fields["level"] != "INFO" {
		t.Errorf("level = %q", fields["level"])
	}
	if fields["msg"] != "started" {
		t.Errorf("msg = %q", fields["msg"])
	}
	if fields["data"] == "" {
		t.Error("expected 'data' field with full JSON")
	}
}

func TestLogfmtDecoder(t *testing.T) {
	factory, ok := eventsource.GetDecoder("logfmt")
	if !ok {
		t.Fatal("logfmt decoder not registered")
	}
	dec, err := factory(nil)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	fields, err := dec.Decode([]byte(`level=INFO msg="server started" port=8080`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if fields["level"] != "INFO" {
		t.Errorf("level = %q", fields["level"])
	}
	if fields["msg"] != "server started" {
		t.Errorf("msg = %q", fields["msg"])
	}
	if fields["port"] != "8080" {
		t.Errorf("port = %q", fields["port"])
	}
}

func TestRegexDecoder(t *testing.T) {
	factory, ok := eventsource.GetDecoder("regex")
	if !ok {
		t.Fatal("regex decoder not registered")
	}
	dec, err := factory(map[string]string{
		"pattern": `WAL: (?P<action>\w+) (?P<path>.+)`,
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	fields, err := dec.Decode([]byte("WAL: fsync /data/wal/000001"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if fields["action"] != "fsync" {
		t.Errorf("action = %q", fields["action"])
	}
	if fields["path"] != "/data/wal/000001" {
		t.Errorf("path = %q", fields["path"])
	}
}

func TestRegexDecoderNoMatch(t *testing.T) {
	factory, _ := eventsource.GetDecoder("regex")
	dec, _ := factory(map[string]string{"pattern": `WAL: (?P<action>\w+)`})

	_, err := dec.Decode([]byte("something else"))
	if err == nil {
		t.Error("expected error on no match")
	}
}

func TestRegexDecoderNoPattern(t *testing.T) {
	factory, _ := eventsource.GetDecoder("regex")
	_, err := factory(map[string]string{})
	if err == nil {
		t.Error("expected error when no pattern provided")
	}
}
