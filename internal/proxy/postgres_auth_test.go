package proxy

import (
	"encoding/binary"
	"net"
	"testing"
)

// The postgres proxy relayed the startup exchange server→client only. Trust
// auth worked, because the server just sends AuthenticationOk and carries on.
// Every method requiring a client answer — SCRAM-SHA-256 (the postgres:14+
// default), MD5, cleartext — deadlocked: the client sent its 'p' response, the
// proxy never forwarded it, and both sides waited until the client's 60-second
// read deadline produced "connection reset by peer".
//
// So pg.sql.exec() could not talk to any password-protected Postgres, and
// reported a timeout rather than an authentication problem. It survived
// because no spec asserted on the result of a postgres step.
func TestAuthNeedsClientReply(t *testing.T) {
	authBody := func(code uint32) []byte {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, code)
		return b
	}

	cases := []struct {
		name string
		code uint32
		want bool
	}{
		// The client must answer these, or the server waits forever.
		{"CleartextPassword", 3, true},
		{"MD5Password", 5, true},
		{"SASL", 10, true},
		{"SASLContinue", 11, true},

		// Terminal. Treating these as needing a reply would hang just as
		// surely, in the other direction: the proxy would block reading a
		// client message that is never coming.
		{"AuthenticationOk", 0, false},
		{"SASLFinal", 12, false},
		{"KerberosV5", 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authNeedsClientReply(authBody(tc.code)); got != tc.want {
				t.Errorf("auth code %d: got %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// A truncated body must not panic or be mistaken for an auth request.
func TestAuthNeedsClientReply_ShortBody(t *testing.T) {
	for _, body := range [][]byte{nil, {}, {0}, {0, 0}, {0, 0, 0}} {
		if authNeedsClientReply(body) {
			t.Errorf("a %d-byte body cannot be an auth code", len(body))
		}
	}
}

// readMessage/writeMessage must round-trip, since the auth relay now depends
// on re-framing messages rather than copying bytes through.
func TestMessageRoundTrip(t *testing.T) {
	cases := []struct {
		msgType byte
		body    []byte
	}{
		{'R', []byte{0, 0, 0, 10}},
		{'p', []byte("SCRAM-SHA-256\x00client-first-message")},
		{'Z', []byte{'I'}},
		{'Q', []byte("SELECT 1\x00")},
		{'S', nil}, // body-less message
	}
	for _, tc := range cases {
		a, b := newPipePair()
		go func(mt byte, body []byte) {
			_ = writeMessage(a, mt, body)
			_ = a.Close()
		}(tc.msgType, tc.body)

		gotType, gotBody, err := readMessage(b)
		if err != nil {
			t.Fatalf("readMessage(%c): %v", tc.msgType, err)
		}
		if gotType != tc.msgType {
			t.Errorf("type = %c, want %c", gotType, tc.msgType)
		}
		if string(gotBody) != string(tc.body) {
			t.Errorf("body = %q, want %q", gotBody, tc.body)
		}
		_ = b.Close()
	}
}

// A length field below the 4-byte minimum is malformed and must be refused
// rather than used to size an allocation.
func TestReadMessageRejectsShortLength(t *testing.T) {
	a, b := newPipePair()
	go func() {
		header := []byte{'R', 0, 0, 0, 2} // length 2 < 4
		_, _ = a.Write(header)
		_ = a.Close()
	}()
	if _, _, err := readMessage(b); err == nil {
		t.Error("a message claiming length < 4 must be rejected")
	}
	_ = b.Close()
}

// newPipePair is an in-memory connection pair. The auth relay only needs
// framing behaviour, so a socket would add setup without adding coverage.
func newPipePair() (net.Conn, net.Conn) { return net.Pipe() }
