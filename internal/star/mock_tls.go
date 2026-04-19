package star

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
	"os"
	"path/filepath"
	"sync"
	"time"
)

// mockTLS holds the per-runtime TLS state — one CA signs all mock server
// certs so a single Faultbox CA bundle can be trusted by every client.
type mockTLS struct {
	mu       sync.Mutex
	ca       *x509.Certificate
	caDER    []byte
	caKey    *ecdsa.PrivateKey
	caPath   string // PEM-encoded CA cert on disk
}

// newMockTLS generates a fresh CA + writes the PEM bundle to a known path
// inside the OS temp dir. The CA is ECDSA P-256 with a 24-hour validity —
// the runtime lifecycle is typically seconds to minutes, but a generous
// window avoids clock-skew surprises.
func newMockTLS() (*mockTLS, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("ca serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Faultbox Mock CA"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse ca: %w", err)
	}

	// Write CA PEM to a well-known path so SUTs can mount it (docker) or
	// read it (binary mode) without guessing.
	path := filepath.Join(os.TempDir(), fmt.Sprintf("faultbox-ca-%d.pem", time.Now().UnixNano()))
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write ca: %w", err)
	}

	return &mockTLS{
		ca:     parsed,
		caDER:  der,
		caKey:  key,
		caPath: path,
	}, nil
}

// serverCert signs a leaf cert for the given hostnames/IPs and returns a
// tls.Certificate suitable for handing to a net/http or grpc server.
// Every TLS-enabled mock gets its own leaf cert signed by the shared CA,
// so clients trusting the CA accept all mocks without extra config.
func (m *mockTLS) serverCert(hostnames []string, ips []net.IP) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("leaf serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Faultbox Mock Server"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hostnames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &leafKey.PublicKey, m.caKey)
	if err != nil {
		return nil, fmt.Errorf("leaf cert: %w", err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, m.caDER},
		PrivateKey:  leafKey,
		Leaf:        nil,
	}, nil
}

// CAPath returns the on-disk PEM bundle path of the runtime's mock CA.
// SUTs can trust Faultbox-issued mock certs by loading this file.
func (m *mockTLS) CAPath() string { return m.caPath }

// getMockTLS returns the runtime's shared mock CA, creating it on first
// use. Thread-safe via sync.Once; subsequent callers see the same CA.
func (rt *Runtime) getMockTLS() (*mockTLS, error) {
	rt.mockTLSOnce.Do(func() {
		rt.mockTLSImpl, rt.mockTLSErr = newMockTLS()
	})
	return rt.mockTLSImpl, rt.mockTLSErr
}

// MockCAPath returns the path to the runtime's mock CA bundle, or ""
// if no TLS mocks have been started. Tests and CLI can surface this to
// the user so SUTs know which file to trust.
func (rt *Runtime) MockCAPath() string {
	if rt.mockTLSImpl == nil {
		return ""
	}
	return rt.mockTLSImpl.CAPath()
}
