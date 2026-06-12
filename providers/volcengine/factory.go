package volcengine

import (
	"context"
	"fmt"
	"strings"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "volcengine"

// DriverConfig is the Volcengine-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All fields
// are optional; the zero value yields a working virtual-host TOS driver
// as long as Region (or Endpoint) is set on uos.Config.
type DriverConfig struct {
	// UseCustomDomain, when true, treats Config.Endpoint as a custom
	// CNAME pointing at TOS (e.g. cdn-cname.example.com). Off by
	// default; the standard "tos-<region>.volces.com" host family uses
	// virtual-host addressing automatically.
	UseCustomDomain bool
	// PathAccessMode forces path-style addressing (bucket in URL path
	// rather than virtual-host subdomain). Off by default; the TOS host
	// family supports virtual-host addressing.
	PathAccessMode bool
	// DisableSSLVerify allows insecure TLS endpoints. Mirrors the vendor
	// SDK's WithEnableVerifySSL(false) ClientOption; off by default.
	DisableSSLVerify bool
	// MaxRetryCount overrides the SDK's internal retry cap. Defaults to 0
	// (retry disabled) so pkg/uos.RetryPolicy is the single source of
	// retry truth — see docs/provider_roadmap.md cross-cutting risk
	// "double-retry storm". Drivers that want vendor-level retries can
	// raise this knob explicitly per call site.
	MaxRetryCount int
}

// Factory returns a uos.Factory for the Volcengine TOS driver. Drivers
// register themselves at init time (or callers may register manually):
//
//	uos.DefaultRegistry().Register(volcengine.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Volcengine TOS.
type factoryImpl struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory() manually.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("volcengine"). Required
// by the uos.Factory interface.
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Region MUST be set (the Endpoint is auto-derived as
// "https://tos-<region>.volces.com" when absent), and CredentialProvider
// MUST be non-nil (TOS rejects anonymous access for the operations the
// contract suite exercises). DriverConfig, when non-nil, must be a
// *DriverConfig.
func (factoryImpl) Validate(cfg uos.Config) error {
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
	if strings.TrimSpace(cfg.Region) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.Region is required for the volcengine driver (used both for SigV4-style signing and to derive the default endpoint)",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the volcengine driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *volcengine.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// *tos.ClientV2 wrapped in a uos.Client. It honors:
//
//   - cfg.Endpoint as the TOS endpoint URL (e.g. "https://tos-cn-beijing.volces.com");
//     when empty, derived from cfg.Region as "https://tos-<region>.volces.com".
//   - cfg.CredentialProvider for AK/SK + optional STS session token.
//   - DriverConfig.UseCustomDomain / PathAccessMode / DisableSSLVerify.
//
// The TOS SDK ships with an internal retryer (StatusCodeClassifier +
// ServerErrorClassifier — see tos/error.go). pkg/uos.RetryPolicy is the
// authoritative retry surface in this SDK, so we set MaxRetryCount=0 by
// default to avoid double-retry storms (per docs/provider_roadmap.md
// cross-cutting risk #1). DriverConfig.MaxRetryCount lets callers
// re-enable vendor-level retries if they need them.
func (f factoryImpl) Open(_ context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}

	cred, err := cfg.CredentialProvider.Resolve(context.Background(), string(providerID))
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

	dc, _ := cfg.DriverConfig.(*DriverConfig)
	if dc == nil {
		dc = &DriverConfig{}
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		// Standard TOS endpoint convention: tos-<region>.volces.com (note
		// the literal "tos-" prefix, distinct from AWS s3-<region> or
		// alibaba oss-<region> — concatenating the wrong prefix produces
		// SignatureDoesNotMatch on every request).
		endpoint = fmt.Sprintf("https://tos-%s.volces.com", cfg.Region)
	}

	creds := tos.NewStaticCredentials(akid, secret)
	if token != "" {
		creds.WithSecurityToken(token)
	}

	clientOpts := []tos.ClientOption{
		tos.WithRegion(cfg.Region),
		tos.WithCredentials(creds),
		// Disable the SDK's internal retryer by default. pkg/uos.RetryPolicy
		// is the single source of retry truth (cross-cutting risk #1).
		tos.WithMaxRetryCount(dc.MaxRetryCount),
	}
	if dc.UseCustomDomain {
		clientOpts = append(clientOpts, tos.WithCustomDomain(true))
	}
	if dc.PathAccessMode {
		clientOpts = append(clientOpts, tos.WithPathAccessMode(true))
	}
	if dc.DisableSSLVerify {
		clientOpts = append(clientOpts, tos.WithEnableVerifySSL(false))
	}

	client, err := tos.NewClientV2(endpoint, clientOpts...)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "tos.NewClientV2",
			Cause:     err,
		}
	}

	return &driverImpl{
		cfg:    cfg,
		client: client,
	}, nil
}

// extractHMAC unwraps the Credential into the (access key, secret, token)
// triple this driver needs. *credential.EnvHMACCredential is the
// reference HMAC payload shape; the function also accepts the value form
// for caller convenience and returns a clear error for unknown payload
// shapes.
func extractHMAC(c credential.Credential) (akid, secret, token string, err error) {
	if c.Scheme != "" && c.Scheme != credential.AuthHMAC {
		return "", "", "", fmt.Errorf("volcengine driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("volcengine driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("volcengine driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"volcengine driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
