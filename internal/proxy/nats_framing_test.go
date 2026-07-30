package proxy

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

// The NATS proxy corrupted every line it forwarded.
//
// It read with a bufio.Scanner (whose ScanLines strips the trailing "\r") and
// wrote with fmt.Fprintln (which appends only "\n"). NATS frames on CRLF, so
// every control line lost a byte in transit. The Go client reported the
// memorable `nats: expected 'PONG', got 'PONG'` — identical text, different
// framing — and publishing failed outright with EOF.
//
// It also split length-prefixed PUB/MSG payloads on newlines, so any payload
// containing one was corrupted and payload bytes could be mistaken for a
// protocol verb.

// relayOnce runs the relay over a scripted input and returns the bytes the
// destination received.
func relayOnce(t *testing.T, input []byte, verb, direction string) []byte {
	t.Helper()

	srcProxy, srcPeer := net.Pipe()
	dstProxy, dstPeer := net.Pipe()

	go func() {
		_, _ = srcPeer.Write(input)
		_ = srcPeer.Close()
	}()

	got := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(dstPeer)
		got <- buf
	}()

	done := make(chan struct{})
	go func() {
		_ = (&natsProxy{}).relayNATS(srcProxy, dstProxy, verb, direction,
			func(int) {}, func() {})
		_ = dstProxy.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayNATS did not return")
	}
	select {
	case b := <-got:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("destination never closed")
		return nil
	}
}

// The core regression: CRLF must survive byte-for-byte.
func TestRelayPreservesCRLF(t *testing.T) {
	in := []byte("PING\r\nPONG\r\n+OK\r\n")
	got := relayOnce(t, in, "PUB ", "publish")
	if string(got) != string(in) {
		t.Errorf("relay altered the byte stream:\n  in:  %q\n  got: %q\n"+
			"NATS frames on CRLF; dropping the \\r is what produced "+
			"\"expected 'PONG', got 'PONG'\"", in, got)
	}
}

// An INFO line is JSON and can be long; it must pass through untouched.
func TestRelayPreservesINFOLine(t *testing.T) {
	in := []byte("INFO {\"server_id\":\"abc\",\"version\":\"2.10.0\",\"max_payload\":1048576}\r\n")
	if got := relayOnce(t, in, "MSG ", "deliver"); string(got) != string(in) {
		t.Errorf("INFO line altered:\n  in:  %q\n  got: %q", in, got)
	}
}

// A PUB payload is length-prefixed and opaque. Forwarding it as "lines" breaks
// any payload containing a newline.
func TestRelayForwardsPayloadContainingNewlines(t *testing.T) {
	payload := "line one\nline two\nline three"
	in := []byte("PUB orders 28\r\n" + payload + "\r\n")
	if len(payload) != 28 {
		t.Fatalf("test payload is %d bytes, header says 28", len(payload))
	}
	got := relayOnce(t, in, "PUB ", "publish")
	if string(got) != string(in) {
		t.Errorf("payload with newlines corrupted:\n  in:  %q\n  got: %q", in, got)
	}
}

// Binary payloads must survive — including NUL and high bytes, which a
// text-oriented path mangles.
func TestRelayForwardsBinaryPayload(t *testing.T) {
	payload := []byte{0x00, 0xFF, 0x0A, 0x0D, 0x1B, 0x7F, 0x80}
	in := append([]byte("PUB bin 7\r\n"), payload...)
	in = append(in, '\r', '\n')
	if got := relayOnce(t, in, "PUB ", "publish"); string(got) != string(in) {
		t.Errorf("binary payload corrupted:\n  in:  %q\n  got: %q", in, got)
	}
}

// A payload that itself begins with "PUB " must not be treated as a control
// line — that is what length-prefixed reading buys.
func TestRelayDoesNotReinterpretPayloadAsVerb(t *testing.T) {
	payload := "PUB evil 99"
	in := []byte("PUB orders 11\r\n" + payload + "\r\n")
	if len(payload) != 11 {
		t.Fatalf("payload is %d bytes, header says 11", len(payload))
	}
	if got := relayOnce(t, in, "PUB ", "publish"); string(got) != string(in) {
		t.Errorf("payload mistaken for a control line:\n  in:  %q\n  got: %q", in, got)
	}
}

// A dropped message must take its payload with it, or the peer reads the
// payload as the next control line and the connection desynchronises. The
// line-based version could not do this at all.
func TestDropRemovesHeaderAndPayload(t *testing.T) {
	p := &natsProxy{}
	p.AddRule(Rule{Topic: "orders", Action: ActionDrop})

	srcProxy, srcPeer := net.Pipe()
	dstProxy, dstPeer := net.Pipe()

	in := []byte("PUB orders 5\r\nhello\r\nPING\r\n")
	go func() {
		_, _ = srcPeer.Write(in)
		_ = srcPeer.Close()
	}()

	got := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(dstPeer)
		got <- buf
	}()
	go func() {
		_ = p.relayNATS(srcProxy, dstProxy, "PUB ", "publish", func(int) {}, func() {})
		_ = dstProxy.Close()
	}()

	select {
	case b := <-got:
		if string(b) != "PING\r\n" {
			t.Errorf("after dropping the PUB, destination saw %q, want %q — "+
				"the payload must be dropped with its header", b, "PING\r\n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestNATSPayloadLen(t *testing.T) {
	cases := []struct {
		line string
		verb string
		want int
	}{
		{"PUB orders 11\r\n", "PUB ", 11},
		{"PUB orders reply.1 11\r\n", "PUB ", 11}, // with reply-to
		{"MSG orders 1 5\r\n", "MSG ", 5},
		{"MSG orders 1 reply.1 5\r\n", "MSG ", 5},
		{"PUB orders 0\r\n", "PUB ", 0}, // empty payload still has CRLF

		// No payload.
		{"PING\r\n", "PUB ", -1},
		{"+OK\r\n", "PUB ", -1},
		{"MSG orders 1 5\r\n", "PUB ", -1}, // wrong direction's verb
		{"PUB\r\n", "PUB ", -1},            // malformed
		{"PUB orders notanumber\r\n", "PUB ", -1},
		{"PUB orders -3\r\n", "PUB ", -1}, // negative size is not a length
	}
	for _, tc := range cases {
		if got := natsPayloadLen([]byte(tc.line), tc.verb); got != tc.want {
			t.Errorf("natsPayloadLen(%q, %q) = %d, want %d", tc.line, tc.verb, got, tc.want)
		}
	}
}

// A truncated payload must end the relay rather than forward a short read.
func TestRelayStopsOnTruncatedPayload(t *testing.T) {
	srcProxy, srcPeer := net.Pipe()
	dstProxy, dstPeer := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, dstPeer) }()

	go func() {
		// Declares 100 bytes, sends 5.
		_, _ = srcPeer.Write([]byte("PUB orders 100\r\nhello"))
		_ = srcPeer.Close()
	}()

	done := make(chan error, 1)
	go func() {
		done <- (&natsProxy{}).relayNATS(srcProxy, dstProxy, "PUB ", "publish",
			func(int) {}, func() {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a truncated payload must surface an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay hung on a truncated payload")
	}
}

// Sanity check on the premise: bufio.Scanner really does drop the \r, which is
// what made the old implementation wrong. If this ever changes, the comments
// explaining the bug should change with it.
func TestScannerStripsCRDocumentingTheOldBug(t *testing.T) {
	r, w := net.Pipe()
	go func() { _, _ = w.Write([]byte("PONG\r\n")); _ = w.Close() }()
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		t.Fatal("no line scanned")
	}
	if got := sc.Text(); got != "PONG" {
		t.Fatalf("scanner returned %q", got)
	}
	if len(sc.Bytes()) != 4 {
		t.Errorf("scanner kept the CR; the premise of the fix no longer holds")
	}
}
