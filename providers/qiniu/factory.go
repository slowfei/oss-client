// Package qiniu is the native uos.Client driver for Qiniu Cloud Storage (Kodo).
// It targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from
// github.com/qiniu/go-sdk/v7 (storage + auth packages).
//
// # Bucket → Bucket mapping
//
// Qiniu Kodo organises objects under Buckets in a Region (zone). The driver
// maps the unified Bucket concept 1:1 onto a Kodo bucket. The Region (Qiniu
// "zone": z0/z1/z2/na0/as0/...) is captured once in DriverConfig.Region or
// uos.Config.Region.
//
// # Auth shape (AuthCustom)
//
// Qiniu uses a single AccessKey + SecretKey pair to derive **three** distinct
// token families at the wire level:
//
//   - Upload Token (PutPolicy.UploadToken) — bearer token POSTed in the
//     "token" form field of the multipart upload to the upload host.
//   - Download Token (auth.Credentials.PrivateUrl / Sign) — query-string
//     signed URL for private-bucket reads.
//   - Manage Token (auth.Credentials.SignRequest) — signs admin requests
//     to the rs/rsf/api hosts (BucketManager).
//
// All three are derived from the SAME AK/SK; the driver therefore exposes
// AuthCustom with a single Credentials payload (no need for distinct
// credential payload types in v0.1) and lets the SDK pick the right signing
// scope per call site. Documented further in package qiniu/credentials.go's
// type doc comment (Credentials — see below).
//
// # M5 milestone validation
//
// Qiniu is the M5 DirectGrant-non-URL milestone driver. The Upload Token is
// expressed via DirectGrant{Mode: DirectGrantModeToken, ...} — semantically
// the bearer-token shape (the caller carries the opaque token and POSTs it
// to a vendor-specific endpoint). This validates DirectGrantModeToken in a
// NEW context (distinct from M4 azure SAS, which is also Token but encoded
// as a URL query string).
//
// SignedURLWrite returns ErrUnsupported with Reason directing callers to
// IssueDirectGrant per provider_matrix.md footnote 4 (Qiniu's write
// authorization is non-URL).
//
// # Multipart mapping
//
// Qiniu has its own resumable upload protocol (RUv2 / Resumable Upload v2).
// MultipartService maps onto storage.ResumeUploaderV2's InitParts /
// UploadParts / CompleteParts. The driver synthesises an UploadID from the
// SDK's InitPartsRet.UploadID (the vendor handle is opaque to callers).
// Per the RUv2 contract, parts are sequential per block — see Lessons (M5).
//
// # Metadata semantics
//
// Qiniu metadata uses the wire prefix "x-qn-meta-*". The unified
// pkg/uos.Metadata contract requires lower-case keys at the driver boundary;
// we apply s3common.LowerMetadataKeys on both ingress and egress.
package qiniu

import (
	"context"
	"fmt"
	"strings"

	qauth "github.com/qiniu/go-sdk/v7/auth"
	"github.com/qiniu/go-sdk/v7/storage"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
const providerID uos.Provider = "qiniu"

// DriverConfig is the Qiniu-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it.
//
// Region is the Qiniu zone identifier (z0/z1/z2/na0/as0/cn-east-2/...);
// either DriverConfig.Region or uos.Config.Region must be set, or
// Endpoint must be supplied.
type DriverConfig struct {
	// Region is the Qiniu zone id (e.g. "z0", "z1", "z2", "na0", "as0",
	// "cn-east-2"). Optional if uos.Config.Region is set or Endpoint is supplied.
	// Falls back to uos.Config.Region in Factory.Open.
	Region string

	// UseHTTPS forces HTTPS for all wire calls. Default true.
	UseHTTPS bool

	// UseCDNDomains routes downloads through Qiniu's CDN domains.
	UseCDNDomains bool

	// Domain is the bucket's bound CDN/source domain used by SignURL for
	// private-bucket downloads (storage.MakePrivateURL requires it). Empty
	// means SignURL with GET returns ErrUnsupported{CapSignedURLRead,
	// Reason="DriverConfig.Domain is required for SignURL"}.
	Domain string

	// UploadEndpoint overrides the upload host returned in DirectGrant.URL
	// (Mode=Token). Empty derives the upload host from the Region. The
	// returned DirectGrant.URL points the caller at the right upload host
	// for their region.
	UploadEndpoint string

	// RsHost / RsfHost / ApiHost / IoHost / UpHost override the underlying
	// SDK Config endpoints for self-hosted Kodo or non-default zones.
	// Empty defers to the Region's defaults.
	RsHost  string
	RsfHost string
	ApiHost string
	IoHost  string
	UpHost  string
}

// Credentials is the AuthCustom Opaque payload for the qiniu driver. It
// carries the AccessKey + SecretKey pair; the same pair derives the Upload
// Token, the Download Token, and the Manage Token at distinct call sites.
//
// Callers supply it via:
//
//	credential.NewStatic(credential.Credential{
//	    Scheme: credential.AuthCustom,
//	    Opaque: &qiniu.Credentials{AccessKey: "…", SecretKey: "…"},
//	})
type Credentials struct {
	// AccessKey is the Qiniu Access Key (AK).
	AccessKey string
	// SecretKey is the Qiniu Secret Key (SK).
	SecretKey string
}

// Factory returns a uos.Factory for the Qiniu Kodo driver.
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Qiniu Kodo.
type factoryImpl struct{}

// init registers this driver with the process-global Registry.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("qiniu").
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Either Region (uos.Config.Region or DriverConfig.Region)
// OR Endpoint (uos.Config.Endpoint or one of DriverConfig's *Host fields)
// must be set. CredentialProvider is required and must yield AuthCustom
// with a *Credentials Opaque payload.
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
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the qiniu driver",
		}
	}
	dc, _ := cfg.DriverConfig.(*DriverConfig)
	if cfg.DriverConfig != nil && dc == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   fmt.Sprintf("DriverConfig must be *qiniu.DriverConfig, got %T", cfg.DriverConfig),
		}
	}
	region := cfg.Region
	if dc != nil && dc.Region != "" {
		region = dc.Region
	}
	endpointSet := cfg.Endpoint != ""
	if dc != nil {
		endpointSet = endpointSet ||
			dc.RsHost != "" || dc.RsfHost != "" ||
			dc.ApiHost != "" || dc.IoHost != "" || dc.UpHost != ""
	}
	if region == "" && !endpointSet {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message: "qiniu driver requires either Config.Region (Qiniu zone, e.g. \"z0\") " +
				"or Config.Endpoint / DriverConfig.{Rs,Rsf,Api,Io,Up}Host",
		}
	}
	return nil
}

// Open resolves the credential and constructs the underlying SDK clients
// (BucketManager, FormUploader, ResumeUploaderV2). All three share the
// same *auth.Credentials and *storage.Config so the AK/SK pair signs every
// scope (manage, upload, download) without distinct credential payloads.
//
// Per docs/provider_roadmap.md cross-cutting risk "double-retry storm",
// the driver does NOT enable any vendor-internal retry layer; caller-side
// pkg/uos.RetryPolicy is the single source of retry truth. Qiniu's high-
// level wrappers don't expose a retry knob equivalent to AWS/Azure SDKs;
// the underlying http.Client is used at default settings.
func (f factoryImpl) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
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
	if cred.Scheme != credential.AuthCustom && cred.Scheme != "" {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message: fmt.Sprintf(
				"qiniu driver requires AuthCustom credential, got %q", string(cred.Scheme),
			),
		}
	}
	creds, err := extractCredentials(cred)
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
	region := cfg.Region
	if dc.Region != "" {
		region = dc.Region
	}

	sdkCfg := &storage.Config{
		UseHTTPS:      dc.UseHTTPS || cfg.Endpoint == "" && !explicitlyDisabledHTTPS(dc),
		UseCdnDomains: dc.UseCDNDomains,
	}
	// Default to HTTPS unless caller wired an explicit non-https endpoint.
	if !dc.UseHTTPS && (dc.RsHost == "" && dc.RsfHost == "" && dc.ApiHost == "" && dc.IoHost == "" && dc.UpHost == "" && cfg.Endpoint == "") {
		sdkCfg.UseHTTPS = true
	}
	if region != "" {
		if r, ok := storage.GetRegionByID(storage.RegionID(region)); ok {
			sdkCfg.Region = &r
			sdkCfg.Zone = &r // zone alias preserved for older SDK call paths
		} else {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message: fmt.Sprintf(
					"unknown qiniu region %q (allowed: z0, z1, z2, na0, as0, cn-east-2)", region,
				),
			}
		}
	}
	if dc.RsHost != "" {
		sdkCfg.RsHost = dc.RsHost
	}
	if dc.RsfHost != "" {
		sdkCfg.RsfHost = dc.RsfHost
	}
	if dc.ApiHost != "" {
		sdkCfg.ApiHost = dc.ApiHost
	}
	if dc.IoHost != "" {
		sdkCfg.IoHost = dc.IoHost
	}
	if dc.UpHost != "" {
		sdkCfg.UpHost = dc.UpHost
	}

	mac := qauth.New(creds.AccessKey, creds.SecretKey)
	return &driverImpl{
		cfg:            cfg,
		dc:             dc,
		region:         region,
		mac:            mac,
		sdkCfg:         sdkCfg,
		bucketManager:  storage.NewBucketManager(mac, sdkCfg),
		formUploader:   storage.NewFormUploader(sdkCfg),
		resumeUploader: storage.NewResumeUploaderV2(sdkCfg),
		uploadSessions: make(map[string]*uploadSession),
	}, nil
}

// extractCredentials unwraps a credential.Credential carrying a *Credentials
// payload (or a value-typed Credentials), validating both AK and SK are set.
func extractCredentials(c credential.Credential) (*Credentials, error) {
	switch v := c.Opaque.(type) {
	case *Credentials:
		if v == nil || v.AccessKey == "" || v.SecretKey == "" {
			return nil, fmt.Errorf("qiniu driver: Credentials missing AccessKey or SecretKey")
		}
		return v, nil
	case Credentials:
		if v.AccessKey == "" || v.SecretKey == "" {
			return nil, fmt.Errorf("qiniu driver: Credentials missing AccessKey or SecretKey")
		}
		return &v, nil
	default:
		return nil, fmt.Errorf(
			"qiniu driver: AuthCustom requires *qiniu.Credentials opaque payload, got %T",
			c.Opaque,
		)
	}
}

// explicitlyDisabledHTTPS reports whether the caller opted into plain HTTP
// via one of the *Host overrides (treats "http://" prefix as opt-in to HTTP).
func explicitlyDisabledHTTPS(dc *DriverConfig) bool {
	for _, h := range []string{dc.RsHost, dc.RsfHost, dc.ApiHost, dc.IoHost, dc.UpHost} {
		if strings.HasPrefix(h, "http://") {
			return true
		}
	}
	return false
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
