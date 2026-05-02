// Package star — RFC-038 Phase 2: TLS material for protocol proxies.
//
// `tls_cert(...)` is the Starlark builtin customers attach to an
// interface via `interface(..., tls=tls_cert(...))`. It carries the
// cert paths the proxy needs to terminate TLS on the listener side
// and (optionally) to dial the upstream over mTLS. Resolution to a
// real *tls.Config happens at proxy-start time via TLSConfigDef.Resolve()
// — paths are checked at spec-load to fail fast when they're missing,
// but the actual PEM bytes get loaded lazily so a session that never
// opens a TLS interface doesn't pay the I/O cost.
//
// Per the RFC's Phase 2 deliverable: the spec language accepts and
// validates TLS config; per-plugin migration that consumes the
// resolved config is Phase 3.

package star

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/proxy"
)

// builtinTLSCert implements:
//
//	tls_cert(
//	    proxy_cert="/path/to/proxy-server.crt",   # cert proxy presents to clients
//	    proxy_key="/path/to/proxy-server.key",
//	    client_cert="/path/to/proxy-client.crt",  # mTLS client cert (optional)
//	    client_key="/path/to/proxy-client.key",
//	    ca="/path/to/upstream-ca.crt",            # CA the proxy trusts for upstream
//	    insecure=False,                           # InsecureSkipVerify on upstream
//	)
//
// All kwargs are optional. `tls_cert()` (no args) is the dev/test
// default — proxy auto-generates a self-signed server cert and trusts
// the system CA pool when verifying upstream.
//
// Validation at spec-load:
//   - proxy_cert and proxy_key must both be set or both empty.
//   - client_cert and client_key must both be set or both empty.
//   - When set, all four cert/key paths must exist on disk.
//   - When set, the CA path must exist and parse as PEM.
//   - When `insecure=True` and CA is also set, that's contradictory —
//     refuse the spec rather than silently honouring one over the other.
//
// Relative paths resolve against the spec directory (rt.baseDir),
// matching the load_file convention.
func (rt *Runtime) builtinTLSCert(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 0 {
		return nil, fmt.Errorf("tls_cert: positional args not supported; use kwargs")
	}

	cfg := &TLSConfigDef{}

	if v, ok := starKwarg(kwargs, "proxy_cert"); ok {
		s, _ := starlark.AsString(v)
		cfg.ProxyCert = s
	}
	if v, ok := starKwarg(kwargs, "proxy_key"); ok {
		s, _ := starlark.AsString(v)
		cfg.ProxyKey = s
	}
	if v, ok := starKwarg(kwargs, "client_cert"); ok {
		s, _ := starlark.AsString(v)
		cfg.ClientCert = s
	}
	if v, ok := starKwarg(kwargs, "client_key"); ok {
		s, _ := starlark.AsString(v)
		cfg.ClientKey = s
	}
	if v, ok := starKwarg(kwargs, "ca") ; ok {
		s, _ := starlark.AsString(v)
		cfg.CA = s
	}
	if v, ok := starKwarg(kwargs, "insecure"); ok {
		if b, ok := v.(starlark.Bool); ok {
			cfg.Insecure = bool(b)
		} else {
			return nil, fmt.Errorf("tls_cert: insecure must be a bool")
		}
	}

	// Pair-up checks first — they're cheaper than I/O and produce
	// the most useful error messages.
	if (cfg.ProxyCert == "") != (cfg.ProxyKey == "") {
		return nil, fmt.Errorf("tls_cert: proxy_cert and proxy_key must both be set or both omitted (got cert=%q key=%q)", cfg.ProxyCert, cfg.ProxyKey)
	}
	if (cfg.ClientCert == "") != (cfg.ClientKey == "") {
		return nil, fmt.Errorf("tls_cert: client_cert and client_key must both be set or both omitted (got cert=%q key=%q)", cfg.ClientCert, cfg.ClientKey)
	}
	if cfg.Insecure && cfg.CA != "" {
		return nil, fmt.Errorf("tls_cert: ca and insecure=True are mutually exclusive (insecure skips CA verification entirely)")
	}

	// File-existence + CA parse checks. Skip when running under a
	// pure spec-validation path that hasn't set baseDir (e.g.
	// faultbox lock --dry-run before spec is on disk); see the
	// matching guard in builtins_load_file.go.
	for _, p := range []struct {
		field string
		path  string
		parse bool
	}{
		{"proxy_cert", cfg.ProxyCert, false},
		{"proxy_key", cfg.ProxyKey, false},
		{"client_cert", cfg.ClientCert, false},
		{"client_key", cfg.ClientKey, false},
		{"ca", cfg.CA, true},
	} {
		if p.path == "" {
			continue
		}
		resolved := p.path
		if !filepath.IsAbs(resolved) && rt.baseDir != "" {
			resolved = filepath.Join(rt.baseDir, p.path)
		}
		if _, err := os.Stat(resolved); err != nil {
			return nil, fmt.Errorf("tls_cert: %s file %q: %w", p.field, p.path, err)
		}
		if p.parse {
			pemBytes, err := os.ReadFile(resolved)
			if err != nil {
				return nil, fmt.Errorf("tls_cert: read ca file %q: %w", p.path, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("tls_cert: ca file %q: no PEM certificates found", p.path)
			}
		}
	}

	return cfg, nil
}

// ResolveServerConfig builds the *tls.Config used by proxy.ListenTLS.
// When ProxyCert/ProxyKey are empty, falls back to
// proxy.GenerateSelfSignedCert with hosts pre-loaded for loopback so
// the host-side dial address from Listen() works without per-test
// SAN config.
//
// baseDir is the spec directory; relative paths resolve against it
// (the runtime passes rt.baseDir at call sites).
//
// Phase 3 plugins call this; Phase 2 ships the helper.
func (t *TLSConfigDef) ResolveServerConfig(baseDir string, extraHosts []string) (*tls.Config, error) {
	if t == nil {
		return nil, fmt.Errorf("nil TLSConfigDef")
	}
	if t.ProxyCert == "" {
		// Auto-generated path — RFC sub-option 1a.
		return proxy.GenerateSelfSignedCert(extraHosts)
	}

	certPath := resolveTLSPath(baseDir, t.ProxyCert)
	keyPath := resolveTLSPath(baseDir, t.ProxyKey)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load proxy keypair (%s, %s): %w", t.ProxyCert, t.ProxyKey, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ResolveClientConfig builds the *tls.Config used by proxy.Dial when
// the proxy itself dials an upstream over TLS. Honours ClientCert /
// ClientKey for mTLS, CA for upstream verification, and Insecure as
// the dev-only escape hatch.
func (t *TLSConfigDef) ResolveClientConfig(baseDir string) (*tls.Config, error) {
	if t == nil {
		return nil, fmt.Errorf("nil TLSConfigDef")
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.Insecure,
	}

	if t.CA != "" {
		caPath := resolveTLSPath(baseDir, t.CA)
		pemBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ca file %q: %w", t.CA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("ca file %q: no PEM certificates found", t.CA)
		}
		cfg.RootCAs = pool
	}

	if t.ClientCert != "" {
		certPath := resolveTLSPath(baseDir, t.ClientCert)
		keyPath := resolveTLSPath(baseDir, t.ClientKey)
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load client keypair (%s, %s): %w", t.ClientCert, t.ClientKey, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

func resolveTLSPath(baseDir, path string) string {
	if filepath.IsAbs(path) || baseDir == "" {
		return path
	}
	return filepath.Join(baseDir, path)
}
