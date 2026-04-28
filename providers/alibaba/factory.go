package alibaba

import (
	"context"
	"fmt"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "alibaba"

// DriverConfig is the Alibaba-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All fields
// are optional; the zero value yields a working virtual-host OSS driver
// as long as Region or Endpoint is set on uos.Config.
type DriverConfig struct {
	// UseCNAME, when true, treats Config.Endpoint as a custom CNAME
	// pointing at OSS (e.g. cdn-cname.example.com). Off by default;
	// virtual-host addressing is the default for the standard OSS hosts.
	UseCNAME bool
	// PathStyle forces path-style addressing (bucket in URL path rather
	// than virtual-host subdomain). Implicitly enabled when
	// uos.Config.Endpoint is non-empty AND not a CNAME (S3-compat /
	// MinIO targets need it).
	PathStyle bool
	// AuthVersion selects the OSS signing algorithm. Empty defaults to
	// SDK default (v1). Use "v4" for SigV4-style signing in newer
	// regions. Mapped to oss.AuthV1 / oss.AuthV2 / oss.AuthV4.
	AuthVersion string
	// DisableSSLVerify allows insecure TLS endpoints. Mirrors the
	// vendor SDK's InsecureSkipVerify ClientOption; off by default.
	DisableSSLVerify bool
}

// Factory returns a uos.Factory for the Alibaba OSS driver. Drivers
// register themselves at init time (or callers may register manually):
//
//	uos.DefaultRegistry().Register(alibaba.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Alibaba OSS.
type factoryImpl struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory() manually.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("alibaba"). Required by
// the uos.Factory interface.
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Either Region or Endpoint MUST be set; CredentialProvider
// is required (OSS rejects anonymous access for the operations the
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
	if strings.TrimSpace(cfg.Region) == "" && strings.TrimSpace(cfg.Endpoint) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.Region or Config.Endpoint is required for the alibaba driver",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the alibaba driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *alibaba.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// *oss.Client wrapped in a uos.Client. It honors:
//
//   - cfg.Endpoint as the OSS endpoint URL (e.g. "https://oss-cn-hangzhou.aliyuncs.com");
//     when empty, derived from cfg.Region as "https://oss-<region>.aliyuncs.com".
//   - cfg.CredentialProvider for AK/SK + optional STS session token.
//   - DriverConfig.UseCNAME / PathStyle / AuthVersion / DisableSSLVerify.
//
// The aliyun-oss-go-sdk does NOT expose a built-in retryer to disable;
// transient retry is the caller's responsibility (the SDK retries some
// network errors at the http.Client transport layer only). pkg/uos.RetryPolicy
// remains the authoritative retry surface — drivers MUST NOT add an
// extra retry layer here (per docs/provider_roadmap.md cross-cutting
// risk: "double-retry").
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
		endpoint = fmt.Sprintf("https://oss-%s.aliyuncs.com", cfg.Region)
	}

	clientOpts := []oss.ClientOption{}
	if token != "" {
		clientOpts = append(clientOpts, oss.SecurityToken(token))
	}
	if cfg.Region != "" {
		clientOpts = append(clientOpts, oss.Region(cfg.Region))
	}
	if dc.UseCNAME {
		clientOpts = append(clientOpts, oss.UseCname(true))
	} else if dc.PathStyle || isCustomEndpoint(endpoint) {
		// Path-style addressing is required for S3-compat / MinIO
		// targets and any non-standard OSS endpoint where the bucket
		// cannot be safely promoted to a virtual-host subdomain.
		clientOpts = append(clientOpts, oss.ForcePathStyle(true))
	}
	switch strings.ToLower(dc.AuthVersion) {
	case "v1", "":
		// SDK default; no option needed.
	case "v2":
		clientOpts = append(clientOpts, oss.AuthVersion(oss.AuthV2))
	case "v4":
		clientOpts = append(clientOpts, oss.AuthVersion(oss.AuthV4))
	default:
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("DriverConfig.AuthVersion=%q is invalid (allowed: v1, v2, v4)", dc.AuthVersion),
		}
	}
	if dc.DisableSSLVerify {
		clientOpts = append(clientOpts, oss.InsecureSkipVerify(true))
	}

	client, err := oss.New(endpoint, akid, secret, clientOpts...)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "oss.New",
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
		return "", "", "", fmt.Errorf("alibaba driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("alibaba driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("alibaba driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"alibaba driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// isCustomEndpoint reports whether endpoint targets something other than
// the standard "*.aliyuncs.com" OSS host family (e.g. MinIO, a private
// OSS-compat service). Custom endpoints default to path-style addressing
// because virtual-host promotion of the bucket name into the hostname is
// only safe for the official OSS host suffix.
func isCustomEndpoint(endpoint string) bool {
	lower := strings.ToLower(endpoint)
	return !strings.Contains(lower, ".aliyuncs.com")
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
