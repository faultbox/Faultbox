package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type grpcProxy struct {
	mu       sync.RWMutex
	rules    []Rule
	target   string
	server   *grpc.Server
	listener net.Listener
	onEvent  OnProxyEvent
	svcName  string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	upstream *grpc.ClientConn
}

func newGRPCProxy(onEvent OnProxyEvent, svcName string) *grpcProxy {
	return &grpcProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *grpcProxy) Protocol() string { return "grpc" }

func (p *grpcProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target
	ctx, p.cancel = context.WithCancel(ctx)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.listener = ln

	// Connect to upstream.
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("connect to upstream: %w", err)
	}
	p.upstream = conn

	// Create gRPC server with unknown service handler (catches all RPCs).
	p.server = grpc.NewServer(
		grpc.UnknownServiceHandler(p.handleStream),
	)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.server.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		p.server.GracefulStop()
	}()

	return ln.Addr().String(), nil
}

// handleStream is the catch-all handler for all gRPC methods.
func (p *grpcProxy) handleStream(srv interface{}, stream grpc.ServerStream) error {
	// Extract method name from context.
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		method = "unknown"
	}

	// Check rules.
	p.mu.RLock()
	rules := make([]Rule, len(p.rules))
	copy(rules, p.rules)
	p.mu.RUnlock()

	for _, rule := range rules {
		if !rule.MatchRequest(method, "", "", "", "", "") {
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
			code := codes.Internal
			if rule.Status > 0 && rule.Status < 17 {
				code = codes.Code(rule.Status)
			}
			errMsg := rule.Error
			if errMsg == "" {
				errMsg = "injected fault"
			}
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "grpc",
					Action:   "error",
					To:       p.svcName,
					Fields:   map[string]string{"method": method, "code": code.String(), "error": errMsg},
				})
			}
			return status.Error(code, errMsg)

		case ActionDelay:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "grpc",
					Action:   "delay",
					To:       p.svcName,
					Fields:   map[string]string{"method": method, "delay_ms": fmt.Sprintf("%d", rule.Delay.Milliseconds())},
				})
			}
			// Fall through to forward.

		case ActionDrop:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "grpc",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"method": method},
				})
			}
			return status.Errorf(codes.Unavailable, "connection dropped")
		}
	}

	// Forward to upstream.
	return p.forwardRPC(stream, method)
}

// forwardRPC proxies a single RPC to the upstream server.
func (p *grpcProxy) forwardRPC(serverStream grpc.ServerStream, method string) error {
	// Get incoming metadata.
	md, _ := metadata.FromIncomingContext(serverStream.Context())
	ctx := metadata.NewOutgoingContext(serverStream.Context(), md)

	// Create client stream to upstream.
	desc := &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}
	clientStream, err := p.upstream.NewStream(ctx, desc, method)
	if err != nil {
		return err
	}

	// Bidirectional forwarding.
	errCh := make(chan error, 2)

	// Client → upstream.
	go func() {
		for {
			msg := make([]byte, 0)
			if err := serverStream.RecvMsg(&msg); err != nil {
				clientStream.CloseSend()
				errCh <- nil
				return
			}
			if err := clientStream.SendMsg(&msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Upstream → client.
	go func() {
		for {
			msg := make([]byte, 0)
			if err := clientStream.RecvMsg(&msg); err != nil {
				errCh <- err
				return
			}
			if err := serverStream.SendMsg(&msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for either direction to finish.
	return <-errCh
}

func (p *grpcProxy) AddRule(rule Rule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(p.rules, rule)
}

func (p *grpcProxy) ClearRules() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = nil
}

func (p *grpcProxy) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.upstream != nil {
		p.upstream.Close()
	}
	p.wg.Wait()
	return nil
}
