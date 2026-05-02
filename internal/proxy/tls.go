// Package proxy — TLS helper material for transparent fault-injection
// proxies. RFC-038 Phase 1: ship the foundation needed to terminate
// TLS at the proxy (so per-plugin handlers keep seeing plaintext) and
// to dial mTLS upstreams.
//
// This file owns the auto-generated self-signed cert path, used when
// a `tls=` interface is declared without an explicit `proxy_cert`.
// In dev/test that's the common path: customer specs say "this
// upstream speaks TLS" without spelling out cert files, and we
// fabricate the proxy-side material on the fly. Stricter
// environments will plug in their own cert via the spec-language
// surface that lands in Phase 2.

package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// GenerateSelfSignedCert returns a *tls.Config with a freshly minted
// self-signed certificate covering the given hostnames / IPs. The
// cert chain is in-memory only; nothing touches disk. Each call
// produces a new cert (new serial, new key, new fingerprint) — fine
// for testing, hostile to certificate-pinned clients. Phase 1 does
// not persist; that's the (1c) sub-option in the RFC and we'll
// revisit if a customer asks.
//
// hosts may include DNS names, IP literals, or both. "localhost"
// and "127.0.0.1" are always included so dev specs that don't list
// hosts still get a working cert against the loopback dial address
// returned by Listen / ListenTLS. An empty hosts slice falls back
// to those two entries only.
//
// The cert is valid for 24 hours. Test runs almost always live
// well under that bound; CI runs that exceed it should regenerate
// per-test rather than rely on long validity.
func GenerateSelfSignedCert(hosts []string) (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Faultbox Auto-Generated (RFC-038)"},
			CommonName:   "faultbox-proxy",
		},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Always include loopback so the host-side dial address from
	// Listen / ListenTLS works without per-test SAN config.
	template.DNSNames = append(template.DNSNames, "localhost", "faultbox-proxy")
	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if h != "" {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
