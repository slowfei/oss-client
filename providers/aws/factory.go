// Package aws is the native AWS S3 provider driver for the universal
// object-storage Go SDK. It implements pkg/uos.Client by translating
// the unified API into aws-sdk-go-v2/service/s3 calls.
//
// The driver targets real AWS S3 by default (virtual-host endpoint,
// SigV4 region-aware) and supports S3-compatible targets (MinIO, Alibaba
// OSS S3-compatible endpoints, etc.) via Config.Endpoint plus
// Config.DriverConfig.PathStyle when the target requires path-style. The aws-sdk-go-v2
// internal retryer is disabled at construction time so retries are
// driven solely by pkg/uos.RetryPolicy (avoiding the documented
// double-retry pitfall in docs/provider_roadmap.md).
//
// Multipart upload is implemented via raw S3 multipart primitives
// (CreateMultipartUpload + UploadPart + CompleteMultipartUpload), not
// via the s3manager.Uploader helper or pkg/uos/transfer.Manager. This
// answers half of ADR Follow-up #1: pkg/uos/transfer.Manager is
// BYPASSED in v0.1; promotion to a unified Uploader interface is
// scheduled for v0.2 once at least two providers have shipped multipart.
package aws

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/maqian/oss-client/pkg/uos"
	"github.com/maqian/oss-client/pkg/uos/credential"
)

// providerID is the canonical id this Factory handles. Mirrors the
// "aws" cell in docs/provider_matrix.md.
const providerID uos.Provider = "aws"

// DriverConfig is the AWS-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All
// fields are optional; the zero value yields a working virtual-host
// AWS S3 driver as long as Region is set on uos.Config.
type DriverConfig struct {
	// PathStyle forces path-style addressing (bucket in URL path rather
	// than virtual-host subdomain). Leave false for virtual-host S3-
	// compatible endpoints such as Alibaba OSS's
	// s3.oss-<region>.aliyuncs.com. Set true for targets such as MinIO
	// that require path-style addressing.
	PathStyle bool
	// DisableHTTPS, when true, allows HTTP-only endpoints. Honoured by
	// the EndpointResolverV2 when uos.Config.Endpoint is non-empty.
	DisableHTTPS bool
	// AccelerateEndpoint enables S3 Transfer Acceleration. Real-AWS
	// only; ignored when uos.Config.Endpoint is set.
	AccelerateEndpoint bool
}

// Factory returns a uos.Factory for the AWS S3 driver. Drivers register
// themselves at init time (or callers may register manually):
//
//	uos.DefaultRegistry().Register(awsdrv.Factory())
func Factory() uos.Factory { return &factoryImpl{} }

// factoryImpl is the concrete uos.Factory for AWS S3.
type factoryImpl struct{}

// Provider returns the canonical provider id ("aws").
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. Region is mandatory; an Endpoint override is permitted
// for S3-compat targets. DriverConfig, when non-nil, must be a
// *DriverConfig.
func (factoryImpl) Validate(cfg uos.Config) error {
	if cfg.Provider != "" && cfg.Provider != providerID {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   fmt.Sprintf("Config.Provider=%q does not match factory id %q", cfg.Provider, providerID),
		}
	}
	if cfg.Region == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Region is required (AWS SigV4 needs a region; pass any region for S3-compat endpoints)",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *aws.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	if cfg.Endpoint != "" {
		if _, err := url.Parse(cfg.Endpoint); err != nil {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("invalid Endpoint URL: %v", err),
			}
		}
	}
	return nil
}

// Open constructs the underlying *s3.Client and returns a *driverImpl
// bound to it. The aws-sdk-go-v2 internal retryer is replaced with a
// single-attempt retryer; pkg/uos.RetryPolicy is the sole retry surface
// (per docs/provider_roadmap.md cross-cutting risk: "double-retry").
func (f factoryImpl) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}
	dc, _ := cfg.DriverConfig.(*DriverConfig)
	if dc == nil {
		dc = &DriverConfig{}
	}
	pathStyle := dc.PathStyle

	credsProvider, err := buildCredentialsProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}

	awsCfg := awsv2.Config{
		Region:      cfg.Region,
		Credentials: credsProvider,
		// Disable the SDK's internal retry layer; pkg/uos.RetryPolicy is
		// the single source of truth for retry behaviour. retry.AddWithMaxAttempts
		// can re-enable it later if requested via DriverConfig.
		RetryMaxAttempts: 1,
		Retryer: func() awsv2.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 1
			})
		},
		// MinIO and other S3-compatibles still require Content-MD5 on
		// DeleteObjects; "WhenRequired" ensures the SDK computes it
		// without forcing it on every request.
		RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = pathStyle
		o.UseAccelerate = dc.AccelerateEndpoint && cfg.Endpoint == ""
		// Belt-and-suspenders: ensure no s3-level retry layer either.
		o.Retryer = retry.NewStandard(func(opts *retry.StandardOptions) {
			opts.MaxAttempts = 1
		})
		if cfg.Endpoint != "" {
			o.EndpointResolverV2 = &staticEndpointResolver{
				endpoint:     cfg.Endpoint,
				disableHTTPS: dc.DisableHTTPS,
			}
		}
	})

	presignClient := s3.NewPresignClient(s3Client)

	return &driverImpl{
		cfg:       cfg,
		s3:        s3Client,
		presigner: presignClient,
	}, nil
}

// buildCredentialsProvider adapts uos.Config.CredentialProvider into an
// aws.CredentialsProvider. Anonymous (nil) callers get
// aws.AnonymousCredentials. HMAC credentials (the only scheme AWS S3
// natively understands) are wrapped in an aws.CredentialsCache so
// per-request lookups stay cheap.
func buildCredentialsProvider(ctx context.Context, cfg uos.Config) (awsv2.CredentialsProvider, error) {
	if cfg.CredentialProvider == nil {
		return awsv2.AnonymousCredentials{}, nil
	}
	// Eagerly resolve once so structural problems surface at Open time
	// rather than on the first request. Subsequent calls re-use the
	// CredentialsProvider's caching path inside aws.CredentialsCache.
	probe, err := cfg.CredentialProvider.Resolve(ctx, string(providerID))
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrUnauthenticated,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "credential provider returned an error",
			Cause:     err,
		}
	}
	if probe.Scheme != credential.AuthHMAC && probe.Scheme != credential.AuthAnonymous {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("AWS S3 driver requires AuthHMAC or AuthAnonymous, got %q", probe.Scheme),
		}
	}
	if probe.Scheme == credential.AuthAnonymous {
		return awsv2.AnonymousCredentials{}, nil
	}
	hmac, ok := probe.Opaque.(*credential.EnvHMACCredential)
	if !ok {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("AWS S3 driver requires *credential.EnvHMACCredential payload, got %T", probe.Opaque),
		}
	}
	staticProv := credentials.NewStaticCredentialsProvider(
		hmac.AccessKeyID,
		hmac.SecretAccessKey,
		hmac.SessionToken,
	)
	// Wrap in CredentialsCache so the SDK's hot path doesn't allocate.
	return awsv2.NewCredentialsCache(staticProv), nil
}

// staticEndpointResolver routes every request to a fixed endpoint
// (used for MinIO and other S3-compatible targets). It implements
// s3.EndpointResolverV2; the resolver is set on the s3.Client.Options
// only when uos.Config.Endpoint is non-empty.
type staticEndpointResolver struct {
	endpoint     string
	disableHTTPS bool
}

// ResolveEndpoint returns the endpoint URL parsed from the configured
// string. For path-style requests the resolver appends the bucket to
// the URL path. For virtual-host requests it prefixes the bucket onto
// the endpoint host.
//
// Note: when UsePathStyle is on, the AWS SDK signals it via the
// EndpointParameters.ForcePathStyle field; we honour it by appending
// the bucket to the resolved endpoint path. When ForcePathStyle is
// false, the bucket is placed in the host for virtual-host addressing
// (for example example-bucket.s3.oss-cn-hangzhou.aliyuncs.com).
func (r *staticEndpointResolver) ResolveEndpoint(ctx context.Context, params s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	raw := r.endpoint
	u, err := url.Parse(raw)
	// url.Parse is permissive: "localhost:9000" parses as Scheme="localhost"
	// Opaque="9000". Treat anything that isn't http/https as a bare
	// host:port and prepend the scheme ourselves.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		scheme := "https"
		if r.disableHTTPS {
			scheme = "http"
		}
		raw = scheme + "://" + raw
		u, err = url.Parse(raw)
		if err != nil {
			return smithyendpoints.Endpoint{}, fmt.Errorf("aws: parse endpoint %q: %w", r.endpoint, err)
		}
	}
	if r.disableHTTPS && u.Scheme == "https" {
		u.Scheme = "http"
	}
	if params.Bucket != nil && *params.Bucket != "" && params.ForcePathStyle != nil && *params.ForcePathStyle {
		u.Path = singleSlashJoin(u.Path, *params.Bucket)
	} else if params.Bucket != nil && *params.Bucket != "" {
		u.Host = *params.Bucket + "." + u.Host
	}
	return smithyendpoints.Endpoint{URI: *u}, nil
}

// singleSlashJoin concatenates base path and segment with exactly one
// "/" between them and a leading "/". Used by the endpoint resolver to
// prepend the bucket name to the endpoint path without doubling slashes.
func singleSlashJoin(base, seg string) string {
	if base == "" {
		return "/" + seg
	}
	if base[len(base)-1] == '/' {
		return base + seg
	}
	return base + "/" + seg
}

// Compile-time guarantees.
var (
	_ uos.Factory               = factoryImpl{}
	_ s3.EndpointResolverV2     = (*staticEndpointResolver)(nil)
	_ awsv2.CredentialsProvider = awsv2.AnonymousCredentials{}
	_                           = errors.Is
)
