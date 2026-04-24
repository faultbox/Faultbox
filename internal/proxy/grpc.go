package proxy

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpcRawCodec is a gRPC codec that marshals/unmarshals *[]byte
// verbatim. It's what lets the proxy forward arbitrary frames between
// the upstream and the caller without the framework attempting proto
// unmarshal — which is the corruption path Bug #1 landed in (v0.11.1
// emitted `failed to unmarshal, message is *[]uint8, want proto.
// Message` on every passthrough RPC, even when rule_count=0).
//
// Registered under the codec name "raw-bytes" (not "proto") so it
// doesn't shadow the default proto codec for other gRPC users in the
// same process. The proxy's server option + client dial option then
// ForceCodec this one explicitly.
type grpcRawCodec struct{}

func (grpcRawCodec) Name() string { return "raw-bytes" }

func (grpcRawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("grpcRawCodec: want *[]byte, got %T", v)
	}
	return *b, nil
}

func (grpcRawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("grpcRawCodec: want *[]byte, got %T", v)
	}
	// Copy into a fresh slice — the bytes gRPC hands us live in an
	// internal buffer that can be reused after Unmarshal returns.
	// Forwarding without this copy causes intermittent corruption
	// under load.
	*b = append((*b)[:0], data...)
	return nil
}

// init registers the codec once at package load. RegisterCodec is
// process-global but our codec name is namespaced so it won't clash
// with user code that registered their own proto codec.
func init() { encoding.RegisterCodec(grpcRawCodec{}) }

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

	// Connect to upstream. ForceCodec on every outbound call so
	// forwardRPC can hand raw bytes straight through without the
	// default proto codec rejecting them (Bug #1).
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(grpcRawCodec{})),
	)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("connect to upstream: %w", err)
	}
	p.upstream = conn

	// Create gRPC server with unknown service handler (catches all RPCs).
	// ForceServerCodec so incoming frames reach handleStream as raw
	// bytes — mirrors the upstream client codec above so passthrough
	// is a byte-identity transform.
	p.server = grpc.NewServer(
		grpc.UnknownServiceHandler(p.handleStream),
		grpc.ForceServerCodec(grpcRawCodec{}),
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

// forwardRPC proxies a single RPC to the upstream server. Handles
// unary, server-streaming, client-streaming, and bidi-streaming by
// treating every call as bidi: one goroutine copies client→upstream,
// the caller loop copies upstream→client, and the two join on
// completion so neither side short-circuits before the response has
// flushed (the race that made v0.11.1 spuriously report "received no
// response message" on unary RPCs under a fixed codec).
func (p *grpcProxy) forwardRPC(serverStream grpc.ServerStream, method string) error {
	md, _ := metadata.FromIncomingContext(serverStream.Context())
	ctx := metadata.NewOutgoingContext(serverStream.Context(), md)

	desc := &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}
	clientStream, err := p.upstream.NewStream(ctx, desc, method)
	if err != nil {
		return err
	}

	// Forward client → upstream asynchronously; report its terminal
	// error (nil on a clean EOF → CloseSend) back via a channel so
	// the upstream→client copy loop can wait for it to finish before
	// returning. This ordering is what makes unary RPCs decode
	// correctly: the response gets flushed before forwardRPC exits.
	clientErr := make(chan error, 1)
	go func() {
		for {
			var msg []byte
			if err := serverStream.RecvMsg(&msg); err != nil {
				if err == io.EOF {
					clientErr <- clientStream.CloseSend()
				} else {
					clientErr <- err
				}
				return
			}
			if err := clientStream.SendMsg(&msg); err != nil {
				clientErr <- err
				return
			}
		}
	}()

	// Forward upstream → client in this goroutine so the server
	// stream's trailers are written on the same call path that gRPC
	// treats as the authoritative response — required for unary
	// "must produce exactly one reply" cardinality checks to pass.
	for {
		var msg []byte
		if err := clientStream.RecvMsg(&msg); err != nil {
			if err == io.EOF {
				// Upstream closed cleanly — wait for the reverse
				// direction to finish, then succeed.
				if cErr := <-clientErr; cErr != nil {
					return cErr
				}
				return nil
			}
			return err
		}
		if err := serverStream.SendMsg(&msg); err != nil {
			return err
		}
	}
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
