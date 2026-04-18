package proxy

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// TestExtractCQL_Query verifies CQL text extraction from QUERY opcode body.
func TestExtractCQL_Query(t *testing.T) {
	cql := "SELECT * FROM orders WHERE id = 42"

	var body bytes.Buffer
	// [long string] = int length + bytes
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(cql)))
	body.Write(lenBuf)
	body.WriteString(cql)
	// Plus consistency + flags (not inspected by extractCQL).
	body.Write([]byte{0, 0x01, 0x00})

	got := extractCQL(cqlOpQuery, body.Bytes())
	if got != cql {
		t.Errorf("got %q, want %q", got, cql)
	}
}

func TestExtractCQL_Execute(t *testing.T) {
	// EXECUTE references a prepared_id, not CQL text. Should return "".
	got := extractCQL(cqlOpExecute, []byte{0, 1, 2, 3})
	if got != "" {
		t.Errorf("EXECUTE should return empty CQL, got %q", got)
	}
}

func TestExtractCQL_MalformedShort(t *testing.T) {
	// Body too short for length prefix.
	got := extractCQL(cqlOpQuery, []byte{0, 0})
	if got != "" {
		t.Errorf("malformed body should return empty, got %q", got)
	}
}

// TestSendCassandraError verifies the ERROR frame has the right opcode,
// response flag, stream ID, and contains the message.
func TestSendCassandraError(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	go func() {
		sendCassandraError(server, 4, 7, "injected write timeout")
		server.Close()
	}()

	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	header := make([]byte, 9)
	if _, err := client.Read(header); err != nil {
		t.Fatalf("read header: %v", err)
	}

	if header[0] != (4 | cqlResponseFlag) {
		t.Errorf("version = %#x, want %#x (with response flag)", header[0], 4|cqlResponseFlag)
	}
	streamID := binary.BigEndian.Uint16(header[2:4])
	if streamID != 7 {
		t.Errorf("stream_id = %d, want 7", streamID)
	}
	if header[4] != cqlOpError {
		t.Errorf("opcode = %#x, want ERROR (%#x)", header[4], cqlOpError)
	}

	bodyLen := binary.BigEndian.Uint32(header[5:9])
	body := make([]byte, bodyLen)
	if _, err := client.Read(body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Body: error_code (4) + string length (2) + message bytes.
	if len(body) < 6 {
		t.Fatalf("body too short: %d bytes", len(body))
	}
	msgLen := binary.BigEndian.Uint16(body[4:6])
	msg := string(body[6 : 6+msgLen])
	if msg != "injected write timeout" {
		t.Errorf("message = %q, want %q", msg, "injected write timeout")
	}
}

func TestCassandraProxy_Protocol(t *testing.T) {
	p, err := newProxy("cassandra", nil, "test")
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	if p.Protocol() != "cassandra" {
		t.Errorf("Protocol() = %q, want cassandra", p.Protocol())
	}
}
