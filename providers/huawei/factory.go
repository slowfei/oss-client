package huawei

import (
	"context"
	"fmt"
	"strings"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "huawei"

// DriverConfig is the Huawei-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All fields
// are optional; the zero value yields a working virtual-host OBS driver
// as long as Endpoint is set on uos.Config.
type DriverConfig struct {
	// UseCNAME, when true, treats Config.Endpoint as a custom CNAME
	// pointing at OBS (e.g. cdn-cname.example.com). Off by default;
	// virtual-host addressing is the default for the standard OBS hosts.
	UseCNAME bool
	// PathStyle forces path-style addressing (bucket in URL path rather
	// than virtual-host subdomain). Implicitly enabled by the SDK when
	// the endpoint host is an IP address; set explicitly for S3-compat
	// targets that cannot use virtual-host.
	PathStyle bool
	// Signature selects the OBS signing algorithm. Empty defaults to the
	// SDK default ("v2"). Allowed: "v2", "v4", "obs". v4 is required by
	// some newer regions (e.g. ap-southeast-2); "obs" is the OBS-native
	// signature that some regions accept.
	Signature string
	// DisableSSLVerify allows insecure TLS endpoints. Mirrors the
	// vendor SDK's WithSslVerify(false) configurer; off by default.
	DisableSSLVerify bool
}

// Factory returns a uos.Factory for the Huawei OBS driver. Drivers
// register themselves at init time (or callers may register manually):
//
//	uos.DefaultRegistry().Register(huawei.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Huawei OBS.
type factoryImpl struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory() manually.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("huawei"). Required by
// the uos.Factory interface.
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Endpoint MUST be set; CredentialProvider is required
// (OBS rejects anonymous access for the operations the contract suite
// exercises). DriverConfig, when non-nil, must be a *DriverConfig.
//
// # Endpoint pairing strictness
//
// Unlike sibling drivers (alibaba/tencent) that accept Region as a
// fallback and derive a default endpoint, the huawei driver REQUIRES
// Endpoint. Region/endpoint pairing is strict on Huawei OBS: a wrong
// pairing yields silent HTTP 301 / 307 redirects rather than a clean
// ErrInvalidArgument (the SDK follows redirects, the caller observes a
// downstream signature failure). Making Endpoint mandatory at Validate
// time forces the misconfiguration to surface here, where the error
// message names the field instead of leaking out as an opaque auth
// failure. See docs/provider_roadmap.md M3 cross-cutting risk.
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
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.Endpoint is required for the huawei driver (region/endpoint pairing is strict on OBS — wrong pairing produces silent 301/307 redirects; set the explicit OBS host for your region, e.g. https://obs.cn-north-4.myhuaweicloud.com)",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the huawei driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *huawei.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// *obs.ObsClient wrapped in a uos.Client. It honors:
//
//   - cfg.Endpoint as the OBS endpoint URL (e.g. "https://obs.cn-north-4.myhuaweicloud.com").
//     The driver does NOT auto-derive Endpoint from cfg.Region — see
//     Validate's docstring for why.
//   - cfg.Region forwarded to the SDK as a hint for v4-style signing.
//   - cfg.CredentialProvider for AK/SK + optional STS session token.
//   - DriverConfig.UseCNAME / PathStyle / Signature / DisableSSLVerify.
//
// The huaweicloud-sdk-go-obs's internal retryer is explicitly disabled
// via obs.WithMaxRetryCount(0). pkg/uos.RetryPolicy is the authoritative
// retry surface — drivers MUST NOT add an extra retry layer here (per
// docs/provider_roadmap.md cross-cutting risk: "double-retry"). The SDK
// already defaults maxRetryCount to -1 internally; the explicit call
// guards against future SDK default changes.
//
// The obs.With* configurers are passed inline because the SDK's
// `configurer` type is unexported (see obs/conf.go) and cannot be named
// from outside the SDK package — we cannot pre-build a typed slice and
// forward via opts....
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

	// Validate Signature up-front so we don't hand off to obs.New on a
	// bad value. The SDK silently defaults unknown strings, which would
	// mask the misconfiguration.
	signature := strings.ToLower(dc.Signature)
	switch signature {
	case "", "v2", "v4", "obs":
		// allowed
	default:
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("DriverConfig.Signature=%q is invalid (allowed: v2, v4, obs)", dc.Signature),
		}
	}

	client, err := openOBSClient(akid, secret, cfg.Endpoint, cfg.Region, token, signature, dc)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "obs.New",
			Cause:     err,
		}
	}

	return &driverImpl{
		cfg:    cfg,
		client: client,
	}, nil
}

// openOBSClient invokes obs.New with the configurers that match the
// supplied driver config. The configurer list is assembled inline at
// the obs.New call site because the SDK's `configurer` type is
// unexported (see obs/conf.go) and cannot be named from outside the
// SDK package.
//
// The configurers fall into two groups:
//
//   - Always-on / safe-with-zero-value: WithMaxRetryCount(0) [the
//     cross-cutting-risk-#1 retry-disable], WithRegion (SDK falls back
//     to default on empty), WithSecurityToken (no-op on empty),
//     WithCustomDomainName (false matches default), WithPathStyle
//     (false matches default).
//   - Conditionally-on: WithSignature (skipped for v2 default),
//     WithSslVerify (only emitted when DisableSSLVerify=true so the
//     secure default stays in effect for the common path).
//
// The dispatch ladder unrolls the (signature × DisableSSLVerify)
// product so each branch enumerates the configurer list inline. The
// product is small (3 × 2 = 6) and every branch is short enough to
// stay readable.
func openOBSClient(ak, sk, endpoint, region, token, signature string, dc *DriverConfig) (*obs.ObsClient, error) {
	switch signature {
	case "v4":
		if dc.DisableSSLVerify {
			return obs.New(ak, sk, endpoint,
				obs.WithMaxRetryCount(0),
				obs.WithRegion(region),
				obs.WithSecurityToken(token),
				obs.WithCustomDomainName(dc.UseCNAME),
				obs.WithPathStyle(dc.PathStyle),
				obs.WithSignature(obs.SignatureV4),
				obs.WithSslVerify(false),
			)
		}
		return obs.New(ak, sk, endpoint,
			obs.WithMaxRetryCount(0),
			obs.WithRegion(region),
			obs.WithSecurityToken(token),
			obs.WithCustomDomainName(dc.UseCNAME),
			obs.WithPathStyle(dc.PathStyle),
			obs.WithSignature(obs.SignatureV4),
		)
	case "obs":
		if dc.DisableSSLVerify {
			return obs.New(ak, sk, endpoint,
				obs.WithMaxRetryCount(0),
				obs.WithRegion(region),
				obs.WithSecurityToken(token),
				obs.WithCustomDomainName(dc.UseCNAME),
				obs.WithPathStyle(dc.PathStyle),
				obs.WithSignature(obs.SignatureObs),
				obs.WithSslVerify(false),
			)
		}
		return obs.New(ak, sk, endpoint,
			obs.WithMaxRetryCount(0),
			obs.WithRegion(region),
			obs.WithSecurityToken(token),
			obs.WithCustomDomainName(dc.UseCNAME),
			obs.WithPathStyle(dc.PathStyle),
			obs.WithSignature(obs.SignatureObs),
		)
	default: // "" or "v2" — SDK default, no WithSignature configurer.
		if dc.DisableSSLVerify {
			return obs.New(ak, sk, endpoint,
				obs.WithMaxRetryCount(0),
				obs.WithRegion(region),
				obs.WithSecurityToken(token),
				obs.WithCustomDomainName(dc.UseCNAME),
				obs.WithPathStyle(dc.PathStyle),
				obs.WithSslVerify(false),
			)
		}
		return obs.New(ak, sk, endpoint,
			obs.WithMaxRetryCount(0),
			obs.WithRegion(region),
			obs.WithSecurityToken(token),
			obs.WithCustomDomainName(dc.UseCNAME),
			obs.WithPathStyle(dc.PathStyle),
		)
	}
}

// extractHMAC unwraps the Credential into the (access key, secret, token)
// triple this driver needs. *credential.EnvHMACCredential is the
// reference HMAC payload shape; the function also accepts the value form
// for caller convenience and returns a clear error for unknown payload
// shapes.
func extractHMAC(c credential.Credential) (akid, secret, token string, err error) {
	if c.Scheme != "" && c.Scheme != credential.AuthHMAC {
		return "", "", "", fmt.Errorf("huawei driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("huawei driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("huawei driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"huawei driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
