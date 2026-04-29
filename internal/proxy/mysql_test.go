package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestMySQLProxy_CheckRules_SQLCanonicalization verifies that the MySQL
// proxy's rule matcher canonicalizes SQL before comparing — so a rule
// keyed on "SELECT * FROM users WHERE id = ?" matches an incoming
// "select * from users where id=$1;" that a driver might actually send.
//
// Without canonicalization this case would miss: filepath.Match is
// case-sensitive on the non-wildcard portion and strings.EqualFold only
// helps on full-string equality, not on placeholder or whitespace drift.
func TestMySQLProxy_CheckRules_SQLCanonicalization(t *testing.T) {
	cases := []struct {
		name        string
		rulePattern string
		query       string
		wantHandled bool
	}{
		{
			name:        "tight driver output matches spaced rule pattern",
			rulePattern: "SELECT * FROM users WHERE id = ?",
			query:       "select * from users where id=$1;",
			wantHandled: true,
		},
		{
			name:        "whitespace + case + trailing semicolon",
			rulePattern: "UPDATE users SET role = ? WHERE id = ?",
			query:       "  UPDATE  users  SET role=$1 WHERE id=$2 ;",
			wantHandled: true,
		},
		{
			name:        "prefix glob on INSERT fires on lowercase driver output",
			rulePattern: "INSERT*",
			query:       "insert into users values (1, 'a')",
			wantHandled: true,
		},
		{
			name:        "non-matching statement is not handled",
			rulePattern: "UPDATE*",
			query:       "SELECT 1",
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newMySQLProxy(nil, "test-svc")
			p.AddRule(Rule{
				Query:  tc.rulePattern,
				Action: ActionError,
				Error:  "injected",
			})

			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			// Continuously drain whatever the proxy writes so checkRules
			// never blocks on a pipe Write.
			go func() {
				client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				_, _ = io.Copy(io.Discard, client)
			}()

			handled := p.checkRules(server, 0, tc.query)
			if handled != tc.wantHandled {
				t.Fatalf("checkRules(%q) with rule %q: got handled=%v, want %v",
					tc.query, tc.rulePattern, handled, tc.wantHandled)
			}
		})
	}
}

// TestMySQLProxy_CheckRules_Probability verifies that Prob on a matching
// rule gates the action — over 2000 trials at Prob=0.3, the observed
// hit rate should fall within a loose ±10% band of the target.
//
// The test uses a matching query every time, so any miss is purely the
// probability gate (not a canonicalization miss). A failure here would
// indicate Prob is dropped somewhere in the plumbing — today it arrives
// from Starlark via the ProxyFaultDef.Probability field.
func TestMySQLProxy_CheckRules_Probability(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	p.AddRule(Rule{
		Query:  "SELECT *",
		Action: ActionError,
		Error:  "maybe",
		Prob:   0.3,
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, client)
	}()

	const trials = 2000
	hits := 0
	for i := 0; i < trials; i++ {
		if p.checkRules(server, 0, "SELECT 1") {
			hits++
		}
	}

	rate := float64(hits) / float64(trials)
	if rate < 0.20 || rate > 0.40 {
		t.Fatalf("Prob=0.3 produced hit rate %.3f over %d trials — expected 0.20..0.40",
			rate, trials)
	}
}

// TestMySQLProxy_CheckRules_EmptyPatternMatchesAll verifies that a rule
// with an empty Query pattern fires on every query — preserves prior
// "no-query-filter = match-all" behavior after the canonicalizer refactor.
func TestMySQLProxy_CheckRules_EmptyPatternMatchesAll(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	p.AddRule(Rule{
		Query:  "", // match all
		Action: ActionError,
		Error:  "all queries fail",
	})

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = io.Copy(io.Discard, client)
	}()

	if !p.checkRules(server, 0, "SELECT anything at all") {
		t.Fatal("expected match-all rule to fire")
	}
}

// makeMySQLPacket returns a full MySQL wire packet given seq id and payload.
// Packet layout: 3-byte little-endian length + 1-byte sequence + payload.
func makeMySQLPacket(seq byte, payload []byte) []byte {
	n := len(payload)
	pkt := make([]byte, 4+n)
	pkt[0] = byte(n)
	pkt[1] = byte(n >> 8)
	pkt[2] = byte(n >> 16)
	pkt[3] = seq
	copy(pkt[4:], payload)
	return pkt
}

// readMySQLPacket reads a full MySQL packet (header + payload) and returns
// the seq + payload. Used by the test fakes to verify the proxy is
// forwarding the right thing in the right direction.
func readMySQLPacket(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Fatalf("read payload: %v", err)
		}
	}
	return header[3], payload
}

// TestMySQLProxy_Handshake_NativePassword verifies the simple 3-packet
// path (mysql_native_password) still works after the v0.12.14 loop
// refactor. Server greeting → client auth → server OK terminates the
// loop on the first iteration via the OK marker.
func TestMySQLProxy_Handshake_NativePassword(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	clientToProxy, proxyFromClient := net.Pipe()
	serverFromProxy, proxyToServer := net.Pipe()
	defer clientToProxy.Close()
	defer serverFromProxy.Close()

	done := make(chan error, 1)
	go func() { done <- p.forwardHandshake(proxyFromClient, proxyToServer) }()

	// Server-side script (the upstream MySQL backend the proxy dialed).
	go func() {
		// 1. Server sends greeting.
		_, _ = serverFromProxy.Write(makeMySQLPacket(0, []byte("greeting")))
		// 2. Server expects client auth response — read it.
		_, _ = readMySQLPacket(t, serverFromProxy)
		// 3. Server sends OK.
		_, _ = serverFromProxy.Write(makeMySQLPacket(2, []byte{mysqlPktOK, 0x00, 0x00, 0x02, 0x00}))
	}()

	// Client-side script (truck-api in the real world).
	// Read greeting, send auth response, read OK.
	_, greeting := readMySQLPacket(t, clientToProxy)
	if string(greeting) != "greeting" {
		t.Errorf("greeting = %q, want %q", greeting, "greeting")
	}
	_, _ = clientToProxy.Write(makeMySQLPacket(1, []byte("auth-response")))
	_, ok := readMySQLPacket(t, clientToProxy)
	if ok[0] != mysqlPktOK {
		t.Errorf("expected OK packet, got first byte 0x%02x", ok[0])
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwardHandshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwardHandshake hung")
	}
}

// TestMySQLProxy_Handshake_CachingSha2FullAuth is the regression test for
// Finding H. Replicates the caching_sha2_password full-auth flow that
// pre-v0.12.14 proxy deadlocked: 7 packets total (greeting, auth_resp,
// auth_more_data, pubkey_request, pubkey, encrypted_password, OK).
//
// Pre-v0.12.14 forwardHandshake exited after the third packet
// (auth_more_data) and entered the command loop expecting a COM_QUERY,
// but the client's next packet was 0x02 (request public key) — the
// loop forwarded it as a "command", forwardResponse mishandled the
// pubkey response as a result set, and the auth state machine drifted
// off until the 60s read deadline fired. Customer Finding H, 2026-04-28.
func TestMySQLProxy_Handshake_CachingSha2FullAuth(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	clientToProxy, proxyFromClient := net.Pipe()
	serverFromProxy, proxyToServer := net.Pipe()
	defer clientToProxy.Close()
	defer serverFromProxy.Close()

	done := make(chan error, 1)
	go func() { done <- p.forwardHandshake(proxyFromClient, proxyToServer) }()

	// Server-side script — emits the caching_sha2_password full-auth
	// dance: greeting, auth_more_data, pubkey, OK.
	go func() {
		_, _ = serverFromProxy.Write(makeMySQLPacket(0, []byte("greeting-sha2")))
		_, authResp := readMySQLPacket(t, serverFromProxy)
		if string(authResp) != "client-auth-hash" {
			t.Errorf("server expected client-auth-hash, got %q", authResp)
		}
		// "perform full authentication" — auth_more_data with status 0x04.
		_, _ = serverFromProxy.Write(makeMySQLPacket(2, []byte{mysqlPktAuthMoreData, 0x04}))
		_, pubReq := readMySQLPacket(t, serverFromProxy)
		if len(pubReq) != 1 || pubReq[0] != 0x02 {
			t.Errorf("server expected pubkey-request (0x02), got %v", pubReq)
		}
		// Public key payload (auth_more_data prefix + fake key bytes).
		_, _ = serverFromProxy.Write(makeMySQLPacket(4, []byte{mysqlPktAuthMoreData, 'P', 'E', 'M'}))
		_, encPw := readMySQLPacket(t, serverFromProxy)
		if string(encPw) != "encrypted-password" {
			t.Errorf("server expected encrypted-password, got %q", encPw)
		}
		_, _ = serverFromProxy.Write(makeMySQLPacket(6, []byte{mysqlPktOK, 0x00, 0x00, 0x02, 0x00}))
	}()

	// Client-side script — drives the full-auth dance from the truck-api side.
	_, greeting := readMySQLPacket(t, clientToProxy)
	if string(greeting) != "greeting-sha2" {
		t.Errorf("greeting = %q", greeting)
	}
	_, _ = clientToProxy.Write(makeMySQLPacket(1, []byte("client-auth-hash")))
	_, authMore := readMySQLPacket(t, clientToProxy)
	if authMore[0] != mysqlPktAuthMoreData {
		t.Errorf("expected auth_more_data, got 0x%02x", authMore[0])
	}
	_, _ = clientToProxy.Write(makeMySQLPacket(3, []byte{0x02}))
	_, pubkey := readMySQLPacket(t, clientToProxy)
	if pubkey[0] != mysqlPktAuthMoreData {
		t.Errorf("expected auth_more_data (pubkey), got 0x%02x", pubkey[0])
	}
	_, _ = clientToProxy.Write(makeMySQLPacket(5, []byte("encrypted-password")))
	_, ok := readMySQLPacket(t, clientToProxy)
	if ok[0] != mysqlPktOK {
		t.Errorf("expected OK, got 0x%02x", ok[0])
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwardHandshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwardHandshake hung — Finding H regression")
	}
}

// TestMySQLProxy_Handshake_ServerErr verifies forwardHandshake terminates
// cleanly when the server rejects auth. Customer-side fault scenarios
// (bad credentials, IP whitelist) must surface as an ERR packet to the
// client, not a hang.
func TestMySQLProxy_Handshake_ServerErr(t *testing.T) {
	p := newMySQLProxy(nil, "test-svc")
	clientToProxy, proxyFromClient := net.Pipe()
	serverFromProxy, proxyToServer := net.Pipe()
	defer clientToProxy.Close()
	defer serverFromProxy.Close()

	done := make(chan error, 1)
	go func() { done <- p.forwardHandshake(proxyFromClient, proxyToServer) }()

	go func() {
		_, _ = serverFromProxy.Write(makeMySQLPacket(0, []byte("greeting")))
		_, _ = readMySQLPacket(t, serverFromProxy)
		// Server rejects with ERR_Packet (0xFF + code + sqlstate + msg).
		_, _ = serverFromProxy.Write(makeMySQLPacket(2, []byte{mysqlPktERR, 0x15, 0x04, '#', '2', '8', '0', '0', '0', 'b', 'a', 'd'}))
	}()

	_, _ = readMySQLPacket(t, clientToProxy)
	_, _ = clientToProxy.Write(makeMySQLPacket(1, []byte("auth-response")))
	_, errPkt := readMySQLPacket(t, clientToProxy)
	if errPkt[0] != mysqlPktERR {
		t.Errorf("expected ERR packet, got 0x%02x", errPkt[0])
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwardHandshake: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwardHandshake hung on ERR")
	}
}
