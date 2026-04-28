package minio

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	miniogo "github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
	"github.com/maqian/oss-client/pkg/uos/httpx"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "minio"

// Factory implements pkg/uos.Factory for the MinIO native driver.
//
// It is exported as a value-typed struct so callers can register it
// explicitly in a custom Registry (the package's init() also registers
// it on uos.DefaultRegistry).
type Factory struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory{} manually.
func init() {
	_ = uos.DefaultRegistry().Register(Factory{})
}

// Provider returns the canonical provider id ("minio") this Factory
// handles. Required by the uos.Factory interface.
func (Factory) Provider() uos.Provider { return providerID }

// Validate inspects cfg for structural errors before any I/O. It rejects
// missing Endpoint (MinIO is always self-hosted; there is no vendor
// default) and missing CredentialProvider (MinIO refuses anonymous
// access by default for the operations the contract suite exercises).
//
// Validate MUST NOT perform network I/O (architecture_plan §1.2 / Factory
// contract). Region is allowed to be empty; minio-go derives one from the
// endpoint when needed.
func (Factory) Validate(cfg uos.Config) error {
	if cfg.Provider != "" && cfg.Provider != providerID {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message: fmt.Sprintf(
				"Config.Provider=%q does not match this Factory (%q)",
				string(cfg.Provider), string(providerID),
			),
		}
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.Endpoint is required for the minio driver",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the minio driver",
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// minio-go Client wrapped in a uos.Client. It honors:
//
//   - cfg.Endpoint scheme (http://... → Secure=false, https://... → true)
//   - cfg.HTTP for transport tuning (timeouts, root CAs, proxy, etc.)
//   - cfg.CredentialProvider for AK/SK + optional session token
//
// minio-go's internal retryer is disabled here (Options.MaxRetries=1)
// per architecture_plan / provider_roadmap cross-cutting risk: drivers
// must route every retry through pkg/uos.RetryPolicy to avoid the
// double-retry storm.
func (Factory) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := (Factory{}).Validate(cfg); err != nil {
		return nil, err
	}

	cred, err := cfg.CredentialProvider.Resolve(ctx, string(providerID))
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrUnauthenticated,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "credential provider failed",
			Cause:     err,
		}
	}

	akid, secret, token, err := extractHMAC(cred)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   err.Error(),
			Cause:     err,
		}
	}

	host, secure, err := splitEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   err.Error(),
			Cause:     err,
		}
	}

	httpClient, err := httpx.NewClient(cfg.HTTP)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "constructing HTTP client",
			Cause:     err,
		}
	}

	opts := &miniogo.Options{
		Creds:        miniocreds.NewStaticV4(akid, secret, token),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: miniogo.BucketLookupPath,
		Transport:    transportFromClient(httpClient),
		// MaxRetries=1 means "do not retry"; pkg/uos.RetryPolicy is the
		// authoritative retry layer. minio-go documents that 1 disables
		// retries (anything else is N retry attempts).
		MaxRetries: 1,
	}

	client, err := miniogo.New(host, opts)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "minio.New",
			Cause:     err,
		}
	}

	core := &miniogo.Core{Client: client}
	return &driverImpl{
		cfg:    cfg,
		client: client,
		core:   core,
	}, nil
}

// extractHMAC unwraps the Credential into the (access key, secret, token)
// triple this driver needs. *credential.EnvHMACCredential is the
// reference HMAC payload shape; the function also accepts the value
// form for caller convenience and returns a clear error for unknown
// payload shapes.
func extractHMAC(c credential.Credential) (akid, secret, token string, err error) {
	if c.Scheme != "" && c.Scheme != credential.AuthHMAC {
		return "", "", "", fmt.Errorf("minio driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("minio driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("minio driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"minio driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// splitEndpoint pulls the scheme off the endpoint and returns
// (host[:port], secure). minio-go expects the host[:port] form without a
// scheme; the scheme bit drives Options.Secure.
//
// A missing scheme is treated as plaintext http (the common
// testcontainers MinIO setup). Trailing paths are stripped because
// minio-go would treat them as part of the bucket lookup.
func splitEndpoint(endpoint string) (host string, secure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("empty endpoint")
	}
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		secure = true
		endpoint = strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		secure = false
		endpoint = strings.TrimPrefix(endpoint, "http://")
	default:
		// No scheme: assume plaintext. Use url.Parse to reject malformed
		// hosts early.
		if u, perr := url.Parse("http://" + endpoint); perr != nil || u.Host == "" {
			return "", false, fmt.Errorf("invalid endpoint %q", endpoint)
		}
	}
	if i := strings.IndexByte(endpoint, '/'); i >= 0 {
		endpoint = endpoint[:i]
	}
	return endpoint, secure, nil
}

// transportFromClient extracts the underlying RoundTripper from a
// stdlib *http.Client so we can hand it to minio-go's Options.Transport.
// minio-go takes a RoundTripper and wraps its own *http.Client around it,
// so the per-request Timeout from cfg.HTTP would otherwise be lost; that
// trade-off is acceptable here because callers are expected to pass a
// context.Context with deadline for fine-grained control (the same
// guidance httpx.HTTPConfig already documents).
func transportFromClient(c *http.Client) http.RoundTripper {
	if c == nil || c.Transport == nil {
		return http.DefaultTransport
	}
	return c.Transport
}
