package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type redisProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// RFC-038 Phase 3: TLS material. Redis 6+ supports TLS via a
	// separate `tls-port` config entry — no in-band SSL upgrade,
	// just "TLS from byte 1" on the configured port. So the
	// wrap-and-dial pattern from http.go applies cleanly.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newRedisProxy(onEvent OnProxyEvent, svcName string) *redisProxy {
	return &redisProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *redisProxy) Protocol() string { return "redis" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *redisProxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *redisProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	var ln net.Listener
	var listenAddr string
	var err error
	if p.serverTLS != nil {
		ln, listenAddr, err = ListenTLS(p.serverTLS)
	} else {
		ln, listenAddr, err = Listen()
	}
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.listener = ln

	ctx, p.cancel = context.WithCancel(ctx)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.handleConn(ctx, conn)
			}()
		}
	}()

	return listenAddr, nil
}

func (p *redisProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	// proxy.Dial routes through tls.Client + HandshakeContext when
	// clientTLS is set; otherwise it's net.DialTimeout. The 5s
	// budget covers both the TCP connect and the TLS handshake.
	serverConn, err := Dial(ctx, p.target, p.clientTLS, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	// RFC-034: per-connection lifecycle tracker. Redis has a thin
	// "handshake" — first command is typically HELLO (RESP3) or AUTH;
	// after the response returns we mark handshake_complete so stall
	// events distinguish handshake-phase from command-phase. Byte
	// counters update inline at each RESP read/write.
	tracker := newConnTracker(p.onEvent, p.svcName, "main", "redis",
		clientConn.RemoteAddr().String(), p.target)
	tracker.EmitOpen()
	closeReason := "client_eof"
	defer func() { tracker.EmitClose(closeReason) }()

	clientReader := bufio.NewReader(clientConn)
	commandsServed := 0

	for {
		select {
		case <-ctx.Done():
			closeReason = "context_cancel"
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))

		// Read RESP command from client.
		args, err := readRESPArray(clientReader)
		if err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}
		if len(args) == 0 {
			continue
		}

		command := strings.ToUpper(args[0])
		key := ""
		if len(args) > 1 {
			key = args[1]
		}

		// Check rules.
		p.mu.RLock()
		rules := make([]Rule, len(p.rules))
		copy(rules, p.rules)
		p.mu.RUnlock()

		handled := false
		for _, rule := range rules {
			if !rule.MatchRequest("", "", "", key, "", command) {
				continue
			}
			if rule.Prob > 0 && rand.Float64() > rule.Prob {
				continue
			}

			if rule.Delay > 0 {
				time.Sleep(rule.Delay)
			}

			switch rule.Action {
			case ActionError:
				errMsg := rule.Error
				if errMsg == "" {
					errMsg = "ERR injected fault"
				}
				fmt.Fprintf(clientConn, "-%s\r\n", errMsg)
				handled = true

				if p.onEvent != nil {
					p.onEvent(ProxyEvent{
						Protocol: "redis",
						Action:   "error",
						To:       p.svcName,
						Fields:   map[string]string{"command": command, "key": key, "error": errMsg},
					})
				}

			case ActionRespond:
				// Return custom value.
				if rule.Body == "" {
					// nil response (cache miss).
					fmt.Fprint(clientConn, "$-1\r\n")
				} else {
					fmt.Fprintf(clientConn, "$%d\r\n%s\r\n", len(rule.Body), rule.Body)
				}
				handled = true

				if p.onEvent != nil {
					p.onEvent(ProxyEvent{
						Protocol: "redis",
						Action:   "respond",
						To:       p.svcName,
						Fields:   map[string]string{"command": command, "key": key},
					})
				}

			case ActionDelay:
				// Delay already applied — fall through to forward.
				if p.onEvent != nil {
					p.onEvent(ProxyEvent{
						Protocol: "redis",
						Action:   "delay",
						To:       p.svcName,
						Fields:   map[string]string{"command": command, "key": key, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
					})
				}

			case ActionDrop:
				clientConn.Close()
				return
			}

			if handled {
				break
			}
		}

		if handled {
			continue
		}

		// Forward to real Redis.
		raw := encodeRESPArray(args)
		tracker.AddBytesC2S(len(raw))
		if _, err := serverConn.Write([]byte(raw)); err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}

		// Read response from Redis and forward to client.
		serverReader := bufio.NewReader(serverConn)
		resp, err := readRESPRaw(serverReader)
		if err != nil {
			closeReason = classifyCloseReason(err, "server")
			return
		}
		tracker.AddBytesS2C(len(resp))
		if _, err := clientConn.Write(resp); err != nil {
			closeReason = classifyCloseReason(err, "client")
			return
		}

		// First successful command round-trip marks handshake as
		// complete. Redis doesn't have a strict handshake boundary
		// (HELLO is optional, AUTH is conditional) so we treat the
		// first round-trip as the proxy's handshake-complete moment.
		commandsServed++
		if commandsServed == 1 {
			authMethod := ""
			if command == "HELLO" {
				authMethod = "hello"
			} else if command == "AUTH" {
				authMethod = "auth"
			}
			tracker.EmitHandshakeComplete(authMethod, 1)
		}
	}
}

func (p *redisProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *redisProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *redisProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		p.listener.Close()
	}
	p.wg.Wait()
	return nil
}

// RESP parsing helpers.

func readRESPArray(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("expected RESP array, got %q", line)
	}
	n, _ := strconv.Atoi(line[1:])
	if n <= 0 {
		return nil, nil
	}
	args := make([]string, n)
	for i := 0; i < n; i++ {
		bulkLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		bulkLine = strings.TrimRight(bulkLine, "\r\n")
		if len(bulkLine) == 0 || bulkLine[0] != '$' {
			return nil, fmt.Errorf("expected bulk string, got %q", bulkLine)
		}
		size, _ := strconv.Atoi(bulkLine[1:])
		if size < 0 {
			args[i] = ""
			continue
		}
		buf := make([]byte, size+2) // +2 for \r\n
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

func encodeRESPArray(args []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return sb.String()
}

func readRESPRaw(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return line, nil
	}

	switch line[0] {
	case '+', '-', ':',
		'_', // RESP3 null
		'#', // RESP3 boolean
		',', // RESP3 double
		'(': // RESP3 big number
		return line, nil
	case '$', '=', '!':
		// Bulk string ($), verbatim string (=), blob error (!) — <type><len>\r\n<bytes>\r\n.
		sizeStr := strings.TrimRight(string(line[1:]), "\r\n")
		size, _ := strconv.Atoi(sizeStr)
		if size < 0 {
			return line, nil
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		return append(line, data...), nil
	case '*', '~', '>':
		// Array (*), RESP3 set (~), RESP3 push (>) — N elements follow.
		result := make([]byte, 0, len(line)+64)
		result = append(result, line...)
		sizeStr := strings.TrimRight(string(line[1:]), "\r\n")
		n, _ := strconv.Atoi(sizeStr)
		for i := 0; i < n; i++ {
			elem, err := readRESPRaw(r)
			if err != nil {
				return nil, err
			}
			result = append(result, elem...)
		}
		return result, nil
	case '%', '|':
		// RESP3 map (%) / attribute (|) — N pairs (2N elements) follow.
		// Attribute additionally precedes a regular reply that the caller treats as one logical value.
		result := make([]byte, 0, len(line)+64)
		result = append(result, line...)
		sizeStr := strings.TrimRight(string(line[1:]), "\r\n")
		n, _ := strconv.Atoi(sizeStr)
		for i := 0; i < 2*n; i++ {
			elem, err := readRESPRaw(r)
			if err != nil {
				return nil, err
			}
			result = append(result, elem...)
		}
		if line[0] == '|' {
			elem, err := readRESPRaw(r)
			if err != nil {
				return nil, err
			}
			result = append(result, elem...)
		}
		return result, nil
	default:
		return line, nil
	}
}
