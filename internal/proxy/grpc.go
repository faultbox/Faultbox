package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
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
	mu          sync.RWMutex
	rules       []Rule
	target      string
	server      *grpc.Server
	listener    net.Listener
	onEvent     OnProxyEvent
	svcName     string
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	upstream    *grpc.ClientConn
	ctx         context.Context
	rpcSequence atomic.Uint64
	stopOnce    sync.Once

	// RFC-038 Phase 3: TLS material. serverTLS wraps the listener
	// (ALPN h2 forced); clientTLS becomes upstream credentials via
	// credentials.NewTLS, replacing the insecure.NewCredentials path.
	serverTLS *tls.Config
	clientTLS *tls.Config
}

func newGRPCProxy(onEvent OnProxyEvent, svcName string) *grpcProxy {
	return &grpcProxy{
		onEvent: onEvent,
		svcName: svcName,
	}
}

func (p *grpcProxy) Protocol() string { return "grpc" }

// SetTLS implements TLSAware. Must be called before Start.
func (p *grpcProxy) SetTLS(server, client *tls.Config) {
	p.serverTLS = server
	p.clientTLS = client
}

func (p *grpcProxy) Start(ctx context.Context, target string) (string, error) {
	p.target = target
	ctx, p.cancel = context.WithCancel(ctx)
	p.ctx = ctx

	// gRPC owns TLS termination on the server side via grpc.Creds —
	// pre-wrapping the listener via ListenTLS would double-up the
	// handshake. Use plain Listen() and pass serverTLS through
	// credentials.NewTLS instead. Mirrors the http2 plugin's spirit
	// (TLS+ALPN h2) but routed via the gRPC framework's own seam.
	ln, listenAddr, err := Listen()
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	p.listener = ln

	// Upstream credentials: clientTLS non-nil ⇒ TLS with ALPN h2.
	// Otherwise insecure (cleartext h2c) — same behavior as before
	// RFC-038. ForceCodec on every outbound call so forwardRPC can
	// hand raw bytes straight through without the default proto
	// codec rejecting them (Bug #1).
	var transportCreds credentials.TransportCredentials
	if p.clientTLS != nil {
		clientCfg := p.clientTLS.Clone()
		if len(clientCfg.NextProtos) == 0 {
			clientCfg.NextProtos = []string{"h2"}
		}
		transportCreds = credentials.NewTLS(clientCfg)
	} else {
		transportCreds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(transportCreds),
		// Payloads are protobuf wire bytes even though our local codec is opaque.
		// Declare proto so typed upstreams never select our private raw codec.
		grpc.WithDefaultCallOptions(grpc.ForceCodec(grpcRawCodec{}), grpc.CallContentSubtype("proto")),
	)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("connect to upstream: %w", err)
	}
	p.upstream = conn

	// Server-side: when serverTLS is set, configure grpc.Creds to
	// terminate TLS at handshake time. ForceServerCodec so incoming
	// frames reach handleStream as raw bytes — mirrors the upstream
	// client codec above so passthrough is a byte-identity transform.
	serverOpts := []grpc.ServerOption{
		grpc.WaitForHandlers(true),
		grpc.UnknownServiceHandler(p.handleStream),
		grpc.ForceServerCodec(grpcRawCodec{}),
	}
	if p.serverTLS != nil {
		serverCfg := p.serverTLS.Clone()
		// gRPC handles ALPN internally but won't override an existing
		// NextProtos set by the customer's cfg; we don't preset it
		// because grpc-go forces "h2" anyway.
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(serverCfg)))
	}
	p.server = grpc.NewServer(serverOpts...)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.server.Serve(ln)
	}()
	go func() {
		<-ctx.Done()
		p.Stop()
	}()

	return listenAddr, nil
}

// handleStream is the catch-all handler for all gRPC methods.
func (p *grpcProxy) handleStream(srv interface{}, stream grpc.ServerStream) error {
	rpcID := fmt.Sprint(p.rpcSequence.Add(1))
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

		if rule.Delay > 0 || rule.Action == ActionDelay {
			if err := p.waitDelay(stream.Context(), method, rpcID, rule.Delay); err != nil {
				return err
			}
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
					Fields:   map[string]string{"method": method, "rpc_id": rpcID, "code": code.String(), "error": errMsg},
				})
			}
			return status.Error(code, errMsg)

		case ActionDelay:
			// The hit was recorded before waiting; fall through to forward.

		case ActionDrop:
			if p.onEvent != nil {
				p.onEvent(ProxyEvent{
					Protocol: "grpc",
					Action:   "drop",
					To:       p.svcName,
					Fields:   map[string]string{"method": method, "rpc_id": rpcID},
				})
			}
			return status.Errorf(codes.Unavailable, "connection dropped")
		}
	}

	// Forward to upstream.
	return p.forwardRPC(stream, method)
}

func (p *grpcProxy) waitDelay(ctx context.Context, method, rpcID string, delay time.Duration) error {
	start := time.Now()
	emit := func(typ, phase string, err error) {
		if p.onEvent == nil {
			return
		}
		fields := map[string]string{"method": method, "rpc_id": rpcID, "phase": phase, "delay_ms": fmt.Sprint(delay.Milliseconds()), "elapsed_ms": fmt.Sprint(time.Since(start).Milliseconds())}
		if err != nil {
			fields["error"] = err.Error()
			fields["code"] = status.Code(err).String()
		}
		p.onEvent(ProxyEvent{Type: typ, Protocol: "grpc", Action: "delay", To: p.svcName, Fields: fields})
	}
	// Count the injection when it starts, not after the client's deadline.
	emit("", "started", nil)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var err error
	select {
	case <-ctx.Done():
		err = status.FromContextError(ctx.Err()).Err()
	case <-p.ctx.Done():
		err = status.Error(codes.Unavailable, "faultbox proxy stopped")
	case <-timer.C:
		// Cancellation and timer expiry can become ready together.
		if ctx.Err() != nil {
			err = status.FromContextError(ctx.Err()).Err()
		} else if p.ctx.Err() != nil {
			err = status.Error(codes.Unavailable, "faultbox proxy stopped")
		}
	}
	if err != nil {
		emit("proxy_delay_cancelled", "cancelled", err)
		return err
	}
	emit("proxy_delay_completed", "completed", nil)
	return nil
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
	p.stopOnce.Do(func() {
		if p.onEvent != nil {
			p.onEvent(ProxyEvent{Type: "proxy_stopping", Protocol: "grpc", To: p.svcName})
		}
		if p.cancel != nil {
			p.cancel()
		}
		// WaitForHandlers keeps terminal events inside teardown. Run Stop in a
		// tracked worker so an unrelated stuck handler cannot bypass the
		// common ProxyStopTimeout bound.
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if p.upstream != nil {
				p.upstream.Close()
			}
			if p.server != nil {
				p.server.Stop()
			}
		}()
	})
	waitConns(&p.wg, p.onEvent, p.svcName, "grpc")
	return nil
}
