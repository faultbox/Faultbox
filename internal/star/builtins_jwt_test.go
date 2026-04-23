package star

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
)

// TestJWTKeypairAndSign drives the three builtins from a Starlark
// spec — same path the @faultbox/mocks/jwt.star wrapper uses — and
// verifies the produced token is a real Ed25519-signed JWS that
// stdlib crypto can verify.
func TestJWTKeypairAndSign(t *testing.T) {
	rt := New(testLogger())
	src := `
keypair = jwt_keypair(kid = "test-key")
token   = jwt_sign(keypair = keypair, claims = {"sub": "alice", "scope": "read"})
jwks    = jwt_jwks(keypair = keypair)
`
	if err := rt.LoadString("jwt.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	tokenVal, ok := rt.globals["token"]
	if !ok {
		t.Fatal("no `token` global")
	}
	tokenStr := strings.Trim(tokenVal.String(), "\"")
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3: %q", len(parts), tokenStr)
	}

	// Extract the public key from JWKS (which is a *starlark.Dict
	// here) and verify the signature.
	jwksGlobal, ok := rt.globals["jwks"]
	if !ok {
		t.Fatal("no `jwks` global")
	}
	jwksJSON, err := starlarkValueToJSON(jwksGlobal)
	if err != nil {
		t.Fatalf("jwks → JSON: %v", err)
	}
	var doc struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(jwksJSON, &doc); err != nil {
		t.Fatalf("jwks unmarshal: %v", err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("jwks has %d keys, want 1", len(doc.Keys))
	}
	if doc.Keys[0]["kid"] != "test-key" {
		t.Errorf("kid = %v, want test-key", doc.Keys[0]["kid"])
	}

	pub, err := base64.RawURLEncoding.DecodeString(doc.Keys[0]["x"].(string))
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), signingInput, sig) {
		t.Error("Ed25519 verify against published JWKS public key failed")
	}

	// Body should round-trip the claims.
	bodyB, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(bodyB, &claims)
	if claims["sub"] != "alice" || claims["scope"] != "read" {
		t.Errorf("claims roundtrip lost values: %+v", claims)
	}
}

// TestJWTServerStdlibE2E spins up a real jwt.server() mock, hits
// `/.well-known/jwks.json` with an HTTP client, and verifies the
// JWKS doc is published correctly. Mints a token via auth.sign() and
// confirms its signature against the served key.
func TestJWTServerStdlibE2E(t *testing.T) {
	port := freePort(t)
	rt := New(testLogger())
	src := fmt.Sprintf(`
load("@faultbox/mocks/jwt.star", "jwt")

auth = jwt.server(
    name      = "auth",
    interface = interface("main", "http", %d),
    issuer    = "https://faultbox-test.invalid",
)

minted_token = auth.sign(claims = {"sub": "user-99"})
`, port)
	if err := rt.LoadString("jwt_e2e.star", src); err != nil {
		t.Fatalf("LoadString: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.startServices(ctx); err != nil {
		t.Fatalf("startServices: %v", err)
	}
	defer rt.stopServices()

	// Fetch the JWKS doc the mock just stood up.
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(
		fmt.Sprintf("http://127.0.0.1:%d/.well-known/jwks.json", port))
	if err != nil {
		t.Fatalf("GET jwks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)

	var doc struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body unmarshal: %v\n%s", err, body)
	}
	if len(doc.Keys) != 1 || doc.Keys[0]["alg"] != "EdDSA" {
		t.Errorf("unexpected JWKS shape: %s", body)
	}

	// Verify the minted token against the served public key.
	tokenVal := rt.globals["minted_token"]
	tokenStr := strings.Trim(tokenVal.String(), "\"")
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		t.Fatalf("token shape: %q", tokenStr)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(doc.Keys[0]["x"].(string))
	signingInput := []byte(parts[0] + "." + parts[1])
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(ed25519.PublicKey(pub), signingInput, sig) {
		t.Error("token signature does not validate against published JWKS")
	}

	// Discovery doc should also be reachable.
	resp2, err := (&http.Client{Timeout: 2 * time.Second}).Get(
		fmt.Sprintf("http://127.0.0.1:%d/.well-known/openid-configuration", port))
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("discovery status = %d, want 200", resp2.StatusCode)
	}
}

// starlarkValueToJSON marshals an arbitrary starlark.Value to JSON
// via the existing starlarkToGo helper. Local to this file because
// it's only useful in JWT round-trip tests.
func starlarkValueToJSON(v starlark.Value) ([]byte, error) {
	g, err := starlarkToGo(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}
