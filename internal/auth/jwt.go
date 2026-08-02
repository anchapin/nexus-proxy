// jwt.go implements the JWTAuthenticator (issue #1152).
//
// The authenticator validates RS256/ES256 JWTs against a JWKS (JSON Web Key
// Set) endpoint — the standard OIDC key-distribution mechanism used by Okta,
// Auth0, Keycloak, Azure AD, and Google. Keys are fetched once and cached;
// a background refresh runs at a configurable interval and on key-not-found.
//
// Validation enforced:
//   - alg allowlist: RS256, ES256 (the "none" alg is always rejected).
//   - iss (issuer) must match NEXUS_OIDC_ISSUER.
//   - aud (audience) must contain NEXUS_OIDC_AUDIENCE.
//   - exp (expiry) and nbf (not-before) are checked with a 30s leeway.
//
// Fail-closed semantics: if the JWKS endpoint is unreachable at boot AND no
// cached keys exist, every request is rejected with 401.

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultJWKSRefreshInterval is how often the background goroutine re-fetches
// the JWKS set. Most IdPs rotate keys every few hours; 15 minutes is a safe
// default that catches rotation without hammering the endpoint.
const DefaultJWKSRefreshInterval = 15 * time.Minute

// allowedAlgorithms is the fixed allowlist of JWT signing algorithms. The
// "none" algorithm is intentionally excluded to prevent algorithm-confusion
// attacks.
var allowedAlgorithms = []string{"RS256", "ES256"}

// JWKSConfig holds the parameters for constructing a JWTAuthenticator.
type JWKSConfig struct {
	// JWKSURL is the OIDC JWKS endpoint (e.g.
	// "https://login.okta.com/oauth2/default/v1/keys").
	JWKSURL string
	// Issuer is the expected "iss" claim value. Empty disables issuer
	// validation (not recommended).
	Issuer string
	// Audience is the expected "aud" claim value. Empty disables audience
	// validation (not recommended).
	Audience string
	// RefreshInterval controls how often the background goroutine
	// re-fetches the JWKS set. <= 0 uses DefaultJWKSRefreshInterval.
	RefreshInterval time.Duration
	// HTTPClient is used for JWKS fetches. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
}

// jwk represents a single JSON Web Key in the JWKS set.
type jwk struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	ALG string `json:"alg"`
	Use string `json:"use"`
	// RSA fields
	N string `json:"n"`
	E string `json:"e"`
	// EC fields
	CRV string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// jwksSet is the top-level JWKS JSON structure.
type jwksSet struct {
	Keys []jwk `json:"keys"`
}

// JWTAuthenticator validates JWT bearer tokens against a JWKS endpoint.
type JWTAuthenticator struct {
	cfg    JWKSConfig
	client *http.Client
	leeway time.Duration
	parser *jwt.Parser
	now    func() time.Time

	mu        sync.RWMutex
	keys      map[string]any // kid → parsed public key
	lastFetch time.Time
	fetchErr  error // last background fetch error (nil = ok)

	stopCh chan struct{}
}

// NewJWTAuthenticator constructs a JWTAuthenticator, performs an initial
// blocking JWKS fetch, and starts the background refresh goroutine. Returns
// an error if the initial fetch fails (fail-closed at boot).
func NewJWTAuthenticator(cfg JWKSConfig) (*JWTAuthenticator, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("jwt: JWKSURL is required")
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultJWKSRefreshInterval
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	a := &JWTAuthenticator{
		cfg:    cfg,
		client: client,
		leeway: 30 * time.Second,
		parser: jwt.NewParser(
			jwt.WithValidMethods(allowedAlgorithms),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(30*time.Second),
		),
		now:    time.Now,
		keys:   make(map[string]any),
		stopCh: make(chan struct{}),
	}

	// Blocking initial fetch — fail-closed at boot.
	if err := a.fetchKeys(context.Background()); err != nil {
		return nil, fmt.Errorf("jwt: initial JWKS fetch: %w", err)
	}

	go a.refreshLoop()
	return a, nil
}

// Close stops the background refresh goroutine. Safe to call multiple times.
func (a *JWTAuthenticator) Close() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
}

// Type returns the authenticator label.
func (a *JWTAuthenticator) Type() string { return "jwt" }

// Authenticate validates rawToken against the cached JWKS keys and claims.
func (a *JWTAuthenticator) Authenticate(rawToken string) error {
	if rawToken == "" {
		return errors.New("jwt: empty token")
	}

	// Parse the unverified token to extract the kid header. We need the
	// kid to look up the correct signing key.
	unverifiedToken, _, err := jwt.NewParser().ParseUnverified(rawToken, &jwt.RegisteredClaims{})
	if err != nil {
		return fmt.Errorf("jwt: parse: %w", err)
	}

	kid, ok := unverifiedToken.Header["kid"].(string)
	if !ok || kid == "" {
		return errors.New("jwt: missing kid header")
	}

	key, err := a.getKey(kid)
	if err != nil {
		return err
	}

	claims := &jwt.RegisteredClaims{}
	_, err = a.parser.ParseWithClaims(rawToken, claims, func(t *jwt.Token) (any, error) {
		// Double-check the alg is in our allowlist (parser enforces this
		// too, but defence in depth).
		alg, _ := t.Header["alg"].(string)
		if !isAllowedAlgorithm(alg) {
			return nil, fmt.Errorf("jwt: unexpected signing method %q", alg)
		}
		return key, nil
	})
	if err != nil {
		return fmt.Errorf("jwt: validation: %w", err)
	}
	return nil
}

// getKey returns the public key for the given kid. If the kid is not in the
// cache, a synchronous refresh is attempted (once) to catch recently rotated
// keys. Returns an error if the key cannot be resolved (fail-closed).
func (a *JWTAuthenticator) getKey(kid string) (any, error) {
	a.mu.RLock()
	key, ok := a.keys[kid]
	a.mu.RUnlock()
	if ok {
		return key, nil
	}

	// Key not found — attempt a synchronous refresh to catch rotation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.fetchKeys(ctx); err != nil {
		return nil, fmt.Errorf("jwt: key %q not found and refresh failed: %w", kid, err)
	}

	a.mu.RLock()
	key, ok = a.keys[kid]
	a.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("jwt: key %q not found in JWKS", kid)
	}
	return key, nil
}

// refreshLoop periodically re-fetches the JWKS set until Close is called.
func (a *JWTAuthenticator) refreshLoop() {
	ticker := time.NewTicker(a.cfg.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := a.fetchKeys(ctx); err != nil {
				slog.Warn("jwt: background JWKS refresh failed",
					slog.String("url", a.cfg.JWKSURL),
					slog.Any("err", err),
				)
			}
			cancel()
		}
	}
}

// fetchKeys fetches and parses the JWKS set, updating the in-memory cache.
func (a *JWTAuthenticator) fetchKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.JWKSURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return err
	}

	var set jwksSet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("parse JWKS JSON: %w", err)
	}

	parsed := make(map[string]any, len(set.Keys))
	for _, k := range set.Keys {
		pk, err := parseJWK(k)
		if err != nil {
			slog.Warn("jwt: skipping unparseable JWKS key",
				slog.String("kid", k.KID),
				slog.String("kty", k.KTY),
				slog.Any("err", err),
			)
			continue
		}
		parsed[k.KID] = pk
	}
	if len(parsed) == 0 {
		return errors.New("JWKS set contained no usable keys")
	}

	a.mu.Lock()
	a.keys = parsed
	a.lastFetch = time.Now()
	a.fetchErr = nil
	a.mu.Unlock()

	slog.Info("jwt: JWKS keys fetched",
		slog.Int("count", len(parsed)),
		slog.String("url", a.cfg.JWKSURL),
	)
	return nil
}

// parseJWK converts a single JWK into a crypto/* public key.
func parseJWK(k jwk) (any, error) {
	switch strings.ToUpper(k.KTY) {
	case "RSA":
		return parseRSAKey(k)
	case "EC":
		return parseECKey(k)
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.KTY)
	}
}

// parseRSAKey builds an *rsa.PublicKey from the JWK n/e components.
func parseRSAKey(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode RSA n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode RSA e: %w", err)
	}

	var exp int
	for _, b := range eBytes {
		exp = exp<<8 + int(b)
	}
	if exp < 3 {
		return nil, errors.New("invalid RSA exponent")
	}

	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exp}
	return pub, nil
}

// parseECKey builds an *ecdsa.PublicKey from the JWK crv/x/y components.
func parseECKey(k jwk) (*ecdsa.PublicKey, error) {
	curve, err := ecCurve(k.CRV)
	if err != nil {
		return nil, err
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("decode EC x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("decode EC y: %w", err)
	}
	pub := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	return pub, nil
}

// isAllowedAlgorithm reports whether alg is in the allowedAlgorithms set.
func isAllowedAlgorithm(alg string) bool {
	for _, a := range allowedAlgorithms {
		if alg == a {
			return true
		}
	}
	return false
}

// ecCurve maps a JWK "crv" value to the corresponding elliptic.Curve.
func ecCurve(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", crv)
	}
}
