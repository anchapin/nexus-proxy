package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksTestEnv bundles a test JWKS server, signing key, and kid so tests
// can mint and validate tokens without external dependencies.
type jwksTestEnv struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	auth     *JWTAuthenticator
	requests int
	keySet   []byte // cached JWKS JSON
}

// newJWTCTestEnv creates a test JWKS server backed by a freshly generated
// 2048-bit RSA key. The server serves the key set as JSON over HTTPS.
// A JWTAuthenticator is constructed against the server URL and returned ready for use.
func newJWTCTestEnv(t *testing.T, issuer, audience string) *jwksTestEnv {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const kid = "test-key-1"

	jwksJSON, err := json.Marshal(jwksSet{
		Keys: []jwk{rsaPubToJWK(&key.PublicKey, kid)},
	})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}

	env := &jwksTestEnv{
		key:      key,
		kid:      kid,
		issuer:   issuer,
		audience: audience,
		keySet:   jwksJSON,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		env.requests++
		w.Header().Set("Content-Type", "application/json")
		w.Write(env.keySet)
	})

	env.server = httptest.NewTLSServer(mux)
	t.Cleanup(env.server.Close)

	tlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	auth, err := NewJWTAuthenticator(JWKSConfig{
		JWKSURL:         env.server.URL + "/.well-known/jwks.json",
		Issuer:          issuer,
		Audience:        audience,
		RefreshInterval: 1 * time.Hour,
		HTTPClient:      tlsClient,
	})
	if err != nil {
		t.Fatalf("NewJWTAuthenticator: %v", err)
	}
	env.auth = auth
	t.Cleanup(auth.Close)
	return env
}

// rsaPubToJWK converts an RSA public key into a jwk struct suitable for
// JWKS JSON serialization.
func rsaPubToJWK(pub *rsa.PublicKey, kid string) jwk {
	return jwk{
		KTY: "RSA",
		KID: kid,
		ALG: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// makeToken signs a JWT with the test key and standard claims.
func (e *jwksTestEnv) makeToken(t *testing.T, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = e.kid
	signed, err := token.SignedString(e.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestBearerAuthenticatorAccepts(t *testing.T) {
	a := NewBearerAuthenticator("secret")
	if err := a.Authenticate("secret"); err != nil {
		t.Errorf("correct key: unexpected error %v", err)
	}
	if a.Type() != "static" {
		t.Errorf("Type = %q, want %q", a.Type(), "static")
	}
}

func TestBearerAuthenticatorRejects(t *testing.T) {
	a := NewBearerAuthenticator("secret")
	if err := a.Authenticate("wrong"); err == nil {
		t.Error("wrong key: expected error, got nil")
	}
}

func TestParseAuthMode(t *testing.T) {
	cases := []struct {
		input string
		want  AuthMode
	}{
		{"static", AuthModeStatic},
		{"jwt", AuthModeJWT},
		{"both", AuthModeBoth},
		{"", AuthModeStatic},
		{"unknown", AuthModeStatic},
		{"JWT", AuthModeStatic}, // case-sensitive
	}
	for _, tc := range cases {
		got := ParseAuthMode(tc.input)
		if got != tc.want {
			t.Errorf("ParseAuthMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMultiAuthenticatorEitherAccepts(t *testing.T) {
	bearer := NewBearerAuthenticator("static-secret")
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	defer env.auth.Close()
	multi := NewMultiAuthenticator(bearer, env.auth)

	validJWT := env.makeToken(t, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})

	if err := multi.Authenticate("static-secret"); err != nil {
		t.Errorf("static key via multi: unexpected error %v", err)
	}
	if err := multi.Authenticate(validJWT); err != nil {
		t.Errorf("valid JWT via multi: unexpected error %v", err)
	}
	if err := multi.Authenticate("garbage"); err == nil {
		t.Error("garbage via multi: expected error, got nil")
	}
}

func TestJWTAcceptsValidRS256(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	token := env.makeToken(t, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})
	if err := env.auth.Authenticate(token); err != nil {
		t.Errorf("valid RS256 token rejected: %v", err)
	}
}

func TestJWTRejectsExpired(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	token := env.makeToken(t, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
	})
	if err := env.auth.Authenticate(token); err == nil {
		t.Error("expired token accepted, want rejection")
	}
}

func TestJWTRejectsWrongIssuer(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	token := env.makeToken(t, jwt.RegisteredClaims{
		Issuer:    "https://wrong-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})
	if err := env.auth.Authenticate(token); err == nil {
		t.Error("wrong issuer accepted, want rejection")
	}
}

func TestJWTRejectsWrongAudience(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	token := env.makeToken(t, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"wrong-audience"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})
	if err := env.auth.Authenticate(token); err == nil {
		t.Error("wrong audience accepted, want rejection")
	}
}

func TestJWTRejectsUnknownKid(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")

	// Sign with a different key but use a kid that's not in the JWKS.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})
	token.Header["kid"] = "nonexistent-kid"
	signed, err := token.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	if err := env.auth.Authenticate(signed); err == nil {
		t.Error("token with unknown kid accepted, want rejection")
	}
}

func TestJWTRejectsAlgNone(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")

	// Forge a token with alg=none and an empty signature.
	header := fmt.Sprintf(`{"alg":"none","typ":"JWT","kid":"%s"}`, env.kid)
	payload := fmt.Sprintf(`{"iss":"https://test-issuer","aud":"nexus-proxy","exp":%d}`,
		time.Now().Add(1*time.Hour).Unix())

	hEnc := base64.RawURLEncoding.EncodeToString([]byte(header))
	pEnc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	forged := hEnc + "." + pEnc + "."

	if err := env.auth.Authenticate(forged); err == nil {
		t.Error("alg=none token accepted, want rejection")
	}
}

func TestJWTFailClosedOnFetchError(t *testing.T) {
	broken := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	tlsClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	_, err := NewJWTAuthenticator(JWKSConfig{
		JWKSURL:    broken.URL + "/keys",
		HTTPClient: tlsClient,
	})
	if err == nil {
		t.Error("expected boot failure when JWKS is unreachable, got nil")
	}
}

func TestJWTFailClosedOnEmptyKeySet(t *testing.T) {
	empty := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"keys":[]}`))
	}))
	defer empty.Close()

	tlsClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	_, err := NewJWTAuthenticator(JWKSConfig{
		JWKSURL:    empty.URL + "/keys",
		HTTPClient: tlsClient,
	})
	if err == nil {
		t.Error("expected boot failure when JWKS is empty, got nil")
	}
}

func TestJWTRejectsMalformedToken(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	if err := env.auth.Authenticate("not-a-jwt"); err == nil {
		t.Error("malformed token accepted, want rejection")
	}
}

func TestJWTRejectsEmptyToken(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	if err := env.auth.Authenticate(""); err == nil {
		t.Error("empty token accepted, want rejection")
	}
}

func TestJWTRequiresJWKSURL(t *testing.T) {
	_, err := NewJWTAuthenticator(JWKSConfig{})
	if err == nil {
		t.Error("expected error when JWKSURL is empty, got nil")
	}
}

func TestJWTRejectsHTTPURL(t *testing.T) {
	_, err := NewJWTAuthenticator(JWKSConfig{
		JWKSURL: "http://example.com/keys",
	})
	if err == nil {
		t.Error("expected error when JWKSURL uses http, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected error to mention https, got: %v", err)
	}
}

func TestJWTRejectsFileURL(t *testing.T) {
	_, err := NewJWTAuthenticator(JWKSConfig{
		JWKSURL: "file:///etc/passwd",
	})
	if err == nil {
		t.Error("expected error when JWKSURL uses file scheme, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("expected error to mention https, got: %v", err)
	}
}

func TestJWTKeyRefreshOnUnknownKid(t *testing.T) {
	env := newJWTCTestEnv(t, "https://test-issuer", "nexus-proxy")
	initialRequests := env.requests

	// Create a second key, update the JWKS to include it, then mint a
	// token signed with the new key. The authenticator should detect
	// the unknown kid and refresh.
	secondKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}
	const secondKid = "test-key-2"
	newSet, _ := json.Marshal(jwksSet{
		Keys: []jwk{
			rsaPubToJWK(&env.key.PublicKey, env.kid),
			rsaPubToJWK(&secondKey.PublicKey, secondKid),
		},
	})
	env.keySet = newSet

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    "https://test-issuer",
		Audience:  []string{"nexus-proxy"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
	})
	token.Header["kid"] = secondKid
	signed, err := token.SignedString(secondKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	if err := env.auth.Authenticate(signed); err != nil {
		t.Errorf("token with refreshed kid rejected: %v", err)
	}
	if env.requests <= initialRequests {
		t.Error("expected JWKS refresh request on unknown kid, but no new request was made")
	}
}
