// Package credential defines the pluggable credential resolution surface
// shared by all provider drivers.
//
// This package is a leaf in the dependency graph: it MUST NOT import
// pkg/uos. The Provider.Resolve target parameter is plain string (the
// provider id, e.g. "aws", "alibaba") rather than uos.Provider in order
// to avoid an import cycle while still letting a single Provider serve
// multiple drivers from one credential chain.
package credential

import (
	"context"
	"time"
)

// AuthScheme tags how a Credential is meant to be consumed by a driver.
// Drivers type-assert Credential.Opaque to their expected concrete type
// based on Scheme; callers should not invent new schemes — extending
// this enum is a minor version bump on pkg/uos/credential.
type AuthScheme string

// The six authentication schemes recognised in v1. See
// architecture_plan §4.7 for semantics.
const (
	// AuthAnonymous is "no credential"; used by public buckets / anonymous gets.
	AuthAnonymous AuthScheme = "anonymous"
	// AuthHMAC covers AWS SigV4 / Aliyun / Tencent / Huawei / Volcengine HMAC families.
	AuthHMAC AuthScheme = "hmac"
	// AuthOAuth2 covers Google Cloud Storage and any OAuth2 bearer-token flow.
	AuthOAuth2 AuthScheme = "oauth2"
	// AuthSharedKey covers Azure Storage shared-key authentication.
	AuthSharedKey AuthScheme = "shared_key"
	// AuthSAS covers Azure Shared Access Signature URLs.
	AuthSAS AuthScheme = "sas"
	// AuthCustom covers vendor-specific schemes (e.g. Qiniu Upload Token, Upyun FORM).
	AuthCustom AuthScheme = "custom"
)

// Credential is an opaque vendor-typed bundle plus metadata. The driver
// type-asserts Opaque to its expected concrete type based on Scheme
// (e.g. *aws.Credentials, *azblob.SharedKeyCredential, qiniu.UploadToken).
type Credential struct {
	// Scheme tells the driver how to interpret Opaque.
	Scheme AuthScheme
	// ExpiresAt is the absolute moment after which the credential is no
	// longer valid. nil means the credential does not expire (static keys)
	// or the expiry is unknown (Provider should refresh on auth failure).
	ExpiresAt *time.Time
	// Opaque carries the vendor-typed credential payload.
	Opaque any
}

// Provider resolves a Credential for a target provider. Implementations
// MUST be safe for concurrent use and SHOULD cache + refresh-before-expiry
// internally so that drivers can call Resolve on every request without
// performance impact.
//
// The target parameter is the provider id (e.g. "aws", "alibaba") as a
// plain string to avoid an import cycle with pkg/uos. A single Provider
// implementation MAY serve multiple drivers by switching on target.
type Provider interface {
	// Resolve returns a Credential valid for the named target provider.
	// The returned Credential.Scheme MUST be one the target driver knows
	// how to consume; mismatch is an authentication-time error.
	Resolve(ctx context.Context, target string) (Credential, error)
}
