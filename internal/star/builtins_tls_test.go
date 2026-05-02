package star

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// runStarTLS evaluates a small Starlark snippet inside a fresh
// Runtime and returns the result of the named global. Tests use
// this to exercise tls_cert() / interface(..., tls=...) end-to-end.
func runStarTLS(t *testing.T, baseDir, code, want string) (starlark.Value, error) {
	t.Helper()
	rt := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	rt.baseDir = baseDir
	thread := &starlark.Thread{Name: "test"}
	globals, err := starlark.ExecFile(thread, "test.star", code, rt.builtins())
	if err != nil {
		return nil, err
	}
	return globals[want], nil
}

// writeTempCertPair generates an ECDSA P-256 self-signed cert/key
// pair to a temp dir and returns the (certPath, keyPath, caPath).
// caPath duplicates certPath (the cert is its own CA) so tests can
// pass the same file as ca= when they need it.
func writeTempCertPair(t *testing.T) (cert, key, ca string) {
	t.Helper()
	dir := t.TempDir()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tls-cert-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	cert = filepath.Join(dir, "cert.pem")
	key = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return cert, key, cert
}

// TestTLSCert_NoArgsIsValid — tls_cert() with no kwargs is the
// "auto-generate everything" dev/test default. The RFC documents
// this as the common path and the builtin must accept it.
func TestTLSCert_NoArgsIsValid(t *testing.T) {
	v, err := runStarTLS(t, "", `t = tls_cert()`, "t")
	if err != nil {
		t.Fatalf("tls_cert(): %v", err)
	}
	if _, ok := v.(*TLSConfigDef); !ok {
		t.Fatalf("got %T, want *TLSConfigDef", v)
	}
}

// TestTLSCert_RejectsPositionalArgs — kwargs-only forces customers
// to spell out which path is which (proxy_cert vs client_cert), so
// a typo can't silently swap server / client material.
func TestTLSCert_RejectsPositionalArgs(t *testing.T) {
	_, err := runStarTLS(t, "", `t = tls_cert("/foo/bar.pem")`, "t")
	if err == nil || !strings.Contains(err.Error(), "positional") {
		t.Fatalf("want positional-args error, got %v", err)
	}
}

// TestTLSCert_PairValidation — half-set proxy or client cert/key
// pairs are caught at spec-load with an error that names both
// fields, not at proxy-start when the customer's cert path turns
// out to be wrong.
func TestTLSCert_PairValidation(t *testing.T) {
	cert, _, _ := writeTempCertPair(t)
	cases := []struct {
		name string
		code string
		want string
	}{
		{"proxy_cert without key", `t = tls_cert(proxy_cert="` + cert + `")`, "proxy_cert and proxy_key"},
		{"client_key without cert", `t = tls_cert(client_key="` + cert + `")`, "client_cert and client_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runStarTLS(t, "", tc.code, "t")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestTLSCert_FileMustExist — when paths are non-empty the files
// must exist on disk. Fast spec-load failure beats "connection
// reset by peer at first dial."
func TestTLSCert_FileMustExist(t *testing.T) {
	_, err := runStarTLS(t, "", `t = tls_cert(proxy_cert="/nonexistent/cert.pem", proxy_key="/nonexistent/key.pem")`, "t")
	if err == nil {
		t.Fatalf("expected error for missing cert file")
	}
	if !strings.Contains(err.Error(), "proxy_cert") {
		t.Errorf("error should name the offending field: %v", err)
	}
}

// TestTLSCert_CAMustParse — the CA file must contain at least one
// PEM certificate. An empty / corrupt CA is caught here, not when
// the proxy first tries to verify an upstream.
func TestTLSCert_CAMustParse(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(garbage, []byte("not actually pem\n"), 0600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_, err := runStarTLS(t, "", `t = tls_cert(ca="`+garbage+`")`, "t")
	if err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("want PEM-parse error, got %v", err)
	}
}

// TestTLSCert_InsecureExclusiveWithCA — insecure=True and ca=...
// contradict each other (one skips verification entirely, the
// other supplies the trust anchor for it). Refuse the spec rather
// than silently pick a winner.
func TestTLSCert_InsecureExclusiveWithCA(t *testing.T) {
	cert, _, _ := writeTempCertPair(t)
	_, err := runStarTLS(t, "", `t = tls_cert(ca="`+cert+`", insecure=True)`, "t")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}
}

// TestTLSCert_RelativePaths — paths resolve against the spec's
// baseDir (the directory the .star file lives in). Customers
// usually keep their cert material next to the spec, so the
// natural form is `tls_cert(proxy_cert="certs/proxy.pem", ...)`.
func TestTLSCert_RelativePaths(t *testing.T) {
	cert, _, _ := writeTempCertPair(t)
	dir := filepath.Dir(cert)
	rel := filepath.Base(cert)
	v, err := runStarTLS(t, dir, `t = tls_cert(proxy_cert="`+rel+`", proxy_key="`+rel+`")`, "t")
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	cfg := v.(*TLSConfigDef)
	if cfg.ProxyCert != rel {
		t.Errorf("ProxyCert stored unresolved: got %q want %q", cfg.ProxyCert, rel)
	}
}

// TestInterfaceTLSKwarg_StoresOnInterfaceDef — the wire-up step.
// `interface(..., tls=tls_cert(...))` must end up with
// InterfaceDef.TLS pointing at the same TLSConfigDef value.
func TestInterfaceTLSKwarg_StoresOnInterfaceDef(t *testing.T) {
	v, err := runStarTLS(t, "", `
t = tls_cert()
i = interface("main", "postgres", 5432, tls=t)
`, "i")
	if err != nil {
		t.Fatalf("interface(...): %v", err)
	}
	iface := v.(*InterfaceDef)
	if iface.TLS == nil {
		t.Fatalf("InterfaceDef.TLS is nil")
	}
}

// TestInterfaceTLSKwarg_RejectsWrongType — passing something other
// than tls_cert() (e.g. a string or a bool) must be caught at
// spec-load with a clear "use tls_cert()" hint, not a silent type
// assertion failure later.
func TestInterfaceTLSKwarg_RejectsWrongType(t *testing.T) {
	_, err := runStarTLS(t, "", `i = interface("main", "postgres", 5432, tls="yes")`, "i")
	if err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("want tls_cert hint, got %v", err)
	}
}

// TestTLSCert_ResolveServerConfig_AutoCert — when ProxyCert is
// empty, ResolveServerConfig falls through to
// proxy.GenerateSelfSignedCert. The cfg comes back populated with
// at least one cert and TLS 1.2 minimum.
func TestTLSCert_ResolveServerConfig_AutoCert(t *testing.T) {
	cfg, err := (&TLSConfigDef{}).ResolveServerConfig("", nil)
	if err != nil {
		t.Fatalf("ResolveServerConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("want 1 auto cert, got %d", len(cfg.Certificates))
	}
}

// TestTLSCert_ResolveServerConfig_LoadsKeypair — when paths are
// set, Resolve loads them via tls.LoadX509KeyPair.
func TestTLSCert_ResolveServerConfig_LoadsKeypair(t *testing.T) {
	cert, key, _ := writeTempCertPair(t)
	cfg, err := (&TLSConfigDef{ProxyCert: cert, ProxyKey: key}).ResolveServerConfig("", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("loaded keypair count = %d, want 1", len(cfg.Certificates))
	}
}

// TestTLSCert_ResolveClientConfig_CARootsAndMTLS — full mTLS
// upstream config: client cert + key + CA pool. Resolve should
// populate Certificates AND RootCAs.
func TestTLSCert_ResolveClientConfig_CARootsAndMTLS(t *testing.T) {
	cert, key, ca := writeTempCertPair(t)
	cfg, err := (&TLSConfigDef{
		ClientCert: cert,
		ClientKey:  key,
		CA:         ca,
	}).ResolveClientConfig("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Errorf("RootCAs not populated")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("client Certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.InsecureSkipVerify {
		t.Errorf("InsecureSkipVerify should be false when CA is set")
	}
}

// TestTLSCert_ResolveClientConfig_Insecure — insecure=True path
// produces InsecureSkipVerify=true and no RootCAs.
func TestTLSCert_ResolveClientConfig_Insecure(t *testing.T) {
	cfg, err := (&TLSConfigDef{Insecure: true}).ResolveClientConfig("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Errorf("InsecureSkipVerify not set under insecure=True")
	}
	if cfg.RootCAs != nil {
		t.Errorf("RootCAs unexpectedly populated under insecure path")
	}
}
