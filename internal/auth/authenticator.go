// authenticator.go defines the pluggable Authenticator interface (issue #1152).
//
// The interface decouples the HTTP middleware from the specific credential
// validation strategy. Two implementations exist:
//
//   - BearerAuthenticator: constant-time comparison against a static shared
//     secret (the pre-#1152 behaviour, byte-for-byte unchanged).
//   - JWTAuthenticator: RS256/ES256 JWT validation against a JWKS endpoint
//     with issuer/audience/expiry enforcement (internal/auth/jwt.go).
//
// NEXUS_AUTH_MODE selects which authenticator(s) the middleware consults.
// In "both" mode a request is accepted if EITHER authenticator validates.

package auth

import (
	"crypto/subtle"
	"errors"
)

// AuthMode controls which authenticator(s) the middleware consults.
type AuthMode string

const (
	// AuthModeStatic uses only the BearerAuthenticator (default,
	// pre-#1152 behaviour).
	AuthModeStatic AuthMode = "static"
	// AuthModeJWT uses only the JWTAuthenticator.
	AuthModeJWT AuthMode = "jwt"
	// AuthModeBoth accepts a request if EITHER the static key or a
	// valid JWT validates.
	AuthModeBoth AuthMode = "both"
)

// ParseAuthMode normalises a mode string. Unrecognised values fall back
// to AuthModeStatic so a typo never disables auth entirely.
func ParseAuthMode(s string) AuthMode {
	switch AuthMode(s) {
	case AuthModeJWT:
		return AuthModeJWT
	case AuthModeBoth:
		return AuthModeBoth
	default:
		return AuthModeStatic
	}
}

// Authenticator validates a raw bearer credential (the token portion of
// "Authorization: Bearer <token>"). It returns nil on success or a
// non-nil error describing why the credential was rejected.
//
// Implementations MUST be safe for concurrent use.
type Authenticator interface {
	// Authenticate validates rawToken and returns nil on success.
	Authenticate(rawToken string) error
	// Type returns a human-readable label for logging/metrics.
	Type() string
}

// BearerAuthenticator validates a token via constant-time comparison
// against a single shared secret. This is the pre-#1152 behaviour,
// extracted into the Authenticator interface unchanged.
type BearerAuthenticator struct {
	key string
}

// NewBearerAuthenticator returns a BearerAuthenticator for the given
// static key. An empty key produces an authenticator that rejects every
// token (the middleware guards the enabled/disabled boundary).
func NewBearerAuthenticator(key string) *BearerAuthenticator {
	return &BearerAuthenticator{key: key}
}

// Authenticate performs a constant-time comparison of rawToken against
// the configured key.
func (b *BearerAuthenticator) Authenticate(rawToken string) error {
	if subtle.ConstantTimeCompare([]byte(rawToken), []byte(b.key)) != 1 {
		return errors.New("invalid API key")
	}
	return nil
}

// Type returns the authenticator label.
func (b *BearerAuthenticator) Type() string { return "static" }

// multiAuthenticator wraps two authenticators; a request is accepted if
// EITHER validates (NEXUS_AUTH_MODE=both).
type multiAuthenticator struct {
	primary   Authenticator
	secondary Authenticator
}

// Authenticate returns nil if either the primary or secondary
// authenticator accepts rawToken.
func (m *multiAuthenticator) Authenticate(rawToken string) error {
	if err := m.primary.Authenticate(rawToken); err == nil {
		return nil
	}
	return m.secondary.Authenticate(rawToken)
}

func (m *multiAuthenticator) Type() string { return "multi" }

// NewMultiAuthenticator returns an Authenticator that accepts a token if
// EITHER primary or secondary validates it.
func NewMultiAuthenticator(primary, secondary Authenticator) Authenticator {
	return &multiAuthenticator{primary: primary, secondary: secondary}
}
