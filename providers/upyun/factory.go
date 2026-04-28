// Package upyun is the native uos.Client driver for Upyun (Up-Cloud) USS
// (Universal Storage Service). It targets the v0.1 frozen pkg/uos surface
// (architecture_plan §1) and implements every method on uos.Client by
// translating to/from github.com/upyun/go-sdk/v3/upyun.
//
// # SDK choice — v3 vs REST direct
//
// Implemented against github.com/upyun/go-sdk/v3@v3.0.4 (the upstream's
// current major). Rationale:
//
//   - The v3 SDK is the upstream-maintained module path (the v2 tag
//     "github.com/upyun/go-sdk@v2.1.0+incompatible" is the legacy
//     non-modules path; v3 is the canonical Go-modules entry point).
//   - The transitive dependency footprint is small (stdlib only).
//   - The upyun.UpYun client wraps the REST + FORM + Multipart endpoints
//     uniformly, so the driver does not need a hand-rolled HTTP client
//     for every code path; the FORM-authorization helpers
//     (MakeUnifiedAuth, FormUploadConfig.Format) are the load-bearing
//     primitives the M5 DirectGrantModeForm validation moment relies on.
//
// REST direct was considered and rejected: re-implementing the unified
// signature scheme (UnifiedAuth) and the Multipart-Stage header dance
// would duplicate ~200 LoC the SDK already ships and verifies in
// upstream tests; the maintenance gain does not justify the cost.
//
// # Bucket → Service mapping
//
// Upyun's storage namespace is the "service" (also called "bucket" in
// upstream docs). The driver maps the unified Bucket concept 1:1 onto
// Upyun services. Services are PROVISIONED via the Upyun web portal
// (https://console.upyun.com/) — there is no programmatic create-service
// API in v0.1. BucketService.Create therefore returns ErrUnsupported with
// a reason pointing at the portal; BucketService.Stat returns the
// configured service via Usage(); BucketService.List returns the
// single configured service. BucketService.Delete also returns
// ErrUnsupported.
//
// # Auth shapes
//
// Two auth shapes are supported via CredentialProvider:
//
//   - AuthCustom (default, RECOMMENDED) — Operator + Password (or signature
//     key); the driver uses Upyun's Unified-Authorization signature
//     mechanism (HMAC-SHA1) for every REST + FORM call. Required for
//     Signer.IssueDirectGrant (the M5 DirectGrantModeForm validation
//     moment).
//   - AuthSharedKey (fallback) — Operator + Password as basic auth on the
//     legacy /api endpoint; DISCOURAGED for production due to
//     rate-limiting and weaker security. Documented for compatibility
//     with environments that haven't migrated off the deprecated API.
//
// Both schemes resolve to the same upyun.UpYunConfig.Operator +
// .Password; the SDK applies a per-call MD5 to Password before signing.
//
// # Multipart mapping
//
// Upyun's resumable upload uses REST PUT with X-Upyun-Multi-* headers
// (Initiate / Upload / Complete stages). The driver maps MultipartService
// onto these primitives:
//
//   - Initiate calls upyun.UpYun.InitMultipartUpload, returning the SDK's
//     UploadID (the X-Upyun-Multi-Uuid header). PartSize is captured for
//     UploadPart's convenience but the wire requires only the SDK to
//     stamp X-Upyun-Multi-Part-Size at Initiate time.
//   - UploadPart calls upyun.UpYun.UploadPart with the part body and
//     PartID. The default Upyun part size is 1 MiB (DefaultPartSize) and
//     part sizes MUST be a multiple of 1 MiB; smaller / non-aligned
//     parts return ErrInvalidArgument.
//   - Complete calls upyun.UpYun.CompleteMultipartUpload to finalise the
//     upload. The optional Md5 (whole-object) check is opt-in via
//     CompleteMultipartUploadConfig.
//   - Abort discards the in-process session record; the Upyun upstream
//     auto-expires uncommitted multi-part state after ~24h.
//   - List calls upyun.UpYun.ListMultipartUploads with the Prefix /
//     Limit headers; cross-process orphan listing IS supported by the
//     vendor here (unlike GCS / Azure where MultipartService.List is
//     in-process only).
//
// # Metadata semantics
//
// Upyun stores user-defined metadata under the x-upyun-meta-* header
// prefix (mirroring the S3 x-amz-meta-* convention). The driver
// lower-cases keys at the boundary via s3common.LowerMetadataKeys so
// round-trip equality is well-defined; the wire prefix is added/stripped
// by the driver, callers pass plain key names.
//
// # Out-of-scope (v1)
//
// Upyun's media-processing pipeline (image / audio / video transformers
// reachable via the /upx_pretreatments endpoint) is explicitly NOT
// surfaced via pkg/uos in v1. Callers needing those features must reach
// the SDK directly via Client.As(target **upyun.UpYun) and consult the
// upstream upyun-go-sdk documentation. See provider_matrix.md footnote 7.
package upyun

import (
	"context"
	"fmt"
	"strings"

	upyunsdk "github.com/upyun/go-sdk/v3/upyun"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
const providerID uos.Provider = "upyun"

// DriverConfig is the Upyun-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it.
//
// Bucket is the mandatory Upyun service name (provisioned via the
// Upyun web console). Region is NOT required: Upyun's REST API is
// global at v0.api.upyun.com and the service name encodes the
// per-region storage class indirectly.
type DriverConfig struct {
	// Bucket is the Upyun service name (REQUIRED). Mirrors the unified
	// Bucket concept 1:1; configured via the Upyun console.
	Bucket string

	// Hosts overrides the per-host endpoint mapping consumed by the
	// upyun SDK (upyun.UpYunConfig.Hosts). Keys are upstream-defined
	// canonical hosts (e.g. "v0.api.upyun.com", "host" for the catch-all);
	// values are the override URLs (without scheme). Empty leaves the
	// SDK on its default global endpoints.
	Hosts map[string]string

	// UseHTTP, when true, sends requests over plain HTTP. Defaults to
	// HTTPS (Upyun's recommended transport). Use only for tests.
	UseHTTP bool

	// UserAgent overrides the default SDK User-Agent header. Empty uses
	// the SDK's "UPYUN Go SDK V2/<sdk-version>" default.
	UserAgent string
}

// OperatorCredential is the concrete Opaque payload for both AuthCustom
// and AuthSharedKey. Callers supply it via:
//
//	credential.NewStatic(credential.Credential{
//	    Scheme: credential.AuthCustom,
//	    Opaque: &upyun.OperatorCredential{
//	        Operator: "your-operator",
//	        Password: "your-operator-password",
//	    },
//	})
//
// The Operator field corresponds to the Upyun "operator" name (a
// service-scoped login configured in the Upyun console); the Password
// field is the operator's password (NOT the account password). The SDK
// applies MD5(Password) before signing — callers MUST NOT pre-hash.
type OperatorCredential struct {
	// Operator is the service-scoped operator name configured in the
	// Upyun console.
	Operator string
	// Password is the operator's plaintext password. The SDK MD5s it
	// before signing; callers MUST NOT pre-hash.
	Password string
}

// Factory returns a uos.Factory for the Upyun USS driver.
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Upyun USS.
type factoryImpl struct{}

// init registers this driver with the process-global Registry.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("upyun").
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. DriverConfig.Bucket is required (Upyun has no analog of
// AWS region — the service name encodes the storage location).
// CredentialProvider is required. DriverConfig must be a *DriverConfig.
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
			Message:   "Config.CredentialProvider is required for the upyun driver",
		}
	}
	if cfg.DriverConfig == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.DriverConfig (*upyun.DriverConfig) is required for the upyun driver",
		}
	}
	dc, ok := cfg.DriverConfig.(*DriverConfig)
	if !ok {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   fmt.Sprintf("DriverConfig must be *upyun.DriverConfig, got %T", cfg.DriverConfig),
		}
	}
	if strings.TrimSpace(dc.Bucket) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "DriverConfig.Bucket (Upyun service name) is required for the upyun driver",
		}
	}
	return nil
}

// Open resolves the credential and constructs the upyun.UpYun client.
// Two auth schemes are supported:
//
//   - AuthCustom — default, signature-based unified auth (RECOMMENDED).
//   - AuthSharedKey — fallback basic-auth on the deprecated REST API
//     (DISCOURAGED for production; documented for compatibility).
//
// The Upyun SDK does NOT ship an internal retryer (only a per-list-call
// retry inside ListObjects), so there is no double-retry surface to
// disable here — pkg/uos.RetryPolicy stays the sole retry orchestrator.
func (f factoryImpl) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}
	dc := cfg.DriverConfig.(*DriverConfig)

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

	op, err := extractOperator(cred)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   err.Error(),
			Cause:     err,
		}
	}

	switch cred.Scheme {
	case credential.AuthCustom, credential.AuthSharedKey:
		// both supported; AuthSharedKey routes through the deprecated path
		// (UseDeprecatedApi) so the legacy REST signature is used.
	default:
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("upyun driver requires AuthCustom (recommended) or AuthSharedKey (fallback), got %q", string(cred.Scheme)),
		}
	}

	sdkConfig := &upyunsdk.UpYunConfig{
		Bucket:    dc.Bucket,
		Operator:  op.Operator,
		Password:  op.Password,
		Hosts:     dc.Hosts,
		UseHTTP:   dc.UseHTTP,
		UserAgent: dc.UserAgent,
	}
	client := upyunsdk.NewUpYun(sdkConfig)
	if cred.Scheme == credential.AuthSharedKey {
		client.UseDeprecatedApi()
	}

	return &driverImpl{
		cfg:        cfg,
		dc:         dc,
		client:     client,
		operator:   op,
		authScheme: cred.Scheme,
	}, nil
}

// extractOperator unwraps a credential carrying a *OperatorCredential payload.
func extractOperator(c credential.Credential) (*OperatorCredential, error) {
	switch v := c.Opaque.(type) {
	case *OperatorCredential:
		if v == nil || v.Operator == "" || v.Password == "" {
			return nil, fmt.Errorf("upyun driver: OperatorCredential missing Operator or Password")
		}
		return v, nil
	case OperatorCredential:
		if v.Operator == "" || v.Password == "" {
			return nil, fmt.Errorf("upyun driver: OperatorCredential missing Operator or Password")
		}
		return &v, nil
	default:
		return nil, fmt.Errorf(
			"upyun driver: %s requires *upyun.OperatorCredential opaque payload, got %T",
			c.Scheme, c.Opaque,
		)
	}
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
