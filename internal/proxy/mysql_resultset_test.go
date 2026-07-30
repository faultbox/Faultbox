package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// The MySQL proxy hung forever on every result set.
//
// forwardResponse forwarded one packet per iteration and then peeked with a
// 100 ms deadline to decide whether more was coming. The peek consumed the
// terminator, so the next iteration issued an unconditional, deadline-free
// read for a packet the server would never send. Because Stop() waited on the
// connection WaitGroup, one stuck handler hung the whole run in teardown —
// after the test body had finished, so no per-test timeout could fire.
//
// `exec()` was unaffected: a single OK packet returns before the loop. So the
// bug was specific to `query()`, and it was invisible until v0.16.1 fixed the
// credentials that let a step authenticate and reach the command phase at all.
//
// Every case below would hang before the fix and must now terminate.

func mysqlPacket(seq byte, payload []byte) []byte {
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, payload...)
}

// forwardResponseResult runs forwardResponse against a scripted server and
// returns what the client saw. It fails the test if the call does not return,
// which is the regression this file exists to catch.
func forwardResponseResult(t *testing.T, serverScript []byte) []byte {
	t.Helper()

	proxyToServer, server := net.Pipe()
	proxyToClient, client := net.Pipe()

	go func() {
		_, _ = server.Write(serverScript)
		// Deliberately do NOT close: a real MySQL connection stays open
		// between statements, so a parser that keeps reading past the
		// terminator blocks rather than seeing EOF. Closing here would
		// hide exactly the bug under test.
	}()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(serverScript))
		n, _ := io.ReadFull(client, buf)
		got <- buf[:n]
	}()

	done := make(chan error, 1)
	go func() {
		_, err := (&mysqlProxy{}).forwardResponse(proxyToServer, proxyToClient)
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwardResponse did not return — this is the hang that wedged " +
			"every query() step through the MySQL proxy")
	}

	select {
	case b := <-got:
		return b
	case <-time.After(2 * time.Second):
		return nil
	}
}

// A single OK packet: what exec() gets. This path always worked.
func TestForwardResponse_OKPacket(t *testing.T) {
	script := mysqlPacket(1, []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00})
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d", len(got), len(script))
	}
}

func TestForwardResponse_ErrPacket(t *testing.T) {
	script := mysqlPacket(1, append([]byte{0xFF, 0x51, 0x04}, []byte("#42S02Table missing")...))
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d", len(got), len(script))
	}
}

// The classic framing: column count, column defs, EOF, rows, EOF.
// A client that did not negotiate CLIENT_DEPRECATE_EOF sees this.
func TestForwardResponse_ResultSetWithEOF(t *testing.T) {
	eof := []byte{0xFE, 0x00, 0x00, 0x02, 0x00}
	var script []byte
	script = append(script, mysqlPacket(1, []byte{0x01})...)       // 1 column
	script = append(script, mysqlPacket(2, []byte("coldef-n"))...) // column def
	script = append(script, mysqlPacket(3, eof)...)                // end of defs
	script = append(script, mysqlPacket(4, []byte{0x01, '7'})...)  // row
	script = append(script, mysqlPacket(5, eof)...)                // end of rows
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d — the whole result set must reach the client",
			len(got), len(script))
	}
}

// CLIENT_DEPRECATE_EOF (what modern drivers negotiate): no EOF after the
// column definitions, and the response ends with an OK rather than an EOF.
func TestForwardResponse_ResultSetDeprecateEOF(t *testing.T) {
	var script []byte
	script = append(script, mysqlPacket(1, []byte{0x01})...)
	script = append(script, mysqlPacket(2, []byte("coldef-n"))...)
	script = append(script, mysqlPacket(3, []byte{0x01, '7'})...)
	script = append(script, mysqlPacket(4, []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00})...)
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d", len(got), len(script))
	}
}

// Multiple columns must all be consumed as definitions, not mistaken for rows.
func TestForwardResponse_MultiColumnResultSet(t *testing.T) {
	eof := []byte{0xFE, 0x00, 0x00, 0x02, 0x00}
	var script []byte
	script = append(script, mysqlPacket(1, []byte{0x03})...) // 3 columns
	for i := 0; i < 3; i++ {
		script = append(script, mysqlPacket(byte(2+i), []byte("coldef"))...)
	}
	script = append(script, mysqlPacket(5, eof)...)
	script = append(script, mysqlPacket(6, []byte{0x01, 'a', 0x01, 'b', 0x01, 'c'})...)
	script = append(script, mysqlPacket(7, eof)...)
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d", len(got), len(script))
	}
}

// An empty result set — the SELECT that matches nothing. No row packets at all,
// so an off-by-one in the terminator logic shows up here.
func TestForwardResponse_EmptyResultSet(t *testing.T) {
	eof := []byte{0xFE, 0x00, 0x00, 0x02, 0x00}
	var script []byte
	script = append(script, mysqlPacket(1, []byte{0x01})...)
	script = append(script, mysqlPacket(2, []byte("coldef"))...)
	script = append(script, mysqlPacket(3, eof)...)
	script = append(script, mysqlPacket(4, eof)...)
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d", len(got), len(script))
	}
}

// A row whose first byte happens to be 0xFE is data, not a terminator — an EOF
// packet is always shorter than 9 bytes. Treating a long 0xFE packet as the end
// would truncate the result set.
func TestForwardResponse_RowStartingWith0xFEIsNotEOF(t *testing.T) {
	eof := []byte{0xFE, 0x00, 0x00, 0x02, 0x00}
	longRow := []byte{0xFE, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	var script []byte
	script = append(script, mysqlPacket(1, []byte{0x01})...)
	script = append(script, mysqlPacket(2, []byte("coldef"))...)
	script = append(script, mysqlPacket(3, eof)...)
	script = append(script, mysqlPacket(4, longRow)...)
	script = append(script, mysqlPacket(5, eof)...)
	if got := forwardResponseResult(t, script); len(got) != len(script) {
		t.Errorf("forwarded %d bytes, want %d — a long 0xFE packet is a row, not EOF",
			len(got), len(script))
	}
}

func TestMySQLLenEncInt(t *testing.T) {
	cases := []struct {
		payload []byte
		want    int
	}{
		{[]byte{0x00}, 0},
		{[]byte{0x01}, 1},
		{[]byte{0x0A}, 10},
		{[]byte{0xFA}, 250},
		{[]byte{0xFC, 0x10, 0x01}, 272},         // 2-byte form
		{[]byte{0xFD, 0x01, 0x00, 0x01}, 65537}, // 3-byte form
		{nil, 0},
		{[]byte{0xFC}, 0}, // truncated: no guessing
	}
	for _, tc := range cases {
		if got := mysqlLenEncInt(tc.payload); got != tc.want {
			t.Errorf("mysqlLenEncInt(%v) = %d, want %d", tc.payload, got, tc.want)
		}
	}
}

// The server read must be bounded. Without a deadline, a malformed or
// truncated response blocks a proxy goroutine forever and Stop() waits on it.
func TestForwardResponse_TruncatedResponseDoesNotHang(t *testing.T) {
	orig := mysqlResponseTimeout
	mysqlResponseTimeout = 300 * time.Millisecond
	defer func() { mysqlResponseTimeout = orig }()

	proxyToServer, server := net.Pipe()
	proxyToClient, client := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, client) }()

	// A column-count packet promising a result set, then nothing.
	go func() { _, _ = server.Write(mysqlPacket(1, []byte{0x01})) }()

	done := make(chan error, 1)
	go func() {
		_, err := (&mysqlProxy{}).forwardResponse(proxyToServer, proxyToClient)
		done <- err
	}()

	// The real deadline is 30s; this asserts the read is bounded at all rather
	// than re-testing the constant, so keep the window generous but finite.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a truncated response must fail, not block a goroutine forever")
	}
}
