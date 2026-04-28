package httpx

import (
	"bytes"
	"crypto/x509"
	"log"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNewClient_Defaults(t *testing.T) {
	t.Parallel()
	c, err := NewClient(HTTPConfig{})
	if err != nil {
		t.Fatalf("NewClient zero value failed: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil client")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.Transport)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("expected ForceAttemptHTTP2=true by default")
	}
	if tr.Proxy == nil {
		t.Error("expected default Proxy resolver")
	}
}

func TestNewClient_HonorsTimeout(t *testing.T) {
	t.Parallel()
	want := 7 * time.Second
	c, err := NewClient(HTTPConfig{Timeout: want})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Timeout != want {
		t.Errorf("Timeout = %v, want %v", c.Timeout, want)
	}
}

func TestNewClient_HonorsProxy(t *testing.T) {
	t.Parallel()
	called := false
	proxy := func(*http.Request) (*url.URL, error) {
		called = true
		return nil, nil
	}
	c, err := NewClient(HTTPConfig{Proxy: proxy})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	req, _ := http.NewRequest("GET", "http://x/", nil)
	_, _ = tr.Proxy(req)
	if !called {
		t.Error("custom Proxy resolver was not wired into Transport")
	}
}

func TestNewClient_HonorsRootCAs(t *testing.T) {
	t.Parallel()
	pool := x509.NewCertPool()
	c, err := NewClient(HTTPConfig{RootCAs: pool})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.TLSClientConfig == nil {
		t.Fatal("expected TLSClientConfig to be set")
	}
	if tr.TLSClientConfig.RootCAs != pool {
		t.Error("RootCAs not propagated to TLSClientConfig")
	}
}

func TestNewClient_HonorsConnectionPool(t *testing.T) {
	t.Parallel()
	c, err := NewClient(HTTPConfig{
		MaxIdleConns:        7,
		MaxIdleConnsPerHost: 3,
		IdleConnTimeout:     11 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.MaxIdleConns != 7 {
		t.Errorf("MaxIdleConns = %d, want 7", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 3 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 3", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 11*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 11s", tr.IdleConnTimeout)
	}
}

func TestNewClient_InsecureSkipVerify_EmitsWarning(t *testing.T) {
	t.Parallel()
	// Capture log output by swapping log.Default()'s writer.
	var buf bytes.Buffer
	orig := log.Default().Writer()
	log.Default().SetOutput(&buf)
	defer log.Default().SetOutput(orig)

	c, err := NewClient(HTTPConfig{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify=true not propagated to TLSClientConfig")
	}
	out := buf.String()
	if out == "" {
		t.Fatal("expected warning log output, got nothing")
	}
	for _, want := range []string{"WARN", "InsecureSkipVerify", "TLS verification disabled"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("warning message missing substring %q; got %q", want, out)
		}
	}
}

func TestNewClient_NoWarning_WhenSecure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	orig := log.Default().Writer()
	log.Default().SetOutput(&buf)
	defer log.Default().SetOutput(orig)

	if _, err := NewClient(HTTPConfig{}); err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero log output for secure default, got %q", buf.String())
	}
}
