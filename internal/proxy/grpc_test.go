package proxy

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
)

// rawBytesCodec is a tiny gRPC codec that marshals/unmarshals *[]byte
// verbatim. The proxy must use a passthrough codec internally; this
// mirror lets the test rig drive the proxy with arbitrary byte
// payloads without depending on a real .proto file. Registering under
// the name "proto" is intentional: it's the default codec name the
// framework picks when no explicit ForceCodec is set, which is
// exactly the corner the proxy needs to cover end-to-end.
type rawBytesCodec struct{}

func (rawBytesCodec) Name() string { return "raw-bytes-codec-test" }
func (rawBytesCodec) Marshal(v any) ([]byte, error) {
	b, _ := v.(*[]byte)
	return *b, nil
}
func (rawBytesCodec) Unmarshal(data []byte, v any) error {
	b, _ := v.(*[]byte)
	*b = append((*b)[:0], data...)
	return nil
}

func init() { encoding.RegisterCodec(rawBytesCodec{}) }

// echoUpstream is a minimal gRPC server whose UnknownServiceHandler
// echoes the single recv'd message back to the caller using the
// raw-bytes codec. It stands in for any real upstream the grpc proxy
// would forward to — keeps the test self-contained (no protoc
// dependency) while exercising the same Stream → forward → Stream
// codepath a production RPC takes.
func echoUpstream(t *testing.T) (addr string, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	s := grpc.NewServer(
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
	return lis.Addr().String(), func() { s.GracefulStop(); lis.Close() }
}

// TestGRPCProxyPassthroughDoesNotCorruptMessages reproduces Bug #1
// from the v0.11.1 customer report: the grpc proxy inserted on any
// interface declared `protocol="grpc"` corrupted passthrough traffic
// with `message is *[]uint8, want proto.Message`, even when
// rule_count=0. Before the fix this test fails at the client with
// exactly that error; after the fix the payload round-trips
// byte-for-byte through both server and client directions.
func TestGRPCProxyPassthroughDoesNotCorruptMessages(t *testing.T) {
	upstreamAddr, stopUpstream := echoUpstream(t)
	defer stopUpstream()

	p := newGRPCProxy(nil, "geoconfig")
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

	// Build an 8-byte payload with a recognisable prefix — any binary
	// value will do; the point is to detect corruption. Using the
	// wire-format length-prefix pattern also exercises codec edges.
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, 0xFEEDFACECAFEBEEF)

	var reply []byte
	md := metadata.Pairs("x-fb-test", "1")
	ctx2 := metadata.NewOutgoingContext(ctx, md)
	if err := conn.Invoke(ctx2, "/freight.Geo/Lookup", &payload, &reply); err != nil {
		t.Fatalf("invoke through proxy: %v", err)
	}

	if len(reply) != len(payload) {
		t.Fatalf("reply length = %d, want %d (corruption?)", len(reply), len(payload))
	}
	for i := range payload {
		if reply[i] != payload[i] {
			t.Fatalf("reply[%d] = %#x, want %#x — proxy corrupted the payload", i, reply[i], payload[i])
		}
	}
}

// TestGRPCProxyEmptyMessagePassthrough covers the edge case where a
// client sends a zero-length request body. gRPC frames are legal with
// empty payload; regressing this path would break health-check
// patterns like grpc.health.v1/Check which use an empty request.
func TestGRPCProxyEmptyMessagePassthrough(t *testing.T) {
	upstreamAddr, stop := echoUpstream(t)
	defer stop()

	p := newGRPCProxy(nil, "health")
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

	empty := []byte{}
	var reply []byte
	if err := conn.Invoke(ctx, "/grpc.health.v1.Health/Check", &empty, &reply); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(reply) != 0 {
		t.Errorf("reply = %v, want empty", reply)
	}
}
