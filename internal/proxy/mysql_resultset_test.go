package proxy

import (
	"bytes"
	"context"
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

// Exercise the full proxy path, including a command with no response and a
// second query on the same connection after the supplied server response.
func forwardResponseResult(t *testing.T, script []byte) []byte {
	t.Helper()
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	done := make(chan error, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(mysqlPacket(0, []byte{0x0a}))
		if _, err = readTestMySQLPacket(c); err != nil {
			done <- err
			return
		}
		c.Write(mysqlPacket(2, []byte{0}))
		if _, err = readTestMySQLPacket(c); err != nil {
			done <- err
			return
		}
		c.Write(script)
		// COM_STMT_CLOSE deliberately has no server response.
		if _, err = readTestMySQLPacket(c); err != nil {
			done <- err
			return
		}
		if _, err = readTestMySQLPacket(c); err != nil {
			done <- err
			return
		}
		_, err = c.Write(mysqlPacket(1, []byte{0}))
		done <- err
	}()
	proxy := newMySQLProxy(nil, "db")
	addr, err := proxy.Start(context.Background(), upstream.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Stop()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	readTestMySQLPacket(c)
	c.Write(mysqlPacket(1, []byte{1}))
	readTestMySQLPacket(c)
	c.Write(mysqlPacket(0, append([]byte{0x16}, []byte("SELECT ?")...)))
	got := make([]byte, len(script))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("response stalled: %v", err)
	}
	c.Write(mysqlPacket(0, []byte{0x19, 1, 0, 0, 0}))
	c.Write(mysqlPacket(0, []byte{0x0e}))
	if _, err := readTestMySQLPacket(c); err != nil {
		t.Fatalf("query after no-response command stalled: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return got
}

func readTestMySQLPacket(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	body := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	_, err := io.ReadFull(r, body)
	return body, err
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

func TestMySQLProxyPreparedMetadataAndBinaryRows(t *testing.T) {
	script := mysqlPacket(1, []byte{0, 1, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0})
	script = append(script, mysqlPacket(2, []byte("parameter"))...)
	script = append(script, mysqlPacket(3, []byte{0xfe, 0, 0, 2, 0})...)
	script = append(script, mysqlPacket(4, []byte("column"))...)
	script = append(script, mysqlPacket(5, []byte{0xfe, 0, 0, 2, 0})...)
	if got := forwardResponseResult(t, script); !bytes.Equal(got, script) {
		t.Fatal("prepared metadata changed")
	}
	rows := append(mysqlPacket(1, []byte{1}), mysqlPacket(2, []byte("column"))...)
	rows = append(rows, mysqlPacket(3, []byte{0, 0, 42, 0, 0, 0})...)
	rows = append(rows, mysqlPacket(4, []byte{0, 0, 43, 0, 0, 0})...)
	rows = append(rows, mysqlPacket(5, []byte{0xfe, 0, 0, 2, 0})...)
	if got := forwardResponseResult(t, rows); !bytes.Equal(got, rows) {
		t.Fatal("binary rows changed")
	}
}
