package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// kafkaEcho is a trivial Kafka-shaped upstream: it reads
// length-prefixed frames and writes a length-prefixed reply that
// echoes the payload. Stands in for the broker without pulling in
// kafka-go or sarama. Returns (addr, stop).
func kafkaEcho(t *testing.T, useTLS bool) (string, *tls.Config, func()) {
	t.Helper()
	var ln net.Listener
	var srvCfg *tls.Config
	if useTLS {
		cfg, err := GenerateSelfSignedCert(nil)
		if err != nil {
			t.Fatalf("upstream cert: %v", err)
		}
		srvCfg = cfg
		ln, err = tls.Listen("tcp", "127.0.0.1:0", cfg)
		if err != nil {
			t.Fatalf("tls.Listen: %v", err)
		}
	} else {
		var err error
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	}

	stopped := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for {
					hdr := make([]byte, 4)
					if _, err := io.ReadFull(c, hdr); err != nil {
						return
					}
					n := int(binary.BigEndian.Uint32(hdr))
					if n <= 0 || n > 64*1024 {
						return
					}
					body := make([]byte, n)
					if _, err := io.ReadFull(c, body); err != nil {
						return
					}
					// Echo back with the same length prefix.
					c.Write(hdr)
					c.Write(body)
				}
			}()
		}
	}()
	return ln.Addr().String(), srvCfg, func() { ln.Close(); close(stopped) }
}

// sendKafkaFrame writes a Kafka request to a connection: 4-byte
// big-endian length prefix + payload. The payload starts with
// api_key(2) + api_version(2) + correlation_id(4) + client_id(2)
// to keep the proxy's parser happy.
func sendKafkaFrame(t *testing.T, c net.Conn, apiKey int16, topic string) {
	t.Helper()
	// Build payload: api_key + api_version + correlation_id +
	// client_id_len(2)=0 + topic_count placeholder + topic_len + topic.
	body := make([]byte, 0, 32+len(topic))
	tmp := make([]byte, 8)
	binary.BigEndian.PutUint16(tmp[0:], uint16(apiKey))
	binary.BigEndian.PutUint16(tmp[2:], 0) // api_version
	binary.BigEndian.PutUint32(tmp[4:], 1) // correlation_id
	body = append(body, tmp...)
	// client_id: int16 length=0
	body = append(body, 0, 0)
	// Topic name (int16 length + bytes) — proxy's extractTopic
	// scans forward for any reasonable string, so this lands.
	tlen := make([]byte, 2)
	binary.BigEndian.PutUint16(tlen, uint16(len(topic)))
	body = append(body, tlen...)
	body = append(body, []byte(topic)...)

	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(body)))
	c.Write(hdr)
	c.Write(body)
}

func readKafkaFrame(t *testing.T, c net.Conn) []byte {
	t.Helper()
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := int(binary.BigEndian.Uint32(hdr))
	body := make([]byte, n)
	if _, err := io.ReadFull(c, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// TestKafkaProxy_Passthrough — basic byte-identity round trip
// through the proxy with no rules. Satisfies the #84 coverage gate
// and serves as the plaintext regression baseline for TLS migration.
func TestKafkaProxy_Passthrough(t *testing.T) {
	upstreamAddr, _, stop := kafkaEcho(t, false)
	defer stop()

	p := newKafkaProxy(nil, "broker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIProduce, "events")
	body := readKafkaFrame(t, c)
	if !strings.Contains(string(body), "events") {
		t.Errorf("echo body missing topic: %q", body)
	}
}

// TestKafkaProxy_TLSEndToEnd — the RFC-038 case: client speaks
// Kafka-over-TLS to the proxy, proxy speaks Kafka-over-TLS to the
// upstream, plaintext frame parsing keeps working in the middle so
// topic-glob rules still fire.
func TestKafkaProxy_TLSEndToEnd(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := kafkaEcho(t, true)
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	p := newKafkaProxy(nil, "broker")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	// Client trusts the proxy's auto-cert.
	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	c, err := tls.Dial("tcp", proxyAddr, &tls.Config{
		RootCAs:    clientPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Dial proxy: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIProduce, "telemetry")
	body := readKafkaFrame(t, c)
	if !strings.Contains(string(body), "telemetry") {
		t.Errorf("echo through TLS missing topic: %q", body)
	}
}

// TestKafkaProxy_TLSRuleInjection — fault rule fires inside the TLS
// tunnel. The customer's exact use case for kafka: TLS broker plus
// topic-glob fault injection.
func TestKafkaProxy_TLSRuleInjection(t *testing.T) {
	upstreamAddr, upstreamCfg, stop := kafkaEcho(t, true)
	defer stop()

	upstreamLeaf, _ := x509.ParseCertificate(upstreamCfg.Certificates[0].Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(upstreamLeaf)
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	serverCfg, _ := GenerateSelfSignedCert(nil)

	var ruleHits int32
	p := newKafkaProxy(func(evt ProxyEvent) {
		if evt.Action == "drop" {
			atomic.AddInt32(&ruleHits, 1)
		}
	}, "broker")
	p.SetTLS(serverCfg, clientCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, _ := p.Start(ctx, upstreamAddr)
	defer p.Stop()

	p.AddRule(Rule{
		Topic:  "events.*",
		Action: ActionDrop,
	})

	leaf, _ := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	clientPool := x509.NewCertPool()
	clientPool.AddCert(leaf)
	c, err := tls.Dial("tcp", proxyAddr, &tls.Config{
		RootCAs:    clientPool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer c.Close()

	// Send a frame matching the topic-glob — proxy should drop the
	// connection per the rule. Reading back must error.
	sendKafkaFrame(t, c, kafkaAPIProduce, "events.orders")

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 4)
	_, err = io.ReadFull(c, hdr)
	if err == nil {
		t.Errorf("expected read error after rule drop, got success")
	}

	// Give the proxy a beat to emit the event.
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&ruleHits) == 0 {
		t.Errorf("expected at least one drop event, got 0")
	}
}

// TestKafkaProxy_PlaintextStillWorks — regression check. Without
// SetTLS the plugin retains pre-RFC-038 behavior verbatim.
func TestKafkaProxy_PlaintextStillWorks(t *testing.T) {
	upstreamAddr, _, stop := kafkaEcho(t, false)
	defer stop()

	p := newKafkaProxy(nil, "broker")
	// No SetTLS call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyAddr, err := p.Start(ctx, upstreamAddr)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	sendKafkaFrame(t, c, kafkaAPIFetch, "logs")
	body := readKafkaFrame(t, c)
	if !strings.Contains(string(body), "logs") {
		t.Errorf("plaintext echo missing topic: %q", body)
	}
}

// TestKafkaProxy_ImplementsTLSAware pins the contract.
func TestKafkaProxy_ImplementsTLSAware(t *testing.T) {
	var _ TLSAware = (*kafkaProxy)(nil)
}
