package uos

import (
	"io"
	"net/http"
	"time"
)

// This file holds the v1-frozen value types shared across all four
// services (BucketService, ObjectService, MultipartService, Signer).
// Per-service request/response shapes live alongside their consumers
// in request_bucket.go / request_object.go / request_multipart.go /
// request_signer.go.
//
// All types here form part of the public surface; field additions are
// additive-only in v1.x and field removals require a major version bump.

// ----------------------------------------------------------------------
// Shared value types (consumed by every service)
// ----------------------------------------------------------------------

// Metadata is the case-insensitive user-defined metadata map. Keys are
// stored in lower-case form by the SDK; drivers MUST lower-case any
// keys they receive from a vendor before populating Metadata so that
// round-trip equality is well-defined.
//
// User metadata values are vendor-stored as UTF-8 strings; drivers that
// need to hold non-UTF-8 bytes should base64-encode at the call site.
type Metadata map[string]string

// ContentHeaders carries the standard HTTP content negotiation fields
// for an object. All fields are optional; empty values mean "do not
// set" on Put and "vendor-default / not present" on Get/Head.
type ContentHeaders struct {
	// ContentType is the object's MIME type, e.g. "image/png".
	ContentType string
	// ContentEncoding is e.g. "gzip", "br" — see RFC 9110 §8.4.
	ContentEncoding string
	// ContentLanguage is e.g. "en-US" — see RFC 9110 §8.5.
	ContentLanguage string
	// ContentDisposition is e.g. `attachment; filename="x.txt"`.
	ContentDisposition string
	// CacheControl is the Cache-Control header value as written.
	CacheControl string
	// Expires is the absolute expiry instant (HTTP Expires header). Zero
	// value means unset.
	Expires time.Time
}

// Checksum is the unified end-to-end integrity value for an object.
// At most one of MD5 / SHA256 / CRC32C / CRC64NVME is set; Type names
// which one. Empty Type means "no checksum supplied / available".
//
// Algorithm choice is a vendor concern: AWS exposes CRC32C / CRC64NVME
// as preferred; GCS exposes CRC32C; Azure exposes MD5. Drivers SHOULD
// pick the strongest algorithm the vendor supports and SHOULD verify
// on Get when a checksum is present.
type Checksum struct {
	// Type is one of "md5", "sha256", "crc32c", "crc64nvme". Lower-case.
	Type string
	// Value is the raw bytes of the checksum (NOT hex-encoded). Drivers
	// hex- or base64-encode for the wire as the vendor requires.
	Value []byte
}

// BucketInfo is the unified bucket descriptor returned by Stat / List.
type BucketInfo struct {
	// Name is the bucket name as the vendor sees it.
	Name string
	// Region is the bucket's home region (vendor-defined identifier).
	Region string
	// CreatedAt is the bucket creation time, when the vendor exposes it.
	CreatedAt time.Time
	// Extra holds vendor-specific raw fields that did not map onto a
	// unified property. Drivers populate this lazily; callers consult
	// Client.As(target) for typed access to vendor SDK objects.
	Extra map[string]string
}

// ObjectInfo is the unified object descriptor returned by Head / List.
type ObjectInfo struct {
	// Bucket is the bucket the object lives in.
	Bucket string
	// Key is the object key, byte-for-byte as supplied (no normalisation).
	Key string
	// Size is the content length in bytes; -1 when the size is unknown
	// (e.g. a partial multipart upload listing).
	Size int64
	// ETag is the vendor-reported entity tag. Format is vendor-defined;
	// drivers MUST NOT auto-promote ETag to MD5 (architecture_plan §2.3).
	ETag string
	// LastModified is the last-modified time as reported by the vendor.
	LastModified time.Time
	// StorageClass is the vendor-defined storage class (e.g. "STANDARD",
	// "ARCHIVE"). Empty when the vendor does not expose one.
	StorageClass string
	// VersionID is the version identifier when bucket versioning is on.
	VersionID string
	// Content carries the standard content headers when known.
	Content ContentHeaders
	// Metadata is the user-defined metadata; lower-cased keys.
	Metadata Metadata
	// Checksum is the integrity value when the vendor reports one.
	Checksum Checksum
	// Extra preserves raw vendor headers/fields not mapped above.
	Extra map[string]string
}

// ObjectReader is the streaming response type for Get. Body is the raw
// payload stream; callers MUST Close it. ContentLength reflects the
// number of bytes available on Body (range-aware: equals the length of
// the requested slice when a Range was specified).
type ObjectReader struct {
	// Body is the response payload stream. Always non-nil on success;
	// callers MUST Close it (typically via defer).
	Body io.ReadCloser
	// ContentLength is the number of bytes that will be read from Body.
	// -1 if the vendor did not report a length.
	ContentLength int64
	// Info is a populated ObjectInfo for the returned object (the same
	// shape Head would have returned).
	Info ObjectInfo
}

// ObjectList is the unified List response. Items is the page of objects;
// CommonPrefixes carries the "directory" rollup when Delimiter was set.
// NextToken is the opaque continuation token; Truncated is true iff
// more pages remain.
type ObjectList struct {
	// Items is the page of objects matching the request.
	Items []ObjectInfo
	// CommonPrefixes is the rollup set returned when Delimiter was set.
	CommonPrefixes []string
	// NextToken is an opaque cursor; pass back as ListObjectsRequest.ContinuationToken.
	NextToken string
	// Truncated is true iff more pages remain. Callers should pass NextToken back to retrieve them.
	Truncated bool
}

// SignedURL is the unified return type for Signer.SignURL. It is a
// pure URL grant; non-URL grants (Azure SAS-as-token, Qiniu Upload
// Token, Upyun FORM) use DirectGrant instead.
type SignedURL struct {
	// URL is the absolute URL the caller should hit.
	URL string
	// Method is the HTTP method the URL is bound to (e.g. "GET", "PUT").
	Method string
	// ExpiresAt is the absolute moment after which the URL is rejected.
	ExpiresAt time.Time
	// Headers lists headers the caller MUST set on the wire (e.g.
	// content-type bound into the signature). nil means none.
	Headers http.Header
}

// DirectGrantMode is a v1-frozen typed string identifying which DirectGrant
// shape a Signer returned. The four values cover the four physical
// dispatch shapes the unified Signer supports:
//
//   - DirectGrantModeURL: the grant is fully encoded in URL (Azure SAS-style).
//   - DirectGrantModeForm: the grant is encoded as multipart/form-data
//     fields (Upyun FORM, S3 PostObject policy).
//   - DirectGrantModeToken: the grant is an opaque bearer token the
//     caller passes to a vendor-specific endpoint (Qiniu Upload Token).
//   - DirectGrantModeHeaders: the grant is a set of headers the caller
//     attaches to a normal HTTP request (signed-headers flow).
//
// The string values are pinned by surface_test.go (Stream C / P4) and
// MUST NOT be changed in v1.x.
type DirectGrantMode string

// The four frozen DirectGrantMode values. Adding a fifth requires a
// minor bump on pkg/uos and at least two providers needing the same shape.
const (
	// DirectGrantModeURL means the grant is encoded entirely in DirectGrant.URL.
	DirectGrantModeURL DirectGrantMode = "url"
	// DirectGrantModeForm means the grant is the FormFields map; the
	// caller submits these as multipart/form-data to DirectGrant.URL.
	DirectGrantModeForm DirectGrantMode = "form"
	// DirectGrantModeToken means the grant is the Token string; the
	// caller carries it as a vendor-defined bearer token.
	DirectGrantModeToken DirectGrantMode = "token"
	// DirectGrantModeHeaders means the grant is the Headers map; the
	// caller attaches them to a normal HTTP request to DirectGrant.URL.
	DirectGrantModeHeaders DirectGrantMode = "headers"
)

// DirectGrant unifies non-URL-shaped time-bounded grants. Mode dispatches
// which subset of fields is meaningful. ExpiresAt MUST be set for every Mode.
type DirectGrant struct {
	// Mode names which dispatch shape this grant uses. See DirectGrantMode.
	Mode DirectGrantMode
	// URL is the absolute endpoint the caller submits to. Always set
	// for Mode==URL; usually set for Form / Headers; may be empty for
	// pure Token grants where the caller already knows the endpoint.
	URL string
	// Method is the HTTP method the grant is bound to (e.g. "PUT", "POST").
	Method string
	// Headers carries headers the caller MUST attach. Required for
	// Mode==Headers; optional otherwise.
	Headers http.Header
	// FormFields carries multipart form fields. Required for Mode==Form;
	// nil otherwise.
	FormFields map[string]string
	// Token is the opaque bearer token. Required for Mode==Token; empty otherwise.
	Token string
	// ExpiresAt is the absolute moment after which the grant is rejected.
	ExpiresAt time.Time
}
