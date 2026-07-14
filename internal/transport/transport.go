// Package transport provides a shared, pre-configured *http.Client for all
// outbound upstream calls (Ollama, frontier API, arbiter synthesis, etc.).
//
// Connection pooling reduces latency and TCP handshake overhead across all
// proxy → upstream traffic. The single client is constructed once at boot
// and passed to every collaborator that needs an HTTP round-trip.
//
// Tuning knobs are exposed via NEXUS_HTTP_* env vars so operators can
// adjust connection-pool behaviour without recompiling.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Config holds the transport-level knobs. Zero values are filled with
// safe defaults by New.
type Config struct {
	// MaxIdleConnsPerHost is the maximum number of idle connections
	// maintained per upstream host. DefaultDefaultMaxIdleConnsPerHost.
	// A host is a "host:port" pair, so a proxy hitting the same
	// Ollama instance on the same port always reuses a warm connection.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost is the maximum total number of connections per
	// upstream host. 0 (the default) means unlimited.
	MaxConnsPerHost int

	// IdleConnTimeout is the maximum time an idle connection sits
	// idle before being closed. DefaultDefaultIdleConnTimeout.
	IdleConnTimeout time.Duration

	// DialContextTimeout is the maximum time a DialContext call
	// may take. DefaultDefaultDialContextTimeout.
	DialContextTimeout time.Duration

	// ClientCertFile is the path to the client certificate file (PEM).
	// When set alongside ClientKeyFile, the transport presents this
	// certificate for mTLS. Empty disables client-cert auth.
	ClientCertFile string

	// ClientKeyFile is the path to the client private key file (PEM).
	// Required when ClientCertFile is set.
	ClientKeyFile string

	// CAFile is the path to the CA certificate file (PEM) used to
	// verify the server certificate. When set, the server certificate
	// must be signed by this CA. Empty uses the system pool.
	CAFile string
}

// New returns a shared, pre-configured *http.Client. The client holds
// a persistent connection pool tuned for the proxy's workload: short,
// bursty requests to local (Ollama) and remote (frontier) upstreams.
//
// The returned client MUST be passed to collaborators at construction
// time; it must never be mutated after construction. The client's
// Transport is configured with a custom DialContext so timeouts are
// wired correctly even when callers pass a background context.
//
// When ClientCertFile and ClientKeyFile are set, New builds a *tls.Config
// with GetClientCertificate so the transport can present a client
// certificate for mTLS environments (corporate proxies, service meshes).
//
// Tuning knobs are read from environment variables prefixed NEXUS_HTTP_;
// see https://github.com/anchapin/nexus-proxy/issues/184.
func New(cfg Config) *http.Client {
	cfg.applyDefaults()

	dialer := &net.Dialer{
		Timeout:   cfg.DialContextTimeout,
		KeepAlive: cfg.IdleConnTimeout,
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          0, // unlimited; governed by MaxIdleConnsPerHost
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if cfg.hasClientCert() {
		tlsConfig, err := buildTLSConfig(cfg)
		if err != nil {
			// Fall back to no TLS config rather than panicking;
			// callers will get a connection error at request time.
			transport.TLSClientConfig = nil
		} else {
			transport.TLSClientConfig = tlsConfig
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   0, // no per-request timeout; callers use context deadlines
	}
}

// NewFromEnv reads NEXUS_HTTP_* knobs from the environment and returns
// a configured *http.Client. It is a convenience wrapper around New
// for packages that need a client without taking a full Config.
func NewFromEnv() *http.Client {
	return New(loadConfigFromEnv())
}

// hasClientCert reports whether ClientCertFile and ClientKeyFile are both set.
func (c *Config) hasClientCert() bool {
	return c.ClientCertFile != "" && c.ClientKeyFile != ""
}

// buildTLSConfig constructs a *tls.Config from the certificate paths in cfg.
// It sets GetClientCertificate so the transport can present a client cert
// for mTLS. If CAFile is set, the server certificate is verified against it.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
	}

	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, err
		}
		tlsConfig.RootCAs = caPool
	}

	return tlsConfig, nil
}

func (c *Config) applyDefaults() {
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if c.IdleConnTimeout <= 0 {
		c.IdleConnTimeout = DefaultIdleConnTimeout
	}
	if c.DialContextTimeout <= 0 {
		c.DialContextTimeout = DefaultDialContextTimeout
	}
}

func loadConfigFromEnv() Config {
	return Config{
		MaxIdleConnsPerHost: parseEnvInt("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", DefaultMaxIdleConnsPerHost),
		MaxConnsPerHost:     parseEnvInt("NEXUS_HTTP_MAX_CONNS_PER_HOST", 0),
		IdleConnTimeout:     parseEnvDuration("NEXUS_HTTP_IDLE_CONN_TIMEOUT", DefaultIdleConnTimeout),
		DialContextTimeout:  parseEnvDuration("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT", DefaultDialContextTimeout),
		ClientCertFile:      os.Getenv("NEXUS_HTTP_CLIENT_CERT_FILE"),
		ClientKeyFile:       os.Getenv("NEXUS_HTTP_CLIENT_KEY_FILE"),
		CAFile:              os.Getenv("NEXUS_HTTP_CA_FILE"),
	}
}

func parseEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func parseEnvDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Default values for Config knobs.
const (
	DefaultMaxIdleConnsPerHost = 100
	DefaultIdleConnTimeout     = 90 * time.Second
	DefaultDialContextTimeout  = 30 * time.Second
)
