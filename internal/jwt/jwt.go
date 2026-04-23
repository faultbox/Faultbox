// Package jwt is a tiny Ed25519-only JWT + JWKS implementation used
// by Faultbox's @faultbox/mocks/jwt.star stdlib. The customer report
// (FAULTBOX_FEEDBACK.md §3 row 2) called out JWT/JWKS as "every
// HTTP-auth service needs this" — shipping a self-contained primitive
// avoids the customer-rebuilt-it-themselves trap.
//
// Scope is deliberately narrow:
//   - Ed25519 signing only (RFC-8037 OKP / "EdDSA"). Most modern OIDC
//     issuers (Auth0, Okta, Keycloak with EdDSA enabled) and customer
//     in-house auth services run EdDSA today; RSA/ECDSA can come
//     later if asked.
//   - Standalone keys per Server — no key rotation, no JWKS with
//     multiple kids.
//   - Header is fixed: {alg:"EdDSA", typ:"JWT", kid:<configured>}.
//   - Claims are passed in as map[string]any from Starlark and
//     marshaled verbatim. We don't enforce iss/aud/exp — that's the
//     spec author's call.
package jwt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Keypair carries a freshly-generated Ed25519 keypair plus its kid.
// Created by NewKeypair; consumed by Sign and JWKS. The private key
// stays in memory for the lifetime of the spec — no persistence to
// disk, no exposure outside the mock service that owns it.
type Keypair struct {
	KID     string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// NewKeypair generates a fresh Ed25519 keypair labelled with kid.
// Each Faultbox spec run gets new keys; tokens minted in one run
// don't validate in another, which is the right behaviour for a
// per-run mock issuer.
func NewKeypair(kid string) (*Keypair, error) {
	if kid == "" {
		kid = "faultbox-mock-key"
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 keypair: %w", err)
	}
	return &Keypair{KID: kid, Private: priv, Public: pub}, nil
}

// Sign produces a compact JWS (header.body.sig) over claims using the
// keypair. Claims are JSON-encoded verbatim; callers that want
// standard timestamps (`iat`, `exp`) populate them in the map.
func (k *Keypair) Sign(claims map[string]any) (string, error) {
	header := map[string]string{
		"alg": "EdDSA",
		"typ": "JWT",
		"kid": k.KID,
	}
	headerB, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}
	bodyB, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal jwt claims: %w", err)
	}
	signingInput := b64url(headerB) + "." + b64url(bodyB)
	sig := ed25519.Sign(k.Private, []byte(signingInput))
	return signingInput + "." + b64url(sig), nil
}

// JWKS returns the standard {"keys":[…]} JSON document publishable at
// `/.well-known/jwks.json`. The returned map is plain Go types so the
// caller can wrap it with json_response() in Starlark without
// re-marshaling.
func (k *Keypair) JWKS() map[string]any {
	return map[string]any{
		"keys": []any{k.JWK()},
	}
}

// JWK returns one entry of the JWKS array — used by callers that
// want to embed the key into a larger discovery document.
func (k *Keypair) JWK() map[string]any {
	return map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"alg": "EdDSA",
		"use": "sig",
		"kid": k.KID,
		"x":   b64url(k.Public),
	}
}

// b64url is the base64url-without-padding encoding the JOSE specs
// mandate. base64.RawURLEncoding emits exactly this — no trailing '='.
func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
