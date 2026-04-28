package tencent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "tencent"

// DriverConfig is the Tencent-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All fields
// are optional; the zero value yields a working virtual-host COS driver
// as long as Region is set on uos.Config.
type DriverConfig struct {
	// AppID, when non-empty, is automatically appended to the bucket
	// name (with a "-" separator) when constructing the BucketURL — this
	// matches the Tencent COS bucket-naming convention <name>-<appid>.
	// Callers MAY instead supply the suffixed name on uos.Config (or per
	// request); when both are present the bucket name is used verbatim
	// and AppID is ignored. See Validate's doc comment for the wire
	// requirement.
	AppID string
	// UseHTTP, when true, builds the BucketURL with the http:// scheme
	// instead of the default https://. Off by default (production
	// callers should keep TLS on).
	UseHTTP bool
}

// Factory returns a uos.Factory for the Tencent COS driver. Drivers
// register themselves at init time (or callers may register manually):
//
//	uos.DefaultRegistry().Register(tencent.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Tencent COS.
type factoryImpl struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory() manually.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("tencent"). Required by
// the uos.Factory interface.
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Region MUST be set; CredentialProvider is required (COS
// rejects anonymous access for the operations the contract suite
// exercises). DriverConfig, when non-nil, must be a *DriverConfig.
//
// # Bucket-name-with-appid quirk
//
// Tencent COS requires every bucket reference to be of the form
// <name>-<appid> (e.g. "examplebucket-1250000000"). Callers MAY:
//
//   - Pass the suffixed name verbatim on every request (Buckets/Objects/...).
//   - OR set DriverConfig.AppID and pass the unsuffixed name; the driver
//     auto-suffixes when constructing the per-bucket BucketURL.
//
// Validate does NOT enforce the suffix because it cannot tell which mode
// the caller intends. The wire layer rejects malformed bucket names
// with InvalidBucketName, mapped to ErrInvalidArgument by error_map.go.
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
			Message:   "Config.Region is required for the tencent driver (e.g. ap-guangzhou)",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the tencent driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *tencent.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// *cos.Client wrapped in a uos.Client. It honors:
//
//   - cfg.Region: required, drives the BucketURL host suffix
//     ("cos.<region>.myqcloud.com").
//   - cfg.Endpoint: optional override; when set, replaces the default
//     "https://<bucket>-<appid>.cos.<region>.myqcloud.com" host. Useful
//     for COS-compatible test endpoints (note: per the driver_test
//     comment, the actual COS HMAC v1 signature is incompatible with
//     MinIO SigV4 — endpoint override is for COS-protocol custom hosts
//     only, not for routing to AWS-protocol services).
//   - cfg.CredentialProvider: AK/SK + optional STS session token.
//   - DriverConfig.AppID / UseHTTP: per-bucket URL composition.
//
// # Internal retry disabled
//
// cos-go-sdk-v5 sets a default RetryOpt.Count = 3 in NewClient; we set
// it to 1 immediately afterwards because pkg/uos.RetryPolicy owns the
// authoritative retry surface (cross-cutting risk #1 in
// docs/provider_roadmap.md — "double-retry storm"). Drivers MUST NOT
// add an extra retry layer here.
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

	// HTTP client wraps cos.AuthorizationTransport, which signs requests
	// with HMAC v1 per COS's signing convention. The underlying transport
	// is http.DefaultTransport — no SDK-internal retryer at the transport
	// layer either.
	httpClient := &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:     akid,
			SecretKey:    secret,
			SessionToken: token,
			Transport:    http.DefaultTransport,
		},
	}

	// The bare client is constructed without per-bucket BaseURL because
	// the bucket name is per-call. ServiceURL covers ListBuckets; per-op
	// BucketURL is set lazily in driverImpl.bucketURL.
	client := cos.NewClient(nil, httpClient)
	// Disable the SDK's internal retryer (default Count=3); pkg/uos
	// owns retry per RetryPolicy. See doc above.
	client.Conf.RetryOpt.Count = 1
	// CRC64 verification adds a per-byte hash of every Put body; leave
	// it ON (matches SDK default) so callers benefit from end-to-end
	// integrity, but document so callers know it is happening.
	// client.Conf.EnableCRC stays at the SDK default (true).

	return &driverImpl{
		cfg:        cfg,
		client:     client,
		httpClient: httpClient,
		region:     cfg.Region,
		appID:      dc.AppID,
		scheme:     pickScheme(cfg.Endpoint, dc.UseHTTP),
		endpoint:   cfg.Endpoint,
	}, nil
}

// extractHMAC unwraps the Credential into the (access key, secret, token)
// triple this driver needs. *credential.EnvHMACCredential is the
// reference HMAC payload shape; the function also accepts the value form
// for caller convenience and returns a clear error for unknown payload
// shapes.
func extractHMAC(c credential.Credential) (akid, secret, token string, err error) {
	if c.Scheme != "" && c.Scheme != credential.AuthHMAC {
		return "", "", "", fmt.Errorf("tencent driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("tencent driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("tencent driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"tencent driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// pickScheme decides whether the per-bucket URL uses http:// or
// https://. An explicit Endpoint takes precedence (its scheme is
// honored); otherwise DriverConfig.UseHTTP toggles HTTP. Default is
// https.
func pickScheme(endpoint string, useHTTP bool) string {
	if endpoint != "" {
		if u, err := url.Parse(endpoint); err == nil && u.Scheme != "" {
			return u.Scheme
		}
	}
	if useHTTP {
		return "http"
	}
	return "https"
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
