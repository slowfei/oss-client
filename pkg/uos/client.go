// Package uos defines the unified object storage public API. Every
// provider driver under providers/<name> implements the Client
// interface declared here; business code consumes Client without ever
// importing a vendor SDK.
//
// See docs/architecture_plan.md §1 for the binding entity inventory and
// §3.3 for the dependency direction (pkg/uos MUST NOT import any
// providers/<name> and MUST NOT import any third-party cloud SDK).
package uos

import (
	"context"

	"github.com/slowfei/oss-client/pkg/uos/capability"
)

// Client is the top-level handle to a provider. Implementations MUST
// be safe for concurrent use; the Buckets / Objects / Multipart /
// Signer accessors typically return value-typed views over a shared
// underlying state.
//
// Lifecycle: a Client is constructed via Registry.Open (or directly via
// Factory.Open) and released via Close. Close is idempotent; calling
// methods after Close yields *Error{Code: ErrInvalidArgument}.
type Client interface {
	// Provider returns the canonical provider id (e.g. "aws", "azure")
	// this Client speaks to.
	Provider() Provider
	// Capabilities returns the Report describing which subset of the
	// unified API this (driver, config, credential) triple actually
	// supports. Drivers MUST populate every key in capability.All().
	Capabilities(ctx context.Context) (capability.Report, error)

	// Buckets returns the BucketService view over this Client.
	Buckets() BucketService
	// Objects returns the ObjectService view over the named bucket.
	Objects(bucket string) ObjectService
	// Multipart returns the MultipartService view over the named bucket.
	Multipart(bucket string) MultipartService
	// Signer returns the Signer view over the named bucket.
	Signer(bucket string) Signer

	// As lets callers reach the underlying SDK client when the unified
	// API is insufficient (vendor-specific operations, advanced tuning,
	// etc.). target MUST be a non-nil pointer to the concrete vendor
	// type (e.g. *s3.Client). Returns true iff target was populated.
	//
	// Callers using As are explicitly opting out of cross-provider
	// portability; the SDK does not promise the underlying type set is
	// stable across vendors.
	As(target any) bool

	// Close releases any resources held by the Client (HTTP transports,
	// credential refresh goroutines, etc.). Idempotent; subsequent
	// method calls return *Error{Code: ErrInvalidArgument}.
	Close() error
}

// BucketService manages buckets within the provider's account namespace.
// Returned by Client.Buckets.
type BucketService interface {
	// List enumerates buckets visible to the configured credential.
	// Pagination is handled by ListBucketsRequest.ContinuationToken; an
	// empty NextToken in the result means the listing is complete.
	List(ctx context.Context, req ListBucketsRequest) ([]BucketInfo, error)
	// Create provisions a new bucket. Returns *Error{Code: ErrAlreadyExists}
	// if the bucket already exists in this namespace.
	Create(ctx context.Context, req CreateBucketRequest) (*BucketInfo, error)
	// Stat returns the BucketInfo for an existing bucket. Returns
	// *Error{Code: ErrNotFound} if the bucket does not exist.
	Stat(ctx context.Context, req StatBucketRequest) (*BucketInfo, error)
	// Delete removes an empty bucket. Returns *Error{Code: ErrConflict}
	// if the bucket is non-empty (vendor-specific).
	Delete(ctx context.Context, req DeleteBucketRequest) error
}

// ObjectService manages objects within a single bucket. Returned by
// Client.Objects(bucket).
//
// Cross-bucket operations (e.g. cross-bucket Copy) take both source and
// destination bucket names in their request structs; the Service is
// bound to the source bucket only by convention.
type ObjectService interface {
	// Put writes an object. Drivers MAY route through transfer.Manager
	// when the body is large enough to warrant multipart; callers who
	// need explicit control should use MultipartService directly.
	Put(ctx context.Context, req PutObjectRequest) (*PutObjectResult, error)
	// Get streams an object. The returned ObjectReader.Body MUST be
	// Closed by the caller.
	Get(ctx context.Context, req GetObjectRequest) (*ObjectReader, error)
	// Head returns the object's metadata without its body. Returns
	// *Error{Code: ErrNotFound} for missing objects.
	Head(ctx context.Context, req HeadObjectRequest) (*ObjectInfo, error)
	// Delete removes a single object. Idempotent: deleting a missing
	// object returns nil error per most vendors.
	Delete(ctx context.Context, req DeleteObjectRequest) error
	// Exists reports whether an object exists. Returns (false, nil) for
	// missing objects (does NOT surface ErrNotFound for the not-found
	// case — that's what Head is for).
	Exists(ctx context.Context, req HeadObjectRequest) (bool, error)
	// DeleteMany removes a batch of objects. The returned
	// DeleteManyResult carries per-key success/failure; a non-nil error
	// MAY accompany a partially-successful result.
	DeleteMany(ctx context.Context, req DeleteManyRequest) (*DeleteManyResult, error)
	// Copy duplicates an object within the same provider. Cross-provider
	// copy is NOT a guaranteed primitive; drivers without server-side
	// support return *Error{Code: ErrUnsupported, Capability: ...}.
	Copy(ctx context.Context, req CopyObjectRequest) (*CopyObjectResult, error)
	// List enumerates objects matching a prefix / delimiter. Pagination
	// is handled by ListObjectsRequest.ContinuationToken.
	List(ctx context.Context, req ListObjectsRequest) (*ObjectList, error)
}

// MultipartService exposes the raw multipart primitives. Most callers
// should prefer the unified upload path (which routes through
// transfer.Manager); MultipartService is for callers that need precise
// control (resumable uploads with custom state stores, parallel
// uploaders, etc.) or for the testkit's orphan-cleanup case.
type MultipartService interface {
	// Initiate starts a multipart upload and returns the handle that
	// UploadPart / Complete / Abort all reference.
	Initiate(ctx context.Context, req InitiateMultipartRequest) (*MultipartUpload, error)
	// UploadPart uploads a single part. Drivers SHOULD verify the part
	// length matches req.Size and reject mismatches as ErrInvalidArgument.
	UploadPart(ctx context.Context, req UploadPartRequest) (*UploadedPart, error)
	// Complete finalises the upload by stitching the supplied parts in
	// order. Parts MUST be presented sorted by PartNumber ascending.
	Complete(ctx context.Context, req CompleteMultipartRequest) (*PutObjectResult, error)
	// Abort cancels an in-flight upload and asks the vendor to release
	// any partial-part storage. Idempotent; aborting a non-existent
	// upload returns nil error per most vendors.
	Abort(ctx context.Context, req AbortMultipartRequest) error
	// List enumerates in-flight multipart uploads in the bucket.
	// Required for orphan cleanup; see docs/provider_roadmap.md.
	List(ctx context.Context, req ListMultipartUploadsRequest) (*MultipartUploadList, error)
}

// Signer issues time-bounded grants. URL-shaped grants (signed URLs)
// and non-URL-shaped grants (Azure SAS tokens, Qiniu Upload Tokens,
// Upyun FORM payloads) are the two unified shapes; see DirectGrant.Mode
// for the dispatch.
type Signer interface {
	// SignURL returns a SignedURL the caller can hand to an HTTP client.
	// Drivers reject unsupported methods with
	// *Error{Code: ErrInvalidArgument}.
	SignURL(ctx context.Context, req SignURLRequest) (*SignedURL, error)
	// IssueDirectGrant returns a non-URL-shaped grant (or, for some
	// vendors, a URL-shaped grant via DirectGrant.Mode == DirectGrantModeURL).
	IssueDirectGrant(ctx context.Context, req DirectGrantRequest) (*DirectGrant, error)
}
