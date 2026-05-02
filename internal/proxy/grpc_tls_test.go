package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// echoUpstreamTLS is the TLS variant of echoUpstream. The upstream
// terminates TLS itself via grpc.Creds, mirroring how a real prod
// gRPC service exposes mTLS — and the path the proxy needs to talk
// to via credentials.NewTLS.
func echoUpstreamTLS(t *testing.T) (addr string, srvCfg *tls.Config, stop func()) {
	t.Helper()
	cfg, err := GenerateSelfSignedCert(nil)
	if err != nil {
		t.Fatalf("upstream cert: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	s := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.ForceServerCodec(rawBytesCodec{}),
		grpc.UnknownServiceHandler(func(srv any, stream grpc.ServerStream) error {
			var in []byte
			if err := stream.RecvMsg(&in); err != nil {
				return err
			}
			return stream.SendMsg(&in)
		}),
	)
	go s.Serve(lis)
	return lis.Addr().String(), cfg, func() { s.GracefulStop(); lis.Close() }
}

// grpcClientCfgFor builds a TLS client config that trusts the given
// server cfg's leaf cert. ServerName=localhost matches the auto-cert
// SAN and the loopback dial address.
func grpcClientCfgFor(t *testing.T, serverCfg *tls.Config) *tls.Config {
	t.Helper()
	leaf, err := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		RootCAs:    pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
}

// TestGRPCProxy_TLSEndToEnd — the headline RFC-038 case for gRPC:
// caller speaks gRPC-over-TLS to the proxy, proxy speaks gRPC-over-TLS
// to the upstream, plaintext rule-matching keeps working in the
// middle. This is what unblocks inDrive's mTLS upstreams.
func TestGRPCProxy_TLSEndToEnd(t *testing.T) {
	upstreamAddr, upstreamCfg, stopUpstream := echoUpstreamTLS(t)
	defer stopUpstream()

	clientCfg := grpcClientCfgFor(t, upstreamCfg)
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newGRPCProxy(nil, "geoconfig")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	// Client trusts the proxy's auto-cert.
	conn, err := grpc.NewClient(proxyAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(grpcClientCfgFor(t, serverCfg))),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawBytesCodec{})),
	)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()

	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, 0xFEEDFACECAFEBEEF)
	var reply []byte
	md := metadata.Pairs("x-fb-test", "1")
	ctx2 := metadata.NewOutgoingContext(ctx, md)
	if err := conn.Invoke(ctx2, "/freight.Geo/Lookup", &payload, &reply); err != nil {
		t.Fatalf("invoke through TLS proxy: %v", err)
	}
	if len(reply) != len(payload) {
		t.Fatalf("reply length = %d, want %d", len(reply), len(payload))
	}
	for i := range payload {
		if reply[i] != payload[i] {
			t.Fatalf("byte %d corrupted through TLS round-trip", i)
		}
	}
}

// TestGRPCProxy_TLSRuleInjection — fault rule fires inside the TLS
// tunnel. This is the customer's exact ask: grpc.error(method=...)
// against an mTLS upstream.
func TestGRPCProxy_TLSRuleInjection(t *testing.T) {
	upstreamAddr, upstreamCfg, stopUpstream := echoUpstreamTLS(t)
	defer stopUpstream()

	clientCfg := grpcClientCfgFor(t, upstreamCfg)
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newGRPCProxy(nil, "geoconfig")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxyAddr, _ := p.Start(ctx, upstreamAddr)
	defer p.Stop()

	p.AddRule(Rule{
		Method: "/freight.Geo/Blocked",
		Action: ActionError,
		Status: int(codes.Unavailable),
		Error:  "geoconfig is down",
	})

	conn, err := grpc.NewClient(proxyAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(grpcClientCfgFor(t, serverCfg))),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawBytesCodec{})),
	)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()

	payload := []byte{}
	var reply []byte
	err = conn.Invoke(ctx, "/freight.Geo/Blocked", &payload, &reply)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", st.Code())
	}
	if !strings.Contains(st.Message(), "geoconfig is down") {
		t.Errorf("message = %q, want substring 'geoconfig is down'", st.Message())
	}
}

// TestGRPCProxy_PlaintextStillWorks — regression check. Without
// SetTLS, the gRPC plugin retains its pre-RFC-038 behavior:
// insecure.NewCredentials on both sides, h2c traffic.
func TestGRPCProxy_PlaintextStillWorks(t *testing.T) {
	upstreamAddr, stopUpstream := echoUpstream(t)
	defer stopUpstream()

	p := newGRPCProxy(nil, "geoconfig")
	// No SetTLS call — plain h2c path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("proxy start: %v", err)
	}
	defer p.Stop()

	conn, err := grpc.NewClient(proxyAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawBytesCodec{})),
	)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	var reply []byte
	if err := conn.Invoke(ctx, "/freight.Geo/Plain", &payload, &reply); err != nil {
		t.Fatalf("plain invoke: %v", err)
	}
	if string(reply) != string(payload) {
		t.Errorf("reply = %x, want %x", reply, payload)
	}
}

// TestGRPCProxy_ImplementsTLSAware pins the TLSAware contract.
func TestGRPCProxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*grpcProxy)(nil)
}
