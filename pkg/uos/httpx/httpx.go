// Package httpx provides a small adapter from a declarative HTTPConfig
// to a configured *http.Client. It lives under pkg/uos so providers in
// separate Go modules can import it without crossing an internal/ boundary.
//
// Scope: stdlib http transport tuning only. No vendor SDK code, no auth,
// no signing, no retry — those belong in the driver and pkg/uos
// middleware/transfer subsystems respectively.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HTTPConfig bundles transport tuning passed to NewClient. All fields
// are optional; the zero value yields an http.Client with stdlib defaults.
type HTTPConfig struct {
	// Timeout is the overall per-request deadline applied as
	// http.Client.Timeout. Zero leaves the stdlib default (no client-level
	// deadline). Drivers and callers should still pass a context.Context
	// with deadline for fine-grained control.
	Timeout time.Duration
	// Proxy is the proxy resolver. nil falls back to
	// http.ProxyFromEnvironment so HTTP(S)_PROXY env vars work transparently.
	Proxy func(*http.Request) (*url.URL, error)
	// RootCAs is the CA pool used to verify server certificates. nil falls
	// back to the system trust store.
	RootCAs *x509.CertPool
	// InsecureSkipVerify disables TLS certificate verification. This is
	// off by default; setting it true emits a one-line warning via
	// log.Default() at NewClient time per architecture_plan §2.3.
	InsecureSkipVerify bool
	// MaxIdleConns mirrors http.Transport.MaxIdleConns. Zero leaves the
	// stdlib default (100).
	MaxIdleConns int
	// MaxIdleConnsPerHost mirrors http.Transport.MaxIdleConnsPerHost.
	// Zero leaves the stdlib default (2). Driver authors targeting a
	// single endpoint typically want this raised explicitly.
	MaxIdleConnsPerHost int
	// IdleConnTimeout mirrors http.Transport.IdleConnTimeout. Zero leaves
	// the stdlib default (90s).
	IdleConnTimeout time.Duration
}

// NewClient builds an *http.Client honoring cfg. It returns an error only
// if the configuration is internally inconsistent; the current v0.1
// surface accepts any combination of fields, so the error return exists
// to keep the signature forward-compatible (v1.x may grow validations
// like mutual TLS material checks).
func NewClient(cfg HTTPConfig) (*http.Client, error) {
	proxy := cfg.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}

	tlsCfg := &tls.Config{
		RootCAs:            cfg.RootCAs,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicit opt-in, warning emitted below
	}

	if cfg.InsecureSkipVerify {
		// One-line warning; goes to whatever log.Default() points at.
		// Drivers / apps that want structured logging should swap the
		// default logger before constructing the client.
		log.Printf("WARN: uos/httpx: TLS verification disabled (InsecureSkipVerify=true); requests are vulnerable to man-in-the-middle attacks")
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy:                 proxy,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
	}, nil
}
