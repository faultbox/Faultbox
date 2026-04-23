package jwt

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestKeypairProducesEd25519(t *testing.T) {
	kp, err := NewKeypair("k1")
	if err != nil {
		t.Fatalf("NewKeypair: %v", err)
	}
	if kp.KID != "k1" {
		t.Errorf("kid = %q, want k1", kp.KID)
	}
	if len(kp.Public) != ed25519.PublicKeySize {
		t.Errorf("public key size = %d, want %d", len(kp.Public), ed25519.PublicKeySize)
	}
	if len(kp.Private) != ed25519.PrivateKeySize {
		t.Errorf("private key size = %d, want %d", len(kp.Private), ed25519.PrivateKeySize)
	}
}

// TestSignRoundTrip mints a token with our Keypair and verifies it
// using the stdlib Ed25519 Verify against the public key extracted
// from JWKS — proves the JWKS doc actually publishes the right key.
func TestSignRoundTrip(t *testing.T) {
	kp, err := NewKeypair("k1")
	if err != nil {
		t.Fatalf("NewKeypair: %v", err)
	}

	token, err := kp.Sign(map[string]any{
		"sub":   "alice",
		"email": "alice@example.com",
		"iat":   1700000000,
		"exp":   1700003600,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Token is header.body.sig (three base64url segments, dot-separated).
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3: %q", len(parts), token)
	}

	// Verify the signature using the Keypair's public key directly,
	// then again using the key extracted from JWKS.
	signingInput := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(kp.Public, signingInput, sig) {
		t.Error("direct Verify failed")
	}

	jwks := kp.JWKS()
	keys, ok := jwks["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("JWKS keys = %v", jwks["keys"])
	}
	jwk := keys[0].(map[string]any)
	xb64 := jwk["x"].(string)
	pubFromJWKS, err := base64.RawURLEncoding.DecodeString(xb64)
	if err != nil {
		t.Fatalf("decode JWKS x: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubFromJWKS), signingInput, sig) {
		t.Error("Verify against JWKS-published public key failed")
	}
}

// TestSignClaimsRoundTrip verifies that the body segment decodes back
// to the original claims map — important so users see the values
// they passed in when their service decodes the token.
func TestSignClaimsRoundTrip(t *testing.T) {
	kp, _ := NewKeypair("k1")
	claims := map[string]any{
		"sub":  "user-42",
		"role": "admin",
	}
	token, _ := kp.Sign(claims)
	parts := strings.Split(token, ".")
	bodyB, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var got map[string]any
	if err := json.Unmarshal(bodyB, &got); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if got["sub"] != "user-42" || got["role"] != "admin" {
		t.Errorf("claims roundtrip lost values: %+v", got)
	}
}

func TestJWKSShape(t *testing.T) {
	kp, _ := NewKeypair("custom-kid")
	jwks := kp.JWKS()
	keys := jwks["keys"].([]any)
	jwk := keys[0].(map[string]any)
	wants := map[string]string{
		"kty": "OKP",
		"crv": "Ed25519",
		"alg": "EdDSA",
		"use": "sig",
		"kid": "custom-kid",
	}
	for k, want := range wants {
		if got := jwk[k]; got != want {
			t.Errorf("jwk[%q] = %v, want %q", k, got, want)
		}
	}
	if jwk["x"].(string) == "" {
		t.Error("jwk.x is empty")
	}
}
