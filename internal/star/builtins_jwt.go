package star

import (
	"fmt"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/jwt"
)

// Three Starlark builtins exposing the internal/jwt package so the
// @faultbox/mocks/jwt.star stdlib can wrap them into a one-liner
// `jwt.server(...)` constructor. Customer ask B3 from the inDrive
// feedback analysis (v0.9.9 release).
//
//   jwt_keypair(kid="…")        → opaque keypair value
//   jwt_sign(keypair, claims)   → signed JWS string
//   jwt_jwks(keypair)           → JWKS dict (publishable as JSON)
//
// Users normally don't touch these directly — the stdlib wrapper is
// the supported surface — but they're standalone-useful for power
// users who want a custom OIDC discovery layout.

// keypairValue wraps internal/jwt.Keypair so it round-trips through
// Starlark dicts and lambdas as an opaque handle. Starlark sees an
// unhashable value ("keypair") with no inspectable attributes —
// callers move it from one builtin to another, that's it.
type keypairValue struct {
	kp *jwt.Keypair
}

var _ starlark.Value = (*keypairValue)(nil)

func (v *keypairValue) String() string        { return fmt.Sprintf("<keypair kid=%q>", v.kp.KID) }
func (v *keypairValue) Type() string          { return "keypair" }
func (v *keypairValue) Freeze()               {}
func (v *keypairValue) Truth() starlark.Bool  { return starlark.True }
func (v *keypairValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: keypair") }

// builtinJWTKeypair generates a fresh Ed25519 keypair. Users name
// the kid; if omitted, a stable default goes in so the JWKS isn't
// nameless. Each call produces independent keys — useful for tests
// that want multiple issuers.
func builtinJWTKeypair(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	kid := "faultbox-mock-key"
	if err := starlark.UnpackArgs("jwt_keypair", args, kwargs, "kid?", &kid); err != nil {
		return nil, err
	}
	kp, err := jwt.NewKeypair(kid)
	if err != nil {
		return nil, fmt.Errorf("jwt_keypair: %w", err)
	}
	return &keypairValue{kp: kp}, nil
}

// builtinJWTSign signs a claims dict with a keypair, returning the
// compact JWS as a Starlark string. Claims are passed verbatim — no
// auto-iat/exp magic — so spec authors stay in control of token
// expiry semantics.
func builtinJWTSign(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		kpVal  starlark.Value
		claims *starlark.Dict
	)
	if err := starlark.UnpackArgs("jwt_sign", args, kwargs, "keypair", &kpVal, "claims?", &claims); err != nil {
		return nil, err
	}
	kp, ok := kpVal.(*keypairValue)
	if !ok {
		return nil, fmt.Errorf("jwt_sign: keypair argument must be a jwt_keypair value (got %s)", kpVal.Type())
	}

	claimMap := map[string]any{}
	if claims != nil {
		converted, err := starlarkToGo(claims)
		if err != nil {
			return nil, fmt.Errorf("jwt_sign claims: %w", err)
		}
		m, ok := converted.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jwt_sign claims: must be a string-keyed dict")
		}
		claimMap = m
	}

	token, err := kp.kp.Sign(claimMap)
	if err != nil {
		return nil, fmt.Errorf("jwt_sign: %w", err)
	}
	return starlark.String(token), nil
}

// builtinJWTJWKS returns the standard {"keys":[…]} document for a
// keypair, ready to be wrapped in json_response() for the mock's
// `/.well-known/jwks.json` route.
func builtinJWTJWKS(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kpVal starlark.Value
	if err := starlark.UnpackArgs("jwt_jwks", args, kwargs, "keypair", &kpVal); err != nil {
		return nil, err
	}
	kp, ok := kpVal.(*keypairValue)
	if !ok {
		return nil, fmt.Errorf("jwt_jwks: argument must be a jwt_keypair value (got %s)", kpVal.Type())
	}
	return goToStarlarkValue(kp.kp.JWKS(), "<keypair>", "jwt_jwks")
}
