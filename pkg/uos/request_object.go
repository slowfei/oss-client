package uos

import (
	"io"
	"time"
)

// Request and response shapes consumed by ObjectService.

// PutObjectRequest is the request shape for ObjectService.Put.
type PutObjectRequest struct {
	// Bucket is the target bucket. Required.
	Bucket string
	// Key is the target object key. Required; supplied byte-for-byte (no normalisation).
	Key string
	// Body is the payload stream. Required and non-nil. Drivers SHOULD
	// avoid buffering the entire body in memory.
	Body io.Reader
	// Size is the body length in bytes. -1 means unknown; drivers
	// dispatch to transfer.Manager's UnknownSizePolicy in that case.
	Size int64
	// Content carries optional content-negotiation headers.
	Content ContentHeaders
	// Metadata is optional user-defined metadata (lower-cased keys).
	Metadata Metadata
	// Checksum is an optional caller-supplied checksum to verify against.
	Checksum Checksum
	// StorageClass is the vendor-defined storage class. Empty means default.
	StorageClass string
	// ACL is the vendor-defined canned ACL. Empty means default.
	ACL string
	// IfMatch is the optional precondition (caller-supplied ETag).
	IfMatch string
	// IfNoneMatch is the optional precondition (caller-supplied ETag, "*" common).
	IfNoneMatch string
}

// PutObjectResult is the response shape for ObjectService.Put.
type PutObjectResult struct {
	// ETag is the vendor-reported entity tag of the stored object.
	ETag string
	// VersionID is the version identifier when bucket versioning is on.
	VersionID string
	// Checksum is the verified integrity value when the vendor returns one.
	Checksum Checksum
	// Extra preserves raw vendor headers/fields not mapped above.
	Extra map[string]string
}

// GetObjectRequest is the request shape for ObjectService.Get.
type GetObjectRequest struct {
	// Bucket is the source bucket. Required.
	Bucket string
	// Key is the source object key. Required.
	Key string
	// VersionID is the optional version to fetch (when versioning is on).
	VersionID string
	// Range is the optional byte range. nil means "whole object".
	Range *ByteRange
	// IfMatch is the optional precondition (caller-supplied ETag).
	IfMatch string
	// IfNoneMatch is the optional precondition (returns 304 / NotModified).
	IfNoneMatch string
	// IfModifiedSince is the optional precondition (returns 304 / NotModified).
	IfModifiedSince time.Time
	// IfUnmodifiedSince is the optional precondition.
	IfUnmodifiedSince time.Time
}

// ByteRange names a half-open byte range for ranged Get requests. End=-1
// means "to end of object".
type ByteRange struct {
	// Start is the first byte to return (0-based, inclusive).
	Start int64
	// End is the last byte to return (inclusive). -1 means "to EOF".
	End int64
}

// HeadObjectRequest is the request shape for ObjectService.Head and
// ObjectService.Exists.
type HeadObjectRequest struct {
	// Bucket is the source bucket. Required.
	Bucket string
	// Key is the source object key. Required.
	Key string
	// VersionID is the optional version to inspect.
	VersionID string
}

// DeleteObjectRequest is the request shape for ObjectService.Delete.
type DeleteObjectRequest struct {
	// Bucket is the source bucket. Required.
	Bucket string
	// Key is the source object key. Required.
	Key string
	// VersionID is the optional version to delete.
	VersionID string
}

// DeleteManyRequest is the request shape for ObjectService.DeleteMany.
// Drivers MAY chunk Keys into vendor-supported batch sizes internally;
// the contract is "delete all of these, report per-key success/failure".
type DeleteManyRequest struct {
	// Bucket is the source bucket. Required.
	Bucket string
	// Keys is the set of keys to delete. Required, non-empty.
	Keys []string
	// Quiet, when true, asks the driver to omit per-key success entries
	// from DeleteManyResult.Deleted (vendor-defined optimisation).
	Quiet bool
}

// DeleteManyResult is the response shape for ObjectService.DeleteMany.
// The contract is partial-success: a non-nil error MAY accompany a
// non-empty Deleted slice.
type DeleteManyResult struct {
	// Deleted is the set of keys the vendor confirmed deleted.
	Deleted []string
	// Failed is the set of keys whose deletion failed, with per-key reason.
	Failed []DeleteFailure
}

// DeleteFailure pairs a key with its vendor-reported failure reason.
type DeleteFailure struct {
	// Key is the object key whose deletion failed.
	Key string
	// Code is the resolved pkg/uos.Code for this per-key failure.
	Code Code
	// Message is the vendor-supplied human-readable detail.
	Message string
}

// CopyObjectRequest is the request shape for ObjectService.Copy.
// Cross-provider copy is NOT a guaranteed primitive (architecture_plan §2.2);
// drivers that don't support cross-bucket copy return ErrUnsupported.
type CopyObjectRequest struct {
	// SourceBucket is the source bucket. Required.
	SourceBucket string
	// SourceKey is the source object key. Required.
	SourceKey string
	// SourceVersionID is the optional source version.
	SourceVersionID string
	// DestBucket is the destination bucket. Required.
	DestBucket string
	// DestKey is the destination object key. Required.
	DestKey string
	// Metadata, when non-nil, replaces the source's user metadata.
	// nil means "copy source metadata verbatim".
	Metadata Metadata
	// Content, when non-zero, replaces the source's content headers.
	Content ContentHeaders
	// MetadataDirective is "COPY" or "REPLACE" — vendor-defined string,
	// empty means "use vendor default".
	MetadataDirective string
	// StorageClass overrides the destination storage class. Empty means default.
	StorageClass string
	// ACL overrides the destination ACL. Empty means default.
	ACL string
	// IfMatch is an optional precondition on the source ETag.
	IfMatch string
	// IfNoneMatch is an optional precondition on the source ETag.
	IfNoneMatch string
}

// CopyObjectResult is the response shape for ObjectService.Copy.
type CopyObjectResult struct {
	// ETag is the destination object's vendor-reported entity tag.
	ETag string
	// VersionID is the destination version id when versioning is on.
	VersionID string
	// LastModified is the destination's last-modified time.
	LastModified time.Time
}

// ListObjectsRequest is the request shape for ObjectService.List.
type ListObjectsRequest struct {
	// Bucket is the source bucket. Required.
	Bucket string
	// Prefix narrows results to keys starting with Prefix.
	Prefix string
	// Delimiter, when set, rolls up keys sharing a prefix into
	// CommonPrefixes (S3-style hierarchical listing). Typical value: "/".
	Delimiter string
	// MaxResults caps the page size. 0 means "use vendor default".
	MaxResults int
	// ContinuationToken is the opaque cursor returned by a prior call.
	// Empty means "start from the beginning".
	ContinuationToken string
	// StartAfter is the optional "list keys lexicographically after this one" hint.
	StartAfter string
}
