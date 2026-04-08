package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memcachedProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newMemcachedProxy(onEvent OnProxyEvent, svcName string) *memcachedProxy {
	return &memcachedProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *memcachedProxy) Protocol() string { return "memcached" }

func (p *memcachedProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target

	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

	return ln.Addr().String(), nil
}

func (p *memcachedProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	serverConn, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	clientReader := bufio.NewReader(clientConn)
	serverReader := bufio.NewReader(serverConn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		clientConn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Read command line from client.
		line, err := clientReader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}

		command := strings.ToUpper(parts[0])
		key := ""
		if len(parts) > 1 {
			key = parts[1]
		}

		// For storage commands (set, add, replace), read the data block too.
		var dataBlock []byte
		if isStorageCommand(command) && len(parts) >= 5 {
			bytes, _ := strconv.Atoi(parts[4])
			dataBlock = make([]byte, bytes+2) // +2 for \r\n
			if _, err := io.ReadFull(clientReader, dataBlock); err != nil {
				return
			}
		}

		// Check rules.
		if handled := p.checkRules(clientConn, command, key); handled {
			continue
		}

		// Forward to server.
		fmt.Fprintf(serverConn, "%s\r\n", line)
		if dataBlock != nil {
			serverConn.Write(dataBlock)
		}

		// Forward response back.
		resp, err := serverReader.ReadString('\n')
		if err != nil {
			return
		}
		fmt.Fprint(clientConn, resp)

		// For get commands, forward VALUE lines + END.
		if command == "GET" || command == "GETS" {
			for !strings.HasPrefix(resp, "END") {
				resp, err = serverReader.ReadString('\n')
				if err != nil {
					return
				}
				fmt.Fprint(clientConn, resp)
				// If VALUE line, also forward the data block.
				if strings.HasPrefix(resp, "VALUE") {
					valParts := strings.Fields(resp)
					if len(valParts) >= 4 {
						bytes, _ := strconv.Atoi(valParts[3])
						data := make([]byte, bytes+2)
						io.ReadFull(serverReader, data)
						clientConn.Write(data)
					}
				}
			}
		}
	}
}

func (p *memcachedProxy) checkRules(clientConn net.Conn, command, key string) bool {
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if rule.Command != "" && !matchGlob(command, rule.Command) {
			continue
		}
		if rule.Key != "" && !matchGlob(key, rule.Key) {
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
				errMsg = "SERVER_ERROR injected fault"
			}
			fmt.Fprintf(clientConn, "%s\r\n", errMsg)
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "memcached",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"command": command, "key": key, "error": errMsg},
				})
			}
			return true

		case ActionRespond:
			// Return NOT_FOUND or custom response.
			resp := rule.Body
			if resp == "" {
				resp = "NOT_FOUND"
			}
			fmt.Fprintf(clientConn, "%s\r\n", resp)
			return true

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "memcached",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"command": command, "key": key, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			return false

		case ActionDrop:
			clientConn.Close()
			return true
		}
	}
	return false
}

func isStorageCommand(cmd string) bool {
	switch cmd {
	case "SET", "ADD", "REPLACE", "APPEND", "PREPEND", "CAS":
		return true
	}
	return false
}

func (p *memcachedProxy) AddRule(rule Rule)  { p.mu.Lock(); p.rules = append(p.rules, rule); p.mu.Unlock() }
func (p *memcachedProxy) ClearRules()        { p.mu.Lock(); p.rules = nil; p.mu.Unlock() }
func (p *memcachedProxy) Stop() error {
	if p.cancel != nil { p.cancel() }
	if p.listener != nil { p.listener.Close() }
	p.wg.Wait()
	return nil
}
