package uos

import (
	"net/http"
	"time"
)

// Request and response shapes consumed by Signer.

// SignURLRequest is the request shape for Signer.SignURL. ExpiresIn is
// the duration from "now" the URL remains valid; drivers convert this
// to the vendor's expiry encoding.
type SignURLRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required.
	Key string
	// Method is the HTTP method to sign for ("GET", "PUT", "HEAD", "DELETE").
	// Drivers reject unsupported methods with ErrInvalidArgument.
	Method string
	// ExpiresIn is the validity duration counted from request time.
	// Must be > 0; drivers MAY clamp to vendor maximums.
	ExpiresIn time.Duration
	// Headers, when non-nil, lists headers the resulting URL is bound to;
	// the caller MUST attach the same headers when issuing the request.
	Headers http.Header
	// Query, when non-empty, adds vendor-specific query parameters that
	// participate in the signature (e.g. response-content-disposition).
	Query map[string]string
	// VersionID is the optional version to sign for (when versioning is on).
	VersionID string
}

// DirectGrantOperation names which operation a DirectGrant authorises.
// It is plain string so callers don't need a constants enum; drivers
// recognise vendor-meaningful values such as "upload" and "download".
type DirectGrantOperation string

// The vendor-neutral DirectGrant operation values. Drivers MAY accept
// additional vendor-specific values; callers SHOULD prefer these.
const (
	// DirectGrantUpload requests a grant for object upload (PUT/POST).
	DirectGrantUpload DirectGrantOperation = "upload"
	// DirectGrantDownload requests a grant for object download (GET).
	DirectGrantDownload DirectGrantOperation = "download"
)

// DirectGrantRequest is the request shape for Signer.IssueDirectGrant.
type DirectGrantRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required for object-scoped grants;
	// some grants are bucket-scoped and accept an empty Key.
	Key string
	// Operation names the high-level intent of the grant.
	Operation DirectGrantOperation
	// ExpiresIn is the validity duration counted from request time. Must be > 0.
	ExpiresIn time.Duration
	// MaxBytes, when > 0, caps the size of any object created via this grant.
	MaxBytes int64
	// ContentType, when non-empty, restricts the Content-Type of any
	// object created via this grant.
	ContentType string
	// Metadata, when non-nil, fixes user-defined metadata that any
	// object created via this grant must carry. Lower-cased keys.
	Metadata Metadata
	// Extra carries vendor-specific extension fields (Qiniu policy
	// overrides, Upyun callback config, etc.). Drivers ignore unknown keys.
	Extra map[string]string
}
